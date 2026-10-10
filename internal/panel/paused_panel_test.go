// paused_panel_test.go 面板侧「暂停选号」的接线测试。
//
// 两个层次，缺一不可：
//   - **行为级**：真的打 /pause 与 /resume 端点，断言池内状态位翻转、
//     并断言 paused 号确实退出了选号（而不是只断言返回 200）。
//   - **接线级**：三态按钮 —— index.html/app.js 双侧存在性（有 DOM 无逻辑 = 死按钮，
//     有逻辑无 DOM = 用户看不见，都是缺陷）。判据沿用 TestAvailabilityUIFullyWired
//     等既有用例的「双侧存在性」范式。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// pausedPanel 建一个带真实 pool 的面板（APIKey 为 test-key，与 newTestPanel 同口径）。
func pausedPanel(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p}), p
}

// postAuth 带鉴权打一个 POST 端点（面板 withAuth 要求 Bearer key）。
func postAuth(t *testing.T, p *Panel, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestPauseEndpointFlipsStateAndStopsPicking ★ 行为级：POST /pause 使账号退出选号，
// POST /resume 使其回池。断言的是**池的真实行为**，不是返回码。
func TestPauseEndpointFlipsStateAndStopsPicking(t *testing.T) {
	pl, p := pausedPanel(t)

	rec := postAuth(t, pl, "/panel/api/accounts/u1/pause")
	if rec.Code != http.StatusOK {
		t.Fatalf("pause 返回 %d want 200: %s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 丢失")
	}
	if !st.Paused {
		t.Error("pause 后 Status.Paused 应为 true")
	}
	if st.Disabled {
		t.Error("pause 不应置 disabled（二者正交）")
	}
	// 关键：paused 号真的不再被选中（唯一健康号被暂停 → 选号无可选）。
	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("pause 后 pick=%v want u2（paused 号必须退出选号）", got)
		}
	}

	rec = postAuth(t, pl, "/panel/api/accounts/u1/resume")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume 返回 %d want 200: %s", rec.Code, rec.Body.String())
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Error("resume 后 Status.Paused 应为 false")
	}
	// resume 后 u1 应重新有可能被选中（多次采样至少命中一次）。
	hit := false
	for i := 0; i < 200; i++ {
		if got := p.Pick(); got != nil && got.UID == "u1" {
			hit = true
			break
		}
	}
	if !hit {
		t.Error("resume 后 u1 从未被选中（恢复选号未真正生效）")
	}
}

// TestPauseResumeEndpointUnknownUID404 不存在的 uid 返回 404（供前端报错）。
func TestPauseResumeEndpointUnknownUID404(t *testing.T) {
	pl, _ := pausedPanel(t)
	for _, path := range []string{
		"/panel/api/accounts/nope/pause",
		"/panel/api/accounts/nope/resume",
	} {
		rec := postAuth(t, pl, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 返回 %d want 404", path, rec.Code)
		}
	}
}

// TestPauseResumeEndpointsRequireAuth 两个新端点必须走 withAuth（公网暴露的安全底线）。
func TestPauseResumeEndpointsRequireAuth(t *testing.T) {
	pl, p := pausedPanel(t)
	for _, path := range []string{
		"/panel/api/accounts/u1/pause",
		"/panel/api/accounts/u1/resume",
	} {
		req := httptest.NewRequest("POST", path, nil) // 不带 key
		rec := httptest.NewRecorder()
		pl.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无鉴权返回 %d want 401", path, rec.Code)
		}
	}
	// 未鉴权请求不得产生状态副作用（401 必须是"拒绝"而非"先改再拒"）。
	if st, _ := p.Status("u1"); st.Paused || st.Disabled {
		t.Errorf("未鉴权请求改动了账号状态: paused=%v disabled=%v", st.Paused, st.Disabled)
	}
}

