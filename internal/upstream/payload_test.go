package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

// TestPrepareBodyDeterministic 序列化稳定性：同输入跑多遍出站字节级一致
// （prompt_cache_key 前缀命中的前提——链中不得注入时间/随机/ID 类不确定源）。
func TestPrepareBodyDeterministic(t *testing.T) {
	inputs := []string{
		`{"model":"glm-5.2","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4","messages":[{"role":"user","content":"写个函数"}],"tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"glm-5.3","messages":[{"role":"developer","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for i, in := range inputs {
		var first []byte
		for round := 0; round < 5; round++ {
			out := PrepareBodyOptWithEfforts([]byte(in), true, map[string][]string{"glm-5.2": {"off", "low", "high"}})
			if round == 0 {
				first = out
				continue
			}
			if string(out) != string(first) {
				t.Fatalf("input #%d round %d differs from round 0:\n%s\n%s", i, round, first, out)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GPT 系 max_tokens 下限（吸收上游 fd3e142）
// ─────────────────────────────────────────────────────────────────────────────

// TestClampGPTMinMaxTokens GPT 系过小 max_tokens 抬到 16；非 GPT / 已达标 / 未携带不动。
//
// 背景：Claude Code 切模型时发极小 max_tokens 探针，上游 GPT 系一律 400
// code=11133（15 拒、16 过）→ 全号轮转同样被拒 → 客户端 503，模型切不过去。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		wantM float64
		wantH bool // 是否应被改
	}{
		{"gpt 系过小 → 抬到 16", `{"model":"gpt-6-sol","max_tokens":5}`, 16, true},
		{"gpt 系恰好 15 → 抬", `{"model":"gpt-5.6-luna","max_tokens":15}`, 16, true},
		{"gpt 系恰好 16 → 不动", `{"model":"gpt-6-sol","max_tokens":16}`, 16, false},
		{"gpt 系较大 → 不动", `{"model":"gpt-6-sol","max_tokens":4096}`, 4096, false},
		{"非 gpt 模型 → 不动", `{"model":"deepseek-v4.1-flash","max_tokens":5}`, 5, false},
		{"hy4 非 gpt → 不动", `{"model":"hy4-preview","max_tokens":1}`, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(c.in), false)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			got, _ := m["max_tokens"].(float64)
			if got != c.wantM {
				t.Errorf("max_tokens=%v want %v（in=%s）", got, c.wantM, c.in)
			}
		})
	}
	// 未携带 max_tokens：不得凭空添加（否则会改变"用默认上限"的语义）
	out := PrepareBodyOpt([]byte(`{"model":"gpt-6-sol","messages":[]}`), false)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, has := m["max_tokens"]; has {
		t.Error("未携带 max_tokens 时不该凭空添加")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 工具 schema pattern 的 \_ 非标转义归一（吸收上游 230140e）
// ─────────────────────────────────────────────────────────────────────────────

// TestNormalizeToolPatterns 归一 tools 子树的 `\_` → `_`；消息正文不碰。
//
// 背景：上游严格文法校验整体拒收含 `\_` 的 pattern（400 code=11129）。
// `\_` 与 `_` 在各引擎匹配语义相同，故归一无损。
func TestNormalizeToolPatterns(t *testing.T) {
	// ⚠️ 消息正文里**故意放一个含 pattern 键的对象**（而非只有字符串）——
	// 否则"越界到全 obj"的变异咬不住：字符串正文本来就不含 pattern 字段。
	in := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"C:\\_path",
		"fake_schema":{"pattern":"^keep\\_this$"}}],
		"tools":[{"type":"function","function":{"name":"agent_run","parameters":{
			"type":"object","properties":{
				"runId":{"type":"string","pattern":"^agent\\_run\\_"},
				"plain":{"type":"string","pattern":"^ok$"}
			},
			"patternProperties":{"^x\\_y":{"type":"string"}}
		}}}]}`
	out := PrepareBodyOpt([]byte(in), false)
	s := string(out)
	// 工具 pattern 已归一
	if strings.Contains(s, `agent\_run\_`) {
		t.Errorf("工具 pattern 的 \\_ 未归一：%s", s)
	}
	if !strings.Contains(s, `^agent_run_`) {
		t.Errorf("归一结果不对（应含 ^agent_run_）：%s", s)
	}
	// patternProperties 键也归一
	if strings.Contains(s, `x\_y`) {
		t.Errorf("patternProperties 键未归一：%s", s)
	}
	// ⚠️ 消息正文里的 `\_` **不得**被碰（那是用户数据，如 Windows 路径）
	if !strings.Contains(s, `C:\\_path`) {
		t.Errorf("消息正文的 \\_ 被误改（应原样保留）：%s", s)
	}
	// ⭐ 消息里的**伪 schema**（带 pattern 键）也不得被改 —— 归一必须**只作用于
	// tools 子树**。这条是"越界到全 obj"变异的判据（仅靠字符串正文咬不住）。
	if !strings.Contains(s, `^keep\\_this$`) {
		t.Errorf("消息内的 pattern 被误改（归一越界到 tools 之外）：%s", s)
	}
	// 无关 pattern 不动
	if !strings.Contains(s, `^ok$`) {
		t.Errorf("无关 pattern 被改动：%s", s)
	}
}
