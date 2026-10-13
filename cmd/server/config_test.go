package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h", c.SoftRateMaxDur)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", c.SoftRateDur)
	}
	if c.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", c.SoftRateMaxDur)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", c.SoftRateMaxDur)
	}
}

func TestBadSoftRateMax(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
	}
}

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true：
// 老 config 不写这些键，行为必须与从前完全一致。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("travel/activity enabled defaults want true/true, got %v/%v",
			c.Schedule.TravelEnabled, c.Schedule.ActivityEnabled)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写签到/保活小时数组，无新键）加载后仍是启用态，
// 新开关缺省 true、新 hours 回落默认——对老配置零影响。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("new switches must default true on legacy config: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
	// 新 hours 缺省 → 回落默认（非空）。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_enabled":false,"activity_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.TravelEnabled || c.Schedule.ActivityEnabled {
		t.Errorf("want travel/activity disabled: %+v", c.Schedule)
	}
	// 签到/保活开关缺省 true（互不干扰）。
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("checkin/keepalive should stay enabled: %+v", c.Schedule)
	}
	// hours 仍回落默认。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21] even when disabled", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10] even when disabled", c.Schedule.ActivityHours)
	}
}

// TestScheduleTravelActivityInvalidHoursRejected 旅行/活跃非法小时报错并指向正确开关。
func TestScheduleTravelActivityInvalidHoursRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"activity_hours":[-1]}}`, "activity_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleTravelActivityExplicitHours 显式配置旅行/活跃小时。
func TestScheduleTravelActivityExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_hours":[9,21],"activity_hours":[11]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 11 {
		t.Errorf("activity_hours=%v want [11]", c.Schedule.ActivityHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null,"travel_hours":null,"activity_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[],"travel_hours":[],"activity_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
				t.Errorf("travel_hours=%v want default [9 21]", c.Schedule.TravelHours)
			}
			if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
				t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
			if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

func TestWriteDefault(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "sub", "config.json") // 顺带验证父目录自动创建
	key, err := WriteDefault(fp)
	if err != nil {
		t.Fatal(err)
	}
	// key 形如 sk-<24字符随机串>，两次生成不重复
	if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
		t.Errorf("key=%q want sk-<random>", key)
	}
	if key2, _ := WriteDefault(filepath.Join(dir, "another.json")); key2 == key {
		t.Errorf("two generated keys identical: %q", key)
	}
	// 落盘文件可被 Load 正常加载，推荐值齐备且 api_key 生效
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if c.APIKey != key {
		t.Errorf("api_key=%q want %q", c.APIKey, key)
	}
	if c.Listen != ":7863" || c.AuthDir != "./auths" || c.StateFile != "./data/state.json" {
		t.Errorf("generated defaults off: %+v", c)
	}
	if len(c.Schedule.CheckinHours) == 0 || !c.Schedule.CheckinEnabled {
		t.Errorf("generated schedule off: %+v", c.Schedule)
	}
	// 已存在的文件不覆盖：二次写入同一路径必须报错
	if _, err := WriteDefault(fp); err == nil {
		t.Error("WriteDefault must refuse to overwrite existing file")
	}
}

func TestBalanceRefreshDefaults(t *testing.T) {
	// 缺省：启用 + 30 分钟
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.BalanceRefreshEnabled || c.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("default balance refresh: enabled=%v interval=%v", c.Schedule.BalanceRefreshEnabled, c.BalanceRefreshInterval)
	}
	// 显式配置 10 分钟
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":10}}`), 0o600)
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.BalanceRefreshInterval != 10*time.Minute {
		t.Errorf("interval=%v want 10m", c2.BalanceRefreshInterval)
	}
	// 显式关闭：interval 归零（不启动）
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_enabled":false}}`), 0o600)
	c3, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c3.BalanceRefreshInterval != 0 {
		t.Errorf("disabled interval=%v want 0", c3.BalanceRefreshInterval)
	}
	// 启用但 minutes<=0 → 回落默认 30
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":-5}}`), 0o600)
	c4, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c4.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("fallback interval=%v want 30m", c4.BalanceRefreshInterval)
	}
}

