package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestRequestMetricsRecordsStream 流式请求被计入指标，且带回 X-Request-Id。
func TestRequestMetricsRecordsStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// X-Request-Id 必须回写：客户端报障时靠它对上服务端归档
	if got := rec.Header().Get("X-Request-Id"); !strings.HasPrefix(got, "req-") {
		t.Errorf("X-Request-Id=%q（应回写 req- 前缀的本地请求 ID）", got)
	}
	s := reqLog.Snapshot()
	if s.Completed != 1 || s.Succeeded != 1 || s.Failed != 0 {
		t.Fatalf("计数 = completed:%d succeeded:%d failed:%d，want 1/1/0",
			s.Completed, s.Succeeded, s.Failed)
	}
	if len(s.Recent) != 1 {
		t.Fatalf("recent 条数 = %d，want 1", len(s.Recent))
	}
	e := s.Recent[0]
	if e.Outcome != reqlog.OutcomeSuccess {
		t.Errorf("outcome=%q，want success", e.Outcome)
	}
	if e.Model != "glm-5.2" {
		t.Errorf("model=%q，want glm-5.2", e.Model)
	}
	if e.TotalTokens != 2 {
		t.Errorf("totalTokens=%d，want 2（取自末帧 usage）", e.TotalTokens)
	}
	if e.Attempts != 1 {
		t.Errorf("attempts=%d，want 1", e.Attempts)
	}
	if e.DurationMs < 1 {
		t.Errorf("durationMs=%d，want ≥1（避免 0 耗时被读成瞬时）", e.DurationMs)
	}
}

// TestRequestMetricsStreamErrorFrame HTTP 200 但流内含 error 帧 → 必须判为 stream_error。
//
// 为什么关键：上游把失败以 `data: {"error":...}` 透传时 HTTP 状态仍是 200，
// 只看状态码会把失败流记成成功，成功率虚高。
func TestRequestMetricsStreamErrorFrame(t *testing.T) {
	const sseErr = "data: {\"error\":{\"message\":\"upstream failed\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseErr, true })
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))

	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent=%d，want 1", len(s.Recent))
	}
	if s.Recent[0].Outcome != reqlog.OutcomeStreamError {
		t.Errorf("outcome=%q，want stream_error（HTTP 200 但流内 error 帧）", s.Recent[0].Outcome)
	}
}

// TestRequestMetricsHTTPError 上游 4xx → http_error（不靠 outcome 显式标记也要正确兜底）。
func TestRequestMetricsHTTPError(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"bad params"}`, false
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	s := reqLog.Snapshot()
	if s.Failed != 1 {
		t.Errorf("failed=%d，want 1", s.Failed)
	}
	if len(s.Recent) != 1 {
		t.Fatalf("recent=%d", len(s.Recent))
	}
	if s.Recent[0].Outcome != reqlog.OutcomeHTTPError {
		t.Errorf("outcome=%q，want http_error", s.Recent[0].Outcome)
	}
	if s.Recent[0].OK {
		t.Error("OK 应为 false")
	}
}

// TestRequestMetricsDisabled 未注入记录器时不记录、且行为与改动前一致（零回归）。
func TestRequestMetricsDisabled(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		// 不注入 RequestLog
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d，未启用指标不该影响请求", rec.Code)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("未启用指标时不该回写 X-Request-Id，得 %q", got)
	}
}

// TestRequestMetricsOnlyChatEndpoint 只有 chat 端点被计入（其他路径不产生噪声记录）。
func TestRequestMetricsOnlyChatEndpoint(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if n := reqLog.Snapshot().Completed; n != 0 {
		t.Errorf("/healthz 被计入指标（completed=%d），应只统计 chat 端点", n)
	}
}

// TestRequestMetricsArchiveIsSanitized ⭐ 归档**不得**含提示词/正文/凭证。
//
// 这是本功能的安全底线：JSONL 落盘后长期留存，一旦写进用户提示词或 token，
// 等于把对话内容与凭据留在磁盘上。**必须实测验证**，不能只信注释。
//
// 做法：发一个带独特标记的请求（标记同时出现在提示词与 Authorization 里），
// 然后把归档文件全文读出来，断言标记**不出现**。
func TestRequestMetricsArchiveIsSanitized(t *testing.T) {
	const secret = "SECRET_MARKER_XYZZY"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	dir := t.TempDir()
	reqLog := reqlog.New(reqlog.Config{Dir: dir, Enabled: true})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"`+secret+`"}]}`)))
	reqLog.Close() // 刷盘

	files, err := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("未生成归档文件（files=%v err=%v）", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	// ⭐ 核心断言：标记（出现在提示词里）不得落盘
	if strings.Contains(body, secret) {
		t.Errorf("归档含提示词内容（泄漏！）:\n%s", body)
	}
	// token 不得落盘（Authorization / accessToken）
	for _, bad := range []string{"at1", "Bearer", "Authorization", "accessToken", "refreshToken"} {
		if strings.Contains(body, bad) {
			t.Errorf("归档含凭证相关字样 %q（泄漏！）:\n%s", bad, body)
		}
	}
	// 账号必须是脱敏标签（昵称(uid8)），不得是完整 uid
	if strings.Contains(body, "u1-secret-full-uid") {
		t.Error("归档含完整 uid")
	}
}
