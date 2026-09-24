package panel

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/balance"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（运行时闸门，与 TestAppJSSyntax 互补）：
// node + DOM 桩真正**执行** app.js 顶层代码（含按 hash 落到各视图的 go() 顶层调用），
// 捕捉 TDZ / ReferenceError 类运行时错误。
//
// 为什么需要（不是重复 TestAppJSSyntax）：语法检查对运行时错误全盲。app.js 的顶层
// let/const 声明散落在整个文件（plansMap@153、mToggleUID@155、queueTimer@1448…），
// 而顶层 go() 调用在 ~148 行——任何在 go() 同步调用链上被提前访问的晚期声明都会抛
// TDZ ReferenceError，使**整个脚本从该行起停止求值**：后续 onclick 全失效、start()
// 不执行，用户看到的是刷新即崩的面板。本项目已两度踩坑（上游 v1.11.3/v1.11.4 两连炸、
// 本地 e5802d1 白屏事故），且 v1.24.0 又引入一例（mToggleUID@2279），故入册为常驻闸门。
//
// 判据：全视图 hash 逐个执行，任一视图未到 EOF 即失败。**视图清单必须与实际路由
// （app.js 的 TITLES）保持一致**——只测部分视图会漏掉分支特有的 TDZ。
// 无 node 环境跳过（不阻塞无 Node 的构建机）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS top-level smoke")
	}
	// harness：在 vm 沙箱里求值 app.js。DOM/网络全部用惰性 Proxy 兜住——本测试
	// 只关心"顶层求值能否走到末行"，不关心渲染结果，故无需真实 DOM 实现。
	// app.js 顶层 start() 里的 setInterval 会让 node 事件循环不退出，成功路径显式 exit(0)。
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	// 9 个视图全覆盖（对齐 app.js 的 TITLES）。空 hash 也测一次——那是默认落地路径。
	for _, hash := range []string{"", "#accounts", "#usage", "#balhist", "#quota", "#packages", "#taskscenter", "#models", "#config", "#logs"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值崩溃（hash=%q）: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js 冒烟未通过（hash=%q）:\n%s", hash, out)
		}
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
	// ⚠️ 判据要精确到「求和」而非「出现 all.reduce(」——
	// 实证误报：新加的账号×模型折线图用 all.reduce(Math.min/max) 求
	// **时间戳范围**（不是求和），被这条粗判据误报。
	// 故改为：只揪「把值累加」的写法（a + ...），放行 min/max/比较类。
	for _, bad := range []string{"all.reduce((a, s) => a +", "all.reduce((a, p) => a +"} {
		if strings.Contains(src, bad) {
			t.Errorf("疑似对未过滤的全量账号求和（跨域总计）：%q —— cn/global 积分不可相加", bad)
		}
	}
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

// TestModelPageRealmParam 模型页必须按域取数：接口接受 realm 参数，
// 且用 PickExcludingForRealm 而非 Pick()。
//
// 为什么：Pick() 取**随机账号**，其域是偶然的 —— 而 cn/global 的上游 base 与
// 模型目录**都不同**，导致同一页面刷新两次可能看到不同列表
// （同一输入不同输出 = 不确定性缺陷）。见 dev-principles.md 第十四节。
func TestModelPageRealmParam(t *testing.T) {
	// APIKey 置空 = 不鉴权，直接进 handler（本测试只关心域参数的路由与文案）
	p := New(Config{Version: "test", APIKey: "", Pool: pool.New("")})
	rec := httptest.NewRecorder()
	// 无账号时返回 503，但错误文案必须点出所请求的域（便于自查）
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/models?realm=global", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503（测试面板无账号）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "global") {
		t.Errorf("503 文案未点出所请求的域，排查困难：%s", rec.Body.String())
	}
}

// TestModelsNoRandomPick 行为级守卫：模型接口不得再用 Pool.Pick()（随机账号）。
// 源码级断言，因为该缺陷的表现是"结果不确定"，无法用单次调用断言。
func TestModelsNoRandomPick(t *testing.T) {
	path, _ := filepath.Abs("panel.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func (p *Panel) models(")
	if i < 0 {
		t.Fatal("models handler 不存在")
	}
	j := strings.Index(src[i:], "\n}\n")
	body := src[i : i+j]
	// 逐行剥离行注释后再判断：函数头注释里提到 Pool.Pick() 是说明历史缺陷，
	// 不能因此判红（变异测试暴露：直接 Contains 会被自己的注释骗过）。
	var code strings.Builder
	for _, ln := range strings.Split(body, "\n") {
		if k := strings.Index(ln, "//"); k >= 0 {
			ln = ln[:k]
		}
		code.WriteString(ln)
		code.WriteString("\n")
	}
	if strings.Contains(code.String(), "Pool.Pick()") {
		t.Error("models 仍用 Pool.Pick()（随机账号）——两域目录不同会导致刷新结果飘忽")
	}
	if !strings.Contains(body, "PickExcludingForRealm") {
		t.Error("models 未按域取号（PickExcludingForRealm）")
	}
	// effort 档位不得写死 "cn"
	if strings.Contains(body, `EffortListing("cn"`) {
		t.Error(`EffortListing 仍写死 "cn"——global 模型会误用 cn 档位表`)
	}
	if !strings.Contains(body, "EffortListing(realm") {
		t.Error("EffortListing 未使用请求的 realm")
	}
}