// TestMaxBodyKeyRemovedLegacyConfigIgnored server.max_body_mb 已移除（BREAKING），
// 但**旧配置里的该键必须被静默忽略、不影响启动**——用户升级后不会因为残留键起不来。
//
// 为什么必须守：这是破坏性变更的唯一"软着陆"承诺。config 用 json.Unmarshal（未设
// DisallowUnknownFields），未知键天然被忽略；本测试把该行为固化，防日后有人加上
// 严格模式而无声打破向后兼容。
// 注：该键被忽略后，配置回写（MarshalIndent）会将其抹除——这是既有行为，非本次引入。
func TestMaxBodyKeyRemovedLegacyConfigIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 含已移除的 server 段 + 一个仍有效的键，确认整体仍能加载。
	os.WriteFile(fp, []byte(`{"listen":":9999","server":{"max_body_mb":8},"cooldown":{"soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("残留的已移除键不应导致启动失败: %v", err)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q want :9999（有效键应照常生效）", c.Listen)
	}
	if c.SoftRateDur != 30*time.Second {
		t.Errorf("soft_rate=%v want 30s（有效键应照常生效）", c.SoftRateDur)
	}
}

// TestMaxBodyEnvRemoved env WB2A_MAX_BODY_MB 已随键移除：设了也不再有任何效果
// （不报错、不改变行为）。守的是"旧部署脚本里残留的该 env 不会让进程起不来"。
func TestMaxBodyEnvRemoved(t *testing.T) {
	t.Setenv("WB2A_MAX_BODY_MB", "64")
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999"}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("残留的 WB2A_MAX_BODY_MB 不应导致启动失败: %v", err)
	}
}

// TestPromptDefaultPassthrough 默认 prompt.mode=passthrough（对齐上游：透传客户端
// 原始 system 是更保守的缺省）；custom 由用户显式选择，此时 PromptText 为内置默认（非空）。
func TestPromptDefaultPassthrough(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("prompt.mode=%q want passthrough", c.Prompt.Mode)
	}
	// passthrough 不加载提示词文本（透传客户端 system）；切 custom 时 normalize 会加载。
}

// TestPromptExplicitPassthrough passthrough 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitPassthrough(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough should not load PromptText, got len=%d", len(c.PromptText))
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	// 用 json.Marshal 拼路径：Windows 反斜杠必须转义，手工字符串拼接会产出非法 JSON。
	cfgJSON, err := json.Marshal(map[string]any{"prompt": map[string]any{"mode": "custom", "file": pf}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cf, cfgJSON, 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "passthrough")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
}

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 缺省 passthrough。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("legacy config should default to passthrough, got %q", c.Prompt.Mode)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q", c.Listen)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
func TestUpstreamUserAgentConfig(t *testing.T) {
	// JSON 配置
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"user_agent":"WorkBuddy/1.2.3"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", c.Upstream.UserAgent)
	}
	// 缺省为空
	if c2, err := Load(""); err != nil || c2.Upstream.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty (err=%v)", c2.Upstream.UserAgent, err)
	}
	// env 覆盖
	t.Setenv("WB2A_USER_AGENT", "EnvAgent/9")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.UserAgent != "EnvAgent/9" {
		t.Errorf("env user_agent=%q want EnvAgent/9", c3.Upstream.UserAgent)
	}
}

// TestLoadConfigPathIsDirectory config 路径是目录时给出可操作提示（Docker bind mount 陷阱）。
// 复现：compose 挂载 ./config.json 但宿主机缺该文件 → Docker 创建同名目录 → 启动失败。
// 旧行为只报 "read config: ... Incorrect function" 之类晦涩错误，无从排查。
func TestLoadConfigPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Load(asDir)
	if err == nil {
		t.Fatal("want error when config path is a directory")
	}
	msg := err.Error()
	if !strings.Contains(msg, "是目录") {
		t.Errorf("error should explain it is a directory: %v", err)
	}
	if !strings.Contains(msg, "config.example.json") {
		t.Errorf("error should suggest the fix (cp config.example.json): %v", err)
	}
}

// TestCostExploreDefault 未配置时回落默认 30m。
func TestCostExploreDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.CostExploreIntervalDur != 30*time.Minute {
		t.Errorf("cost_explore_interval=%v want 30m", c.CostExploreIntervalDur)
	}
}

// TestCostExploreExplicit 显式时长生效。
func TestCostExploreExplicit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"5m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 5*time.Minute {
		t.Errorf("cost_explore_interval=%v want 5m", c.CostExploreIntervalDur)
	}
}

// TestCostExploreZeroDisables "0" 必须被当成**显式关停**而非"未配置回落默认"。
//
// 这是本键最容易写错的地方：本地其它 pool 键的归一逻辑都是"空/0 → 回落默认"
// （如 expiring_soon 的"空/0 = 禁用分桶"），若照抄，用户填的 "0" 会被静默改成 30m，
// 表现为"我明明关了，路由却还在改道"。
func TestCostExploreZeroDisables(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"0"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 0 {
		t.Errorf(`cost_explore_interval="0" 应关停（0），得 %v`, c.CostExploreIntervalDur)
	}
}

// TestCostExploreNegativeClamped 负值归一为关停（不报错，防御性）。
func TestCostExploreNegativeClamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"-1m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 0 {
		t.Errorf("负值应归一为 0，得 %v", c.CostExploreIntervalDur)
	}
}

// TestCostExploreInvalidFailFast 拼写错误必须 fail fast——静默回落默认会让
// "我配了但没生效"无从排查（同 expiring_soon 风格）。
func TestCostExploreInvalidFailFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"30min"}}`), 0o600)
	_, err := Load(fp)
	if err == nil {
		t.Fatal("want error for 非法时长 30min")
	}
	if !strings.Contains(err.Error(), "pool.cost_explore_interval") {
		t.Errorf("error 应点明配置键: %v", err)
	}
}

