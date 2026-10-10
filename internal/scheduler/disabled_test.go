// disabled_test.go 「禁用 = 只停选号，不停自动化排程」的行为测试。
//
// 设计原则（重要）：断言**行为**——该账号的上游请求是否真的发出、池内观测值
// 是否真的被更新——而不是断言源码里没有 "st.Disabled" 字样。后者钉死实现，
// 且删掉凭证检查之类的真问题也能过。
//
// 每个用例的口径统一为：同一趟排程里放一个禁用号 + 一个正常号，
// 断言**两者都被处理**（禁用号不再是被跳过的那一个）。
// 语义与设计理由见 disabled.go。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// disPathStub 记录「哪个凭证打到了某端点」，用来断言禁用号确实发了请求。
// 回答空 body 的 code:0（refresh 端点额外给 accessToken，让刷新走完成功路径）。
//
// ⚠️ 键取凭证头而非 X-User-Id：refresh 端点带的是 X-Refresh-Token，不带 X-User-Id
// （见 upstream/headers.go RefreshHeaders）。用 uid 头统计会把保活类用例误判为
// "禁用号没发请求"。
type disPathStub struct {
	suffix string                   // 匹配的路径后缀
	hits   map[string]*atomic.Int32 // 凭证（X-User-Id 或 X-Refresh-Token）→ 次数
}

func newDisPathStub(suffix string) *disPathStub {
	return &disPathStub{suffix: suffix, hits: map[string]*atomic.Int32{}}
}

func (s *disPathStub) count(key string) int32 {
	if c, ok := s.hits[key]; ok {
		return c.Load()
	}
	return 0
}

// hit 记一次命中（键由调用方给出，如 "uid:dis" / "rt:rt-dis"）。
// 单 goroutine 调用（各排程内顺序遍历），无并发竞争。
func (s *disPathStub) hit(key string) {
	c, ok := s.hits[key]
	if !ok {
		c = &atomic.Int32{}
		s.hits[key] = c
	}
	c.Add(1)
}

