package server

import (
	"testing"
	"time"
)

// tokensPerSecond 的边界用例。
//
// 核心语义：分母 = 端到端 − TTFB（TTFB 是排队/建连，与吐字快慢无关），
// 但有三个必须退回端到端的边界 —— 否则会产出幻数速率。
func TestTokensPerSecond(t *testing.T) {
	cases := []struct {
		name   string
		toks   int64
		total  time.Duration
		ttfb   time.Duration
		want   float64
		wantOK bool
	}{
		// 扣 TTFB 的正常情形：4.8s 总耗时、TTFB 4.209s → 生成 591ms
		{"扣 TTFB", 204, 4800 * time.Millisecond, 4209 * time.Millisecond, 204 / 0.591, true},
		// 非流式（无 TTFB 观测）→ 不扣，退回端到端
		{"无 TTFB 观测", 100, 2 * time.Second, 0, 50, true},
		// TTFB >= total（异常观测）→ 退回端到端，避免零/负分母
		{"TTFB 等于 total", 100, 2 * time.Second, 2 * time.Second, 50, true},
		{"TTFB 超过 total", 100, 2 * time.Second, 5 * time.Second, 50, true},
		// ⭐ 假流式：首末帧几乎同时到达，扣除后只剩几毫秒 → 必须退回端到端，
		// 否则 500 token / 5ms = 10 万 tok/s 的幻数
		{"假流式（剩余 < 200ms）", 500, 2 * time.Second, 2*time.Second - 5*time.Millisecond, 250, true},
		// 剩余恰好 200ms（边界）→ 认为可测，用扣除后的
		{"剩余恰好 200ms", 100, 1 * time.Second, 800 * time.Millisecond, 500, true},
		// 无 token / 无耗时 → 不可测（调用方不写该字段）
		{"零 token", 0, 2 * time.Second, 0, 0, false},
		{"零耗时", 100, 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := tokensPerSecond(c.toks, c.total, c.ttfb)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if diff := got - c.want; diff > 1 || diff < -1 {
				t.Errorf("tps=%.2f want ~%.2f", got, c.want)
			}
		})
	}
}

// TestTokensPerSecondTTFBMonotonic ⭐ 回归的**核心不变式**：
// 同一 total 与 token 数下，**TTFB 越大，速率应当越大**（因为扣掉的排队时间更多）。
//
// 修复前的行为恰好相反：分母恒为端到端，TTFB 完全不参与 → 速率是常数，
// 或（换上游后 TTFB 涨）被读成"模型变慢"。这条单调性正是本次修复的判据。
func TestTokensPerSecondTTFBMonotonic(t *testing.T) {
	const toks = 1000
	const total = 5 * time.Second
	var prev float64
	for _, ttfb := range []time.Duration{
		0, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 3 * time.Second,
	} {
		tps, ok := tokensPerSecond(toks, total, ttfb)
		if !ok {
			t.Fatalf("ttfb=%v 应可测", ttfb)
		}
		if ttfb > 0 && tps < prev {
			t.Errorf("TTFB=%v 时 tps=%.1f < 前一档 %.1f —— 违反单调性（TTFB 越大速率应越大）", ttfb, tps, prev)
		}
		prev = tps
	}
}
