package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"encoding/json"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestProbeAvailabilityStripsRealmPrefix 发给上游的 model 必须是**裸名**：
// realm 前缀（cn:/global:）是网关侧路由协议，上游不认 —— 带前缀会被判
// 11102 model_blocked，导致**全部模型被误判不可用**（实测踩过）。
func TestProbeAvailabilityStripsRealmPrefix(t *testing.T) {
	var gotModel string
	var gotStream bool
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			gotModel = jsonField(string(b), "model")
			gotStream = jsonField(string(b), "stream") == "true"
			return sseResp(200, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"), nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	res, err := c.ProbeAvailability(context.Background(), a, []string{"cn:glm-5.2"}, 1)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotModel != "glm-5.2" {
		t.Errorf("上游收到 model=%q, want glm-5.2（必须剥掉 realm 前缀）", gotModel)
	}
	// 上游只接受流式：非流式会返回 400 code=11101（实测）
	if !gotStream {
		t.Error("出站请求未强制 stream=true —— 上游只接受流式，非流式会被判不可用")
	}
	if r := res["cn:glm-5.2"]; !r.OK {
		t.Errorf("应判为可用，得 %+v", r)
	}
	// 返回的 key 保持带前缀（面板按前缀索引）
	if _, ok := res["cn:glm-5.2"]; !ok {
		t.Error("结果键应保留 realm 前缀（面板据此索引）")
	}
}

// TestProbeAvailabilityNon200IsUnavailable 非 200 必须判不可用并记录分类，
// **不返回 error** —— 单模型失败是探测的产出，不是调用者的错误。
func TestProbeAvailabilityNon200IsUnavailable(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResp(400, `{"code":11102,"msg":"model [x] service info not found"}`), nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	res, err := c.ProbeAvailability(context.Background(), a, []string{"cn:x"}, 1)
	if err != nil {
		t.Fatalf("单模型失败不应让整体返回 error: %v", err)
	}
	r := res["cn:x"]
	if r.OK {
		t.Error("400 应判为不可用")
	}
	if r.Status != 400 {
		t.Errorf("Status = %d, want 400", r.Status)
	}
	if r.Kind == "" {
		t.Error("应记录错误分类（kind），供面板展示原因")
	}
	if !strings.Contains(r.Msg, "service info not found") {
		t.Errorf("应保留上游原文便于排查，得 %q", r.Msg)
	}
}

// TestProbeAvailabilityEmptyStreamIsUnavailable 200 但流里无有效 data 帧
// 必须判不可用 —— 有些网关回 200+空流，只看状态码会误判为可用。
func TestProbeAvailabilityEmptyStreamIsUnavailable(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return sseResp(200, "data: [DONE]\n\n"), nil // 只有结束帧
		})},
		ChatBaseCN: "https://chat.example",
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	res, _ := c.ProbeAvailability(context.Background(), a, []string{"cn:x"}, 1)
	r := res["cn:x"]
	if r.OK {
		t.Error("200 但无有效帧应判不可用（静默失败）")
	}
	if r.Kind != "empty_stream" {
		t.Errorf("Kind = %q, want empty_stream", r.Kind)
	}
}

// TestScanSSEFrames 帧扫描与 usage 提取。
func TestScanSSEFrames(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		got    bool
		tokens int
	}{
		{"只有DONE", "data: [DONE]\n\n", false, 0},
		{"一帧无usage", "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n", true, 0},
		{"末帧带usage", "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"usage\":{\"total_tokens\":42}}\n\ndata: [DONE]\n\n", true, 42},
		{"空行与空帧", "\n\ndata: \n\ndata: [DONE]\n", false, 0},
	}
	for _, c := range cases {
		got, tok := scanSSEFrames([]byte(c.in))
		if got != c.got || tok != c.tokens {
			t.Errorf("%s: got=(%v,%d) want=(%v,%d)", c.name, got, tok, c.got, c.tokens)
		}
	}
}

// jsonField 取 JSON 字符串里的顶层字段值（测试辅助，避免引入 map 断言噪音）。
func jsonField(body, key string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	v, _ := m[key].(string)
	if v == "" {
		if b, ok := m[key].(bool); ok {
			if b {
				return "true"
			}
			return "false"
		}
	}
	return v
}
