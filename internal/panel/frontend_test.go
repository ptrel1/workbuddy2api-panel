package panel

import (
	"encoding/json"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/balance"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestRealmSegregatedViewsFrontend 前端必须**按域过滤**后再渲染，而不是把
// cn 与 global 的账号混在一张图/一张表里 —— 两域积分不同源、不可互换，
// 混画会让 global 的低水位被 cn 的高水位压成贴底直线（信息量为 0）。
//
// 断言方式：检查 app.js 源码里存在"过滤 + 分域渲染"的调用链。
// 这类断言对"是否真的过滤"没有行为级验证能力，故同时断言**不得**出现
// 跨域求和的特征（见下一个测试），两道合起来才咬得住。
func TestRealmSegregatedViewsFrontend(t *testing.T) {
	src := readAppJS(t)
	for _, want := range []string{
		"function realmOf(",
		"function pickRealm(",
		"function filterRealm(",
		"renderRealmSeg(",
		"renderBalanceFoot(",
		"bhRealmSeg",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少分域渲染要素 %q", want)
		}
	}
	// renderBalanceChart / renderDailyBars 必须以**过滤后**的 rows 调用，
	// 而非原始 all —— 否则图例与图形会包含另一个域的账号。
	if !strings.Contains(src, "renderBalanceChart(rows)") {
		t.Error("renderBalanceChart 未使用过滤后的 rows")
	}
	if !strings.Contains(src, "renderDailyBars(rows)") {
		t.Error("renderDailyBars 未使用过滤后的 rows")
	}
	// 关键断言：rows 必须来自 filterRealm(all, ...)，且不得退化为 all。
	// 变异测试暴露的教训：只查 renderXXX(rows) 咬不住——把 filterRealm
	// 换成 all 后，调用点写法一字未变，测试仍全绿。必须钉住 rows 的赋值来源。
	if !strings.Contains(src, "filterRealm(all, bhRealm)") {
		t.Error("rows 未由 filterRealm 派生（分域过滤失效）")
	}
	if strings.Contains(src, "const rows = all") {
		t.Error("rows 直接取自全量账号，分域过滤被绕过")
	}
}

// TestNoCrossRealmTotal 不得出现跨域总计：cn 与 global 的积分不同源
// （连上游 base 与模型目录都不同），相加得出的数没有意义。
//
// 与 TestRealmSegregatedViewsFrontend 互补：前者查"有没有分域"，
// 本测试查"有没有偷偷跨域求和"。变异验证：把 filterRealm 改成返回 rows
// （不过滤）不会让本测试变红，但会让 §1 的 rows 断言语义失效——故两者必须同时存在。
func TestNoCrossRealmTotal(t *testing.T) {
	src := readAppJS(t)
	if strings.Contains(src, "all.reduce(") {
		t.Error("疑似对未过滤的全量账号求和（跨域总计）——cn/global 积分不可相加")
	}
	// 合计行必须显式声明"合计（某域）"，让用户知道这是域内小计而非全局
	if !strings.Contains(src, "esc(realmLabel(bhRealm))") {
		t.Error("合计行未标注所属域，会被误读为全局总计")
	}
}

// TestFootDoesNotSumMinMax 「最低/最高」列**不可求和**：
// 各账号的低谷/高峰不在同一时刻，Σ最低 会得出一个从未存在过的水位
// （§十：不得产出物理上不可能的值）。故合计行该两列必须填 "—"。
//
// 断言强度说明：本条**结构性扫描**而非字符串白名单——变异测试暴露过
// 白名单式守卫（列举 "+= s.min" 等字面量）一改写法就漏（如 cur += Number(s.min)）。
// 现在改为：在函数体内，凡出现 s.min / s.max 的**读写**即视为违规，
// 因为该函数对它们唯一合法的用法就是"不碰"。
func TestFootDoesNotSumMinMax(t *testing.T) {
	body := funcBody(t, "function renderBalanceFoot(")

	if !strings.Contains(body, `class="num na"`) {
		t.Error("合计行的最低/最高列未用 na 占位（疑似被求和）")
	}
	// 结构性守卫：函数体内不得出现任何对 s.min / s.max 的引用。
	// （合法实现只需输出占位符 "—"，无需读取这两个字段。）
	for _, field := range []string{"s.min", "s.max"} {
		if strings.Contains(body, field) {
			t.Errorf("合计行引用了不可加的字段 %q——各账号低谷/高峰不同时刻，求和无物理意义", field)
		}
	}
}