// TestModelProbeKeyExactMatch 探测键必须按「域前缀精确匹配」，不得用
// endsWith(':'+id) 模糊查找 —— 跨域同名模型时 find() 只命中第一个，
// 会让 global 表格显示 cn 的探测结果（静默串数据）。
func TestModelProbeKeyExactMatch(t *testing.T) {
	src := readAppJS(t)
	if strings.Contains(src, "endsWith(':' + id)") || strings.Contains(src, `endsWith(":" + id)`) {
		t.Error("probeOf 仍用 endsWith 模糊匹配——跨域同名模型会串用探测数据")
	}
	if !strings.Contains(src, "probes[realm + ':' + id]") {
		t.Error("probeOf 未按 realm 前缀精确匹配")
	}
}

// TestMdRealmSegWired 模型页的域切换器必须存在且与账�balances 页共用 BH_REALMS。
func TestMdRealmSegWired(t *testing.T) {
	js := readAppJS(t)
	for _, want := range []string{"mdRealmSeg", "renderMdRealmSeg", "let mdRealm"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q", want)
		}
	}
	html := readIndexHTML(t)
	if !strings.Contains(html, `id="mdRealmSeg"`) {
		t.Error("index.html 缺少模型页域切换容器 #mdRealmSeg")
	}
}

// readIndexHTML 读取 index.html 源码文本。
func readIndexHTML(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLogoPreviewPage Logo 候选页必须可访问、无鉴权（只含展示内容，不含账号/密钥），
// 且引用同源脚本（严格 CSP 下内联脚本会被拦，页面将白屏）。
func TestLogoPreviewPage(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "secret-key"}) // 故意启用鉴权
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/logo", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /panel/logo = %d, want 200（展示页不需鉴权）", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("含内联 <script> 会被严格 CSP 拦截导致白屏")
	}
	if !strings.Contains(body, `src="logo.js"`) {
		t.Error("未引用同源 logo.js")
	}
	// 展示页不得泄露任何密钥/账号信息
	for _, leak := range []string{"secret-key", "api_key", "access_token", "refresh_token"} {
		if strings.Contains(body, leak) {
			t.Errorf("展示页泄露敏感内容 %q", leak)
		}
	}
	// 脚本必须可访问且是 JS
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, httptest.NewRequest("GET", "/panel/logo.js", nil))
	if rec2.Code != 200 {
		t.Fatalf("GET /panel/logo.js = %d, want 200", rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("logo.js Content-Type = %q, want javascript", ct)
	}
	// 安全头与面板其它页面一致
	if rec2.Header().Get("Content-Security-Policy") == "" {
		t.Error("logo.js 缺少 CSP 头")
	}
}

// TestLogoJSNoInlineHandler logo.js 不得含内联事件属性（CSP script-src 'self'
// 只允许外部脚本，内联 onclick/onload 属性同样会被拦）。
func TestLogoJSNoInlineHandler(t *testing.T) {
	b, err := os.ReadFile("logo.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// 允许 .onclick = fn 这类 DOM 属性赋值；禁止 HTML 属性形式的 on*="..."
	for _, bad := range []string{`onclick="`, `onload="`, `onchange="`} {
		if strings.Contains(src, bad) {
			t.Errorf("logo.js 含内联事件属性 %s（会被 CSP 拦截）", bad)
		}
	}
}

// TestFaviconEmbedded favicon 必须内嵌 data URI（零额外路由、零外部请求），
// 且解码后是合法 SVG。含糊或缺失会让浏览器标签显示默认空白图标。
func TestFaviconEmbedded(t *testing.T) {
	html := readIndexHTML(t)
	m := regexp.MustCompile(`<link rel="icon"[^>]*href="([^"]+)"`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("index.html 缺少 <link rel=\"icon\">（浏览器标签会显示空白图标）")
	}
	uri := m[1]
	const pfx = "data:image/svg+xml,"
	if !strings.HasPrefix(uri, pfx) {
		t.Fatalf("favicon 未内嵌 data URI：%s", uri[:min(40, len(uri))])
	}
	svg, err := url.QueryUnescape(strings.TrimPrefix(uri, pfx))
	if err != nil {
		t.Fatalf("favicon data URI 解码失败：%v", err)
	}
	// 必须是合法 XML（否则部分浏览器不渲染）
	if _, err := xmlDecode(svg); err != nil {
		t.Fatalf("favicon 不是合法 SVG：%v\n%s", err, svg)
	}
	// 必须跟随系统明暗，否则浅色系统下是深底方块、深色下是白底方块
	if !strings.Contains(svg, "prefers-color-scheme") {
		t.Error("favicon 未按 prefers-color-scheme 适配明暗（favicon 读不到页面 data-theme，只能用媒体查询）")
	}
}

// TestBrandLogoPresent 顶栏 brand 必须有图标（此前只有纯文字）。
// 图标用 currentColor 单色，随主题变色；不得写死颜色。
func TestBrandLogoPresent(t *testing.T) {
	html := readIndexHTML(t)
	i := strings.Index(html, `class="brand"`)
	if i < 0 {
		t.Fatal("未找到 .brand 区块")
	}
	j := strings.Index(html[i:], "</div>\n    <ul>")
	body := html[i : i+j]
	if !strings.Contains(body, `class="mark"`) {
		t.Error("顶栏 brand 缺少图标（.mark）")
	}
	if !strings.Contains(body, "currentColor") {
		t.Error("brand 图标未用 currentColor（写死颜色会不随主题变色）")
	}
	// 样式必须给 .brand .mark 定尺寸，否则 SVG 会用默认 300x150 撑破侧栏
	css := html[:strings.Index(html, "</style>")]
	if !strings.Contains(css, ".brand .mark") {
		t.Error("缺少 .brand .mark 尺寸样式（SVG 无尺寸会撑破 208px 侧栏）")
	}
}

// TestLogoPageStillListsAllCandidates Logo 对比页必须仍列出全部 6 个候选
// （选定 2 号后，对比页保留作为决策留痕，不得删减）。
func TestLogoPageStillListsAllCandidates(t *testing.T) {
	b, err := os.ReadFile("logo.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	n := strings.Count(src, "\n    ['")
	if n != 6 {
		t.Errorf("候选数 = %d, want 6（对比页应保留全部候选作为留痕）", n)
	}
	for _, name := range []string{"池 · 三层环", "W 折线", "汇聚路由", "六边节点", "双环交错", "盾 · 闸"} {
		if !strings.Contains(src, name) {
			t.Errorf("候选 %q 缺失", name)
		}
	}
}

// xmlDecode 只校验 XML 合法性（不需要取内容）：用 Decoder 遍历一次即可。
func xmlDecode(s string) (any, error) {
	d := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return nil, nil
			}
			return nil, err
		}
	}
}

