// paused_test.go 「暂停选号」状态的行为测试。
//
// 设计原则（与 disabled_test.go 同）：断言**行为**——账号是否真的退出选号候选、
// 状态位是否真的翻转、重启后是否真的读回——而不是断言源码里有没有 "paused" 字样。
//
// 语义与设计理由见 transition.go 的迁移矩阵与 disabled.go 第五节。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 一、paused 号不参与选号
// ---------------------------------------------------------------------------

// TestPausedNotPicked paused 号退出选号候选（healthy 判否），流量全部落到正常号。
func TestPausedNotPicked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "p"})
	p.Add(&auth.Auth{UID: "ok"})
	p.Pause("p")
	// 让 paused 号的积分远高于 ok —— 若它仍参与选号，权重会把它压倒性选中。
	p.SetCredits("p", 1000000, 0)
	p.SetCredits("ok", 1, 0)

	for i := 0; i < 200; i++ {
		if got := p.Pick(); got == nil || got.UID != "ok" {
			t.Fatalf("pick=%v want ok（paused 号不得参与选号）", got)
		}
	}
}

// TestPausedNotPickedForModel 模型维度同样不豁免 paused：paused 在账号级就退出，
// 不存在"对某模型仍可用"的情况（与 ModelBlocked 口径一致）。
func TestPausedNotPickedForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "p"})
	p.Add(&auth.Auth{UID: "ok"})
	p.Pause("p")

	for i := 0; i < 50; i++ {
		if got := p.PickExcludingForModel(nil, "glm-5.2"); got == nil || got.UID != "ok" {
			t.Fatalf("pick(model)=%v want ok（paused 对任何模型都不可选）", got)
		}
	}
}

// TestPausedExcludedFromFallback ★ 全冷却兜底路径同样排除 paused：
// 当池内只剩冷却号时，paused 号不得被兜底"捞出来"。
func TestPausedExcludedFromFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "paused"})
	p.Add(&auth.Auth{UID: "cool"})
	p.Pause("paused")
	p.Cooldown("cool", CoolSoft, time.Hour, "429")

	// 无 healthy 候选 → 走兜底；只有 cool 参与（paused 必须被排除）。
	got := p.Pick()
	if got == nil || got.UID != "cool" {
		t.Fatalf("fallback pick=%v want cool（paused 号永不参与兜底）", got)
	}
}

// TestPausedOnlyAccountYieldsNil 池内唯一账号被 paused 时选号返回 nil
// （与 disabled 的既有行为一致：退出选号 = 真的选不到）。
func TestPausedOnlyAccountYieldsNil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "only"})
	p.Pause("only")
	if got := p.Pick(); got != nil {
		t.Fatalf("pick=%v want nil（唯一账号被暂停 → 无可选）", got)
	}
}

// TestPausedReflectedInStatusAndCounts Status.Paused 透出，且计数归入"不可选"档。
func TestPausedReflectedInStatusAndCounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "p"})
	p.Add(&auth.Auth{UID: "ok"})
	p.Pause("p")

	st, ok := p.Status("p")
	if !ok {
		t.Fatal("status missing")
	}
	if !st.Paused {
		t.Error("Status.Paused 应为 true")
	}
	if st.Disabled {
		t.Error("paused 不应使 Status.Disabled 为 true（二者正交）")
	}
	// 计数口径：paused 计入 disabled 档（不可选），healthy 只剩 ok。
	total, healthy, _, disabled, _ := p.CountsDetailed()
	if total != 2 || healthy != 1 || disabled != 1 {
		t.Errorf("counts total=%d healthy=%d disabled=%d want 2/1/1", total, healthy, disabled)
	}
}

// ---------------------------------------------------------------------------
// 二、暂停 / 恢复的迁移语义
// ---------------------------------------------------------------------------

// TestResumeReturnsToPickable resume 后账号立刻回池（无需重登或解冻）。
func TestResumeReturnsToPickable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	if got := p.Pick(); got != nil {
		t.Fatalf("paused 期间 pick=%v want nil", got)
	}
	if !p.Resume("u1") {
		t.Fatal("Resume 应成功")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Error("Resume 后 Paused 应为 false")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("resume 后 pick=%v want u1", got)
	}
}

