package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// enterpriseClient 构造一个只对企业额度端点返回指定 body 的 fake client。
func enterpriseClient(t *testing.T, seenPath *string, body string) *Client {
	t.Helper()
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if seenPath != nil {
				*seenPath = r.URL.Path
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestEnterpriseResourceInvertsCreditSemantics ⭐ 企业口径 `credit` 是**本周期已用**，
// 与个人 CapacityRemain（剩余）语义相反 —— 这是本功能最容易搞反的地方。
//
// 实测上游返回：{"credit":812.45,"limitNum":2000,...} → 剩余必须是 2000-812=1188。
// 若照个人口径直接取 credit 当"剩余"，会把"已用 812"读成"剩 812"。
func TestEnterpriseResourceInvertsCreditSemantics(t *testing.T) {
	body := `{"code":0,"data":{"credit":812.45,"limitNum":2000,
		"cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"2099-10-26 23:59:59",
		"cycleResetTime":"2099-10-27 00:00:00"}}`
	var path string
	c := enterpriseClient(t, &path, body)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "ent-1"}

	remain, total, _, err := c.UserResourceDetailed(a, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if remain != 1188 {
		t.Errorf("remain=%d，want 1188（2000 分配 - 812 已用；注意 credit 是已用不是剩余）", remain)
	}
	if total != 2000 {
		t.Errorf("total=%d，want 2000（分配额度）", total)
	}
	// 走的是企业端点，不是个人资源包端点
	if !strings.Contains(path, "get-enterprise-user-usage") {
		t.Errorf("未走企业端点，实际 path=%q", path)
	}
}

// TestEnterpriseResourceUnlimited 不限量（limitNum=-1）：剩余取大值、total 记 -1。
//
// 为什么 total 记 -1：池内没有"不限"状态位，credits 同时参与路由权重与 credit_floor
// 判定，必须两处都"不构成限制"。total 仅用于展示（前端据此显示「不限」）。
func TestEnterpriseResourceUnlimited(t *testing.T) {
	body := `{"code":0,"data":{"credit":0,"limitNum":-1,"cycleEndTime":"2099-10-26 23:59:59"}}`
	c := enterpriseClient(t, nil, body)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "ent-1"}

	remain, total, _, err := c.UserResourceDetailed(a, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if total != -1 {
		t.Errorf("total=%d，want -1（前端据此显示「不限」）", total)
	}
	if remain < 1<<30 {
		t.Errorf("remain=%d，应取大值让路由权重与 credit_floor 都不构成限制", remain)
	}
}

// TestEnterpriseResourceRoundsUsed 已用 credit 是浮点 → **四舍五入**（不是向下取整）。
//
// 为什么关键：向下取整会**低报**剩余额度。812.45 → 812（12.45 → 12）；但 812.75
// 向下取整是 812、四舍五入是 813，两者剩余差 1 —— 展示"额度还有多少"取整方向要偏保守解释：
// 上游 credit 是"已用"，四舍五入 = 剩余偏保守（宁可显示少一点）。
func TestEnterpriseResourceRoundsUsed(t *testing.T) {
	body := `{"code":0,"data":{"credit":812.75,"limitNum":2000,"cycleEndTime":"2099-10-26 23:59:59"}}`
	c := enterpriseClient(t, nil, body)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "ent-1"}
	remain, _, _, err := c.UserResourceDetailed(a, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 812.75 → 813 → remain = 2000-813 = 1187
	if remain != 1187 {
		t.Errorf("remain=%d，want 1187（812.75 四舍五入为 813）", remain)
	}
}

// TestEnterpriseResourceSnakeCaseFields 兼容 snake_case 字段名
// （桌面端两条解析路径分别读 camelCase 与 snake_case）。
func TestEnterpriseResourceSnakeCaseFields(t *testing.T) {
	body := `{"code":0,"data":{"used_num":100,"limit_num":500,"cycleEndTime":"2099-10-26 23:59:59"}}`
	c := enterpriseClient(t, nil, body)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "ent-1"}
	remain, total, _, err := c.UserResourceDetailed(a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if remain != 400 || total != 500 {
		t.Errorf("remain/total=%d/%d，want 400/500（snake_case 兼容）", remain, total)
	}
}

// TestEnterpriseResourceCycleExpiring 周期末计入 expiring 批次（未用完即作废）。
func TestEnterpriseResourceCycleExpiring(t *testing.T) {
	// 周期在 1 天后结束，soon = 48h → 应计入 expiring
	end := time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	body := `{"code":0,"data":{"credit":100,"limitNum":500,"cycleEndTime":"` + end + `"}}`
	c := enterpriseClient(t, nil, body)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "ent-1"}
	remain, _, expiring, err := c.UserResourceDetailed(a, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if remain != 400 {
		t.Fatalf("remain=%d，want 400", remain)
	}
	if expiring != 400 {
		t.Errorf("expiring=%d，want 400（周期末应参与 prefer_expiring，别浪费即将作废的配额）", expiring)
	}
}

// TestPersonalAccountStillUsesPersonalPath 个人号**不得**走企业端点（零回归）。
func TestPersonalAccountStillUsesPersonalPath(t *testing.T) {
	var path string
	c := enterpriseClient(t, &path, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"} // 无 EnterpriseID

	if _, _, _, err := c.UserResourceDetailed(a, 0); err != nil {
		t.Fatalf("个人号 err: %v", err)
	}
	if strings.Contains(path, "enterprise") {
		t.Errorf("个人号误走企业端点：%q", path)
	}
}