// TestModelHintNoStaleModelName 模型页提示语不得硬编码**具体模型名与数值**。
//
// 事故背景（20260917）：提示原文写「实测多数模型被限制在 40K 左右（如
// deepseek-v4.1-flash 仅 32K）」，但随后模型目录改为双 UA 并集，
// deepseek-v4.1-flash 的显示值变成 393216 —— **提示与列表自相矛盾**，
// 且该模型当时甚至不在列表里（只在 CLI 目录）。
//
// 判据：提示只描述**口径与不确定性**，不引用会随上游变动的具体事实。
func TestModelHintNoStaleModelName(t *testing.T) {
	html := readIndexHTML(t)
	i := strings.Index(html, "模型能力")
	if i < 0 {
		t.Fatal("未找到模型页标题")
	}
	j := strings.Index(html[i:], "</h3>")
	hint := html[i : i+j]

	for _, bad := range []string{"40K", "deepseek", "32K"} {
		if strings.Contains(hint, bad) {
			t.Errorf("模型页提示含硬编码的具体事实 %q（会随后续改动失真，与列表矛盾）:\n%s", bad, hint)
		}
	}
	// 必须保留「仅供参考/未验证」的不确定性声明 —— 这是我们诚实的边界
	if !strings.Contains(hint, "参考") {
		t.Error("提示丢失「仅供参考」声明：会把声称值当保证值")
	}
	// 必须说明本页取较大值（否则用户不知道为何与官方不同）
	if !strings.Contains(hint, "较大值") {
		t.Error("提示未说明「取较大值」口径（用户会疑惑为何与官方不同）")
	}
}

// TestAvailabilityUIFullyWired 可用性功能的**每个 UI 要素**都必须在
// index.html 与 app.js 两侧都存在。
//
// 为何需要这条（20260918 实踩）：我此前只加了 thead 的「可用性」列与 app.js 的
// 渲染逻辑，**按钮与结果摘要位漏加**（编辑脚本的 assert 锚点因上下文变化静默失配），
// 结果后端接口已上线、列头也有，但**用户根本找不到触发入口**。
// 源码级断言抓不到"两侧不配对"，故这条**逐个要素核对双侧存在性**。
func TestAvailabilityUIFullyWired(t *testing.T) {
	html := readIndexHTML(t)
	js := readAppJS(t)

	// 必须在 index.html 存在的 DOM 锚点
	for _, id := range []string{"btnProbeAvail", "mdAvailNote"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s（可用性功能入口/摘要缺失）", id)
		}
		// 且 app.js 必须真的用了它（只加 DOM 不接线 = 死按钮）
		if !strings.Contains(js, id) {
			t.Errorf("app.js 未引用 #%s（DOM 存在但无逻辑，等于死按钮）", id)
		}
	}
	// 表头列
	if !strings.Contains(html, "可用性</th>") {
		t.Error("index.html 表头缺少「可用性」列")
	}
	// 渲染函数与后端调用
	for _, want := range []string{"function availCell(", "model_availability/probe", "model_availability'"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q", want)
		}
	}
	// 按钮必须真的绑定了 handler（不能只有 DOM）
	if !strings.Contains(js, "$('btnProbeAvail').onclick") {
		t.Error("按钮未绑定 onclick —— 点击无反应")
	}
}