// TestCostExploreEnvOverride env WB2A_COST_EXPLORE_INTERVAL 非空覆盖 JSON 值。
func TestCostExploreEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"5m"}}`), 0o600)
	t.Setenv("WB2A_COST_EXPLORE_INTERVAL", "45m")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 45*time.Minute {
		t.Errorf("cost_explore_interval=%v want env 45m", c.CostExploreIntervalDur)
	}
}

// TestRequestArchiveDefaultEnabled ⭐ 缺 logging 段的老配置也必须**默认开启**归档。
//
// 为什么必须钉住：`RequestArchiveEnabled` 是 bool，Go 零值是 **false**；而 Load 是
// 「先 Default() 再被 JSON 覆盖」。若只在 normalize 里做数值回落（bool 没有 "<=0" 可判），
// 缺 logging 段的生产配置会**静默保持 false 把归档关掉** —— 实测踩过（面板显示"归档已关闭"，
// 而上线说明写着"缺省开启"）。这条测试就是那个不一致的回归网。
func TestRequestArchiveDefaultEnabled(t *testing.T) {
	// 1) Default() 必须置 true
	if !Default().Logging.RequestArchiveEnabled {
		t.Error("Default() 的 RequestArchiveEnabled 应为 true")
	}

	// 2) 老配置（无 logging 段）加载后必须仍为 true
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":7863","api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Logging.RequestArchiveEnabled {
		t.Error("无 logging 段的老配置加载后归档应默认开启（bool 零值陷阱）")
	}
	if c.Logging.RequestRetentionDays != 7 {
		t.Errorf("retentionDays=%d，want 7", c.Logging.RequestRetentionDays)
	}
	if c.Logging.RequestArchiveMaxMB != 100 {
		t.Errorf("archiveMaxMB=%d，want 100", c.Logging.RequestArchiveMaxMB)
	}

	// 3) 用户显式关闭必须生效（JSON 覆盖优先级高于 Default）
	path2 := filepath.Join(dir, "c2.json")
	if err := os.WriteFile(path2, []byte(`{"logging":{"request_archive_enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Logging.RequestArchiveEnabled {
		t.Error("显式 request_archive_enabled=false 未被尊重（JSON 覆盖应优先于 Default）")
	}

	// 4) <=0 回落默认
	path3 := filepath.Join(dir, "c3.json")
	if err := os.WriteFile(path3, []byte(`{"logging":{"request_retention_days":0,"request_archive_max_mb":-5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(path3)
	if err != nil {
		t.Fatal(err)
	}
	if c3.Logging.RequestRetentionDays != 7 || c3.Logging.RequestArchiveMaxMB != 100 {
		t.Errorf("<=0 应回落默认，得 %d/%d",
			c3.Logging.RequestRetentionDays, c3.Logging.RequestArchiveMaxMB)
	}
}
