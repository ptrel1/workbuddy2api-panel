package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 3 || after.Totals.PromptTokens != 15 {
		t.Fatalf("折叠后 totals = %d/%d, want 3/15", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 2 || after.Series[0].Scope != "day" || after.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", after.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(24, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Snapshot 把小时窗口外的细粒度并入日点，时序不出现空洞。
func TestSnapshotStitching(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外 → 日点
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内 → 小时点
	s := r.Snapshot(24, nil)
	if len(s.Series) != 2 || s.Series[0].Scope != "day" || s.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v", s.Series)
	}
	if s.Series[0].PromptTokens != 5 || s.Series[1].PromptTokens != 3 {
		t.Fatalf("series tokens = %d/%d, want 5/3", s.Series[0].PromptTokens, s.Series[1].PromptTokens)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// 样本数透出 + 均值按样本加权（不是对各桶均值再取平均）。
//
// 为什么单独测：按模型表的均速率/均延迟用来回答"哪个模型快"。若均值口径错成
// "各桶均值的简单平均"，一个只有 1 次样本的桶会和 1000 次样本的桶等权，数字失真。
// 同时样本数必须透出——前端靠它决定少样本时显示 "—" 而不是一个有误导性的数字。
func TestAvgPerfWeightedBySamples(t *testing.T) {
	r := New("")
	now := time.Now()
	// 同一模型两个桶：桶 A 1 次样本 100 tok/s，桶 B 3 次样本 10 tok/s。
	// 加权正确 = (100*1 + 10*3) / 4 = 32.5；简单平均会是 (100+10)/2 = 55。
	r.Add(now, "cn", "u1", "m1", Delta{TokensPerSecond: 100, HasTPS: true, LatencyMs: 1000, HasLatency: true}, true)
	// 换账号落到另一个桶（按 uid 分桶），保证是两个独立样本数。
	r.Add(now, "cn", "u2", "m1", Delta{TokensPerSecond: 10, HasTPS: true, LatencyMs: 100, HasLatency: true}, true)
	r.Add(now, "cn", "u3", "m1", Delta{TokensPerSecond: 10, HasTPS: true, LatencyMs: 100, HasLatency: true}, true)

	s := r.Snapshot(24, nil)
	if len(s.ByModel) != 1 {
		t.Fatalf("by_model 应有 1 项（m1），得 %d", len(s.ByModel))
	}
	got := s.ByModel[0]
	// 注意：同账号同模型的多次 Add 会落进同一个桶，故这里 3 个不同 uid = 3 个桶。
	if got.TPSSamples != 3 || got.LatSamples != 3 {
		t.Fatalf("样本数 = tps:%d lat:%d, want 3/3", got.TPSSamples, got.LatSamples)
	}
	want := (100.0 + 10.0 + 10.0) / 3
	if diff := got.AvgTPS - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("avg_tps = %v, want %v（按样本加权，非简单平均）", got.AvgTPS, want)
	}
}

// 无样本时样本数为 0、均值为零值——前端据此渲染 "—"（而非显示 0 tok/s 误导）。
func TestNoPerfSamplesMeansZero(t *testing.T) {
	r := New("")
	// 失败尝试：无 usage → 无延迟/速率样本。
	r.Add(time.Now(), "cn", "u1", "m1", Delta{}, false)

	s := r.Snapshot(24, nil)
	if s.Totals.TPSSamples != 0 || s.Totals.LatSamples != 0 {
		t.Fatalf("无样本时样本数应为 0，得 tps:%d lat:%d", s.Totals.TPSSamples, s.Totals.LatSamples)
	}
	if s.Totals.AvgTPS != 0 || s.Totals.AvgLatencyMs != 0 {
		t.Fatalf("无样本时均值应为零值，得 tps:%v lat:%v", s.Totals.AvgTPS, s.Totals.AvgLatencyMs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 前缀缓存命中率（吸收上游 5e0adb3）
//
// 为什么值得观测：网关**已注入 prompt_cache_key 省钱**（见 upstream/cache_key.go），
// 但此前看不到它到底有没有生效 —— 命中与不命中的费用差可达 **17 倍**。
// ─────────────────────────────────────────────────────────────────────────────

// TestCacheHitRateAggregation 命中率 = hit/(hit+miss)，跨桶累加。
func TestCacheHitRateAggregation(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m1", Delta{
		CacheHitTokens: 900, HasCacheHitTokens: true,
		CacheMissTokens: 100, HasCacheMissTokens: true,
	}, true)
	r.Add(now, "cn", "u1", "m1", Delta{
		CacheHitTokens: 800, HasCacheHitTokens: true,
		CacheMissTokens: 200, HasCacheMissTokens: true,
	}, true)

	var hit, miss int64
	for _, b := range r.buckets {
		hit += b.CH
		miss += b.CM
	}
	if hit != 1700 || miss != 300 {
		t.Fatalf("累计 hit=%d miss=%d，want 1700/300", hit, miss)
	}
	// 命中率 1700/2000 = 0.85
	a := r.Snapshot(24, nil).Totals
	if diff := a.CacheHitRate - 0.85; diff > 0.001 || diff < -0.001 {
		t.Errorf("CacheHitRate=%.4f，want 0.85", a.CacheHitRate)
	}
}

// TestCacheHitRateNoSamplesIsMinusOne ⭐ 无样本时命中率必须是 **-1**，不是 0。
//
// 为什么关键：0 会被读成"命中率极低（缓存完全没生效）"，而事实是"上游没返回
// 该维度/本窗口无请求"。这是"数据不报错但会误导人"的一类缺陷 —— 与 LatSamples
// 用样本数让前端显示 "—" 是同哲学。
func TestCacheHitRateNoSamplesIsMinusOne(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m1", Delta{HasTotal: true, TotalTokens: 10}, true)
	a := r.Snapshot(24, nil).Totals
	if a.CacheHitRate != -1 {
		t.Errorf("无缓存样本时 CacheHitRate=%.2f，want -1（样本不足哨兵值）", a.CacheHitRate)
	}
	if a.CacheHitTokens != 0 || a.CacheMissTokens != 0 {
		t.Errorf("无样本时命中/未命中应为 0，得 %d/%d", a.CacheHitTokens, a.CacheMissTokens)
	}
}

// TestCacheHitRateSkipsAbsentDimension 上游未返回该维度时**不得用 0 冒充**
// （Has* 为 false 应跳过累加，否则会把"无数据"算成"全部未命中"）。
func TestCacheHitRateSkipsAbsentDimension(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m1", Delta{
		CacheMissTokens: 0, HasCacheMissTokens: false, // 维度缺失
	}, true)
	var miss int64
	for _, b := range r.buckets {
		miss += b.CM
	}
	if miss != 0 {
		t.Errorf("缺失维度不该累加，miss=%d", miss)
	}
	if a := r.Snapshot(24, nil).Totals; a.CacheHitRate != -1 {
		t.Errorf("缺失维度应判为无样本（-1），得 %.2f", a.CacheHitRate)
	}
}

// TestCacheHitRateFullHitAndFullMiss 边界：全命中 = 1.0；全未命中 = 0.0（**不是** -1）。
func TestCacheHitRateFullHitAndFullMiss(t *testing.T) {
	rHit := New("")
	rHit.Add(time.Now(), "cn", "u1", "m1", Delta{
		CacheHitTokens: 500, HasCacheHitTokens: true,
		CacheMissTokens: 0, HasCacheMissTokens: true,
	}, true)
	if a := rHit.Snapshot(24, nil).Totals; a.CacheHitRate != 1.0 {
		t.Errorf("全命中应得 1.0，得 %.2f", a.CacheHitRate)
	}

	rMiss := New("")
	rMiss.Add(time.Now(), "cn", "u1", "m1", Delta{
		CacheHitTokens: 0, HasCacheHitTokens: true,
		CacheMissTokens: 500, HasCacheMissTokens: true,
	}, true)
	// ⭐ 全未命中是**真实观测到的 0%**，与"无样本 -1"必须能区分。
	if a := rMiss.Snapshot(24, nil).Totals; a.CacheHitRate != 0.0 {
		t.Errorf("全未命中应得 0.0（真实观测到的零命中），得 %.2f", a.CacheHitRate)
	}
}
