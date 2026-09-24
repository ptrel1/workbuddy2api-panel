// errors.go 错误观测：把日志里的 WARN/ERR 行按「账号 × 错误类型」聚合，
// 供面板回答运维最常问的三个问题：
//
// ① 谁在报错？   （哪个账号）
// ② 报什么错？   （类型 + 次数 + 最近一次）
// ③ 何时恢复？   （模型级限流的解除时刻）
//
// 设计心理学依据（为何"聚合"而不是"再开一个日志流"）：
//   - **前注意加工**：人眼在 200ms 内抓的是"显著特征"。原始日志里 100+ 行
//     WARN 是**同质噪音**，读者无法从中看出"32256b6a 报错 44 次、
//     685754d3 只报 1 次"这个**真正重要的差异**（44×）。
//   - **组块化（chunking）**：米勒定律——短期记忆容量 7±2。按
//     (账号, 类型) 聚合把 N 行压成 M 条（M≪N），才**可被理解**。
//   - **层级**：汇总在前（一眼看全貌）+ 明细可展开（需要时下钻），
//     避免"要么全淹没、要么看不到"的两难。
//
// 数据源：面板日志环形缓冲（Ring）。不额外引入存储 —— 日志本来就在内存里。
package panel

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// errSeverity 错误严重度（驱动 UI 语义色，对齐 dev-principles §2.4「语义化色彩」）。
type errSeverity string

const (
	sevBad  errSeverity = "bad"  // 需处理：账号被限/禁用/传输失败
	sevWarn errSeverity = "warn" // 需留意：偶发限流、参数问题
	sevInfo errSeverity = "info" // 正常但值得知道：轮转退避
)

// ErrStat 单条「账号 × 错误类型」聚合行。
type ErrStat struct {
	UID    string `json:"uid"`              // 账号 uid（空 = 非账号级错误，如 fetch）
	Kind   string `json:"kind"`             // 错误类型（中文短语，直接可读）
	Code   string `json:"code,omitempty"`   // 上游业务码（如 6004/11102），便于检索
	Count  int    `json:"count"`            // 累计次数
	Last   string `json:"last"`             // 最近一次时刻（HH:MM:SS）
	Sev    string `json:"sev"`              // bad | warn | info
	Until  string `json:"until,omitempty"`  // 解除时刻（仅模型级限流有）
	Model  string `json:"model,omitempty"`  // 相关模型（若有）
	Sample string `json:"sample,omitempty"` // 原始样本（截断，供下钻）
}

