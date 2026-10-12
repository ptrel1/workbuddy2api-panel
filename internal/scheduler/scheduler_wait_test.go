// scheduler_wait_test.go 钉住 waitSlot 的三态与墙钟口径。
//
// 背景：timer 的等待基于**单调时钟**，机器睡眠（macOS / Windows Modern Standby）
// 会冻结它——睡眠时长不足整个等待周期时，fire 被顺延「睡眠时长」，墙钟已过点而
// timer 还要继续等，**时点被错过且不会立即补跑**。分段等待 + 每段墙钟重判把冻结的
// 影响限制在一段之内。本文件是该修复的回归网。
package scheduler

import (
	"context"
	"testing"
	"time"
)

func newTestScheduler() *Scheduler {
	return New(Config{})
}

// TestWaitSlotFiresAtWallclock 到点返回 slotFired。
func TestWaitSlotFiresAtWallclock(t *testing.T) {
	s := newTestScheduler()

	// ⭐ 修复的核心形态：墙钟已越过时点（睡眠冻结 timer 后醒来的第一件事）。
	// 必须**立即**返回，而不是继续等剩余时长。
	past := time.Now().Add(-time.Second)
	if got := s.waitSlot(context.Background(), past, time.Hour); got != slotFired {
		t.Fatalf("已越过时点应立即 slotFired，got %v", got)
	}

	// 未来时点：多段等待到点，且不早于计划时刻。
	next := time.Now().Add(45 * time.Millisecond)
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotFired {
		t.Fatalf("到点应 slotFired，got %v", got)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("提前返回：等待 %v，未到计划时刻", time.Since(start))
	}
}

// TestWaitSlotRearm 等待中收到 rearmSchedule（在线改配置重排）→ 立即 slotRearm，
// 不等满剩余时长（段长不影响 rearm 响应性）。
func TestWaitSlotRearm(t *testing.T) {
	s := newTestScheduler()
	next := time.Now().Add(2 * time.Second)
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.rearmSchedule <- struct{}{}
	}()
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotRearm {
		t.Fatalf("rearm 应返回 slotRearm，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("rearm 响应过慢：%v", time.Since(start))
	}
}

// TestWaitSlotCancel ctx 取消 → slotCancel，优雅退出。
func TestWaitSlotCancel(t *testing.T) {
	s := newTestScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next := time.Now().Add(2 * time.Second)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if got := s.waitSlot(ctx, next, 10*time.Millisecond); got != slotCancel {
		t.Fatalf("取消应返回 slotCancel，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("取消响应过慢：%v", time.Since(start))
	}
}

// TestWaitSlotIsSegmented ⭐ 抗睡眠冻结的**判据级**测试：等待必须**分段**，
// 且每段都**重读墙钟**，而不是信任 timer 的累计等待。
//
// ⚠️ 为什么不能用「已过去的 next」或「cancel/rearm 响应快」来测：
// 那些路径在循环第一行就返回、或经 select 立即返回，**与是否分段无关** ——
// 实测把实现变异成"一把睡到底"后它们照样通过（咬不住）。
//
// 唯一可区分的观测点 = **墙钟被重读的次数**。故用 slotClockNow 接缝注入
// 一个「快进时钟」：每次读取都往前跳一大步（模拟"墙钟在走而 timer 被冻结"）。
//   - 分段实现：每段结束回到循环头**重读**，很快发现 wallRemain <= 0 → 立即 slotFired；
//   - 一把睡到底：只在进入循环时读一次，之后睡满该次算出的时长 → 耗时 ≈ 那一次的
//     wallRemain（我们让它返回一个很大的值），远超断言阈值。
func TestWaitSlotIsSegmented(t *testing.T) {
	orig := slotClockNow
	defer func() { slotClockNow = orig }()

	// 注入快进时钟：每次读取都推进 1 秒。起始刻意设在 next 之前（wallRemain > 0），
	// 使实现必须先进入等待循环；随后每重读一次就快进，几次内必然越过 next。
	base := time.Now()
	var reads int
	slotClockNow = func() time.Time {
		reads++
		return base.Add(time.Duration(reads) * time.Second)
	}
	next := base.Add(5 * time.Second) // 需约 5 次重读才越过

	s := newTestScheduler()
	start := time.Now()
	got := s.waitSlot(context.Background(), next, 20*time.Millisecond)
	elapsed := time.Since(start)
	if got != slotFired {
		t.Fatalf("got %v, want slotFired", got)
	}
	// ⭐ 核心断言：墙钟被重读了多次（分段）——一把睡到底只会读 1 次。
	if reads < 3 {
		t.Errorf("墙钟只被重读 %d 次（want ≥3）—— 未按段重判，说明是一把睡到底", reads)
	}
	// 且不应睡满整个 wallRemain（分段后每段 ≤ step）。
	if elapsed > 500*time.Millisecond {
		t.Errorf("耗时 %v 过长，疑似未分段（每段应 ≤ step=20ms）", elapsed)
	}
}
