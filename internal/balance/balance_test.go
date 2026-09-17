package balance

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 固定时钟，避免依赖真实时间。
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestAddRespectsInterval(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := now
	r.clock = func() time.Time { return cur }

	if !r.Add("u1", 100) {
		t.Fatal("首次采样应被接受")
	}
	// 同一时刻重复调用：节流拒绝
	if r.Add("u1", 101) {
		t.Error("间隔不足应被节流拒绝")
	}
	// 推进 29 分钟：仍拒绝
	cur = now.Add(29 * time.Minute)
	if r.Add("u1", 102) {
		t.Error("29 分钟仍不足采样间隔，应拒绝")
	}
	// 推进 30 分钟：接受
	cur = now.Add(30 * time.Minute)
	if !r.Add("u1", 103) {
		t.Error("满 30 分钟应被接受")
	}
	if got := len(r.points); got != 2 {
		t.Errorf("点数 = %d, 期望 2", got)
	}
}

func TestAddSkipsEmptyUID(t *testing.T) {
	r := New("")
	if r.Add("", 100) {
		t.Error("空 uid 不应记录")
	}
	if len(r.points) != 0 {
		t.Errorf("点数 = %d, 期望 0", len(r.points))
	}
}

// 关键行为：超期原始点折叠为日粒度，每账号每天只留「当日最后一个值」。
func TestRollupFoldsToDailyKeepLast(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	// 手工构造 3 天前的多个点（同一天 3 个，值递增）
	d0 := base
	r.points = []Point{
		{TS: d0.Add(0 * time.Hour).Unix(), UID: "u1", Credits: 500},
		{TS: d0.Add(2 * time.Hour).Unix(), UID: "u1", Credits: 480},
		{TS: d0.Add(4 * time.Hour).Unix(), UID: "u1", Credits: 460}, // 当日最后
	}
	// 现在 = 31 天后 → 超过 detailedKeep(30 天)，上面 3 个点全部超期
	now := base.Add(31 * 24 * time.Hour)
	r.Rollup(now)

	if got := len(r.points); got != 1 {
		t.Fatalf("折叠后点数 = %d, 期望 1（每天 1 个）", got)
	}
	p := r.points[0]
	if p.Day == "" {
		t.Error("折叠点应带 Day 标记")
	}
	if p.Credits != 460 {
		t.Errorf("应保留当日最后一个值 460, 得到 %d", p.Credits)
	}
}

// 多账号折叠互相独立：同名日不同账号不得互相覆盖。
func TestRollupPerUID(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 100},
		{TS: base.Add(time.Hour).Unix(), UID: "u2", Credits: 200},
	}
	r.Rollup(base.Add(72 * time.Hour))
	if got := len(r.points); got != 2 {
		t.Errorf("两个账号应各留一个日点, 得到 %d", got)
	}
}

// 未超期的点不得被折叠（边界：刚过 detailedKeep 才折）。
func TestRollupKeepsRecent(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{{TS: now.Add(-time.Hour).Unix(), UID: "u1", Credits: 999}}
	r.Rollup(now)
	if len(r.points) != 1 || r.points[0].Day != "" {
		t.Error("近期点不应被折叠")
	}
}

// 超期日粒度点应被丢弃（dayKeep 上限）。
func TestRollupPrunesOldDays(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	old := now.Add(-400 * 24 * time.Hour)
	r := New("")
	r.points = []Point{{TS: old.Unix(), UID: "u1", Credits: 1, Day: old.In(time.Local).Format(dayLayout)}}
	r.Rollup(now)
	if len(r.points) != 0 {
		t.Errorf("超期日点应被丢弃, 得到 %d", len(r.points))
	}
}

func TestSnapshotSeriesStats(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: now.Add(-2 * time.Hour).Unix(), UID: "u1", Credits: 1000},
		{TS: now.Add(-1 * time.Hour).Unix(), UID: "u1", Credits: 700},
		{TS: now.Unix(), UID: "u1", Credits: 900},
		{TS: now.Unix(), UID: "u2", Credits: 50},
	}
	r.SetName("u1", "主号")
	got, _ := r.Snapshot(time.Time{})
	if len(got) != 2 {
		t.Fatalf("序列数 = %d, 期望 2", len(got))
	}
	// 按最新值降序：u1(900) 在前
	if got[0].UID != "u1" {
		t.Errorf("首个序列应为 u1, 得到 %s", got[0].UID)
	}
	s := got[0]
	if s.Current != 900 || s.Min != 700 || s.Max != 1000 || s.Change != -100 {
		t.Errorf("统计错误: current=%d min=%d max=%d change=%d", s.Current, s.Min, s.Max, s.Change)
	}
	if s.Name != "主号" {
		t.Errorf("昵称未透出: %q", s.Name)
	}
}