// TestBalanceHistoryExposesRealm 行为级测试：/panel/api/balance_history 返回的
// 每条序列必须带 realm，且值来自**池中账号的实际域**。
//
// 为什么必须有行为级测试：前端的分域渲染完全依赖该字段。若后端漏注入
// （SetRealm 未调用），前端所有账号都会落进 "other" 组 —— 表现为"切换器
// 只有一个'其他'"、分域功能静默失效，而**源码级断言全都通过**
// （变异测试实证：移除 SetRealm 注入时，仅查源码的守卫不咬人）。
func TestBalanceHistoryExposesRealm(t *testing.T) {
	pl := pool.New("")
	cn := &auth.Auth{UID: "uid-cn", AccessToken: "a", Domain: "www.codebuddy.cn"}
	gb := &auth.Auth{UID: "uid-global", AccessToken: "b", Domain: "www.workbuddy.ai"}
	cn.BackfillRealm()
	gb.BackfillRealm()
	pl.Add(cn)
	pl.Add(gb)

	rec := balance.New("")
	rec.SetName("uid-cn", "国内号")
	rec.Add("uid-cn", 100)
	rec.SetName("uid-global", "国际号")
	rec.Add("uid-global", 200)

	p := New(Config{Version: "test", APIKey: "", Pool: pl, Balance: rec})
	r := httptest.NewRequest("GET", "/panel/api/balance_history?days=30", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Series []struct {
			UID   string `json:"uid"`
			Realm string `json:"realm"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	got := map[string]string{}
	for _, s := range out.Series {
		got[s.UID] = s.Realm
	}
	if got["uid-cn"] != "cn" {
		t.Errorf("uid-cn realm = %q, want cn（SetRealm 注入缺失会让前端全部落进 other）", got["uid-cn"])
	}
	if got["uid-global"] != "global" {
		t.Errorf("uid-global realm = %q, want global", got["uid-global"])
	}
}

// funcBody 取某函数（以 sig 开头）的函数体文本，用于结构级断言。
func funcBody(t *testing.T, sig string) string {
	t.Helper()
	src := readAppJS(t)
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("未找到函数 %q", sig)
	}
	// 从签名后第一个 '{' 起做花括号配平，避免被函数内的字符串/嵌套括号误导。
	j := strings.Index(src[i:], "{")
	if j < 0 {
		t.Fatalf("函数 %q 无函数体", sig)
	}
	depth := 0
	for k := i + j; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : k+1]
			}
		}
	}
	t.Fatalf("函数 %q 花括号未配平", sig)
	return ""
}

// TestFailoverColorSemantics 账号身份色板不得包含涨跌语义色（--ok 绿 / --bad 红）：
// 同一颜色承载两个互斥含义（"涨"与"某账号"）会造成双重编码冲突。
func TestFailoverColorSemantics(t *testing.T) {
	src := readAppJS(t)
	i := strings.Index(src, "const BH_COLORS = [")
	if i < 0 {
		t.Fatal("BH_COLORS 不存在")
	}
	j := strings.Index(src[i:], "];")
	def := src[i : i+j]
	if strings.Contains(def, "var(--ok)") || strings.Contains(def, "var(--bad)") {
		t.Errorf("账号身份色板复用了涨跌语义色，会与每日变化柱的双色语义冲突：%s", def)
	}
}

// readAppJS 读取 app.js 源码文本（供源码级断言；行为级验证由 JS 语法测试兜底）。
func readAppJS(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