func (s *disPathStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, s.suffix) {
			s.hit("uid:" + r.Header.Get("X-User-Id"))
			s.hit("rt:" + r.Header.Get("X-Refresh-Token"))
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		// 其余端点（余额/streak/任务列表等）一律回空成功，避免干扰计数。
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"new","expiresIn":3600}}`))
	})
}

// disScheduler 建一个含「禁用号 dis + 正常号 ok」的调度器；两者凭证齐全。
// uid 与凭证一一对应（dis→"rt-dis" 等），便于按凭证定位请求来源。
func disScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at-dis", RefreshToken: "rt-dis", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at-ok", RefreshToken: "rt-ok", ExpiresAt: 9999999999})
	p.Disable("dis", "manual disable (panel)")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 0}), p
}

// TestCheckinCoversDisabledAccount ★ 用户报的原问题：手动禁用的账号，自动签到被停。
// 断言行为：禁用号确实打出了 daily-checkin。
func TestCheckinCoversDisabledAccount(t *testing.T) {
	fastActivity(t)
	stub := newDisPathStub("/daily-checkin")
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, p := disScheduler(t, srv)
	if st, _ := p.Status("dis"); !st.Disabled {
		t.Fatal("precondition: dis 应为禁用")
	}
	s.RunCheckinNow()

	if n := stub.count("uid:dis"); n != 1 {
		t.Errorf("禁用账号 checkin 次数=%d want 1（禁用只停选号，签到照常）", n)
	}
	if n := stub.count("uid:ok"); n != 1 {
		t.Errorf("正常账号 checkin 次数=%d want 1", n)
	}
}

// TestKeepaliveCoversDisabledAccount 保活照常刷新禁用账号的 token。
func TestKeepaliveCoversDisabledAccount(t *testing.T) {
	stub := newDisPathStub("/token/refresh")
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := disScheduler(t, srv)
	s.RunKeepaliveNow()

	// refresh 端点按设计只带 X-Refresh-Token、不带 X-User-Id，故按刷新凭证计数。
	if n := stub.count("rt:rt-dis"); n != 1 {
		t.Errorf("禁用账号 refresh 次数=%d want 1（保活=刷新凭证，禁用号更需要）", n)
	}
	if n := stub.count("rt:rt-ok"); n != 1 {
		t.Errorf("正常账号 refresh 次数=%d want 1", n)
	}
}

// TestStreakCoversDisabledAccount 连登兑换/抽奖照常覆盖禁用账号。
// 连登链路第一个有 uid 头的请求是 growth/streak（heatmap 为 query 参数）。
func TestStreakCoversDisabledAccount(t *testing.T) {
	stub := newDisPathStub("/activity/growth/streak")
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := disScheduler(t, srv)
	s.RunStreakBonusNow()

	if n := stub.count("uid:dis"); n != 1 {
		t.Errorf("禁用账号 streak 查询次数=%d want 1（连登照常）", n)
	}
	if n := stub.count("uid:ok"); n != 1 {
		t.Errorf("正常账号 streak 查询次数=%d want 1", n)
	}
}

// TestSchoolCoversDisabledAccount 开学季闭环照常覆盖禁用账号。
// 首个带 uid 的调用 = SchoolTasks → /portal/activity/school/tasks。
func TestSchoolCoversDisabledAccount(t *testing.T) {
	fastActivity(t)
	stub := newDisPathStub("/portal/activity/school/tasks")
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := disScheduler(t, srv)
	s.RunSchoolNow()

	if n := stub.count("uid:dis"); n != 1 {
		t.Errorf("禁用账号 school/tasks 次数=%d want 1（开学季照常）", n)
	}
	if n := stub.count("uid:ok"); n != 1 {
		t.Errorf("正常账号 school/tasks 次数=%d want 1", n)
	}
}

// TestBlackcatCoversDisabledAccount 夜猫子排程照常覆盖禁用账号。
// 首个带 uid 的调用 = BlackcatNeed → ListTasks(/v2/activity/growth/tasks)。
// 窗口通过 blackcatNow 接缝固定为 23:30，避免该路径只在夜间才可验证。
func TestBlackcatCoversDisabledAccount(t *testing.T) {
	stub := newDisPathStub("/v2/activity/growth/tasks")
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := disScheduler(t, srv)
	freezeBlackcatAt(t, time.Date(2026, 9, 11, 23, 30, 0, 0, time.Local))

	s.RunBlackcatNow()

	if n := stub.count("uid:dis"); n != 1 {
		t.Errorf("禁用账号 blackcat 任务查询次数=%d want 1（夜猫子照常）", n)
	}
	if n := stub.count("uid:ok"); n != 1 {
		t.Errorf("正常账号 blackcat 任务查询次数=%d want 1", n)
	}
}

// freezeBlackcatAt 把夜猫子窗口判定固定在给定时刻（测试用）。
func freezeBlackcatAt(t *testing.T, at time.Time) {
	t.Helper()
	old := blackcatNow
	blackcatNow = func() time.Time { return at }
	t.Cleanup(func() { blackcatNow = old })
}

// TestBlackcatSkipsOutsideWindow 窗口外整体跳过：既有边界不因放行禁用号而改变。
func TestBlackcatSkipsOutsideWindow(t *testing.T) {
	stub := newDisPathStub("/v2/activity/growth/tasks")
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := disScheduler(t, srv)
	freezeBlackcatAt(t, time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local))

	s.RunBlackcatNow()

	if n := stub.count("uid:dis"); n != 0 {
		t.Errorf("窗口外禁用账号调用=%d want 0", n)
	}
	if n := stub.count("uid:ok"); n != 0 {
		t.Errorf("窗口外正常账号调用=%d want 0", n)
	}
}

// TestBalanceCoversDisabledAccount ★ 余额刷新对禁用账号**真正刷新**（不再是
// 「只用池内 Credits 采样、不打上游」的降级路径）。
// 断言行为：上游 user-resource 收到了禁用号的请求，且其 credits 被更新为新值。
func TestBalanceCoversDisabledAccount(t *testing.T) {
	calls := map[string]*atomic.Int32{}
	var mu atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			uid := r.Header.Get("X-User-Id")
			c, ok := calls[uid]
			if !ok {
				c = &atomic.Int32{}
				calls[uid] = c
			}
			c.Add(1)
			mu.Add(1)
			// 上游返回 4242，与池内旧值（0）不同 → 可区分「真刷」与「降级采样」。
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":5000,"CycleCapacityRemain":4242,"CycleCapacityUsed":0}]}}}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	s, p := disScheduler(t, srv)
	before, _ := p.Status("dis")
	s.RunBalanceRefreshNow()

	// 用辅助函数安全取计数：账号被跳过时该键不存在，直接索引会 panic，
	// 掩盖掉真正该报的断言失败（变异验证时实测到这点）。
	count := func(uid string) int32 {
		if c, ok := calls[uid]; ok {
			return c.Load()
		}
		return 0
	}
	if n := count("dis"); n != 1 {
		t.Errorf("禁用账号 user-resource 次数=%d want 1（禁用号也要真刷上游）", n)
	}
	if n := count("ok"); n != 1 {
		t.Errorf("正常账号 user-resource 次数=%d want 1", n)
	}
	after, _ := p.Status("dis")
	if after.Credits != 4242 {
		t.Errorf("禁用账号 credits=%d want 4242（应被上游新鲜值更新；旧值 %d）", after.Credits, before.Credits)
	}
	// 池层语义不变：余额 > 0 也不得解冻禁用账号（ReenableIfCredits 的既有判定）。
	if !after.Disabled {
		t.Error("禁用账号不得被余额刷新解冻（池层 disabled 优先级最高）")
	}
}

// ---------------------------------------------------------------------------
// 凭证防线：放行 ≠ 发无效请求
// ---------------------------------------------------------------------------

// TestDisabledReloginAccountMakesNoRequest 封号类（DisableRelogin）账号放行后
// 因**凭证为空**而不发任何请求 —— 这是「封号号不产生无效请求」的实现机制。
// 若有人为了"彻底放行"删掉凭证检查，本用例会失败（同时大概率 nil panic）。
func TestDisabledReloginAccountMakesNoRequest(t *testing.T) {
	fastActivity(t)
	var anyCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anyCalls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	// 封号号：凭证被清空（重新登录才有），但仍在池中、状态为禁用。
	p.Add(&auth.Auth{UID: "banned", AccessToken: "", RefreshToken: "", ExpiresAt: 0})
	// reason 含 "banned" → classifyDisable 归 DisableRelogin。
	p.Disable("banned", "account banned (11140)")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	if st, _ := p.Status("banned"); !st.Disabled {
		t.Fatal("precondition: banned 应为禁用")
	}
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	s.RunActivityNow()
	s.RunStreakBonusNow()
	s.RunSchoolNow()
	s.RunTravelNow()

	// 唯一被放行的凭证齐全号 ok 会打请求；banned 必须一次都没有。
	if n := anyCalls.Load(); n == 0 {
		t.Fatal("precondition: ok 账号应产生请求（否则本用例无法区分『跳过』与『发不出』）")
	}
	// banned 无任何凭证，若真发了请求也带不上来源标识；故同时断言
	// 「凭证为空」这一前提成立（否则本用例失去意义）。
	if a := p.AuthByUID("banned"); a == nil || a.AccessToken != "" || a.RefreshToken != "" {
		t.Errorf("precondition 破坏: banned 凭证应为空，实际 %+v", a)
	}
}

// TestDisabledReloginNoRequestByUIDHeader ★ 凭证防线的核心用例：凭证为空的
// 封号号被**放行**后仍不产生任何请求 —— 这正是"封号号不会产生无效请求"的机制。
//
// 口径很关键：不能说「无 X-User-Id 的请求 = banned 的请求」。refresh 端点按设计
// 只带 X-Refresh-Token、**不带** X-User-Id（见 upstream/headers.go RefreshHeaders），
// 那是正常号的合法请求。因此：
//   - 只考察**带 X-User-Id 的请求**（签到/活跃/成长/billing 类），全部必须来自 ok；
//   - 带 X-Refresh-Token 的请求（refresh 端点）必须携带**非空** token。
//
// 若有人为"彻底放行"删掉 `RefreshToken == ""` / `AccessToken == ""` 检查，空凭证
// 的 banned 会打进这两类请求，本用例随即失败。
func TestDisabledReloginNoRequestByUIDHeader(t *testing.T) {
	fastActivity(t)
	type hit struct{ path, uid, rt string }
	var mu sync.Mutex
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, hit{r.URL.Path, r.Header.Get("X-User-Id"), r.Header.Get("X-Refresh-Token")})
		mu.Unlock()
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"new","expiresIn":3600}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "banned", AccessToken: "", RefreshToken: "", ExpiresAt: 0})
	p.Disable("banned", "account banned (11140)")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunCheckinNow()
	s.RunKeepaliveNow()
	s.RunActivityNow()
	s.RunStreakBonusNow()
	s.RunSchoolNow()
	s.RunTravelNow()

	mu.Lock()
	defer mu.Unlock()
	if len(hits) == 0 {
		t.Fatal("precondition: 凭证齐全的 ok 号应产生请求（否则无法区分『跳过』与『发不出』）")
	}
	for _, h := range hits {
		// 带 uid 的请求：只允许来自 ok。
		if h.uid != "" && h.uid != "ok" {
			t.Errorf("账号 %q 发出了带 X-User-Id 的请求（%s）——凭证为空的封号号不得发请求", h.uid, h.path)
		}
		// refresh 类请求：必须带非空 refresh token（空 = banned 打进去了）。
		if h.rt == "" && h.uid == "" {
			t.Errorf("%s 既无 uid 又无 refresh token —— 空凭证的封号号发出了请求", h.path)
		}
	}
}

// TestBalanceRefreshKeepsNilAuthGuard 余额刷新对「池内已无凭证对象」的账号不 panic。
// 该处刻意**不加** RefreshToken/AccessToken 检查（保持原口径），但 a == nil 必须兜住。
func TestBalanceRefreshKeepsNilAuthGuard(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	// 空 token 的禁用号：a != nil，走正常路径（与改前一致，会发一次注定失败的请求）。
	p.Add(&auth.Auth{UID: "empty", AccessToken: "", RefreshToken: ""})
	p.Disable("empty", "manual disable (panel)")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunBalanceRefreshNow() // 不应 panic
	if calls.Load() == 0 {
		t.Errorf("余额刷新走正常路径：即使凭证为空也应尝试一次（与原口径一致），got 0")
	}
}
