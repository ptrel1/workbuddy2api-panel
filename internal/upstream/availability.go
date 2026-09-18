// availability.go 模型「可用性探测」：手动触发一次，对每个模型发极小的真实请求，
// 记录能否调通、延迟、错误分类，结果持久化到独立文件供面板展示。
//
// 与 scripts/probe_max_tokens.py 的分工（两者写**不同文件**，互不覆盖）：
//   - probe_max_tokens.py        → output_probes.json  ：测**真实输出上限**（烧额度、慢）
//   - 本文件（可用性探测）        → model_avail.json    ：测**能不能用**（极省、快）
//
// 设计取舍（为何不用"目录是否返回"当可用性）：目录里有 ≠ 能调通。
// 实测过两类「目录有但调不通」：11102（该后端无此模型）、账号/模型级限额。
// 故必须发一次真实请求才算数。
//
// 成本：单模型 1 ~ 数 tokens（prompt 一行 + max_tokens=1）。几十个模型合计
// 远小于一次正常对话，但仍**只在用户手动点击时执行**，不做定时。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Availability 单模型的可用性探测结果。
type Availability struct {
	Model     string    `json:"model"`            // 带域前缀的完整 id（cn:glm-5.2）
	OK        bool      `json:"ok"`               // 是否调通（HTTP 200 且拿到 content）
	Status    int       `json:"status,omitempty"` // HTTP 状态码
	Kind      string    `json:"kind,omitempty"`   // 错误分类（ErrKind 的字符串名）
	Msg       string    `json:"msg,omitempty"`    // 上游原文（截断，便于排查）
	LatencyMS int64     `json:"latency_ms"`       // 首字/整体耗时（毫秒）
	TestedAt  time.Time `json:"tested_at"`        // 本次探测时刻
	Tokens    int       `json:"tokens,omitempty"` // 本次探测实际消耗的 token（成本留痕）
}

// AvailabilityReport 一次探测的完整结果（持久化契约）。
type AvailabilityReport struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	// Results 键 = 带域前缀的模型 id（与 output_probes.json 的键口径一致）。
	Results map[string]Availability `json:"results"`
}

// availabilityPrompt 探测用的最小提示词。
// 极短（约 10 tokens）—— 目的是"能否调通"，不是测能力。
const availabilityPrompt = "hi"

// ProbeAvailability 并发探测给定模型列表的可用性。
//
// models 为**已带域前缀**的完整 id（如 "cn:glm-5.2" / "global:gpt-5.5"），
// 由调用方（面板）拼好 —— 本函数不解析 realm，保持无状态可测。
//
// concurrency 控制并发上限（<=0 时取 2）：太大会触发上游限流，把"探测本身"
// 变成"制造 429"，反而测不准 —— 这是有意的保守取值。
func (c *Client) ProbeAvailability(ctx context.Context, a *auth.Auth, models []string, concurrency int) (map[string]Availability, error) {
	if a == nil {
		return nil, fmt.Errorf("no account")
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	out := make(map[string]Availability, len(models))
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, m := range models {
		wg.Add(1)
		go func(model string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := c.probeOne(ctx, a, model)
			mu.Lock()
			out[model] = res
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	return out, nil
}

// probeOne 发一次最小请求判定可用性。**任何失败都归为"不可用"并记录原因**，
// 不返回 error —— 逐个模型的失败是探测的**产出**，不是调用者的错误。
func (c *Client) probeOne(ctx context.Context, a *auth.Auth, model string) Availability {
	start := time.Now()
	res := Availability{Model: model, TestedAt: start}

	// ⚠️ 发给上游的 model 必须是**裸名**：realm 前缀（cn: / global:）是
	// **网关侧的路由协议**，上游不认（带前缀会被判 11102
	// model_blocked，实测踩过：全部模型都会被误判不可用）。
	// 这与 handler 侧的行为一致（见 handler.go 的 rewriteModel）。
	bare := model
	if i := strings.Index(bare, ":"); i >= 0 {
		bare = bare[i+1:]
	}
	body, err := json.Marshal(map[string]any{
		"model":      bare,
		"messages":   []map[string]string{{"role": "user", "content": availabilityPrompt}},
		"max_tokens": 1, // 只要 1 token：探测意图是"能否调通"，不是测能力
	})
	if err != nil {
		res.Msg = "build body: " + err.Error()
		return res
	}

	// ⚠️ 必须走与真实请求**完全相同**的预处理管线：上游只接受
	// **流式**（非流式返回 400 code=11101 "Non-stream chat request is
	// currently not supported"）。而 PrepareBodyOpt 负责强制 stream=true
	// 并补 stream_options.include_usage。若跳过它直接发，每个模型
	// 都会被误判为"不可用"（实测踩过：三个模型全非流式→全 400）。
	body = PrepareBodyOpt(body, c.SanitizeFingerprints)

	endpoint := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		res.Msg = "build req: " + err.Error()
		return res
	}
	c.ChatHeaders(req, a, "", ChatMeta{})

	resp, err := c.HTTP.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Msg = "transport: " + truncate(err.Error(), 160)
		res.Kind = "transport"
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	res.Status = resp.StatusCode

	if resp.StatusCode != http.StatusOK {
		k := Classify(resp.StatusCode, string(raw))
		res.Kind = k.String()
		res.Msg = truncate(string(raw), 200)
		return res
	}

	// 200：响应是 SSE 流，需从里面找到**至少一个有效 data 帧**才算真调通
	// （有网关会回 200 但流里只有 [DONE] 或空帧 —— 静默失败）。
	gotFrame, usage := scanSSEFrames(raw)
	if !gotFrame {
		res.Msg = "200 但未收到有效 data 帧（静默失败）"
		res.Kind = "empty_stream"
		return res
	}
	res.OK = true
	res.Tokens = usage
	return res
}

// scanSSEFrames 扫 SSE 响应，报告是否出现过**非 [DONE] 的 data 帧**，
// 并尽力取出 usage.total_tokens（末帧才有，取不到就为 0）。
func scanSSEFrames(raw []byte) (bool, int) {
	got := false
	tokens := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if strings.TrimSpace(payload) == "" {
			continue
		}
		got = true
		// usage 通常在末帧（需 stream_options.include_usage，预处理已补）
		var f struct {
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &f) == nil && f.Usage != nil && f.Usage.TotalTokens > 0 {
			tokens = f.Usage.TotalTokens
		}
	}
	return got, tokens
}

// SortedModelKeys 返回报告里按 id 升序的模型键（供稳定输出/展示）。
func (r AvailabilityReport) SortedModelKeys() []string {
	keys := make([]string, 0, len(r.Results))
	for k := range r.Results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
