package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// cleanupOrphanToolCalls：出站请求体的孤儿 tool_call↔tool 配对清理
//
// ⚠️ 这段逻辑此前**无任何测试覆盖**（本次随 11133 修复一起补上）——而它守的是
// "整条会话生死"：坏历史一旦出站，上游会对之后每条消息都返 400，整条会话报废。
// ─────────────────────────────────────────────────────────────────────────────

// tpMsgs 把 JSON 数组解析成 []any（测试输入构造）。
func tpMsgs(t *testing.T, raw string) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("测试输入非法: %v", err)
	}
	return out
}

// tpSummary 把消息列表压成可读摘要：role + 该消息带有的 tool_call id / 本身是哪个结果。
// 例：`assistant(c1,c2)`、`tool(c1)`、`user(-)`。用于断言"删了哪些、留了哪些"。
func tpSummary(messages []any) []string {
	var out []string
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if tcs, ok := msg["tool_calls"].([]any); ok {
			var ids []string
			for _, tci := range tcs {
				if tc, ok := tci.(map[string]any); ok {
					if id, ok := tc["id"].(string); ok {
						ids = append(ids, id)
					}
				}
			}
			out = append(out, role+"("+strings.Join(ids, ",")+")")
			continue
		}
		if id, ok := msg["tool_call_id"].(string); ok && id != "" {
			out = append(out, role+"("+id+")")
			continue
		}
		out = append(out, role+"(-)")
	}
	return out
}

func tpAssert(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("消息序列=%v，want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("消息[%d]=%q，want %q（完整 %v）", i, got[i], want[i], got)
		}
	}
}

// TestCleanupOrphanToolCallsEmptyName ⭐ 空 function.name 的调用必须剔除（线上 11133）。
//
// 来源：上游流式响应在工具调用 name 分片到达前异常结束（200 空流也算），客户端把
// `name:""` 的条目写进会话历史；下一轮原样转发被上游判
// `400 {"code":11133,..."tool_calls[i].function.name must be a non-empty string"}`，
// **且带不带配对结果都会拒** → 整条会话从此每轮 503，只能手动清空历史。
func TestCleanupOrphanToolCallsEmptyName(t *testing.T) {
	t.Run("空name带结果：调用与结果成对剔除、assistant 保留", func(t *testing.T) {
		in := tpMsgs(t, `[
{"role":"user","content":"q"},
{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"","arguments":"{}"}}]},
{"role":"tool","tool_call_id":"c1","content":"r1"},
{"role":"user","content":"next"}
]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望剔除空 name 调用")
		}
		tpAssert(t, tpSummary(out), []string{"user(-)", "assistant(-)", "user(-)"})
	})

	t.Run("空name且无结果：仍走孤儿路径删除", func(t *testing.T) {
		// ⭐ 回归钉桩：若改成"空 name 不登记 callIDs"，hasTraffic 会被判 false 早退，
		// 该脏条目反而**漏出站** —— 必须仍被删除。
		in := tpMsgs(t, `[
{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"","arguments":"{}"}}]},
{"role":"user","content":"go"}
]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望删除空 name 孤儿调用")
		}
		msg := out[0].(map[string]any)
		if _, has := msg["tool_calls"]; has {
			t.Error("空 name 孤儿调用的 tool_calls 键应被删除")
		}
	})

	t.Run("纯空白name判脏", func(t *testing.T) {
		in := tpMsgs(t, `[
{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"   ","arguments":"{}"}}]},
{"role":"tool","tool_call_id":"c1","content":"r1"}
]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("纯空白 name（含 tab/空格）应判脏并剔除")
		}
		tpAssert(t, tpSummary(out), []string{"assistant(-)"})
	})

	t.Run("一脏一净：只剔除脏项，净项与其结果保留", func(t *testing.T) {
		in := tpMsgs(t, `[
{"role":"assistant","content":"","tool_calls":[
  {"id":"c1","function":{"name":"","arguments":"{}"}},
  {"id":"c2","function":{"name":"read_file","arguments":"{}"}}]},
{"role":"tool","tool_call_id":"c1","content":"r1"},
{"role":"tool","tool_call_id":"c2","content":"r2"}
]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望对称裁剪脏项")
		}
		tpAssert(t, tpSummary(out), []string{"assistant(c2)", "tool(c2)"})
	})

	t.Run("function或name缺失不判脏（不扩大打击面）", func(t *testing.T) {
		// 历史报文里存在不带 function 的占位条目，上游不会因此拒参 —— 不误伤。
		in := tpMsgs(t, `[
{"role":"assistant","content":null,"tool_calls":[{"id":"c1"},{"id":"c2"}]},
{"role":"tool","tool_call_id":"c1","content":"r1"},
{"role":"tool","tool_call_id":"c2","content":"r2"}
]`)
		out, changed := cleanupOrphanToolCalls(in)
		if changed {
			t.Error("changed=true，缺 function 字段不应判脏")
		}
		tpAssert(t, tpSummary(out), []string{"assistant(c1,c2)", "tool(c1)", "tool(c2)"})
	})
}

// TestCleanupOrphanToolCallsSymmetricTrim 既有语义回归：半截配对必须**双侧对称**裁剪。
//
// 为什么必须有这条：整批删 tool_calls 但留下 tool 结果的旧实现会产出
// 「无 tool_calls 的 assistant + 孤儿 tool」→ 上游判 11148 顶死会话。
func TestCleanupOrphanToolCallsSymmetricTrim(t *testing.T) {
	// 批 [c1,c2] 只回了 c1：c2 两侧都该消失，c1 两侧都该保留。
	in := tpMsgs(t, `[
{"role":"assistant","content":"","tool_calls":[
  {"id":"c1","function":{"name":"read_file","arguments":"{}"}},
  {"id":"c2","function":{"name":"write_file","arguments":"{}"}}]},
{"role":"tool","tool_call_id":"c1","content":"r1"}
]`)
	out, changed := cleanupOrphanToolCalls(in)
	if !changed {
		t.Fatal("changed=false，期望剔除无结果的 c2")
	}
	tpAssert(t, tpSummary(out), []string{"assistant(c1)", "tool(c1)"})
}

// TestCleanupOrphanToolCallsNoTraffic 无任何工具消息时原样返回、不判改动。
func TestCleanupOrphanToolCallsNoTraffic(t *testing.T) {
	in := tpMsgs(t, `[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]`)
	out, changed := cleanupOrphanToolCalls(in)
	if changed {
		t.Error("无工具消息时不该判改动")
	}
	if len(out) != 2 {
		t.Errorf("消息数=%d，want 2", len(out))
	}
}
