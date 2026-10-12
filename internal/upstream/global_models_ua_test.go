package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ─────────────────────────────────────────────────────────────────────────────
// global /v3/config 三路 UA 并集（吸收上游 7b24e22）
//
// 背景：/v3/config 对 User-Agent 敏感，不同 UA 下发**不同模型集合**。
// 此前 global 探测只走 IDE UA 一路 → 客户端可见而网关目录缺失的模型永远拉不到
// （gpt-6-sol / gpt-6-luna / grok-4.7 / gemini-3.8-flash 只在桌面端 UA 下出现）。
// ─────────────────────────────────────────────────────────────────────────────

func mkUAModel(id string, ctx, out int64, credits string, efforts []string) ModelInfo {
	return ModelInfo{ID: id, ContextWindow: ctx, MaxTokens: out, Credits: credits, Efforts: efforts}
}

// TestMergeV3RoutesUnionAndPriority 三路并集 + **主路字段权威**。
func TestMergeV3RoutesUnionAndPriority(t *testing.T) {
	desktop := probeResult{
		names: []string{"shared", "only-desktop"},
		infos: []ModelInfo{
			mkUAModel("shared", 1000000, 128000, "x1.33", []string{"low", "high"}),
			mkUAModel("only-desktop", 500000, 64000, "x0.10", []string{"medium"}),
		},
	}
	ide := probeResult{
		names: []string{"shared", "only-ide"},
		infos: []ModelInfo{
			// 同 id 但字段不同（窄）：**不得覆盖主路**。
			mkUAModel("shared", 176000, 24000, "x9.99", []string{"max"}),
			mkUAModel("only-ide", 200000, 32000, "x0.20", nil),
		},
	}
	cli := probeResult{
		names: []string{"shared", "only-cli"},
		infos: []ModelInfo{
			mkUAModel("shared", 272000, 72000, "x8.88", []string{"xhigh"}),
			mkUAModel("only-cli", 256000, 32000, "x0.30", nil),
		},
	}
	got := mergeV3Routes([]uaRoute{
		{"desktop-UA", desktop},
		{"IDE-UA", ide},
		{"CLI-UA", cli},
	})
	if got.err != nil {
		t.Fatalf("unexpected err: %v", got.err)
	}
	// 三路独有模型必须**都在**（这是本次修复的核心价值）。
	want := map[string]bool{"shared": true, "only-desktop": true, "only-ide": true, "only-cli": true}
	if len(got.names) != len(want) {
		t.Fatalf("names=%v, want 4 条（三路并集）", got.names)
	}
	for _, id := range got.names {
		if !want[id] {
			t.Errorf("多出意外模型 %q（%v）", id, got.names)
		}
	}
	// ⭐ 字段权威：shared 的窗口/兜底必须来自**主路**（桌面端），不被 IDE/CLI 覆盖。
	var shared *ModelInfo
	for i := range got.infos {
		if got.infos[i].ID == "shared" {
			shared = &got.infos[i]
		}
	}
	if shared == nil {
		t.Fatal("shared 缺失")
	}
	if shared.ContextWindow != 1000000 {
		t.Errorf("shared 窗口=%d，want 1000000（主路桌面端值，未被 IDE/CLI 覆盖）", shared.ContextWindow)
	}
	if shared.Credits != "x1.33" {
		t.Errorf("shared credits=%q，want x1.33（主路值）", shared.Credits)
	}
}

// TestMergeV3RoutesDesktopOnlyCarriesNewModels 桌面端路独有的模型（gpt-6-sol 等）
// 必须进入结果 —— 这正是修复前缺失的部分。
func TestMergeV3RoutesDesktopOnlyCarriesNewModels(t *testing.T) {
	desktop := probeResult{
		names: []string{"gpt-6-sol", "gpt-6-luna"},
		infos: []ModelInfo{mkUAModel("gpt-6-sol", 1, 1, "", nil), mkUAModel("gpt-6-luna", 1, 1, "", nil)},
	}
	ide := probeResult{names: []string{"o4-mini"}, infos: []ModelInfo{mkUAModel("o4-mini", 1, 1, "", nil)}}
	got := mergeV3Routes([]uaRoute{{"desktop-UA", desktop}, {"IDE-UA", ide}})
	if got.err != nil {
		t.Fatal(got.err)
	}
	found := map[string]bool{}
	for _, n := range got.names {
		found[n] = true
	}
	if !found["gpt-6-sol"] || !found["gpt-6-luna"] {
		t.Errorf("桌面端独有模型缺失（这正是修复目标）：%v", got.names)
	}
	if !found["o4-mini"] {
		t.Errorf("IDE 独有模型缺失：%v", got.names)
	}
}

// TestMergeV3RoutesDegrades 单路失败降级为其余路；全失败才返回 err。
func TestMergeV3RoutesDegrades(t *testing.T) {
	ok := probeResult{names: []string{"m1"}, infos: []ModelInfo{mkUAModel("m1", 1, 1, "", nil)}}
	bad := probeResult{err: errors.New("boom")}

	// 主路失败 → 退到 IDE 路结果（不返回 err）。
	got := mergeV3Routes([]uaRoute{{"desktop-UA", bad}, {"IDE-UA", ok}})
	if got.err != nil {
		t.Fatalf("单路失败不该整体失败：%v", got.err)
	}
	if len(got.names) != 1 || got.names[0] != "m1" {
		t.Errorf("降级结果=%v，want [m1]", got.names)
	}

	// 全失败 → 返回 err（下游走"v3 路失败"分支，维持负缓存语义）。
	all := mergeV3Routes([]uaRoute{{"desktop-UA", bad}, {"IDE-UA", bad}, {"CLI-UA", bad}})
	if all.err == nil {
		t.Error("三路全失败应返回 err（负缓存语义）")
	}
}