// TestAppJSStartsWithUseStrict app.js 必须以 'use strict' 开头（第 1 行）。
//
// 事故（20260918，严重）：我用脚本"回退"一段错误实现时，`index()` 匹配到了
// **另一处**同名代码，结果删错范围 —— 把正确实现的函数体**整段搬到了文件顶部**，
// 留在 'use strict' 之前。因此处仍是合法 JS（只是顶层多了一段孤立代码），
// **node --check 与所有 Go 测试全绿**，但运行时 `'use strict'` 不再是首条语句，
// 且原函数体已空 —— **整个面板白屏、什么都加载不出来**。
//
// 判据：这类"文件结构被破坏但语法仍合法"的缺陷，只有**结构断言**能抓到。
// 语法检查（TestAppJSSyntax）抓不到，因为它只验证"能否解析"。
func TestAppJSStartsWithUseStrict(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) == 0 {
		t.Fatal("app.js 为空")
	}
	if strings.TrimSpace(lines[0]) != "'use strict';" {
		t.Errorf("app.js 第 1 行 = %q，want 'use strict';"+
			"（前面有游离代码说明文件结构被破坏——运行时可能整体失效）",
			strings.TrimSpace(lines[0]))
	}
	// 反向守卫：'use strict' 只能出现一次（重复出现是被搬移的迹象）
	if n := strings.Count(string(raw), "'use strict';"); n != 1 {
		t.Errorf("'use strict' 出现 %d 次，want 1（重复 = 代码被搬移/复制）", n)
	}
}

// TestChartFunctionsHaveBody 每个自绘图表函数体内必须**真的有坐标系定义**。
//
// 与上一条互补：这条防"函数体被挖空"（定义为空壳但语法合法）。
// 事故同源：搬移后 renderBalanceChart 的体被清空，函数仍存在、仍可解析，
// 但一调用就不产出图形。
func TestChartFunctionsHaveBody(t *testing.T) {
	for _, fn := range []string{"function renderBalanceChart(", "function renderDailyBars(", "function renderUsageChart("} {
		body := funcBody(t, fn)
		// 每个图表都必须：量宽取 W、算出 iw/ih、构造 svg
		for _, need := range []string{"measureWidth(host)", "const iw =", "viewBox="} {
			if !strings.Contains(body, need) {
				t.Errorf("%s 的体内缺少 %q（函数可能被挖空）", fn, need)
			}
		}
	}
}

// TestPoolHealthUIFullyWired 池子健康横幅与负载倾斜条必须**双侧存在**。
// （教训：v1.17 曾出现"后端+列头都有、按钮漏加"，故逐个要素核对。）
func TestPoolHealthUIFullyWired(t *testing.T) {
	html := readIndexHTML(t)
	js := readAppJS(t)
	for _, id := range []string{"poolAlert", "skewBar"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s", id)
		}
	}
	for _, fn := range []string{"function renderPoolHealth(", "function renderSkew("} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	// 必须真的被 renderAccounts 调用（只定义不调用 = 死代码）
	if !strings.Contains(js, "renderPoolHealth(list)") || !strings.Contains(js, "renderSkew(list)") {
		t.Error("renderAccounts 未调用 renderPoolHealth/renderSkew（定义了却不生效）")
	}
}

// TestRateLimitDetailRendered 限流详情必须渲染 `rate_limited_models`。
//
// 背景：后端 pool.Status 早有 RateLimitedModels（注释写明"运维据此看到账号 A 的
// 模型 X 还在限额中，预计 Z 时间恢复"），但前端**从未渲染** —— 用户只看到
// "限流冷却·22h"四个字，不知道是**哪个模型**被限，也就想不到可以切模型绕开。
func TestRateLimitDetailRendered(t *testing.T) {
	js := readAppJS(t)
	if !strings.Contains(js, "rate_limited_models") {
		t.Error("前端未渲染 rate_limited_models（后端有数据、运维看不到是哪个模型被限）")
	}
	// 必须展示恢复时间（reset_at 是上游权威墙钟）
	if !strings.Contains(js, "reset_at") {
		t.Error("限流详情未展示恢复时间（reset_at）——用户无法判断何时能再用")
	}
}

// TestSkewComputedPerRealm 负载倾斜必须**按域分组**计算。
//
// 为何（真实数据验证）：global 号与 cn 号不共享负载（不同上游 base/模型目录）。
// 实测 cn 三号 2059/2780/7114、global 号仅 27 次 —— 混算会把倾斜度从 1.79×
// 虚高到 2.38×，误导运维以为 cn 也不均衡。
func TestSkewComputedPerRealm(t *testing.T) {
	body := funcBody(t, "function renderSkew(")
	if !strings.Contains(body, "realm") {
		t.Fatal("renderSkew 未按 realm 分组")
	}
	// 必须有按域分组的动作（groups[realm] 或等价结构）
	if !strings.Contains(body, "groups[") {
		t.Error("renderSkew 未按域隔离统计（混算会让 global 号拉偏 cn 的倾斜度）")
	}
	// 关键：分组的**键**必须真的来自 s.realm。
	// 变异测试暴露——只查 `groups[` 咬不住：把键换成常量 'all'（即混算）
	// 后 `groups[` 依然存在，测试仍绿。必须钉住“键由 realm 派生”。
	if !strings.Contains(body, "s.realm") {
		t.Error("renderSkew 的分组键未取自 s.realm（用常量键 = 实际是混算，指标会失真）")
	}
	// 单账号域不该画倾斜条（无"分布"可言）
	if !strings.Contains(body, "length < 2") {
		t.Error("renderSkew 未跳过单账号域（单号无倾斜度可言，画出来是噪音）")
	}
}