// since 过滤：只返回窗口内的点。
func TestSnapshotSinceFilter(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: now.Add(-48 * time.Hour).Unix(), UID: "u1", Credits: 1},
		{TS: now.Add(-1 * time.Hour).Unix(), UID: "u1", Credits: 2},
	}
	got, _ := r.Snapshot(now.Add(-24 * time.Hour))
	if len(got) != 1 || len(got[0].Points) != 1 {
		t.Fatalf("窗口过滤失败: %+v", got)
	}
	if got[0].Points[0].Credits != 2 {
		t.Errorf("应只保留窗口内的点")
	}
}

// 落盘 + 重载：数据不丢，且 lastSample 以最后点恢复（避免重启后立刻补点）。
func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "balance_history.json")
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	r := New(path)
	r.clock = fixedClock(now)
	r.Add("u1", 1234)
	r.Save()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件未生成: %v", err)
	}
	// 权限 0600（含账号数据）
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("权限 = %o, 期望 600", fi.Mode().Perm())
	}

	r2 := New(path)
	got, _ := r2.Snapshot(time.Time{})
	if len(got) != 1 || got[0].Current != 1234 {
		t.Fatalf("重载失败: %+v", got)
	}
	if len(r2.lastSample) == 0 {
		t.Error("重载后每账号的 lastSample 应恢复")
	}
}