// TestMergeV3RoutesEmptyNotError 空结果不算失败（该 UA 可能本就不下发模型），
// 但不得作为合并起点（否则会用空结果当权威）。
func TestMergeV3RoutesEmptyNotError(t *testing.T) {
	empty := probeResult{names: nil, infos: nil}
	ok := probeResult{names: []string{"m1"}, infos: []ModelInfo{mkUAModel("m1", 42, 42, "", nil)}}
	got := mergeV3Routes([]uaRoute{{"desktop-UA", empty}, {"IDE-UA", ok}})
	if got.err != nil {
		t.Fatalf("空结果不该算失败：%v", got.err)
	}
	if len(got.names) != 1 || got.names[0] != "m1" {
		t.Errorf("got %v, want [m1]", got.names)
	}
	if got.infos[0].ContextWindow != 42 {
		t.Error("空路被当成了合并起点（字段丢失）")
	}
}

// TestProbeGlobalModelsSendsThreeUAs ⭐ **接线级**测试：probeGlobalModels 必须
// 真的发起**三路** /v3/config 请求（桌面端 + IDE + CLI）。
//
// 为什么必须有这条：上面几个测试只覆盖纯函数 mergeV3Routes —— 若把接线改成
// "只跑 IDE 一路"（即修复前的行为），纯函数测试**照样通过**（实测：该变异咬不住）。
// 唯一能验证"接线正确"的方式是看**上游实际收到了哪些 UA**。
//
// 判据：fake HTTP 记录每个 /v3/config 请求的 User-Agent，断言三路 UA 都出现。
func TestProbeGlobalModelsSendsThreeUAs(t *testing.T) {
	// ⚠️ 三路 UA 是**并发**探测（三个 goroutine 同时打 /v3/config）→ 记录 uas
	// 必须加锁，否则 data race（实测被 `-race` 抓到，且会导致测试时好时坏）。
	var (
		mu  sync.Mutex
		uas []string
	)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				mu.Lock()
				uas = append(uas, r.Header.Get("User-Agent"))
				mu.Unlock()
				// 每路都返回一个独有模型，便于顺带验证并集。
				// ⚠️ id 必须用**能区分三路**的标记：桌面端与 CLI 的 UA 第一段都是
				// `WorkBuddy/5.5.4`，用 Fields(ua)[0] 会让两路 id 相撞 → 并集去重后
				// 少一个，测试假红（实测踩过）。
				ua := r.Header.Get("User-Agent")
				id := "m-unknown"
				switch {
				case strings.Contains(ua, "WorkBuddy AI"):
					id = "m-desktop"
				case strings.Contains(ua, "CodeBuddyIDE"):
					id = "m-ide"
				case strings.Contains(ua, "CLI/"):
					id = "m-cli"
				}
				body := `{"code":0,"data":{"models":[{"id":"` + id + `","name":"` + id + `"}]}}`
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			// 企业端点家族：返回 500 让主路结果胜出（本测试只关心 UA 接线）。
			return &http.Response{
				StatusCode: 500,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	// global 账号：Realm() 依赖全局开关，故临时开启并显式写入 realm。
	// ⚠️ 必须**保存原值再恢复**，不能硬编码恢复为 false —— 该开关默认 true
	// （auth 的 init 里 Store(true)），硬编码 false 会**污染同包其它测试**
	// （实测：导致 TestChatTimingAccumulatesAcrossFallbackRetry 的 global
	// fallback 路径失效、单独跑过而全量跑挂）。
	origGlobal := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(origGlobal)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatal(err)
	}

	names, _, _, _, err := c.probeGlobalModels(a)
	if err != nil {
		t.Fatalf("probeGlobalModels err: %v", err)
	}

	// ⭐ 核心断言：三路 UA 都发过请求。
	mu.Lock()
	gotUAs := append([]string(nil), uas...)
	mu.Unlock()
	var hasDesktop, hasIDE, hasCLI bool
	for _, ua := range gotUAs {
		switch {
		case strings.Contains(ua, "WorkBuddy AI"): // 桌面端（global 平台段）
			hasDesktop = true
		case strings.Contains(ua, "CodeBuddyIDE"):
			hasIDE = true
		case strings.Contains(ua, "WorkBuddy/") && strings.Contains(ua, "CLI/"):
			// CLI UA 形如 `WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1`
			// （注意不要与桌面端 UA 混淆：桌面端含 "WorkBuddy AI" 平台段）。
			if !strings.Contains(ua, "WorkBuddy AI") {
				hasCLI = true
			}
		}
	}
	if !hasDesktop {
		t.Errorf("未发起桌面端 UA 探测（这是本次修复的核心）—— 实际 UA 列表：%v", gotUAs)
	}
	if !hasIDE {
		t.Errorf("未发起 IDE UA 探测 —— 实际：%v", gotUAs)
	}
	if !hasCLI {
		t.Errorf("未发起 CLI UA 探测 —— 实际：%v", gotUAs)
	}
	// 顺带：三路独有模型应都进入并集（每路返回一个 id 不同的模型）。
	if len(names) < 3 {
		t.Errorf("并集应含三路各自的模型，得 %v", names)
	}
}
