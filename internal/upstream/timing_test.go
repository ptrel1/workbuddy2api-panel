package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// sseResp 构造一个 text/event-stream 响应（本文件独立定义，避免与既有测试
// helper 命名冲突）。
func sseResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestChatTimingNilPointerSafe 未传 timing（nil）时不得 panic——
// 生产默认路径 ChatStreamContext 就是传 nil，回归防线。
func TestChatTimingNilPointerSafe(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return sseResp(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "uid-nil"}
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m"}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
}

// TestChatTimingCollectsPrepareAndDo timing 非 nil 时必须采集到两段耗时，
// 且两段之和 ≤ 调用方观测到的总耗时（§十 不变量：不得产出物理上不可能的值）。
func TestChatTimingCollectsPrepareAndDo(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		time.Sleep(20 * time.Millisecond) // 制造可观测的 Do 段
		return sseResp(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "uid-timed"}

	var tm ChatTiming
	outerStart := time.Now()
	rc, status, _, err := c.ChatStreamContextTimed(context.Background(), a, []byte(`{"model":"m"}`), "", ChatMeta{}, &tm)
	outerTotal := time.Since(outerStart)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}

	if tm.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", tm.Attempts)
	}
	if tm.Do < 20*time.Millisecond {
		t.Errorf("Do = %v, want >= 20ms (注入的固定延迟)", tm.Do)
	}
	if tm.Prepare < 0 || tm.Do < 0 {
		t.Errorf("分段耗时为负：Prepare=%v Do=%v（时钟回拨/计时点错误）", tm.Prepare, tm.Do)
	}
	if tm.Total() > outerTotal {
		t.Errorf("不变量违反：分段合计 %v > 外层总耗时 %v", tm.Total(), outerTotal)
	}
}

// TestChatTimingAccumulatesAcrossFallbackRetry global 路径 404 → fallback 重试时，
// Do 必须累加两次尝试（否则「慢」会被误判为单次慢而非重试放大）。
func TestChatTimingAccumulatesAcrossFallbackRetry(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(r.URL.Path, globalChatConsolePath) {
			time.Sleep(15 * time.Millisecond)
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		}
		time.Sleep(15 * time.Millisecond)
		return sseResp(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil
	})
	c.GlobalEnabled = true
	// realm=global 走 [console, v2] 双候选，console 404 → fallback v2。
	a := &auth.Auth{AccessToken: "at", UID: "uid-fb", Domain: "www.workbuddy.ai"}

	var tm ChatTiming
	rc, status, _, err := c.ChatStreamContextTimed(context.Background(), a, []byte(`{"model":"m"}`), "", ChatMeta{}, &tm)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status = %d, want 200 (fallback 后成功)", status)
	}
	if calls != 2 {
		t.Fatalf("上游调用次数 = %d, want 2（console 404 + v2 成功）", calls)
	}
	if tm.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2（必须反映真实尝试次数）", tm.Attempts)
	}
	if tm.Do < 30*time.Millisecond {
		t.Errorf("Do = %v, want >= 30ms（两次各 15ms 累加）", tm.Do)
	}
}

// TestChatTimingTransportErrorStillRecorded 传输层错误（Do 失败）时 Do 段仍应记录，
// 否则「连不上上游导致的慢」会完全不可见（错误路径最需要观测）。
func TestChatTimingTransportErrorStillRecorded(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		time.Sleep(10 * time.Millisecond)
		return nil, errors.New("connection reset")
	})
	a := &auth.Auth{AccessToken: "at", UID: "uid-err"}

	var tm ChatTiming
	_, _, _, err := c.ChatStreamContextTimed(context.Background(), a, []byte(`{"model":"m"}`), "", ChatMeta{}, &tm)
	if err == nil {
		t.Fatal("want transport error")
	}
	if tm.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", tm.Attempts)
	}
	if tm.Do < 10*time.Millisecond {
		t.Errorf("Do = %v, want >= 10ms（错误路径也要记录耗时）", tm.Do)
	}
}

// TestChatTimingAddSkipsUnmeasured 零值分段（未测量）不应被 Add 摊成 0ms 虚增分母。
func TestChatTimingAddSkipsUnmeasured(t *testing.T) {
	var a ChatTiming
	a.Add(ChatTiming{Prepare: 5 * time.Millisecond, Do: 10 * time.Millisecond, Attempts: 1})
	// 零值项：不入统计（模拟「未测量」而非「0ms」）
	a.Add(ChatTiming{})
	if a.Prepare != 5*time.Millisecond || a.Do != 10*time.Millisecond {
		t.Errorf("零值 Add 污染了已有统计：%+v", a)
	}
	if a.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1（零值项不递增）", a.Attempts)
	}
	// 非零项正常累加
	a.Add(ChatTiming{Prepare: 1 * time.Millisecond, Do: 2 * time.Millisecond, Attempts: 3})
	if a.Prepare != 6*time.Millisecond || a.Do != 12*time.Millisecond || a.Attempts != 4 {
		t.Errorf("累加错误：%+v", a)
	}
}
