// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// costExploreInterval costTier 条件探索窗口（SetCostExploreInterval 注入；
	// 默认 defaultCostExploreInterval 30m）。
	//
	// 为什么需要：costTier 是**硬过滤**（只保留最优层），这带来一个必然副作用——
	// 某模型一旦被某号实测为免费（tier 0），其他未知号（tier 1）就永远进不了候选集，
	// 永远拿不到该模型的样本，于是"到底还有几个号在这模型上免费"永远学不到。
	// 本窗口即该副作用的解药：tier 0 垄断 + 存在 tier 1 且距上次探索 ≥ 窗口时，
	// 本次 pick 的生效层切到 tier 1-only —— **搭车改道**把一个既有真实请求改道给
	// 未知号（零新增上游请求，IP 维度零增量，WAF 友好）。
	// 成功 → NoteModelCost 首观测 → 毕业（下一轮 pick 立即按新 tier 生效）；
	// 失败 → 既有错误策略照常，不产生探测风暴。
	//
	// 0 = 关停（完全回到"硬过滤无探索"的原行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 至多 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 仅在探索事件发生时写入（tier 1 枯竭期间停走，陈旧条目无害），不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（透出到面板/状态；pick 写锁内 ++，无需 atomic）。
	costExploreEvents int64
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// pickSeq 单调递增的选号序号：每次 pick 选中账号时自增并记到 entry.usedSeq，
	// 为 LRU 兜底/防惊群提供与 time.Now() 精度无关的严格全序（Windows ~0.5ms 精度下
	// lastUsed 墙钟会全等）。仅 pick 写锁路径读写，无需 atomic。
	pickSeq uint64
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口。用户经 config 显式 "0" 关停
		// （SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// Close 停止后台落盘 goroutine 并做最后一次落盘（幂等）。
// 进程退出前调用，消除 startFlusher 的 goroutine 泄漏；不调用也不影响正确性
// （进程退出即回收），仅是生命周期卫生。
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	p.Flush()
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用）。
//
// ⚠️ 语义与其它 Set* 不同：**0 是有效值**（显式关停探索），故不能沿用"非正值保留原值"
// 的写法——那会让 config 里的 "0" 被当成"没配"而静默启用默认 30m。负值按 0 处理
// （解析侧已钳，这里再兜一次防御）。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.costExploreInterval = d
}

// CostExploreStatus 返回探索台账快照，供 /status 透出（运维据此解释"为什么这次没走最省钱的号"）。
//
// 用读锁而非写锁：本方法是只读语义，且 /status 可能被周期性轮询——不该与 pick 的
// 写锁热路径争抢。返回的 map 是副本，避免调用方持有内部 map 后并发读写。
// interval 一并返回（0 = 关停），让面板能区分"关停了"与"开着但还没探过"。
func (p *Pool) CostExploreStatus() (interval time.Duration, events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		last[k] = ts
	}
	return p.costExploreInterval, p.costExploreEvents, last
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// Remove 从池中移除账号并立即落盘（管理面板用）。返回被移除账号的凭证
// （含 FilePath，供调用方删除 auth 文件）；uid 不存在返回 nil。
// 在途请求的 Release 对已删条目是 no-op，无需等待。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