// TestResumeIsIdempotent 对未暂停账号 Resume 是空操作（仍返回 true，幂等）。
func TestResumeIsIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Resume("u1") {
		t.Error("Resume 未暂停账号应返回 true（幂等）")
	}
	if st, _ := p.Status("u1"); st.Paused || st.Disabled {
		t.Errorf("空操作后状态被改动: %+v", st)
	}
}

// TestPausedUnknownUIDRejected 不存在的 uid 返回 false（供面板区分 404）。
func TestPausedUnknownUIDRejected(t *testing.T) {
	p := New("")
	if p.Pause("nope") {
		t.Error("Pause(不存在) 应返回 false")
	}
	if p.Resume("nope") {
		t.Error("Resume(不存在) 应返回 false")
	}
}

// TestPauseDoesNotTouchPenaltyDomain ★ paused 只置一个标志：不清冷却域、不清熔断、
// 不重置计数。这是「临时让位」与「判死」的核心差别（对照 disableLocked）。
func TestPauseDoesNotTouchPenaltyDomain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500, 0)
	p.CooldownSoftForModel("u1", time.Hour, time.Time{}, "glm-5.2", "6004")
	p.NoteError("u1") // 喂一次熔断失败计数
	before, _ := p.Status("u1")

	p.Pause("u1")
	after, _ := p.Status("u1")

	if !after.Paused {
		t.Fatal("Pause 未生效")
	}
	// 冷却域必须原样保留（disableLocked 会清，paused 不能）。
	if before.Cooling != after.Cooling {
		t.Errorf("Pause 改动了冷却状态: before=%v after=%v", before.Cooling, after.Cooling)
	}
	if before.Reason != after.Reason {
		t.Errorf("Pause 改动了 reason: %q → %q", before.Reason, after.Reason)
	}
	if before.BreakerFails != after.BreakerFails {
		t.Errorf("Pause 改动了熔断计数: %d → %d", before.BreakerFails, after.BreakerFails)
	}
	if before.ErrTotal != after.ErrTotal || before.SuccessCount != after.SuccessCount {
		t.Errorf("Pause 改动了成功/错误计数: %+v → %+v", before, after)
	}
	if before.Credits != after.Credits {
		t.Errorf("Pause 改动了积分: %d → %d", before.Credits, after.Credits)
	}
	// 模型级冷却台账也应保留（paused 不动惩罚域）。
	if len(before.RateLimitedModels) != len(after.RateLimitedModels) {
		t.Errorf("Pause 清掉了模型级限流台账: %v → %v", before.RateLimitedModels, after.RateLimitedModels)
	}
}

// TestDisableAbsorbsPaused ★ 硬约束 1：disableLocked 顺带清 paused（禁用是更强的终态，
// 二者不叠加）。先 paused 再 disable → 只剩 disabled。
func TestDisableAbsorbsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Disable("u1", "manual disable (panel)")

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Error("应处于 disabled")
	}
	if st.Paused {
		t.Error("禁用应吸收 paused（disabled+paused 叠加态按设计不存在）")
	}
}

// TestPauseOnDisabledAccountDoesNotCreateOverlap 反向：对一个已禁用账号 Pause，
// 不得产生 disabled+paused 叠加态（paused 被 disabled 吸收）。这与上一条同源 ——
// 正交的硬约束是「二者永不同时为真」。
func TestPauseOnDisabledAccountDoesNotCreateOverlap(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "manual disable (panel)")
	p.Pause("u1")

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Error("应仍处于 disabled")
	}
	if st.Paused {
		t.Error("对已禁用账号 Pause 不得产生 disabled+paused 叠加态")
	}
}

// TestReviveClearsPaused ★ 硬约束：Revive（解冻）是全清，paused 一并清掉。
func TestReviveClearsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	if !p.Revive("u1") {
		t.Fatal("Revive 应成功")
	}
	st, _ := p.Status("u1")
	if st.Paused || st.Disabled {
		t.Errorf("Revive 后应完全可用，实际 paused=%v disabled=%v", st.Paused, st.Disabled)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("Revive 后 pick=%v want u1（应回池）", got)
	}
}