// 错误行解析：从面板日志行里抽出 uid / 上游状态 / 业务码。
//
// 样本（真实）：
//
// WARN: [upstream] chat_stream uid=32256b6a: upstream 429 soft_rate body={"code":6004,...}
// ERR:  [upstream] chat_stream uid=685754d3: transport error: Post ...: context canceled
// WARN: [upstream] fetch models: v3/config IDE-UA probe failed ...
var (
	// reErrTS 行首时间戳（可选）。Ring 存的是**已剥离时间戳**的文本，
	// 而直接读日志文件/测试样本会带时间戳 —— 两种形态都要能处理。
	reErrTS    = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} )?(\d{2}:\d{2}:\d{2}) `)
	reErrUID   = regexp.MustCompile(`uid=([0-9a-f]{8})`)
	reErrUp    = regexp.MustCompile(`upstream (\d{3}) ([a-z_]+)`)
	reErrCode  = regexp.MustCompile(`"code"\s*:\s*"?(\d+)"?`)
	reErrReset = regexp.MustCompile(`将在\s*([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})`)
	reErrModel = regexp.MustCompile(`model \[([^\]]+)\] service info not found`)
)

// kindOf 把一行错误归类为「人话类型 + 严重度 + 业务码」。
//
// 为何不直接用上游原文当类型：原文含 requestId（每次都不同）会导致
// **同一类错误被拆成 N 条**，聚合失效。必须归一到稳定短语。
func kindOf(line string) (kind, code string, sev errSeverity) {
	sev = sevWarn
	if m := reErrUp.FindStringSubmatch(line); m != nil {
		code = m[1]
		switch m[2] {
		case "soft_rate", "hard_credit":
			kind = "限流/额度"
			sev = sevBad
		case "server":
			kind = "上游 5xx"
			sev = sevBad
		case "waf_block":
			kind = "WAF 拦截"
			sev = sevBad
		case "model_blocked":
			kind = "模型不可用"
			sev = sevBad
		case "bad_params", "client":
			kind = "请求参数错误"
			sev = sevInfo // 请求本身的问题，不罚账号、不影响他人
		default:
			kind = "上游 " + m[2]
		}
	}
	if kind == "" {
		switch {
		case strings.Contains(line, "transport error"):
			kind, code, sev = "传输失败", "", sevBad
		case strings.Contains(line, "read body"):
			kind, code, sev = "响应体读取失败", "", sevBad
		case strings.Contains(line, "rotate backoff"):
			kind, code, sev = "轮转退避", "", sevInfo
		case strings.Contains(line, "probe failed"), strings.Contains(line, "fetch models"):
			kind, code, sev = "模型目录拉取失败", "", sevWarn
		default:
			return "", "", ""
		}
	}
	// 业务码细化（6004 是模型级限流，与账号级 429 语义不同）
	if m := reErrCode.FindStringSubmatch(line); m != nil && code == "" {
		code = m[1]
	}
	if strings.Contains(line, "6004") {
		kind = "模型限流(6004)"
	}
	if mm := reErrModel.FindStringSubmatch(line); mm != nil {
		kind = "模型不存在(11102)"
		code = "11102"
	}
	return kind, code, sev
}

// LogLine 聚合输入：日志文本 + 该行时刻。
//
// 为何需要独立的时刻字段：Ring 存的 Text **已剥离时间戳**（详见 ring.go），
// 故不能从文本里解析时刻；而 TS 是 Ring 记录的真实时刻。
// 首版只传 Text → 「最近」列恒为空（实测被用户发现）。
type LogLine struct {
	Text string
	TS   time.Time
}

// AggregateErrors 把日志行聚合为「账号 × (类型,模型)」统计。
//
// 只统计 WARN/ERR 行 —— INFO 不是错误（但 kindOf 会过滤掉无法归类的行）。
// 输出按 严重度 → 次数 降序：**最该看的排最前**（视觉权重与重要性一致）。
// AggregateErrors 聚合日志行（带时刻，生产路径用这个）。
func AggregateErrors(lines []LogLine) []ErrStat {
	return aggregateErrors(lines)
}

// AggregateErrorsText 聚合纯文本行（无时刻）—— 仅供测试与离线分析。
func AggregateErrorsText(lines []string) []ErrStat {
	conv := make([]LogLine, 0, len(lines))
	for _, l := range lines {
		conv = append(conv, LogLine{Text: l})
	}
	return aggregateErrors(conv)
}

func aggregateErrors(lines []LogLine) []ErrStat {
	type key struct{ uid, kind, model string }
	agg := map[key]*ErrStat{}
	order := []key{}

	for _, ln := range lines {
		txt := strings.TrimSpace(ln.Text)
		// 剥离可选的行首时间戳（并顺便取出时刻）。Ring 已剥过，日志文件未剥。
		at := ""
		if m := reErrTS.FindStringSubmatch(txt); m != nil {
			at = m[2]
			txt = txt[len(m[0]):]
		}
		if !strings.HasPrefix(txt, "WARN") && !strings.HasPrefix(txt, "ERR") {
			continue
		}
		kind, code, sev := kindOf(txt)
		if kind == "" {
			continue
		}
		uid := ""
		if m := reErrUID.FindStringSubmatch(txt); m != nil {
			uid = m[1]
		}
		model := ""
		if m := reErrModel.FindStringSubmatch(txt); m != nil {
			model = m[1]
		}
		// 模型限流：从上游文案抽"将在 … 重置"，那是**真正何时能用**
		until := ""
		if m := reErrReset.FindStringSubmatch(txt); m != nil {
			until = m[1]
		}

		k := key{uid, kind, model}
		st, ok := agg[k]
		if !ok {
			st = &ErrStat{UID: uid, Kind: kind, Code: code, Sev: string(sev),
				Model: model, Until: until,
				Sample: truncateLine(txt, 160)}
			agg[k] = st
			order = append(order, k)
		}
		st.Count++
		if until != "" {
			st.Until = until // 后来的覆盖（更接近当前状态）
		}
		// 「最近」优先用 Ring 的真实时刻（生产路径）；
		// 无时刻时退回行首解析（离线分析纯文本日志）。
		if !ln.TS.IsZero() {
			st.Last = ln.TS.Format("15:04:05")
		} else if at != "" {
			st.Last = at
		}
	}

	out := make([]ErrStat, 0, len(agg))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	// 排序：严重度优先，其次次数（多的在前）
	rank := map[string]int{"bad": 0, "warn": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Sev] != rank[out[j].Sev] {
			return rank[out[i].Sev] < rank[out[j].Sev]
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// truncateLine 截断日志行（保留头部，便于识别）。
// truncateLine 截断日志行（保留头部，便于识别）。
//
// ⚠️ 必须按 **rune** 截断而非字节：日志里大量中文（"您的使用量已超出频率限制"），
// 汉字 3 字节，按字节切会把某个字切成半截 → 产生**非法 UTF-8** →
// JSON 编码成 \ufffd、前端显示成乱码方块。实测踩过（truncateLine(s,57) 切出 \xe6\x82）。
//
// 顺带把换行/Tab 压成空格：这些样本用于 HTML title 属性，换行会被浏览器吞掉。
func truncateLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// errorsUpdatedAt 给前端显示"统计截至何时"（§十一：口径要标注时间窗）。
func errorsUpdatedAt() string {
	return time.Now().Format("15:04:05")
}
