// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、暂停选号(paused)、账号级
// 冷却(until/coolKind)、模型级冷却(modelCooldowns)、熔断(breakerUntil)。维度之间
// 以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口（applyErrorPolicy /
// refresh / keepalive / 签到 / 选号 / 面板 Pause·Resume）对状态的改动都必须经本文件
// 的原语或经 Cooldown/NoteError/NoteSuccess 等封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）；Revive 清
//	paused             ← pauseLocked（Pool.Pause）；resumeLocked / disableLocked / Revive 清
//	until/coolKind     ← Cooldown(CoolSoft/Hard) / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked 清
//	breakerUntil       ← recordBreakerFailureLocked（Cooldown/NoteError 喂入）；NoteSuccess 清
//	softStreak         ← Cooldown(CoolSoft)/CooldownSoftForModel；NoteSuccess/reviveCoolingLocked 清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - paused 与 disabled 正交且**强弱有序**：paused 只置一个标志（不清任何惩罚域），
//     disabled 是更强终态（清冷却域 + 吸收 paused）。故 paused 可随时 resume 回池，
//     disabled 必须 revive/重登。二者叠加态（disabled && paused）按设计不存在。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 与
//     reviveCoolingLocked（签到解冻）共用，二者对冷却域的处置因此永远一致。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，会出现
// 「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个先被
// 硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
//
// 顺带清 paused：禁用是比暂停**更强**的终态（禁用须人工 revive 或重登才可复用，
// 而 paused 随时可 resume）。二者不叠加——否则会出现「disabled=true 且 paused=true」
// 的叠加态，revive 时得同时清两个标志，且面板三态按钮的判定分支会互相遮盖。
// 清掉后语义唯一：disabled 单独成立即代表"判死"。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.paused = false          // 禁用是更强的终态，二者不叠加（paused 被 disabled 吸收）
	e.disabledAt = time.Now() // 供「临时禁用到期自动重试」计算
	e.reason = reason
	p.dirty.Store(true)
}

// pauseLocked 暂停选号迁移：置 paused 使账号退出选号候选（healthy 判否），
// 但**不动任何惩罚域** —— paused 是「临时让位」，账号本身健康，恢复后冷却/熔断
// 观测仍有效（与 disableLocked 清冷却域形成对照）。
//
// 关键正交性：paused 是**独立于 disabled 的第三个状态**，不写 reason、不清冷却域、
// 不重置任何计数。自动化排程遍历只按 Disabled 过滤，故 paused 号天然继续参与
// 签到 / 活跃 / 保活 / 余额 / 旅行 / 开学季 / 连登 —— 这正是「暂停选号但不掉保号」
// 的实现基础（唯夜猫子例外，它发真实模型对话，见 scheduler/blackcat.go）。
//
// ⚠️ 已禁用账号上不置 paused（提前返回）：硬约束是「二者永不同时为真」。
// disableLocked 清 paused 只覆盖「先 paused 后禁用」的顺序；反过来「先禁用后暂停」
// 若也置位，就会得到 disabled+paused 叠加态 —— 面板判分支、revive 语义都会变得二义。
// 禁用已是更强的终态，此时"暂停"是多余且有害的操作，故静默忽略（幂等、不报错）。
func (p *Pool) pauseLocked(e *entry) {
	if e.disabled {
		return // 禁用是更强的终态，不叠加 paused（保证二者互斥）
	}
	e.paused = true
	p.dirty.Store(true)
}

// resumeLocked 解除暂停选号（幂等）：只清 paused，账号若不在其它惩罚期即恢复可选。
// 与 disableLocked 对照：paused 是瞬时态，无需清冷却/计数，一次 resume 即可回池。
func (p *Pool) resumeLocked(e *entry) {
	e.paused = false
	p.dirty.Store(true)
}

// reviveCoolingLocked 只清冷却域（until/coolKind/reason/softStreak/modelCooldowns）
// 并更新 credits/creditsTotal，不动熔断器（fails/retryCount/breakerUntil）。签到解冻走这里：
// 签到成功只证明余额恢复与 billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx
// 信号）不应被签到覆盖。
// softStreak 属冷却域（与 until/coolKind 同域），随冷却一并清零——与「解冻只清冷却
// 不清熔断」的既有语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是
// 历史软冷却累积。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits, total int64) {
	e.credits = credits
	e.creditsTotal = total
	e.clearCoolingLocked()
}