// TestErrorsUIFullyWired 错误汇总的每个 UI 要素必须**双侧存在**。
// （教训：v1.17 曾出现"后端+列头都有、按钮漏加"→ 用户找不到入口。）
func TestErrorsUIFullyWired(t *testing.T) {
	html := readIndexHTML(t)
	js := readAppJS(t)
	for _, id := range []string{"errBox", "errBody", "errNote", "btnErrors"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s", id)
		}
	}
	for _, fn := range []string{"function renderErrors(", "async function loadErrors(", "function isErrLine("} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	// 必须真的被调用（定义≠生效）
	if !strings.Contains(js, "loadErrors()") {
		t.Error("loadErrors 未被调用（定义了却不生效）")
	}
	if !strings.Contains(js, "$('btnErrors').onclick") {
		t.Error("刷新按钮未绑定 onclick —— 点击无反应")
	}
	// 错误频道 chip 必须在（否则看不到逐行错误）
	if !strings.Contains(html, `data-ch="err"`) {
		t.Error("日志页缺少「错误」频道 chip")
	}
}

// TestErrChannelFilterIsStrict 错误频道必须用严格口径（WARN/ERR 开头），
// 不得用 /error|失败/ 模糊匹配 —— 后者会把正常行的"0 个失败"也算成错误。
func TestErrChannelFilterIsStrict(t *testing.T) {
	body := funcBody(t, "function isErrLine(")
	if !strings.Contains(body, "startsWith('WARN')") || !strings.Contains(body, "startsWith('ERR')") {
		t.Error("isErrLine 未用严格的 WARN/ERR 前缀判定")
	}
	if strings.Contains(body, "test(") {
		t.Error("isErrLine 用了正则模糊匹配（会把正常行误判为错误）")
	}
}

// TestErrorsRenderUntilTime 汇总必须展示「解除时间」—— 这是运维最关心的问题
// （"什么时候能恢复"），后端已从上游文案抽出，前端不得丢弃。
func TestErrorsRenderUntilTime(t *testing.T) {
	body := funcBody(t, "function renderErrors(")
	// 关键：必须把 **st.until 的值**渲染出来，而不是只出现 "until" 这个字样。
	// 变异测试暴露——原先只查 `"until"` 存在，把整段换成常量 '—' 后仍绿。
	if !strings.Contains(body, "st.until") {
		t.Error("renderErrors 未读取 st.until（解除时间未被展示，用户无法判断何时恢复）")
	}
	if !strings.Contains(body, "esc(st.until)") {
		t.Error("renderErrors 读取了 st.until 但未渲染其值（只读不用 = 白读）")
	}
	// 无 until 时必须显式显示 —，不得留空（留空会被读成"没有这个问题"）
	if !strings.Contains(body, "'—'") && !strings.Contains(body, `"—"`) {
		t.Error("无解除时间时未显式标注 —（留空会被误读）")
	}
	// 无 uid 的错误（如全局 fetch 失败）必须与"某账号有问题"区分
	if !strings.Contains(body, "全局") {
		t.Error("未区分「全局错误」与「账号错误」——会把全局问题栽给某个账号")
	}
}

// TestErrHandlerPassesTimestamp errorsHandler 必须把 Ring 的 TS **传给**聚合函数。
//
// 变异测试暴露：只测聚合函数（AggregateErrors）咬不住这个 ——
// 把 handler 里的 `TS: e.TS` 删掉（重放原始 bug），聚合函数的单测仍全绿，
// 但生产路径的「最近」列会再次恒空。**必须在 handler 层面钉住。**
func TestErrHandlerPassesTimestamp(t *testing.T) {
	path, _ := filepath.Abs("panel.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func (p *Panel) errorsHandler(")
	if i < 0 {
		t.Fatal("errorsHandler 不存在")
	}
	j := strings.Index(src[i:], "\n}\n")
	body := src[i : i+j]
	if !strings.Contains(body, "e.TS") {
		t.Error("errorsHandler 未把 LogEntry.TS 传给聚合（Ring 的 Text 已剥时间戳 → 「最近」列会恒空）")
	}
}

// TestErrorAccountShowsNickname 错误汇总的账号列必须优先显示**昵称**。
//
// 用户实测反馈「怎么没写哪个账号」：只显示 8 位 uid（1aeaf3ba）
// 等于让用户自己做「哈希 → 人」的映射，这是把认知负担推给用户。
// 昵称（周五口/pt/汪羽杰）才是用户认知里的"账号"。
func TestErrorAccountShowsNickname(t *testing.T) {
	js := readAppJS(t)
	body := funcBody(t, "function renderErrors(")
	if !strings.Contains(body, "uidNames") {
		t.Error("renderErrors 未查昵称映射（只显示 uid，用户认不出是哪个账号）")
	}
	if !strings.Contains(body, "uidName") {
		t.Error("renderErrors 未使用昵称变量")
	}
	// 必须真的把昵称渲染出来（只查不改 = 白查）
	if !strings.Contains(body, "esc(uidName)") {
		t.Error("renderErrors 查了昵称但未渲染（只查不用）")
	}
	// 昵称映射必须从 overview 重建（昵称可能在面板里被改）
	if !strings.Contains(js, "refreshUIDNames(") {
		t.Error("缺少 refreshUIDNames（昵称映射未建立/未刷新）")
	}
	if !strings.Contains(js, "refreshUIDNames(d.accounts") {
		t.Error("loadOverview 未重建昵称映射（改昵称后错误汇总不更新）")
	}
}

