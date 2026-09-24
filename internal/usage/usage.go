// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个 (时间片, realm, uid, model) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"` // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`
	UID   string  `json:"u"`
	Model string  `json:"m"`
	Req   int64   `json:"q"`  // 请求数（含失败）
	Err   int64   `json:"e"`  // 失败数
	PT    int64   `json:"p"`  // prompt tokens
	CT    int64   `json:"c"`  // completion tokens
	TT    int64   `json:"t"`  // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`  // 延迟累计（ms）
	LatN  int64   `json:"ln"` // 延迟样本数
	TPS   float64 `json:"v"`  // 吐字速率累计
	TPSN  int64   `json:"vn"` // 速率样本数
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model
	dirty   bool
	started time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	LatencyMs        int64
	HasLatency       bool
	TokensPerSecond  float64
	HasTPS           bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTPS        float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// SeriesPoint 时间序列上的一个点（按小时桶）。
type SeriesPoint struct {
	Slot   string `json:"slot"`   // 2006-01-02T15
	Reqs   int64  `json:"reqs"`   // 成功请求数
	Tokens int64  `json:"tokens"` // prompt + completion
}

// Series 一条「账号 × 模型」的时序（供折线图）。
//
// ⚠️ 可画性：调用方**必须**检查 len(Points) —— 实测 30 个组合中仅 3 个
// 累计到 ≥7 个小时点，其余多为 1~2 点（**画不成折线**，只能显示为孤立点）。
// 故本结构附带 Enough 标志，由前端决定"画线 / 只显示数字"，
// 避免产出"看起来像故障"的单点折线（§十：不得产出无意义的值）。
type Series struct {
	UID    string        `json:"uid"`
	Model  string        `json:"model"`
	Points []SeriesPoint `json:"points"`
	Reqs   int64         `json:"reqs"`   // 窗口内合计
	Tokens int64         `json:"tokens"` // 窗口内合计
	Enough bool          `json:"enough"` // 点数是否够画折线（>= minLinePoints）
}

// minLinePoints 画折线所需的最少点数。
//
// 取 7：3 点连成的"线"几乎看不出趋势（两点定一线，三点只是折角）；
// 参考 buka-wms 的折线判据「≥7 点」（见 plans/frontend.md §14.3.1）。
const minLinePoints = 7

// AccountModelSeries 按「账号 × 模型」返回小时级时序，供用量折线图。
//
// 只返回**有点的组合**；Enough=false 的也返回（前端可降级为"只显示合计数字"，
// 比整条丢掉更有用 —— 用户至少知道"这个号用过这个模型"）。
func (r *Recorder) AccountModelSeries() []Series {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	type key struct{ uid, model string }
	agg := map[key]*Series{}
	order := []key{}
	for _, b := range r.buckets {
		// 只取小时桶（日桶是折叠产物，混入会重复计数）
		if len(b.Scope) < 2 || b.Scope[0] != 'h' {
			continue
		}
		k := key{b.UID, b.Model}
		s, ok := agg[k]
		if !ok {
			s = &Series{UID: b.UID, Model: b.Model}
			agg[k] = s
			order = append(order, k)
		}
		slot := b.Scope[2:] // "2006-01-02T15"
		s.Points = append(s.Points, SeriesPoint{
			Slot: slot, Reqs: b.Req - b.Err, Tokens: b.PT + b.CT,
		})
		s.Reqs += b.Req - b.Err
		s.Tokens += b.PT + b.CT
	}
	out := make([]Series, 0, len(order))
	for _, k := range order {
		s := agg[k]
		// 按时间升序（折线必须按时间排；桶是无序 append 的）
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].Slot < s.Points[j].Slot })
		s.Enough = len(s.Points) >= minLinePoints
		out = append(out, *s)
	}
	// 稳定排序：合计 token 降序（最该看的在前），同值按 uid+model
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		if out[i].UID != out[j].UID {
			return out[i].UID < out[j].UID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Daily 单日单账号的用量（额度评估用）。
type Daily struct {
	Day     string `json:"day"` // 2006-01-02
	UID     string `json:"uid"`
	Reqs    int64  `json:"reqs"`     // 成功请求数
	ErrReqs int64  `json:"err_reqs"` // 失败请求数
	Tokens  int64  `json:"tokens"`   // prompt + completion
}

// DailyUsage 按「日 × 账号」汇总用量，供额度评估（quota.go）反推能力上界。
//
// 为何单独开一个方法而不复用 Snapshot：Snapshot 是**展示视图**（含时序插值、
// 按 realm/model 分组），而额度评估只需要"每天每号用了多少"这一最朴素的口径。
// 复用会让 quota 依赖大量无关结构，且 Snapshot 的时序处理会扭曲日聚合。
func (r *Recorder) DailyUsage() []Daily {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	type key struct{ day, uid string }
	agg := map[key]*Daily{}
	order := []key{}
	for _, b := range r.buckets {
		day := b.Scope
		// 只取小时桶（"h:2006-01-02T15"）汇总到日；日桶（"d:..."）跳过避免重复计算
		if len(day) < 12 || day[0] != 'h' {
			continue
		}
		day = day[2:12]
		k := key{day, b.UID}
		d, ok := agg[k]
		if !ok {
			d = &Daily{Day: day, UID: b.UID}
			agg[k] = d
			order = append(order, k)
		}
		d.Reqs += b.Req - b.Err
		d.ErrReqs += b.Err
		d.Tokens += b.PT + b.CT
	}
	out := make([]Daily, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals    Agg        `json:"totals"`
	ByRealm   []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel   []KeyedAgg `json:"by_model"`
	Series    []Point    `json:"series"`
	Buckets   int        `json:"buckets"`
	FileBytes int64      `json:"file_bytes"`
	Since     string     `json:"since,omitempty"`
	Generated string     `json:"generated"`
}

// Snapshot 聚合当前全部桶。hours 控制时序返回多少个小时点（其余按日折叠）。
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	if hours <= 0 || hours > 24*60 {
		hours = 72
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}

	nowHour := time.Now().Truncate(time.Hour)
	hourFrom := nowHour.Add(-time.Duration(hours-1) * time.Hour)

	for i := range bs {
		b := &bs[i]
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
		// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		scope := strings.TrimPrefix(b.Scope, "h:")
		isHour := strings.HasPrefix(b.Scope, "h:")
		if isHour {
			ts, err := time.ParseInLocation(hourLayout, scope, time.Local)
			if err != nil {
				continue
			}
			if !ts.Before(hourFrom) {
				if hourSeries[scope] == nil {
					hourSeries[scope] = &aggAcc{}
				}
				hourSeries[scope].add(b)
			} else {
				// 超出小时窗口的细粒度数据并入其所在日，避免时序出现空洞。
				d := ts.Format(dayLayout)
				if daySeries[d] == nil {
					daySeries[d] = &aggAcc{}
				}
				daySeries[d].add(b)
			}
		} else {
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
	}

	snap := Snapshot{
		Totals:  total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) { return k, "" }),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:   keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		Buckets:   len(bs),
		Generated: time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// 最早的分片即数据起点。
	if len(snap.Series) > 0 {
		snap.Since = snap.Series[0].T
	}
	return snap
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