// 损坏文件不应让服务起不来（过滤非法点）。
func TestLoadSkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "balance_history.json")
	raw := `{"version":1,"saved":"x","points":[{"t":0,"u":"u1","c":1},{"t":100,"u":"","c":2},{"t":200,"u":"u2","c":3}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	if len(r.points) != 1 {
		t.Errorf("非法点应被过滤, 得到 %d 个", len(r.points))
	}
}

func TestLoadMissingFileStartsEmpty(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "nope.json"))
	if len(r.points) != 0 {
		t.Error("缺文件应空历史启动")
	}
}

func TestDescribe(t *testing.T) {
	r := New("")
	r.points = []Point{{TS: 1, UID: "u1", Credits: 1}, {TS: 2, UID: "u1", Credits: 2, Day: "x"}}
	if s := r.Describe(); s == "" {
		t.Error("Describe 不应为空")
	}
}

func TestNilRecorderSafe(t *testing.T) {
	var r *Recorder
	if r.Add("u1", 1) {
		t.Error("nil 应安全返回 false")
	}
	r.Rollup(time.Now()) // 不应 panic
	if got, _ := r.Snapshot(time.Time{}); got != nil {
		t.Error("nil 应返回空")
	}
	r.SetName("u1", "x") // 不应 panic
}

// ⭐ 回归网：节流必须按账号独立。
// 曾经的缺陷：lastSample 是全局单个时间戳，而采样由 RunBalanceRefreshNow 并发
// 触达所有账号 → 同一窗口只有"抢到"的账号落点，其余被静默丢弃，症状是
// 「多个账号只有一条线」。单账号时该缺陷不显形，故当初漏测——本用例专门钉住它。
func TestAddThrottleIsPerAccount(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := now
	r.clock = func() time.Time { return cur }

	// 同一时刻、三个账号依次采样：全部都应被接受（各自独立窗口）。
	for _, uid := range []string{"A", "B", "C"} {
		if !r.Add(uid, 100) {
			t.Fatalf("账号 %s 首次采样应被接受（节流必须按账号独立）", uid)
		}
	}
	if len(r.points) != 3 {
		t.Fatalf("三个账号各应落一点，共 3 点, 得到 %d", len(r.points))
	}

	// 同一账号在窗口内重复：应被拒。
	if r.Add("A", 101) {
		t.Error("同账号在 30 分钟内重复应被节流")
	}

	// 推进 30 分钟：三个账号都应再次落点。
	cur = now.Add(30 * time.Minute)
	for _, uid := range []string{"A", "B", "C"} {
		if !r.Add(uid, 200) {
			t.Errorf("满 30 分钟后账号 %s 应可再次采样", uid)
		}
	}
	if len(r.points) != 6 {
		t.Errorf("应累计 6 点, 得到 %d", len(r.points))
	}
	// 且每条序列各 2 点（前端一条线一账号的依据）
	got, _ := r.Snapshot(time.Time{})
	if len(got) != 3 {
		t.Fatalf("应有 3 条序列, 得到 %d", len(got))
	}
	for _, s := range got {
		if len(s.Points) != 2 {
			t.Errorf("账号 %s 应有 2 点, 得到 %d", s.UID, len(s.Points))
		}
	}
}

// 回归网：重载后每账号的节流时刻各自恢复（否则重启后会给某账号连补多点）。
func TestReloadRestoresPerAccountThrottle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	r := New(path)
	cur := t0
	r.clock = func() time.Time { return cur }
	r.Add("A", 1000)
	cur = t0.Add(10 * time.Minute)
	r.Add("B", 2000) // B 晚 10 分钟
	r.Save()

	r2 := New(path)
	if len(r2.points) != 2 {
		t.Fatalf("重载应得 2 点, 得到 %d", len(r2.points))
	}
	// A 的最后采样是 t0，距 t0+10min 仅 10 分钟 → 应被节流
	cur2 := t0.Add(10 * time.Minute)
	r2.clock = func() time.Time { return cur2 }
	if r2.Add("A", 1001) {
		t.Error("A 距上次仅 10 分钟，重载后应仍被节流")
	}
	// B 的最后采样就是此刻 → 也应被节流
	if r2.Add("B", 2001) {
		t.Error("B 恰在采样时刻，应被节流")
	}
	// 满 30 分钟后放行
	cur2 = t0.Add(30 * time.Minute)
	if !r2.Add("A", 1002) {
		t.Error("A 满 30 分钟应放行")
	}
}

// 每日聚合：口径 = 末值 − 首值；跨日要正确切分。
func TestDailyOfBasic(t *testing.T) {
	d1 := time.Date(2026, 9, 16, 8, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 17, 8, 0, 0, 0, time.Local)
	pts := []Point{
		{TS: d1.Unix(), Credits: 1000},
		{TS: d1.Add(30 * time.Minute).Unix(), Credits: 900},
		{TS: d1.Add(time.Hour).Unix(), Credits: 800}, // 9/16 末值 800 → 变化 -200
		{TS: d2.Unix(), Credits: 1200},
		{TS: d2.Add(30 * time.Minute).Unix(), Credits: 1100}, // 9/17 末值 1100 → 变化 -100
	}
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.Local) // 站在 9/18 看，两天都已过完
	got := dailyOf(pts, now)
	if len(got) != 2 {
		t.Fatalf("应 2 天, 得到 %d", len(got))
	}
	if got[0].Day != "2026-09-16" || got[1].Day != "2026-09-17" {
		t.Errorf("日期错误: %s / %s", got[0].Day, got[1].Day)
	}
	if got[0].First != 1000 || got[0].Last != 800 || got[0].Change != -200 {
		t.Errorf("9/16 聚合错误: first=%d last=%d change=%d", got[0].First, got[0].Last, got[0].Change)
	}
	if got[0].Min != 800 || got[0].Max != 1000 {
		t.Errorf("9/16 min/max 错误: %d/%d", got[0].Min, got[0].Max)
	}
	if got[1].Change != -100 {
		t.Errorf("9/17 变化应为 -100, 得到 %d", got[1].Change)
	}
}

// ⭐ 关键边界：今日必须标记为「不完整」——否则"今天只涨一点"会被误读。
func TestDailyOfTodayNotFull(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	// 今日只采了 3 点（远少于 expectedPerDay=40）
	pts := []Point{
		{TS: now.Add(-2 * time.Hour).Unix(), Credits: 500},
		{TS: now.Add(-time.Hour).Unix(), Credits: 450},
		{TS: now.Unix(), Credits: 400},
	}
	got := dailyOf(pts, now)
	if len(got) != 1 {
		t.Fatalf("应 1 天, 得到 %d", len(got))
	}
	if got[0].Full {
		t.Error("今日必须标记为不完整（Full=false），否则读数会被误读")
	}
}

// 历史完整日（点数达标）应标记 Full=true。
func TestDailyOfPastFullDay(t *testing.T) {
	day := time.Date(2026, 9, 16, 0, 0, 0, 0, time.Local)
	var pts []Point
	for i := 0; i < 48; i++ { // 全天 48 点
		pts = append(pts, Point{TS: day.Add(time.Duration(i) * 30 * time.Minute).Unix(), Credits: 1000 - int64(i)})
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local)
	got := dailyOf(pts, now)
	if len(got) != 1 || !got[0].Full {
		t.Errorf("48 点的历史日应 Full=true, 得到 %+v", got)
	}
}

// 点数不足的历史日（服务中途启用）也应标记不完整。
func TestDailyOfPastPartialDay(t *testing.T) {
	day := time.Date(2026, 9, 16, 20, 0, 0, 0, time.Local)
	pts := []Point{
		{TS: day.Unix(), Credits: 100},
		{TS: day.Add(30 * time.Minute).Unix(), Credits: 90},
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local)
	got := dailyOf(pts, now)
	if len(got) != 1 {
		t.Fatalf("应 1 天, 得到 %d", len(got))
	}
	if got[0].Full {
		t.Error("仅 2 点的历史日应标记不完整")
	}
}

func TestDailyOfEmpty(t *testing.T) {
	if got := dailyOf(nil, time.Now()); got != nil {
		t.Errorf("空输入应返回 nil, 得到 %v", got)
	}
}

// Snapshot 应把 daily 一并带出（前端每日对照视图的数据来源）。
func TestSnapshotIncludesDaily(t *testing.T) {
	base := time.Date(2026, 9, 16, 8, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 1000},
		{TS: base.Add(time.Hour).Unix(), UID: "u1", Credits: 1200},
	}
	got, _ := r.Snapshot(time.Time{})
	if len(got) != 1 {
		t.Fatalf("应 1 序列")
	}
	if len(got[0].Daily) != 1 {
		t.Fatalf("应带出 1 天 daily, 得到 %d", len(got[0].Daily))
	}
	if got[0].Daily[0].Change != 200 {
		t.Errorf("daily 变化应为 +200, 得到 %d", got[0].Daily[0].Change)
	}
}