// TestModelRateLimitShownInAccountPool 模型级限流必须显示在**账号池**的状态列。
//
// 用户实测反馈「不能在账号池里写哪个账号哪个模型有限流吗」。
// 根因：6004 是**模型级**限流 —— 它写 `modelCooldowns`，
// **不设账号级 until**（cooldown.go 注释明写"不写 until（全账号级冷却不受
// 模型级限流污染）"）。所以 `cooling` 仍为 false，而账号对该模型**实际不可用**。
// 首版把限流详情嵌在 `else if (cool > 0)` 分支里 → **模型级限流永远不显示**。
//
// 断言方式：**逐字符跟踪花括号深度**，确认 rlm 块与 cooling 分支**同深度**
// （即在其之外），而不是被嵌进去。
// ⚠️ 我第一版用「括号计数 > 」的粗判据，误报了一次 —— 必须精确跟踪深度。
func TestModelRateLimitShownInAccountPool(t *testing.T) {
	src := readAppJS(t)
	i := strings.Index(src, "function renderAccounts(")
	if i < 0 {
		t.Fatal("renderAccounts 不存在")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}\n"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "rate_limited_models") {
		t.Fatal("账号池未读取 rate_limited_models")
	}

	// 逐字符跟踪深度：记录 cooling 分支的深度与 rlm 块的深度
	depth := 0
	coolDepth, rlmDepth := -1, -1
	lines := strings.Split(body, "\n")
	for _, ln := range lines {
		// 进入该行前先看它属于哪个深度
		if strings.Contains(ln, "else if (cool > 0)") && coolDepth < 0 {
			coolDepth = depth
		}
		if strings.Contains(ln, "const rlm = s.rate_limited_models") && rlmDepth < 0 {
			rlmDepth = depth
		}
		depth += strings.Count(ln, "{") - strings.Count(ln, "}")
	}
	if coolDepth < 0 {
		t.Fatal("未找到 cooling 分支")
	}
	if rlmDepth < 0 {
		t.Fatal("未找到 rlm 块")
	}
	if rlmDepth > coolDepth {
		t.Errorf("限流块深度 %d > cooling 分支深度 %d —— 它被嵌在 cooling 内，"+
			"而模型级限流不设账号级 until（cooling 恒 false）→ **永远不显示**",
			rlmDepth, coolDepth)
	}
}

// TestPartialLimitStatusTag 有模型被限时状态标签不得是绿色"可用"。
//
// 判据（§十 同源）：不得产出与事实矛盾的展示 ——
// "可用"紧挨"⛔ 模型限流"就是自相矛盾，用户无法判断到底能不能用。
func TestPartialLimitStatusTag(t *testing.T) {
	body := funcBody(t, "function renderAccounts(")
	if !strings.Contains(body, "部分受限") {
		t.Error("有模型被限时仍显示绿色「可用」（与下方的⛔限流自相矛盾）")
	}
}

// TestPlanUIFullyWired 套餐标记 + 筛选器的每个要素必须**双侧存在且被调用**。
func TestPlanUIFullyWired(t *testing.T) {
	html := readIndexHTML(t)
	js := readAppJS(t)
	if !strings.Contains(html, `id="accPlanFilter"`) {
		t.Error("index.html 缺少套餐筛选器 #accPlanFilter")
	}
	for _, fn := range []string{"function planTag(", "function loadPlans("} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	// 必须真的被用（定义≠生效）
	if !strings.Contains(js, "planTag(s.uid)") {
		t.Error("renderAccounts 未调用 planTag（套餐标记不会显示）")
	}
	if !strings.Contains(js, "loadPlans()") {
		t.Error("loadPlans 未被调用（套餐数据永不加载）")
	}
	if !strings.Contains(js, "$('accPlanFilter').onchange") {
		t.Error("筛选器未绑定 onchange（选了没反应）")
	}
}

// TestPlanFilterAppliedBeforeRender 套餐筛选必须**在渲染前**过滤，
// 否则统计卡与表格会不一致（表里 2 个、卡说 4 个）。
func TestPlanFilterAppliedBeforeRender(t *testing.T) {
	body := funcBody(t, "function renderAccounts(")
	iFilter := strings.Index(body, "planFilter")
	iRender := strings.Index(body, "tb.innerHTML =")
	if iFilter < 0 {
		t.Fatal("renderAccounts 未实现套餐筛选")
	}
	if iRender >= 0 && iFilter > iRender {
		t.Error("套餐筛选在渲染之后才做 —— 统计卡与表格会不一致")
	}
}

// TestQuotaUIFullyWired 额度评估视图的每个要素必须**双侧存在且被调用**。
// （这条断言此前救过一次：plan_js.py 脚本中途失败只落一半改动，是它先报红。）
func TestQuotaUIFullyWired(t *testing.T) {
	html := readIndexHTML(t)
	js := readAppJS(t)
	for _, id := range []string{"view-quota", "quotaBody", "quotaCaveat", "quotaStats", "btnQuota"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s", id)
		}
	}
	if !strings.Contains(html, `data-view="quota"`) {
		t.Error("侧栏缺少「额度评估」导航项（页面存在但进不去）")
	}
	for _, fn := range []string{"function renderQuota(", "async function loadQuota(", "function fmtTokens("} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	if !strings.Contains(js, "if (v === 'quota') loadQuota()") {
		t.Error("路由未接 loadQuota（点导航没反应）")
	}
	if !strings.Contains(js, "$('btnQuota').onclick") {
		t.Error("刷新按钮未绑定 onclick")
	}
}

