package pool

import "testing"

// 真实套餐名（取自生产 /panel/api/packages）。
func TestClassifyPlanRealCases(t *testing.T) {
	cases := []struct {
		name     string
		packages []string
		want     PlanKind
	}{
		{
			"ptrel1（global，仅 Free Plan + Bonus）",
			[]string{"Free Plan Subscription", "Bonus Pack", "Bonus Pack", "Bonus Pack"},
			PlanFree,
		},
		{
			"汪羽杰（有标准版 + 大量活动包）",
			[]string{"Buddy AI个人标准版", "Codebuddy版本赠送包",
				"CodeBuddy个人版拉新权益包", "CodeBuddy个人版国内运营裂变包"},
			PlanStandard, // 有订阅 → 标准版（不该被判成活动号）
		},
		{
			"pt（只有活动包，无订阅）",
			[]string{"CodeBuddy个人版拉新权益包", "CodeBuddy个人版国内运营裂变包", "CodeBuddy个人体验版"},
			PlanTrial, // 含"体验版" → 试用（比 activity 更具体）
		},
		{
			"纯活动包（无体验版）",
			[]string{"CodeBuddy个人版拉新权益包", "CodeBuddy个人版国内运营裂变包"},
			PlanActivity,
		},
		{"无包信息 → 不猜", nil, PlanUnknown},
	}
	for _, c := range cases {
		got, _ := ClassifyPlan(c.packages)
		if got != c.want {
			t.Errorf("%s: ClassifyPlan = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestClassifyPlanStandardBeatsActivity 有正式订阅的账号不得被判为"活动号" ——
// 否则运维会误以为它"额度用完就没了"，低估其价值。
func TestClassifyPlanStandardBeatsActivity(t *testing.T) {
	// 100 个活动包 + 1 个标准版 → 必须判 standard
	pkgs := make([]string, 0, 101)
	for i := 0; i < 100; i++ {
		pkgs = append(pkgs, "CodeBuddy个人版拉新权益包")
	}
	pkgs = append(pkgs, "Buddy AI个人标准版")
	if got, _ := ClassifyPlan(pkgs); got != PlanStandard {
		t.Errorf("有订阅却被判 %q，应为 standard（活动包数量不该压过订阅）", got)
	}
}

// TestClassifyPlanUnknownNotGuessed 无法判定时返回 unknown，**不猜** ——
// 编造一个类型比"未分类"更糟（§十）。
func TestClassifyPlanUnknownNotGuessed(t *testing.T) {
	if got, _ := ClassifyPlan([]string{"某个没见过的新包名"}); got != PlanUnknown {
		t.Errorf("无法判定时返回 %q，应为 unknown（不得猜）", got)
	}
}
