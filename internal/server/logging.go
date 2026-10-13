// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// timing 上游分段耗时（纯观测，见 upstream/timing.go）。零值 = 未采集/未测量。
	timing upstream.ChatTiming

	// 请求级指标（reqlog）与扩展流水行所需字段。
	trace     *requestTrace // 请求 trace（nil = reqlog 未启用，流水行保持原格式）
	nick      string        // 账号昵称（随选号同步）；流水行经 logfmt.Label 拼成 "昵称(uid8)"
	outcome   string        // reqlog 结果分类（success/http_error/stream_error/interrupted）
	attempts  int           // 上游尝试次数（换号/降级重试都算）
	credit    float64       // 本次真实扣费积分
	hasCredit bool
	// promptTokens/completionTokens/totalTokens 供 reqlog.Event 记录用量。
	promptTokens     int64
	completionTokens int64
	totalTokens      int64

	logged bool
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
// 经 context 传递（而非闭包捕获）：handler 内部有流式/非流式/错误多个 return 路径，
// 逐个改 return 既易漏又侵入；context 让出口统一在一处。
type requestTrace struct {
	id    string
	start time.Time
	stat  *chatStat
}

// requestTraceFrom 从请求 context 取 trace（无则 nil，调用方需判空）。
func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

// event 把本次请求的统计快照成 reqlog.Event（**全部脱敏**：账号只留
// "昵称(uid8)" 标签，无完整 UID、无提示词、无响应正文、无凭证）。
// 该函数是脱敏边界的单一事实来源 —— 只允许从 chatStat 的**元数据字段**取，
// 任何"把 body/header 记进来"的改动都会让 TestRequestMetricsArchiveIsSanitized 失败。
func (t *requestTrace) event(status int) reqlog.Event {
	e := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      "/v1/chat/completions",
		Status:    status,
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		// 0 耗时会被读成"瞬时完成"；最小时长 1ms 更诚实（也便于前端排序）。
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.Account = logfmt.Label(s.uid, s.nick)
		e.Model = s.model
		e.Outcome = s.outcome
		if s.outcome == "" {
			// 未显式标记结果时按状态码兜底分类（错误路径可能提前 return，
			// 来不及标记 outcome —— 兜底比丢失更可靠）。
			e.Outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				e.Outcome = reqlog.OutcomeSuccess
			}
		}
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
		if s.toks >= 0 {
			e.CompletionTokens = int64(s.toks)
			if e.TotalTokens == 0 {
				e.TotalTokens = int64(s.toks)
			}
		}
	}
	e.OK = e.Status >= 200 && e.Status < 300
	return e
}

// responseObserver 包一层 ResponseWriter 以捕获最终状态码。
// 为什么需要：流式响应一旦 Write 就无法再改状态码，而指标要记录**实际**发出的码
// （handler 内部可能已 WriteHeader 后继续流式输出）。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 writer（否则 Flush/SetWriteDeadline 失效）。
func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	// 有请求 trace（即 reqlog 启用）时走扩展行：多出 rid/out/try/credit 四个字段，
	// 让流水行能直接对上面板的请求指标与 JSONL 归档。
	// ⚠️ 无 trace 时**保持原格式完全不变**（老部署/测试的日志解析零影响）。
	if s.trace != nil {
		logChatRowEx(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
			s.timing, s.trace.id, s.outcome, s.attempts, s.credit, s.hasCredit)
		return
	}
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks, s.timing)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit bool
	credit    float64
	// 前缀缓存命中/未命中 token 数（费用观测核心维度）。
	// 为什么要观测：网关**已注入 prompt_cache_key** 为省费用（见 cache_key.go），
	// 但此前看不到它到底有没有命中 —— 命中与不命中的费用差可达 **17 倍**。
	cacheHitTokens  int
	hasCacheHit     bool
	cacheMissTokens int
	hasCacheMiss    bool
	errorFrame      bool   // 流中透传过 SSE error 帧（HTTP 200 但内容失败）
	pend            []byte // 已读未返回的行缓存
}