// TestReviveDisabledDoesNotClearPaused ReviveDisabled 只清禁用（语义是"复活禁用号"），
// 不属"全清"，故 paused 保持 —— 固化二者边界，避免将来被误合并。
func TestReviveDisabledDoesNotClearPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "reason")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("ReviveDisabled 应清 disabled")
	}
	// 该账号从未被 paused，此处只断言"不被额外置位"。
	if st.Paused {
		t.Error("ReviveDisabled 不应引入 paused")
	}
}

// ---------------------------------------------------------------------------
// 三、落盘往返 + 向后兼容（硬约束 2）
// ---------------------------------------------------------------------------

// TestPausedPersistsAcrossReload paused=true 存盘 → 读回仍为 true（跨重启不丢）。
func TestPausedPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Pause("u1")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// 落盘 JSON 必须含 paused 键且 u1 为 true。注意 omitempty：u2 的 false 不写。
	// 落盘走 MarshalIndent（键与值之间有空格），故用宽松匹配 "paused": true。
	if !strings.Contains(string(raw), `"paused": true`) {
		t.Errorf("state.json 应含 paused=true:\n%s", raw)
	}

	// 模拟重启：新池加载同一文件。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	st1, ok := p2.Status("u1")
	if !ok {
		t.Fatal("u1 未加载")
	}
	if !st1.Paused {
		t.Error("u1 重启后 paused 丢失（应跨重启保留）")
	}
	if st1.Disabled {
		t.Error("u1 重启后不应变成 disabled")
	}
	// 语义也要保留：重启后 paused 号仍不参与选号。
	if got := p2.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("重启后 pick=%v want u2（paused 号重启后仍不得参与选号）", got)
	}
	st2, _ := p2.Status("u2")
	if st2.Paused {
		t.Error("u2 未被暂停，重启后不应变成 paused")
	}
}

// TestPausedFalseNotWritten omitempty：未暂停账号不落盘 paused 键（保持文件精简，
// 也是"旧格式 ↔ 新格式"差异最小的写法）。
func TestPausedFalseNotWritten(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush()

	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), `"paused"`) {
		t.Errorf("未暂停账号不应落盘 paused 键:\n%s", raw)
	}
}

