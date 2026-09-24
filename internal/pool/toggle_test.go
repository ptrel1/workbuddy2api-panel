package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestManualOffBlocksSelection 手动关闭的 (账号,模型) 必须**不可被选中**。
func TestManualOffBlocksSelection(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	p.Add(a)

	if !p.pickOK(t, "glm-5.3") {
		t.Fatal("前置：未关闭时应可选")
	}
	if !p.SetModelOff("u1", "glm-5.3", "质量差") {
		t.Fatal("SetModelOff 失败")
	}
	if p.pickOK(t, "glm-5.3") {
		t.Error("手动关闭后仍可选 —— 开关无效")
	}
	// 其他模型不受影响（精确到组合，不是整个账号）
	if !p.pickOK(t, "kimi-k3") {
		t.Error("关闭 A 模型影响了 B 模型（应精确到 (账号,模型)）")
	}
}

// TestManualOffBeatsAutoRecovery 「手动优先」：手动关掉的组合，
// **自动机制（11102/6004 冷却到期）不能把它打开**。
//
// 用户选定该语义：「我关了它，一次自动恢复又开了」会让人无法信任这个开关。
func TestManualOffBeatsAutoRecovery(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	p.Add(a)
	p.SetModelOff("u1", "glm-5.3", "")

	// 模拟自动机制：写入 11102 负缓存，然后"成功"清除它（自动恢复路径）
	p.BlockModelBackoff("u1", "glm-5.3", "11102 test")
	p.BlockModelClear("u1", "glm-5.3")

	if p.pickOK(t, "glm-5.3") {
		t.Error("自动恢复把手动关闭打开了 —— 违背「手动优先」（用户会无法信任该开关）")
	}
}

// TestSetModelOnClears 手动启用必须能解除（否则开关是单向的）。
func TestSetModelOnClears(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	p.Add(a)
	p.SetModelOff("u1", "glm-5.3", "临时排查")
	if p.pickOK(t, "glm-5.3") {
		t.Fatal("前置失败")
	}
	if !p.SetModelOn("u1", "glm-5.3") {
		t.Error("SetModelOn 应报告有变更")
	}
	if !p.pickOK(t, "glm-5.3") {
		t.Error("解除后仍不可选")
	}
	// 幂等：再次启用无变更
	if p.SetModelOn("u1", "glm-5.3") {
		t.Error("重复启用应返回 false（幂等）")
	}
}

// TestModelOffPersists 手动关闭必须**跨重启保留**且**不被过期逻辑清理**。
//
// 关键差异：modelCooldowns 过期会被 prune 删掉；modelOff 是永久语义，
// 绝不能走同一套清理 —— 否则"永久关闭"会在重启/后台清理后悄悄失效。
func TestModelOffPersists(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	p.Add(a)
	p.SetModelOff("u1", "glm-5.3", "长期关闭")
	p.Flush()

	p2 := New(fp)
	p2.RestoreFromSnapshot()
	if p2.pickOK(t, "glm-5.3") {
		t.Fatal("重启后手动关闭丢失 —— 永久语义失效")
	}
	got := p2.ModelOffOf("u1")
	if len(got) != 1 || got[0].Model != "glm-5.3" {
		t.Errorf("ModelOffOf = %+v, want 1 条 glm-5.3", got)
	}
	if got[0].Note != "长期关闭" {
		t.Errorf("备注丢失：%q", got[0].Note)
	}
}

// TestModelOffScopedToAccount 关闭 A 账号的模型不得影响 B 账号（精确到组合）。
func TestModelOffScopedToAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "b", Domain: "www.codebuddy.cn"})
	p.SetModelOff("u1", "glm-5.3", "")

	st2, _ := p.Status("u2")
	_ = st2
	// u2 用同模型应仍可选（用 TransientRetryCandidates 之外的方式验证）
	p.mu.Lock()
	ok := p.byUID["u2"].healthyForModel(nowT(), "glm-5.3")
	p.mu.Unlock()
	if !ok {
		t.Error("关闭 u1 的模型影响了 u2（应精确到 (账号,模型)）")
	}
}

// TestModelOffUnknownAccount SetModelOff 对不存在的账号返回 false（不静默成功）。
func TestModelOffUnknownAccount(t *testing.T) {
	p := New("")
	if p.SetModelOff("nope", "m", "") {
		t.Error("不存在的账号应返回 false（静默成功会让用户以为设置生效了）")
	}
}

// pickOK 报告某模型当前是否可选（测试辅助：走与选号同一谓词 healthyForModel）。
func (p *Pool) pickOK(t *testing.T, model string) bool {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID["u1"]
	if !ok {
		return false
	}
	return e.healthyForModel(time.Now(), model)
}

func nowT() time.Time { return time.Now() }
