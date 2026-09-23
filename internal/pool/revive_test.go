package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func mkPool(t *testing.T) (*Pool, *auth.Auth) {
	t.Helper()
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	p.Add(a)
	return p, a
}

// TestClassifyDisable 三类禁用原因必须被正确分类 ——
// 分类错会导致「人工禁用被自动解」或「真失效的号被反复重试」。
func TestClassifyDisable(t *testing.T) {
	cases := []struct {
		reason string
		want   DisableKind
	}{
		{"manual disable (panel)", DisableManual},
		{"12153 session dead", DisableTransient},
		{"refresh session dead", DisableRelogin},
		{"account banned by upstream (11140 request illegal), re-login required", DisableRelogin},
		{"某个没见过的新原因", DisableRelogin}, // 未知 → 保守（不自动恢复）
	}
	for _, c := range cases {
		if got := classifyDisable(c.reason); got != c.want {
			t.Errorf("classifyDisable(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

// TestManualDisableNeverAutoRevives 人工禁用**永不出现在重试候选里** ——
// 自动解开人工禁用等于违背用户明确操作（最严重的一类错误）。
func TestManualDisableNeverAutoRevives(t *testing.T) {
	p, _ := mkPool(t)
	p.Disable("u1", "manual disable (panel)")
	// 把禁用时刻推到很久以前（远超 TTL）——若实现只看到期时间就会误选
	p.mu.Lock()
	p.byUID["u1"].disabledAt = time.Now().Add(-24 * time.Hour)
	p.mu.Unlock()

	if got := p.TransientRetryCandidates(time.Now()); len(got) != 0 {
		t.Errorf("人工禁用被选为自动重试候选：%v（会违背用户意图）", got)
	}
}

// TestTransientDisableRetriedAfterTTL 临时禁用过了冷却期必须成为候选。
func TestTransientDisableRetriedAfterTTL(t *testing.T) {
	p, _ := mkPool(t)
	p.Disable("u1", "12153 session dead")

	// TTL 内：不选
	if got := p.TransientRetryCandidates(time.Now()); len(got) != 0 {
		t.Errorf("冷却期未到就被选中：%v", got)
	}
	// 推过 TTL：应被选中
	p.mu.Lock()
	p.byUID["u1"].disabledAt = time.Now().Add(-transientDisableTTL - time.Minute)
	p.mu.Unlock()
	got := p.TransientRetryCandidates(time.Now())
	if len(got) != 1 || got[0] != "u1" {
		t.Errorf("过冷却期后未成为候选：%v", got)
	}
}

// TestReloginDisableNotRetried 需重登类（凭证失效/封号）不得被自动重试 ——
// refresh 必然失败，试只会反复骚扰上游。
func TestReloginDisableNotRetried(t *testing.T) {
	p, _ := mkPool(t)
	p.Disable("u1", "refresh session dead")
	p.mu.Lock()
	p.byUID["u1"].disabledAt = time.Now().Add(-24 * time.Hour)
	p.mu.Unlock()
	if got := p.TransientRetryCandidates(time.Now()); len(got) != 0 {
		t.Errorf("需重登的号被选为自动重试候选：%v", got)
	}
}

// TestNoteTransientRetryFailedEscalates 重试失败必须**升级为需重登**并停止重试 ——
// 否则会每 30 分钟无限重试一个确认失效的号。
func TestNoteTransientRetryFailedEscalates(t *testing.T) {
	p, _ := mkPool(t)
	p.Disable("u1", "12153 session dead")
	p.mu.Lock()
	p.byUID["u1"].disabledAt = time.Now().Add(-transientDisableTTL - time.Minute)
	p.mu.Unlock()
	if len(p.TransientRetryCandidates(time.Now())) != 1 {
		t.Fatal("前置条件不成立：应为候选")
	}

	p.NoteTransientRetryFailed("u1")

	if got := p.TransientRetryCandidates(time.Now()); len(got) != 0 {
		t.Errorf("重试失败后仍被选为候选（会无限重试）：%v", got)
	}
	st, _ := p.Status("u1")
	if st.DisableHint == nil || st.DisableHint.Kind != DisableRelogin {
		t.Errorf("重试失败后提示未升级为需重登：%+v", st.DisableHint)
	}
}

// TestStatusExposesDisableHint 禁用账号必须透出「该怎么办」的说明 ——
// 只给"已禁用"用户不知道该干什么（三种情况动作完全不同）。
func TestStatusExposesDisableHint(t *testing.T) {
	p, _ := mkPool(t)
	p.Disable("u1", "12153 session dead")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("状态不存在")
	}
	if st.DisableHint == nil {
		t.Fatal("未透出 disable_hint")
	}
	if st.DisableHint.Kind != DisableTransient {
		t.Errorf("kind = %q, want transient", st.DisableHint.Kind)
	}
	if st.DisableHint.AutoRetryAt.IsZero() {
		t.Error("临时禁用未给出自动重试时刻（用户不知道何时会恢复）")
	}
	if st.DisableHint.Hint == "" {
		t.Error("hint 为空（用户不知道下一步做什么）")
	}
}

// TestDisabledAtPersists 禁用时刻必须持久化 —— 否则重启后 disabledAt 归零，
// 「临时禁用到期」永远算不出到期（候选恒为空 → 自动重试形同虚设）。
func TestDisabledAtPersists(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	p.Disable("u1", "12153 session dead")
	p.Flush()

	// 重启
	p2 := New(fp)
	p2.RestoreFromSnapshot()
	at := p2.DisabledAt("u1")
	if at.IsZero() {
		t.Fatal("重启后 disabledAt 归零 —— 临时禁用永远算不出到期，自动重试失效")
	}
	// 且该账号仍应是「临时禁用」类（reason 也须持久化）
	st, _ := p2.Status("u1")
	if st.DisableHint == nil || st.DisableHint.Kind != DisableTransient {
		t.Errorf("重启后禁用性质丢失：%+v", st.DisableHint)
	}
}
