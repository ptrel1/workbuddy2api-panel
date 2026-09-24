package panel

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 真实日志样本（取自生产日志，含各类错误形态）。
var realErrLines = []string{
	`2026/09/23 16:40:56 WARN: [upstream] chat_stream uid=32256b6a: upstream 429 soft_rate body={"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-24 13:46:45 UTC+8 重置，您也可以切换其他模型继续使用。"}`,
	`2026/09/23 16:40:58 WARN: [upstream] chat_stream uid=1aeaf3ba: upstream 429 soft_rate body={"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-24 13:02:47 UTC+8 重置。"}`,
	`2026/09/23 16:40:57 WARN: [upstream] chat_stream uid=32256b6a: upstream 429 soft_rate body={"code":6004,"msg":"将在 2026-09-24 13:46:45 UTC+8 重置"}`,
	`2026/09/20 16:50:11 ERR: [upstream] chat_stream uid=685754d3: transport error: Post "https://copilot.tencent.com/v2/chat/completions": net/http: timeout awaiting response headers`,
	`2026/09/23 15:35:56 WARN: [upstream] chat_stream uid=685754d3: upstream 429 soft_rate body={"code":14003,"msg":"too many requests"}`,
	`2026/09/23 10:00:00 WARN: [upstream] chat_stream uid=32256b6a: upstream 400 model_blocked body={"code":11102,"msg":"model [deepseek-v3-1] service info not found"}`,
	`2026/09/18 10:36:19 WARN: [upstream] fetch models: v3/config IDE-UA probe failed (CLI-UA only): timeout`,
	`2026/09/23 16:32:35 WARN: [server] rotate backoff aborted: ctx cancelled`,
	`2026/09/17 08:00:00 INFO: [upstream] balance-sample abc: credits=100`, // 非错误，应被忽略
}

// TestAggregateErrorsGroupsByAccountAndKind 聚合必须按「账号 × 类型」分组，
// 把 N 行原始日志压成 M 条（M≪N）—— 这是"能看懂"的前提。
func TestAggregateErrorsGroupsByAccountAndKind(t *testing.T) {
	stats := AggregateErrorsText(realErrLines)
	if len(stats) == 0 {
		t.Fatal("未聚合出任何错误")
	}
	var rate *ErrStat
	for i := range stats {
		if stats[i].UID == "32256b6a" && strings.Contains(stats[i].Kind, "6004") {
			rate = &stats[i]
		}
	}
	if rate == nil {
		t.Fatalf("未找到 32256b6a 的 6004 限流条目，实际: %+v", stats)
	}
	if rate.Count != 2 {
		t.Errorf("32256b6a 6004 次数 = %d, want 2（同一类必须合并，不能被 requestId 拆开）", rate.Count)
	}
	// 必须带"何时解除"（上游文案里的重置时间）—— 这是运维最关心的
	if !strings.Contains(rate.Until, "2026-09-24 13:46:45") {
		t.Errorf("未抽出解除时间，得 %q", rate.Until)
	}
	if rate.Sev != "bad" {
		t.Errorf("限流应判 bad，得 %q", rate.Sev)
	}
}

// TestAggregateErrorsSortedBySeverityThenCount 排序必须「严重度优先，其次次数」——
// 最该看的排最前（视觉权重与重要性一致）。
func TestAggregateErrorsSortedBySeverityThenCount(t *testing.T) {
	stats := AggregateErrorsText(realErrLines)
	rank := map[string]int{"bad": 0, "warn": 1, "info": 2}
	for i := 1; i < len(stats); i++ {
		a, b := stats[i-1], stats[i]
		if rank[a.Sev] > rank[b.Sev] {
			t.Errorf("严重度排序错：%s(%s) 排在 %s(%s) 之前", a.Sev, a.Kind, b.Sev, b.Kind)
		}
		if rank[a.Sev] == rank[b.Sev] && a.Count < b.Count {
			t.Errorf("同严重度应按次数降序：%d 排在 %d 之前", a.Count, b.Count)
		}
	}
}

// TestAggregateErrorsIgnoresNonErrors INFO/正常行不得计入（否则统计被稀释）。
func TestAggregateErrorsIgnoresNonErrors(t *testing.T) {
	stats := AggregateErrorsText([]string{
		`2026/09/17 08:00:00 INFO: [upstream] balance-sample abc: credits=100`,
		`2026/09/17 08:00:01 loaded 4 account(s) from ./auths`,
		`| #001 | 16:52:06 | cn:deepseek | sync | 200 | uid=685754d3 | TTFB=1.2s |`,
	})
	if len(stats) != 0 {
		t.Errorf("非错误行被计入: %+v", stats)
	}
}