// TestQuotaCaveatRenderedFromServer 口径声明必须**由服务端下发并原样渲染**。
//
// 为何不能在服务端写死字符串、前端自己拼：那样两边可能漂移（改了一边忘另一边），
// 而这段文案是本模块**诚实性的载体** —— 少了它，峰值会被当成官方额度。
func TestQuotaCaveatRenderedFromServer(t *testing.T) {
	body := funcBody(t, "function renderQuota(")
	if !strings.Contains(body, "rep.caveat") {
		t.Fatal("renderQuota 未使用服务端下发的 caveat（口径声明可能缺失或前端自造）")
	}
	if !strings.Contains(body, "esc(rep.caveat") {
		t.Error("caveat 未渲染出来（读了不用）")
	}
}

// TestModelToggleUIFullyWired 模型开关的每个要素必须**双侧存在且被调用**。
// （这条断言此前已救过两次 —— 脚本中途失败只落一半改动。）
func TestModelToggleUIFullyWired(t *testing.T) {
	html := readIndexHTML(t)
	js := readAppJS(t)
	for _, id := range []string{"mToggleVeil", "mToggleBody", "mToggleWho", "btnMToggleClose",
		"btnMToggleAllOn", "btnMToggleAllOff"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s", id)
		}
	}
	for _, fn := range []string{"function openModelToggle(", "function renderModelToggle(", "function toggleModel(", "function toggleAllModels("} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	// 账号行的「模型」按钮 + 动作分发（只加按钮不接线 = 点了没反应）
	if !strings.Contains(js, `data-a="models"`) {
		t.Error("账号行缺少「模型」按钮")
	}
	if !strings.Contains(js, `a === 'models'`) {
		t.Error("动作分发未接 models（按钮点了没反应）")
	}
	if !strings.Contains(js, "openModelToggle(u)") {
		t.Error("未调用 openModelToggle")
	}
	// 格子点击必须绑定（动态渲染 → 用事件委托）
	if !strings.Contains(js, "$('mToggleBody').addEventListener") {
		t.Error("模型格子未绑定点击（用委托）—— 点了没反应")
	}
}

// TestToggleRollbackOnFailure 开关失败必须**回滚乐观更新**。
// 停在乐观值上会让用户以为设置成功了，而实际没生效 —— 比不反馈更糟。
func TestToggleRollbackOnFailure(t *testing.T) {
	body := funcBody(t, "async function toggleModel(")
	if !strings.Contains(body, "catch") {
		t.Fatal("toggleModel 未处理失败")
	}
	// 必须把状态改回去（而不是只弹个 toast）
	if !strings.Contains(body, "chip.classList.toggle('off', wasOff)") {
		t.Error("失败后未回滚 UI 状态 —— 用户会以为开关已生效")
	}
}

// TestModelToggleHidesOnlyPermanentlyUnavailable 只隐藏**永久不可用**（11102
// model_blocked），必须**保留**临时限流（soft_rate）等其他 kind。
//
// 为何关键：实测主力模型 deepseek-v4.1-flash 曾因 429 限流被判 ok=false，
// 若按「不可用就隐藏」一刀切，**用户会看不到自己的主力模型**（而它几小时后
// 就能用）。判据必须精确到 kind。
func TestModelToggleHidesOnlyPermanentlyUnavailable(t *testing.T) {
	body := funcBody(t, "async function openModelToggle(")
	if !strings.Contains(body, "model_blocked") {
		t.Fatal("未按 model_blocked 过滤（不可用模型会被列出）")
	}
	if !strings.Contains(body, "a.kind === 'model_blocked'") {
		t.Error("过滤条件未精确到 kind（一刀切会隐藏临时限流的模型）")
	}
	// 必须只在 !a.ok 且 kind=model_blocked 时隐藏
	if !strings.Contains(body, "!a.ok") {
		t.Error("未检查 a.ok —— 会把可用模型也隐藏")
	}
}

// TestModelToggleGhostStateGuarded 筛选/切页时必须关掉模型开关 modal ——
// 否则用户会看到"一个已不在表里的账号"的开关（对象身份与视图不一致）。
func TestModelToggleGhostStateGuarded(t *testing.T) {
	src := readAppJS(t)
	// ① 切页时关
	if !strings.Contains(src, "if (v !== 'accounts') closeModelToggle()") {
		t.Error("切页时未关 modal（离开再回来它还开着，用户已忘了点的哪个账号）")
	}
	// ② 筛选后账号不可见时关
	body := funcBody(t, "function renderAccounts(")
	if !strings.Contains(body, "closeModelToggle()") {
		t.Error("筛选后未检查抽屉所属账号是否仍可见（幽灵状态）")
	}
}