// TestLoadLegacyStateWithoutPaused ★★ 硬约束 2 的核心：**旧格式 state.json**
// （完全没有 paused 字段）必须能正常装载，paused 取零值 false，不报错。
//
// 这是生产安全的关键 —— 生产环境有真实 state.json，读不了会导致账号池起不来。
func TestLoadLegacyStateWithoutPaused(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 旧格式：既有字段齐全，但**无任何 paused 痕迹**（模拟升级前的真实文件）。
	legacy := `{"accounts":{
		"old1":{"credits":1234,"disabled":false,"reason":"","cool_kind":0,"success_count":7,
		        "err_total":2,"soft_streak":1,"session_dead_fails":0,"consecutive_fails":0,
		        "credits_expiring":50},
		"old2":{"credits":99,"disabled":true,"reason":"manual disable (panel)",
		        "disabled_at":"2026-10-01T10:00:00+08:00","cool_kind":0,
		        "success_count":0,"err_total":0,"soft_streak":0,"session_dead_fails":0,
		        "consecutive_fails":0,"credits_expiring":0}
	}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	p.Add(&auth.Auth{UID: "old1"})
	p.Add(&auth.Auth{UID: "old2"})

	// 旧格式必须**能装载**且既有字段正确（不是"加载失败但测试没发现"）。
	st1, ok := p.Status("old1")
	if !ok {
		t.Fatal("旧格式 state.json 的 old1 未加载 —— 向后兼容被破坏")
	}
	if st1.Credits != 1234 || st1.SuccessCount != 7 || st1.ErrTotal != 2 {
		t.Errorf("旧格式既有字段读错: %+v", st1)
	}
	// 缺 paused 键 → 零值 false（而非报错 / 而非变成 true）。
	if st1.Paused {
		t.Error("旧格式缺 paused 键，应取零值 false")
	}
	if st1.Disabled {
		t.Error("old1 旧格式为未禁用")
	}
	// old2 是禁用号，同样无 paused 键。
	st2, ok := p.Status("old2")
	if !ok {
		t.Fatal("旧格式 state.json 的 old2 未加载")
	}
	if !st2.Disabled {
		t.Error("old2 旧格式 disabled=true 应保留")
	}
	if st2.Paused {
		t.Error("old2 旧格式缺 paused 键，应取零值 false")
	}
	// 语义：old1 应正常参与选号（不被误判为 paused）。
	if got := p.Pick(); got == nil || got.UID != "old1" {
		t.Fatalf("旧格式加载后 pick=%v want old1（不得被误判为暂停）", got)
	}
}

// TestLoadLegacyStatePreservesExplicitPaused 反向保护：新格式里显式 paused=true
// 必须被读出 true（防"兼容旧格式"的实现顺手把新字段也忽略掉）。
func TestLoadLegacyStatePreservesExplicitPaused(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	newFmt := `{"accounts":{"u1":{"credits":10,"paused":true,"disabled":false}}}`
	if err := os.WriteFile(fp, []byte(newFmt), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 未加载")
	}
	if !st.Paused {
		t.Error("新格式 paused=true 未被读出（兼容层不得吃掉新字段）")
	}
}

// ---------------------------------------------------------------------------
// 四、状态位不泄漏到无关字段（结构级守卫）
// ---------------------------------------------------------------------------

// TestPausedSurvivesJSONRoundTrip stateAccount 的 paused 字段 JSON 往返无损。
// 直接对结构体做编解码，锁住 tag 名与语义（防止有人改名 "pause" 破坏兼容）。
func TestPausedSurvivesJSONRoundTrip(t *testing.T) {
	orig := stateAccount{Credits: 5, Paused: true}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"paused":true`) {
		t.Fatalf("紧凑序列化缺 paused 键或值不对: %s", raw)
	}
	var back stateAccount
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Paused {
		t.Error("JSON 往返后 paused 丢失")
	}
}

// ---------------------------------------------------------------------------
// 五、层级别守卫：钉住「在**这一层**生效」
//
// ⚠️ 为何必须单独写这几条（本项目反复踩到的测试盲区）：
//   healthyForModel / 兜底路径的 paused 判否**彼此冗余** —— healthyForModel 末尾会
//   回落到 healthy(now)，而 healthy 也判 paused。于是"只从 Pick 观察行为"的测试
//   在删掉 healthyForModel 的 `|| e.paused` 时**仍然通过**（被 healthy 那一层兜住），
//   变异验证实测到这点（M2/M3 咬不住）。
//   判据（对齐 cross 的"钉住值的来源与去向 + 在哪一层生效"）：
//   **断言必须直接调用被测的那一层**，不能依赖上层行为间接观察。
// ---------------------------------------------------------------------------

// TestHealthyForModelRejectsPausedAtThisLayer 直接调 healthyForModel（不经 Pick），
// 断言 paused 号在**模型维度**也不可选（模型维度不构成豁免）。
//
// ⚠️ 诚实的边界（变异验证实测结论，不要误解为本测试的强度）：
// healthyForModel 末尾 `return e.healthy(now)` 会回落到同样判 paused 的 healthy()，
// 因此**删掉本函数首行的 `|| e.paused` 后，本测试与其它任何测试都仍会通过** ——
// 该处判否是"防御性冗余"（两道门互为兜底），不是可独立观测的行为。
// 我实测确认过这一点（对 entry.go 该行做变异后跑 `go test ./internal/pool/` 全绿）。
// 保留它的理由：语义显式（读代码即知 paused 在模型维度也被判否）+ 将来若
// healthy() 的判否被挪走，本层仍能独立成立。**它的正确性由 healthy() 那条测试覆盖。**
func TestHealthyForModelRejectsPausedAtThisLayer(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")

	p.mu.RLock()
	e := p.byUID["u1"]
	// 先证明前提：该账号的账号级 healthy 与模型级 healthyForModel 都应为 false。
	now := time.Now()
	healthy := e.healthy(now)
	hfm := e.healthyForModel(now, "glm-5.2")
	p.mu.RUnlock()

	if healthy {
		t.Error("paused 号 healthy() 应为 false")
	}
	if hfm {
		t.Error("paused 号 healthyForModel() 应为 false（模型维度不构成豁免）")
	}
}