// TestAggregateErrorsModelBlocked 11102 必须识别为「模型不可用」并带模型名。
func TestAggregateErrorsModelBlocked(t *testing.T) {
	stats := AggregateErrorsText(realErrLines)
	var found *ErrStat
	for i := range stats {
		if stats[i].Code == "11102" {
			found = &stats[i]
		}
	}
	if found == nil {
		t.Fatal("未识别 11102 模型不可用")
	}
	if found.Model != "deepseek-v3-1" {
		t.Errorf("未抽出模型名，得 %q", found.Model)
	}
}

// TestAggregateErrorsTransportFailures 传输层失败无业务码，也必须被归类。
func TestAggregateErrorsTransportFailures(t *testing.T) {
	stats := AggregateErrorsText(realErrLines)
	var found bool
	for _, s := range stats {
		if s.Kind == "传输失败" && s.UID == "685754d3" {
			found = true
		}
	}
	if !found {
		t.Errorf("未归类传输失败: %+v", stats)
	}
}

// TestAggregateErrorsUsesRingTimestamp 「最近」时刻必须取自 Ring 的 TS 字段。
//
// 事故（用户实测发现"最近"列恒为 —）：Ring 存的 Text **已剥离时间戳**
// （ring.go 的 tsPrefixRe），所以从文本里解析时刻**永远解析不到**。
// 首版只把 Text 传给聚合 → 「最近」列恒空。
// 必须传结构化 LogLine{Text, TS}。
func TestAggregateErrorsUsesRingTimestamp(t *testing.T) {
	ts := time.Date(2026, 9, 23, 16, 40, 56, 0, time.Local)
	// 模拟 Ring 的输出：Text 无时间戳，TS 单独带
	lines := []LogLine{{
		Text: `WARN: [upstream] chat_stream uid=32256b6a: upstream 429 soft_rate body={"code":6004}`,
		TS:   ts,
	}}
	stats := AggregateErrors(lines)
	if len(stats) != 1 {
		t.Fatalf("want 1 stat, got %d", len(stats))
	}
	if stats[0].Last != "16:40:56" {
		t.Errorf("Last = %q, want 16:40:56（必须用 TS 字段，不能从已剥时间戳的 Text 里解析）", stats[0].Last)
	}
}

// TestAggregateErrorsTextFallback 纯文本入口（离线分析）仍能从行首解析时刻 ——
// 但不能因此以为生产路径也能解析（生产走 Ring，Text 已剥）。
func TestAggregateErrorsTextFallback(t *testing.T) {
	stats := AggregateErrorsText([]string{
		`2026/09/23 16:40:56 WARN: [upstream] chat_stream uid=abc12345: upstream 429 soft_rate body={"code":6004}`,
	})
	if len(stats) != 1 {
		t.Fatalf("want 1, got %d", len(stats))
	}
	if stats[0].Last != "16:40:56" {
		t.Errorf("Last = %q, want 16:40:56（纯文本入口应从行首解析）", stats[0].Last)
	}
}

// TestTruncateLineKeepsValidUTF8 截断必须按 **rune** 而非字节 ——
// 日志里大量中文（汉字 3 字节），按字节切会产生**非法 UTF-8**，
// JSON 编码成 \ufffd、前端显示乱码方块。实测 truncateLine(s,57) 切出 \xe6\x82。
func TestTruncateLineKeepsValidUTF8(t *testing.T) {
	// 构造边界：让第 n 字节正好落在汉字中间
	s := strings.Repeat("x", 55) + "您的使用量已超出频率限制"
	got := truncateLine(s, 57) // 57 = 55 + 2，落在"您"的第 3 字节之前
	if !utf8.ValidString(got) {
		t.Errorf("截断产生非法 UTF-8：%q", got)
	}
	// 且不应把字符数切多（按 rune 计应恰好 57 + 省略号）
	if n := utf8.RuneCountInString(got); n != 58 {
		t.Errorf("rune 数 = %d, want 58（57 + 省略号）", n)
	}
}

// TestTruncateLineStripsNewlines 换行/Tab 必须压成空格 ——
// 样本用于 HTML title 属性，换行会被浏览器吞掉导致文本粘在一起。
func TestTruncateLineStripsNewlines(t *testing.T) {
	got := truncateLine("a\nb\tc\rd", 100)
	if strings.ContainsAny(got, "\n\t\r") {
		t.Errorf("未清理换行/Tab：%q", got)
	}
	if got != "a b c d" {
		t.Errorf("got %q, want %q", got, "a b c d")
	}
}