// TestPausedUIFullyWired 三态按钮的双侧存在性：DOM 与逻辑必须都在。
//
// 「有 DOM 无引用 = 死按钮」正是本项目 TestAvailabilityUIFullyWired 的既有判据。
func TestPausedUIFullyWired(t *testing.T) {
	js := readAppJS(t)

	// 1. 状态标签：已暂停选号（且文案必须区别于 disabled 的「需重新登录」语义）。
	if !strings.Contains(js, "已暂停选号") {
		t.Error("app.js 缺少「已暂停选号」标签（paused 号会被误显示成别的东西）")
	}
	// 2. 两个动作按钮：暂停选号 / 恢复选号。
	for _, want := range []string{`data-a="pause"`, `data-a="resume"`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 账号行缺少按钮 %s", want)
		}
	}
	// 3. 两个按钮必须各自出现在**文案**里（只有 data-a 而无可见文字 = 空按钮）。
	if !strings.Contains(js, ">暂停选号<") {
		t.Error("缺少「暂停选号」按钮文案")
	}
	if !strings.Contains(js, ">恢复选号<") {
		t.Error("缺少「恢复选号」按钮文案")
	}
	// 4. 点击分发：两个动作都要有对应的 api() 调用（只画按钮不接线 = 点了没反应）。
	if !strings.Contains(js, "'/pause'") && !strings.Contains(js, "/pause'") {
		t.Error("app.js 未调用 /pause 端点")
	}
	if !strings.Contains(js, "/resume'") {
		t.Error("app.js 未调用 /resume 端点")
	}
}

// TestPausedTagDistinctFromDisabled paused 的状态标签必须与 disabled **不同** ——
// 这是引入该状态的全部意义：面板提示要说"已暂停选号"，而不是 disabled 的
// "已禁用/需重新登录"。若两者文案相同，用户仍然无法区分"让位"与"真坏了"。
func TestPausedTagDistinctFromDisabled(t *testing.T) {
	js := readAppJS(t)
	if !strings.Contains(js, "s.disabled") {
		t.Error("缺少 disabled 分支（本用例依赖它做对照）")
	}
	// ⚠️ 必须钉住**标签分支本身**：只查 "s.paused" 字样会咬不住 ——
	// 文件里还有 renderPoolHealth / renderSkew / 按钮区三处 s.paused，
	// 把状态标签的 `else if (s.paused)` 改成 `else if (false)` 后仍能通过
	//（变异验证实测到这点）。故此处匹配完整分支头 + 紧随的标签文案。
	if !strings.Contains(js, "else if (s.paused) {") {
		t.Error("app.js 缺少 `else if (s.paused)` 状态标签分支（paused 号会落进 disabled/冷却分支）")
	}
	// 标签必须由 s.paused 分支产出（下方断言两者在相邻位置，即同一分支）。
	idx := strings.Index(js, "else if (s.paused) {")
	if idx >= 0 {
		seg := js[idx:min(idx+400, len(js))]
		if !strings.Contains(seg, "已暂停选号") {
			t.Error("`else if (s.paused)` 分支内未产出「已暂停选号」标签")
		}
	}
	// 两个标签文案必须共存且不同（分别对应两种语义）。
	if !strings.Contains(js, ">已禁用<") {
		t.Error("缺少「已禁用」标签")
	}
	if !strings.Contains(js, ">已暂停选号<") {
		t.Error("缺少「已暂停选号」标签")
	}
}

// TestPausedCountsAsUnavailableInFrontend 前端两处"可用性"统计必须把 paused
// 当不可用处理，否则会出现「横幅说 1 个可用，Pick 却返回 503」的自相矛盾。
//
// 两处：renderPoolHealth（健康横幅）与 renderSkew（负载倾斜分布）。
func TestPausedCountsAsUnavailableInFrontend(t *testing.T) {
	// 复用 frontend_test.go 的 funcBody（花括号配平取函数体）——
	// 它是本项目既有的结构级断言工具，避免用裸 strings.Index 截断导致
	// 断言命中相邻函数（本项目有多次此类测试盲区记录）。
	body := funcBody(t, "function renderPoolHealth(")
	if !strings.Contains(body, "s.paused") {
		t.Error("renderPoolHealth 未排除 paused → 暂停唯一健康号后横幅仍报「1 个可用」")
	}
	skew := funcBody(t, "function renderSkew(")
	if !strings.Contains(skew, "s.paused") {
		t.Error("renderSkew 未排除 paused → 暂停号的陈旧 success_count 会污染负载分布口径")
	}
}