// TestHealthyForModelPausedIndependentOfHealthy 结构性前提校验：证明两条判否
// **不是同一条**（若有人把 healthyForModel 的判否删成只依赖 healthy，
// 本用例并不阻止 —— 真正阻止它的是上面那条直接断言）。
// 这里用「modelExempt 也要判 paused」补上第三个判否点的直接断言。
func TestModelExemptRejectsPausedAtThisLayer(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 造出一个「有模型级冷却 + 未禁用」的形态，使 modelExempt 的前置条件成立。
	// ⚠️ 必须给**非零 resetAt**：零值走"普通软冷却"分支（写 until 并清 modelCooldowns），
	// 不会在 modelCooldowns 留下条目 → modelExempt 的前置 len(modelCooldowns)>0 不成立。
	p.CooldownSoftForModel("u1", time.Hour, time.Now().Add(time.Hour), "glm-5.2", "6004")

	p.mu.RLock()
	e := p.byUID["u1"]
	before := e.modelExempt() // 未暂停：应处于模型豁免形态
	p.mu.RUnlock()
	if !before {
		t.Fatal("precondition: 未暂停且有模型级冷却时 modelExempt 应为 true")
	}

	p.Pause("u1")
	p.mu.RLock()
	after := p.byUID["u1"].modelExempt()
	p.mu.RUnlock()
	if after {
		t.Error("paused 号 modelExempt 应为 false（暂停后不得保留模型豁免形态）")
	}
}

// TestFallbackDirectlyExcludesPaused 直接调 pickEarliestExpiryLocked（不经 pick），
// 钉住兜底这一层自身排除 paused —— 不依赖"该号是否先被正常路径过滤掉"。
func TestFallbackDirectlyExcludesPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "paused"})
	p.Add(&auth.Auth{UID: "cool"})
	p.Pause("paused")
	p.Cooldown("cool", CoolSoft, time.Hour, "429")

	p.mu.Lock()
	got := p.pickEarliestExpiryLocked(nil, time.Now(), "", "")
	p.mu.Unlock()

	if got == nil || got.UID != "cool" {
		t.Fatalf("兜底=%v want cool（兜底层自身必须排除 paused）", got)
	}

	// 反向对照（关键）：给 paused 号造出**有效的冷却截止**，再验证
	//   (a) 仍处于 paused 时兜底不选它；
	//   (b) resume 之后兜底**能**选到它。
	// 只有 (b) 成立，才能证明 (a) 的结果确实来自 paused 判定，
	// 而不是因为该号压根不满足兜底条件（否则本用例是个恒真的假断言）。
	// 让 paused 号的冷却**比其他号更早到期**，这样 resume 后兜底必然选中它
	//（兜底口径是"取截止最早者"，否则 cool 的 1h 会比它的 2h 更早，对照失效）。
	p.Cooldown("paused", CoolSoft, time.Minute, "429")
	p.mu.Lock()
	pausedExpiry := p.byUID["paused"].expiry(time.Now())
	stillPaused := p.pickEarliestExpiryLocked(nil, time.Now(), "", "")
	p.mu.Unlock()
	if pausedExpiry.IsZero() {
		t.Fatal("precondition: paused 号应处于有效冷却期（否则兜底条件不成立，断言无意义）")
	}
	if stillPaused == nil || stillPaused.UID != "cool" {
		t.Errorf("paused 号有冷却时兜底=%v want cool（不得选 paused）", stillPaused)
	}

	p.Resume("paused")
	p.mu.Lock()
	after := p.pickEarliestExpiryLocked(nil, time.Now(), "", "")
	p.mu.Unlock()
	if after == nil || after.UID != "paused" {
		t.Fatalf("resume 后兜底=%v want paused（证明前面的排除确实来自 paused 判定）", after)
	}
}
