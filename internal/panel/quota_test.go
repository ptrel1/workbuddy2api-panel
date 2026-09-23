package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

var qLabels = map[string]string{
	"standard": "标准版", "trial": "体验版", "activity": "活动号",
	"free": "免费版", "unknown": "未分类",
}

// TestQuotaPeakIsPerAccountBestDay 峰值必须是「该账号**最好的一天**」——
// 不是总和、不是平均。总和会随观测天数增长（不可比），平均会掩盖能力上界。
func TestQuotaPeakIsPerAccountBestDay(t *testing.T) {
	days := []usage.Daily{
		{Day: "2026-09-16", UID: "aaaaaaaa", Reqs: 100, Tokens: 1000},
		{Day: "2026-09-17", UID: "aaaaaaaa", Reqs: 500, Tokens: 9000}, // 最好的一天
		{Day: "2026-09-18", UID: "aaaaaaaa", Reqs: 200, Tokens: 3000},
	}
	rep := BuildQuotaReport(days, map[string]string{"aaaaaaaa": "standard"}, qLabels, nil)
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rep.Rows))
	}
	if rep.Rows[0].PeakTokens != 9000 {
		t.Errorf("PeakTokens = %d, want 9000（最好的一天，不是总和 %d）",
			rep.Rows[0].PeakTokens, 1000+9000+3000)
	}
	if rep.Rows[0].PeakDay != "2026-09-17" {
		t.Errorf("PeakDay = %q, want 2026-09-17", rep.Rows[0].PeakDay)
	}
	if rep.Rows[0].ObservedDays != 3 {
		t.Errorf("ObservedDays = %d, want 3（口径标注必须准确）", rep.Rows[0].ObservedDays)
	}
}

// TestQuotaSkipsTinySampleDays 成功数太少的天不得纳入 ——
// 那些是"服务刚启用/账号刚加"的残天，会把峰值拉低造成误判。
func TestQuotaSkipsTinySampleDays(t *testing.T) {
	days := []usage.Daily{
		{Day: "2026-09-16", UID: "aaaaaaaa", Reqs: 3, Tokens: 50}, // 残天，应跳过
		{Day: "2026-09-17", UID: "aaaaaaaa", Reqs: 100, Tokens: 5000},
	}
	rep := BuildQuotaReport(days, map[string]string{"aaaaaaaa": "trial"}, qLabels, nil)
	if rep.Rows[0].PeakTokens != 5000 {
		t.Errorf("PeakTokens = %d, want 5000（残天的 50 不应参与）", rep.Rows[0].PeakTokens)
	}
	if rep.Rows[0].ObservedDays != 1 {
		t.Errorf("ObservedDays = %d, want 1（只算有效天）", rep.Rows[0].ObservedDays)
	}
}

// TestQuotaMedianNeedsTwoSamples 中位数必须**样本≥2**才有意义 ——
// 单个值当"中位数"是伪统计，会误导读者以为有多账号支撑。
func TestQuotaMedianNeedsTwoSamples(t *testing.T) {
	one := BuildQuotaReport([]usage.Daily{
		{Day: "2026-09-17", UID: "aaaaaaaa", Reqs: 100, Tokens: 5000},
	}, map[string]string{"aaaaaaaa": "free"}, qLabels, nil)
	if one.Tiers[0].MedianTokens != 0 {
		t.Errorf("单账号套餐给了中位数 %d（应为 0 = 不算，样本不足）", one.Tiers[0].MedianTokens)
	}

	two := BuildQuotaReport([]usage.Daily{
		{Day: "2026-09-17", UID: "aaaaaaaa", Reqs: 100, Tokens: 4000},
		{Day: "2026-09-17", UID: "bbbbbbbb", Reqs: 100, Tokens: 8000},
	}, map[string]string{"aaaaaaaa": "trial", "bbbbbbbb": "trial"}, qLabels, nil)
	if two.Tiers[0].MedianTokens != 6000 {
		t.Errorf("两样本中位数 = %d, want 6000", two.Tiers[0].MedianTokens)
	}
}

// TestQuotaUnknownNeverGuessed 无套餐信息的账号归 unknown，**不猜**（§十）。
func TestQuotaUnknownNeverGuessed(t *testing.T) {
	rep := BuildQuotaReport([]usage.Daily{
		{Day: "2026-09-17", UID: "cccccccc", Reqs: 100, Tokens: 5000},
	}, map[string]string{}, qLabels, nil)
	if rep.Rows[0].Plan != "unknown" {
		t.Errorf("无套餐信息时判为 %q，应为 unknown（不得猜）", rep.Rows[0].Plan)
	}
}

// TestQuotaAlwaysCarriesCaveat 口径说明必须**始终随报告返回** ——
// 它是本模块诚实性的载体：产出是"实测推断"，不是官方额度。
// 少了它，读者会把峰值当成官方上限。
func TestQuotaAlwaysCarriesCaveat(t *testing.T) {
	rep := BuildQuotaReport(nil, nil, qLabels, nil)
	if rep.Caveat == "" {
		t.Fatal("缺 caveat —— 会被误读为官方额度")
	}
	for _, kw := range []string{"实测", "非上游公布", "下界"} {
		if !strings.Contains(rep.Caveat, kw) {
			t.Errorf("caveat 缺少关键限定词 %q：%s", kw, rep.Caveat)
		}
	}
}

// TestQuotaSortStandardFirst 标准版应排最前（"最该指望的号"）。
func TestQuotaSortStandardFirst(t *testing.T) {
	days := []usage.Daily{
		{Day: "2026-09-17", UID: "aaaaaaaa", Reqs: 100, Tokens: 99999}, // 免费版但量最大
		{Day: "2026-09-17", UID: "bbbbbbbb", Reqs: 100, Tokens: 100},
	}
	plans := map[string]string{"aaaaaaaa": "free", "bbbbbbbb": "standard"}
	rep := BuildQuotaReport(days, plans, qLabels, nil)
	if rep.Rows[0].Plan != "standard" {
		t.Errorf("首位是 %q，want standard（套餐优先级应压过峰值）", rep.Rows[0].Plan)
	}
}
