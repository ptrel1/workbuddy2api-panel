package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ─────────────────────────────────────────────────────────────────────────────
// 积分保底 credit_floor
//
// 防的是"收费模型把余额打穿 → 连免费模型都 402 冷却到次日"的可用性悬崖。
// 实测事故（上游记录）：某 x1.62 模型两笔扣 111.73 分把号从 100 打到 0，
// 被硬冷却到次日 04:00 —— 最坏约 11.5 小时该号完全不可用。
// ─────────────────────────────────────────────────────────────────────────────

// TestCreditFloorBlocksPaidModel 触底号不接**实测收费**模型（核心行为）。
func TestCreditFloorBlocksPaidModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 50, 0)  // 触底（< floor 100）
	seedCost(p, "poor", "m1", 5) // tier 2：实测收费 5 credits/1K

	p.SetCreditFloor(100)
	if got := p.PickExcludingForModel(nil, "m1"); got != nil {
		t.Errorf("触底号不该接收费模型，却选中了 %s", got.UID)
	}
}

// TestCreditFloorAllowsFreeModel 触底号**仍可接实测免费模型**。
//
// 为什么必须允许：保底保的是「留余额给免费模型用」，不是「什么都别调」。
// 免费请求 credit=0 不再扣减余额，放行零成本且保住可用性。
func TestCreditFloorAllowsFreeModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 50, 0)
	seedCost(p, "poor", "free-m", 0) // tier 0：实测免费

	p.SetCreditFloor(100)
	if got := p.PickExcludingForModel(nil, "free-m"); got == nil || got.UID != "poor" {
		t.Errorf("触底号应可接免费模型，得 %v", got)
	}
}

// TestCreditFloorAllowsUnknownModel 触底号可接**无观测**模型（tier 1）。
//
// 为什么必须允许（防死锁）：若拦 tier 1，触底号永远拿不到该模型的首笔观测 →
// 永远停在"未知" → 永远被拦，锁死在"学不回来"。
// 与 costTier 探索窗口「给未知号机会」是同一考量。
func TestCreditFloorAllowsUnknownModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 50, 0) // 无任何 modelCost 观测 = tier 1

	p.SetCreditFloor(100)
	if got := p.PickExcludingForModel(nil, "never-tried"); got == nil || got.UID != "poor" {
		t.Errorf("触底号应可试无观测模型（否则永远学不到成本），得 %v", got)
	}
}

// TestCreditFloorDisabledByDefault 缺省（0）时行为与改动前完全一致（零回归）。
func TestCreditFloorDisabledByDefault(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 1, 0)
	seedCost(p, "poor", "m1", 99) // 极贵
	// 不调 SetCreditFloor（缺省 0 = 关闭）
	if got := p.PickExcludingForModel(nil, "m1"); got == nil || got.UID != "poor" {
		t.Errorf("未开启保底时应照常选中，得 %v", got)
	}
	if p.CreditFloor() != 0 {
		t.Errorf("缺省 CreditFloor 应为 0（关闭），得 %d", p.CreditFloor())
	}
}

// TestCreditFloorBoundaryAtFloor 恰好等于 floor 时**放行**（严格小于才拦）。
func TestCreditFloorBoundaryAtFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0) // 恰好等于 floor
	seedCost(p, "u1", "m1", 5)
	p.SetCreditFloor(100)
	if got := p.PickExcludingForModel(nil, "m1"); got == nil {
		t.Error("credits == floor 应放行（判据是严格小于），却拦了")
	}
}

// TestCreditFloorRecoversAfterCheckin 签到回血越过 floor 后自动恢复。
func TestCreditFloorRecoversAfterCheckin(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	seedCost(p, "u1", "m1", 5)
	p.SetCreditFloor(100)
	if got := p.PickExcludingForModel(nil, "m1"); got != nil {
		t.Fatalf("触底时应被拦，却选中 %s", got.UID)
	}
	// 签到回血（SetCreditsDetailed 是签到权威值的入口）
	p.SetCreditsDetailed("u1", 5000, 5000, 0)
	if got := p.PickExcludingForModel(nil, "m1"); got == nil || got.UID != "u1" {
		t.Errorf("回血越过 floor 后应恢复可选，得 %v", got)
	}
}

// TestCreditFloorExcludesFallback 全冷却兜底路径同样受保底约束（易漏点）。
//
// 为什么关键：floor 把健康号全拦掉后 cands 为空 → 走兜底 → 若兜底不判保底，
// 触底号会被"捞回来"继续接收费模型，表现为同一条警告反复刷同一个号
// （上游实测：credits=1 < floor=150 仍持续中选 13 分钟 10 次）。
// 兜底是最后一道选号路径，保底在它之前挡不住就等于没挡。
func TestCreditFloorExcludesFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1, 0) // 触底
	seedCost(p, "u1", "m1", 5)
	p.SetCreditFloor(150)
	// 把账号打成软冷却 → 健康候选为空 → 走兜底路径
	p.CooldownSoftRate("u1", time.Minute, time.Now().Add(time.Minute), "test")

	got := p.pickEarliestExpiryLocked(nil, time.Now(), "", "m1")
	if got != nil {
		t.Errorf("兜底路径不该把触底号捞回来接收费模型，得 %s", got.UID)
	}
	// 反向对照：换免费模型（tier 0）则兜底应能选到它 —— 证明排除确实来自
	// 保底判据，而不是"兜底恒返回 nil"这种假绿。
	seedCost(p, "u1", "free-m", 0)
	got2 := p.pickEarliestExpiryLocked(nil, time.Now(), "", "free-m")
	if got2 == nil {
		t.Error("免费模型下兜底应能选中（证明上一条的排除来自保底判据）")
	}
}

// TestCreditFloorNotAppliedWithoutModel model 为空时不判保底（面板探活/healthz 走这条）。
func TestCreditFloorNotAppliedWithoutModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1, 0)
	seedCost(p, "u1", "m1", 99)
	p.SetCreditFloor(1000)
	// Pick() 不带模型 → 无成本维度 → 不拦（否则探活会被保底误伤）
	if got := p.Pick(); got == nil {
		t.Error("无模型上下文时不该判保底（面板探活/healthz 依赖此语义）")
	}
}

// TestCreditFloorStickyPath 粘性路径同样受保底约束。
func TestCreditFloorStickyPath(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	seedCost(p, "u1", "m1", 5)
	p.SetCreditFloor(100)
	if got := p.PickByUIDForModel("u1", "m1"); got != nil {
		t.Errorf("粘性路径不该让触底号接收费模型，得 %s", got.UID)
	}
	// 免费模型仍可
	seedCost(p, "u1", "free-m", 0)
	if got := p.PickByUIDForModel("u1", "free-m"); got == nil {
		t.Error("粘性路径下免费模型应可用")
	}
}