// SawErrorFrame 报告流中是否透传过 SSE error 帧（供 reqlog 分类 outcome）。
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// CacheHit / CacheMiss 返回前缀缓存命中/未命中 token 数（缺失时 ok=false）。
func (s *chatStatsReader) CacheHit() (int, bool)  { return s.cacheHitTokens, s.hasCacheHit }
func (s *chatStatsReader) CacheMiss() (int, bool) { return s.cacheMissTokens, s.hasCacheMiss }

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
		HasCacheHitTokens:   s.hasCacheHit,
		CacheHitTokens:      int64(s.cacheHitTokens),
		HasCacheMissTokens:  s.hasCacheMiss,
		CacheMissTokens:     int64(s.cacheMissTokens),
	}
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
		Usage *struct {
			PromptTokens     *int     `json:"prompt_tokens"`
			CompletionTokens *int     `json:"completion_tokens"`
			TotalTokens      *int     `json:"total_tokens"`
			Credit           *float64 `json:"credit"`
			// 前缀缓存命中/未命中（费用观测核心维度，见 upstream/cache_key.go：
			// 同一段 8k 前缀带 cache key 后 credit 从 0.34 → 0.02，**降约 17×**）。
			// 上游字段名以 prompt_cache_hit_tokens / prompt_cache_miss_tokens 为准；
			// 部分形态用 cache_hit_tokens 短名，两个都收（优先长名）。
			CacheHitTokens      *int `json:"prompt_cache_hit_tokens"`
			CacheHitTokensShort *int `json:"cache_hit_tokens"`
			CacheMissTokens     *int `json:"prompt_cache_miss_tokens"`
		} `json:"usage"`
	}
	// SSE error 帧检测（纯观测，供 reqlog 分类 outcome）：
	// 上游把失败以 "data: {...,"error":...}" 透传时，HTTP 状态仍是 200，
	// 只看状态码会把失败流记成成功。
	if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Error) > 0 {
		s.errorFrame = true
	}
	if chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
	// 缓存命中数：长名优先，短名兜底（同一字段的两种上游形态）。
	if chunk.Usage.CacheHitTokens != nil {
		s.hasCacheHit = true
		s.cacheHitTokens = *chunk.Usage.CacheHitTokens
	} else if chunk.Usage.CacheHitTokensShort != nil {
		s.hasCacheHit = true
		s.cacheHitTokens = *chunk.Usage.CacheHitTokensShort
	}
	if chunk.Usage.CacheMissTokens != nil {
		s.hasCacheMiss = true
		s.cacheMissTokens = *chunk.Usage.CacheMissTokens
	} else if s.hasCacheHit && s.hasPromptTokens {
		// miss 缺失时按 prompt - hit 推导（上游不总是显式给 miss）。
		// 推导值为负说明两字段口径不一致 → 视为不可信，不给该维度（宁缺勿滥）。
		if d := s.promptTokens - s.cacheHitTokens; d >= 0 {
			s.hasCacheMiss = true
			s.cacheMissTokens = d
		}
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
//
// timing 为上游分段耗时（见 upstream/timing.go）。语义澄清（本次排查的核心产出）：
// 本行 TTFB 的口径是「首个 SSE data 帧到达 − 请求进 handler」，**包含**选号、
// 建连、本地请求体准备、上游首字节、SSE 解析——不等于「上游首字节」，直接拿它
// 与 curl time_starttransfer 对比会系统性偏高（实测差 ~0.5s）。分段字段用于
// 把这段差拆开：
//
//	seg=prepNms/doNms — 本地准备 / 上游 Do（含建连+首字节）耗时
//	×N                — 上游尝试次数（>1 表示 global 404/405 fallback 重试）
//
// 不变量（upstream.ChatTiming.Total() ≤ 本行 total）：分段合计必然不超过总耗时，
// 差值即 handler 自身开销。若违反则该观测本身不可信——见 timing_test.go 断言。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int, timing upstream.ChatTiming) {
	logChatRowLine(ttfb, total, model, mode, uidPrefix(uid), status, toks, timing, "")
}

// logChatRowLine 是 logChatRow / logChatRowEx 的公共打印体。
//   - uidLabel 已由调用方决定形态（logChatRow 传 uidPrefix，扩展行走昵称标签）；
//   - extra 为空时输出**与改造前逐字节相同**的行（零回归）；
//   - 非空时追加在行尾（扩展字段），不改变既有字段的位置与格式。
func logChatRowLine(ttfb, total time.Duration, model, mode, uidLabel string, status int, toks int,
	timing upstream.ChatTiming, extra string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		// 与 handler 的 recordAttempt 共用同一函数（tokensPerSecond）——本行本来就
		// 收到了 ttfb 却没用它算速率，导致**流水行的 tok/s 与面板数字对不上**
		// （面板扣了 TTFB、流水行没扣，同一请求两个数）。共用即根治该漂移。
		if tps, ok := tokensPerSecond(int64(toks), total, ttfb); ok {
			tokpsField = fmt.Sprintf("%.1f", tps)
		} else if total > 0 {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	// 分段字段：仅在采集到数据时展示（未采集保持旧行形态，零回归）。
	segField := ""
	if timing.Attempts > 0 {
		segField = fmt.Sprintf(" | seg=prep%dms/do%dms×%d",
			timing.Prepare.Milliseconds(), timing.Do.Milliseconds(), timing.Attempts)
	}
	extraField := ""
	if extra != "" {
		extraField = " " + extra + " |"
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s%s | tok=%s | %stok/s | total=%.1fs |%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidLabel,
		ttfbMS,
		segField,
		tokField,
		tokpsField,
		total.Seconds(),
		extraField,
	)
}

// logChatRowEx 带请求 ID / 结果 / 重试次数 / 扣费的扩展流水行。
//
// 与 logChatRow 的差异仅在**追加**四个字段（rid/out/try/credit），其余格式完全一致 ——
// 刻意不改列宽/对齐体系（那是上游另一套日志重构，与本次指标功能无关），
// 避免牵动本地既有格式与解析测试。
//
// outcome 为空时按状态码兜底分类（错误路径可能提前 return，来不及标记）。
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	timing upstream.ChatTiming, requestID, outcome string, attempts int, credit float64, hasCredit bool) {
	if outcome == "" {
		outcome = reqlog.OutcomeHTTPError
		if status >= 200 && status < 300 {
			outcome = reqlog.OutcomeSuccess
		}
	}
	creditField := "-"
	if hasCredit {
		creditField = fmt.Sprintf("%.4f", credit)
	}
	// 账号列在有昵称时显示 "昵称(uid8)" —— 与面板/归档的 label 口径一致。
	// 昵称为空时 Label 自动退回 uid8（不产生空括号）。
	label := logfmt.Label(uid, nick)
	logChatRowLine(ttfb, total, model, mode, label, status, toks, timing,
		fmt.Sprintf("rid=%s out=%s try=%d credit=%s", requestID, outcome, attempts, creditField))
}