// TestAmsDegradesWhenTooFewPoints 账号×模型折线图必须**区分**够画与不够画 ——
// 实测 30 个组合中仅 3 个 ≥7 点，其余 1~2 点连不成线（画出来像故障）。
func TestAmsDegradesWhenTooFewPoints(t *testing.T) {
	js := readAppJS(t)
	// 关键：必须**真的用它做过滤**，而不是只出现 s.enough 这个字样。
	// 变异实证：把 `series.filter(s => s.enough)` 改成 `series` 后，
	// 只查符号存在的断言仍绿。
	if !strings.Contains(js, "series.filter(s => s.enough)") {
		t.Error("未用 enough 过滤（1~2 点的组合也会被画成折线，看起来像故障）")
		if !strings.Contains(js, "function loadAms(") || !strings.Contains(js, "function renderAms(") {
			t.Error("缺少 loadAms/renderAms")
		}
	}
	// 必须显式告知"可画 N / 共 M"（否则读者不知道为什么只有几条线）
	if !strings.Contains(js, "可画 ") {
		t.Error("未标注可画条数（读者会以为数据缺失）")
	}
	// 点数不足时必须给降级说明而非空图
	if !strings.Contains(js, "点数不足") {
		t.Error("点数不足时未降级说明")
	}
}

// TestUsageModelTablePerf 按模型表必须带「均延迟 / 均速率」两列。
//
// 为什么需要：此前只有「按账号」表有这两列，按模型统计看不到速度——而"哪个模型快"
// 正是用量页最该回答的问题之一。数据后端早已聚合（KeyedAgg 内嵌 Agg），只是渲染时
// 用 withPerf=false 关掉了，属"数据在但没接出来"。
// 本测试同时守住**列数与 colspan 一致**：usRow 的 withPerf 多输出两列，若表头/
// colspan 忘了同步就是列错位（usRow 注释里明确警告过这类缺陷）。
func TestUsageModelTablePerf(t *testing.T) {
	html := readIndexHTML(t)

	// 1) 模型表渲染必须开 withPerf（否则两列永远不输出）。
	body := funcBody(t, "function renderUsage(")
	mIdx := strings.Index(body, "$('usModelBody').innerHTML")
	if mIdx < 0 {
		t.Fatal("renderUsage 未渲染 usModelBody")
	}
	// 该赋值语句跨行（usRow(...) 在下一行），取到下一个空行为止再判定。
	line := body[mIdx:]
	if end := strings.Index(line, "\n\n"); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, ", true)") {
		t.Errorf("模型表未开启 withPerf（均延迟/均速率列不会显示）:\n%s", line)
	}

	// 2) 表头必须有这两列，且措辞与账号表一致（统一视觉语言）。
	seg := html
	if i := strings.Index(html, `id="usModelBody"`); i > 0 {
		seg = html[:i]
	}
	if j := strings.LastIndex(seg, "<table"); j >= 0 {
		seg = seg[j:]
	}
	for _, h := range []string{">均延迟<", ">均速率<"} {
		if !strings.Contains(seg, h) {
			t.Errorf("按模型表头缺少 %s（表头与行会错位）", h)
		}
	}

	// 3) 空行 colspan 必须等于列数：mark + 模型 + 5 数据列 + 2 性能列 = 9。
	if !strings.Contains(body, `colspan="9"`) {
		t.Error(`模型表空行 colspan 应为 9（mark+模型+请求/失败/Prompt/Completion/合计+均延迟+均速率）`)
	}
}

// TestPerfSampleGate 性能均值必须过样本数闸门，不得裸展示。
//
// 为什么需要：均值按样本加权，但样本数可能只有个位数（冷门模型/短窗口）。把
// 「8 个请求的均值」当「该模型的速度」展示是误导——数据不会报错但会误导人。
// 前端必须：① 经 fmtPerf 渲染 ② 低于阈值显示 "—" ③ 悬停可见样本数（不静默隐藏）。
func TestPerfSampleGate(t *testing.T) {
	js := readAppJS(t)

	if !strings.Contains(js, "const PERF_MIN_SAMPLES") {
		t.Fatal("缺少 PERF_MIN_SAMPLES 阈值常量")
	}
	if !strings.Contains(js, "function fmtPerf(") {
		t.Fatal("缺少 fmtPerf（样本数判据渲染函数）")
	}
	// usRow 的两列必须走 fmtPerf，且传入后端样本数字段。
	row := funcBody(t, "function usRow(")
	for _, want := range []string{"fmtPerf(a.avg_latency_ms, a.latency_samples", "fmtPerf(a.avg_tokens_per_second, a.tps_samples"} {
		if !strings.Contains(row, want) {
			t.Errorf("usRow 未按样本数闸门渲染: 缺少 %s", want)
		}
	}
	// 不得回退成裸 fmtRate/fmtMs（那会绕过闸门）。
	if strings.Contains(row, "fmtMs(a.avg_latency_ms)") {
		t.Error("usRow 仍裸调 fmtMs(avg_latency_ms) —— 绕过了样本数闸门")
	}
	if strings.Contains(row, "fmtRate(a.avg_tokens_per_second)") {
		t.Error("usRow 仍裸调 fmtRate(avg_tokens_per_second) —— 绕过了样本数闸门")
	}
	// 样本数不足时要有明确占位与说明，不能静默输出空串。
	if !strings.Contains(js, "不足") {
		t.Error("fmtPerf 未对少样本给出说明文案（会静默显示 —，用户不知为何）")
	}
}
