package panel

import (
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
