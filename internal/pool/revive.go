// revive.go 「临时禁用 → 自动重试」策略。
//
// 背景（用户提「加临时禁用自动重试以及原因提示手动解封」）：
// 改动前 Disable 是**一刀切**的 —— 一旦禁用，`RunBalanceRefreshNow` 的循环直接
// `continue` 跳过（"禁用账号不刷新"），**永不自动恢复**，只能人工点面板「解冻」。
//
// ⚠️ 该跳过已于本次改动移除：禁用账号现在与普通账号一起刷新余额，本恢复路径
// 因此有了可用节拍（它挂在余额刷新循环里）。禁用语义见
// internal/scheduler/disabled.go ——「禁用只停选号(chat)，不停账号维护/增长」。
//
// 三种禁用原因的**可恢复性完全不同**：
//
// reason                        性质          自动重试？
// ─────────────────────────────────────────────────────────────────
// manual disable (panel)        用户意图      ❌ 绝不（否则违背用户操作）
// 12153 session dead            连续3次会话失效 ⚠️ **值得试一次**
//
//	（可能网络抖动/   （临时性故障占相当比例：
//	 上游闪断）        源码注释记载"13 个 disabled 号
//	                 全部 refresh 成功，是历史误判受害者"）
//
// refresh session dead          refreshToken 失效 ❌ 需重新登录
// account banned (11140)        上游封号       ❌ 需重新登录
//
// 设计原则（对齐 dev-principles §十：不得产出与事实矛盾的展示）：
//   - **区分「可恢复」与「需人工」**，而不是笼统"禁用了"；
//   - 面板必须**说明原因 + 下一步动作**（"需重新登录"vs"将自动重试"），
//     否则用户看到一个"已禁用"不知道该干什么。
//   - **人工禁用永不自动解** —— 那是明确的用户意图。
package pool

import (
	"strings"
	"time"
)

// DisableKind 禁用性质（决定能否自动重试 + 面板文案）。
type DisableKind string

const (
	// DisableManual 人工禁用（面板点击）。**永不自动恢复** —— 违背用户意图。
	DisableManual DisableKind = "manual"
	// DisableTransient 临时性禁用（会话连续失效，疑网络/上游抖动）。到期自动重试。
	DisableTransient DisableKind = "transient"
	// DisableRelogin 需重新登录（refreshToken 失效 / 上游封号）。自动重试无意义：
	// token 已不可用，放回池子只会继续失败，必须人工重新登录换新凭证。
	DisableRelogin DisableKind = "relogin"
)

// transientDisableTTL 临时禁用的自动重试冷却期。
//
// 为何取 30 分钟：上游故障（闪断/限流风暴）通常分钟级恢复；太短会反复无效重试
// 放大失败，太长则错过恢复窗口。且重试**只试一次**（见 RetryTransientOnce），
// 失败则回到禁用态并升级为「需重新登录」，避免无限循环。
const transientDisableTTL = 30 * time.Minute

// classifyDisable 由 reason 字符串判定禁用性质。
//
// 为何按 reason 文本判定而非新增字段：reason 是既有的持久化字段，历史数据
// （旧 state.json）只有它。新增字段会让旧数据落在"未知"分支 → 行为退化。
// 文本判定则对历史数据同样有效（这些文案是代码里写死的，未改动过）。
func classifyDisable(reason string) DisableKind {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.HasPrefix(r, "manual"):
		return DisableManual
	case strings.Contains(r, "11140"), strings.Contains(r, "banned"),
		strings.Contains(r, "re-login"), strings.Contains(r, "refresh session dead"):
		return DisableRelogin
	case strings.Contains(r, "12153"), strings.Contains(r, "session dead"):
		return DisableTransient
	default:
		// 未知原因：保守归入「需人工」——不自动恢复比误恢复安全
		// （误恢复一个真失效的号会持续产生失败请求）。
		return DisableRelogin
	}
}

// DisableInfo 禁用的完整说明（供面板展示"该怎么办"）。
type DisableInfo struct {
	Kind DisableKind `json:"kind"`
	// Hint 人话说明「下一步该做什么」—— 面板直接显示，不要求用户理解内部语义。
	Hint string `json:"hint"`
	// AutoRetryAt 非零 = 将于该时刻自动重试（仅 transient）。
	AutoRetryAt time.Time `json:"auto_retry_at,omitempty"`
}

// describeDisable 生成禁用说明（面板文案的单一事实来源）。
func describeDisable(reason string, disabledAt time.Time) DisableInfo {
	k := classifyDisable(reason)
	switch k {
	case DisableManual:
		return DisableInfo{Kind: k, Hint: "人工禁用 · 在面板点「解冻」恢复"}
	case DisableTransient:
		at := time.Time{}
		if !disabledAt.IsZero() {
			at = disabledAt.Add(transientDisableTTL)
		}
		hint := "会话连续失效（疑网络/上游抖动）"
		if !at.IsZero() {
			hint += " · 将于 " + at.Format("01-02 15:04") + " 自动重试一次"
		} else {
			hint += " · 将自动重试一次"
		}
		return DisableInfo{Kind: k, Hint: hint, AutoRetryAt: at}
	case DisableRelogin:
		return DisableInfo{Kind: k, Hint: "凭证已失效 · 需重新登录（面板「添加账号」用同一账号登录）"}
	}
	return DisableInfo{Kind: k, Hint: "需人工处理"}
}

// DisabledAt 返回账号的禁用时刻（未禁用返回零值）。调用方无需持锁。
func (p *Pool) DisabledAt(uid string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		return e.disabledAt
	}
	return time.Time{}
}

// TransientRetryCandidates 返回「临时禁用且已过冷却期」的账号 uid，供调用方
// 逐个**验证并恢复**（不是直接解禁 —— 见下方说明）。
//
// 为何不在这里直接解禁：临时禁用的成因是"会话连续失效"，**是否真恢复只有
// refresh 才知道**。若直接解禁放回池子，第一次请求大概率仍失败，反而：
//   - 污染成功率统计；
//   - 若那是真的凭证失效，会再触发一次禁用（无意义的抖动）。
//
// 故本函数只**挑候选**，由调用方 refresh 成功后再 Revive（见 scheduler 的调用）。
//
// 只返回 DisableTransient 的账号：人工禁用不得自动解（用户意图）；
// 需重登类（refreshToken 失效/封号）refresh 必然失败，试也无用。
func (p *Pool) TransientRetryCandidates(now time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for uid, e := range p.byUID {
		if !e.disabled {
			continue
		}
		if classifyDisable(e.reason) != DisableTransient {
			continue
		}
		if e.disabledAt.IsZero() || now.Sub(e.disabledAt) < transientDisableTTL {
			continue // 冷却期未到
		}
		out = append(out, uid)
	}
	return out
}

// NoteTransientRetryFailed 记录一次「临时禁用重试失败」：把禁用性质升级为
// DisableRelogin（改写 reason），使其**不再被自动重试** —— 避免每 30 分钟
// 无限重试一个确认失效的账号，同时让面板提示改为"需重新登录"。
//
// 这是"试一次"语义的落地：试过不行就转人工，不反复骚扰上游。
func (p *Pool) NoteTransientRetryFailed(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.disabled {
		return
	}
	if classifyDisable(e.reason) != DisableTransient {
		return
	}
	e.reason = "retry failed, re-login required (was: " + e.reason + ")"
	e.disabledAt = time.Now() // 刷新时刻，避免下次立刻又被选中
	p.dirty.Store(true)
}
