package auth

import "testing"

// IsEnterprise 判定：enterpriseId 非空白即企业版。
//
// 背景：企业版账号**没有个人成长体系**，上游对签到/成长端点一律 400 code 10001
// 或 403「growth system is only available for personal users」。不门控就会每日
// 空发注定失败的请求。
func TestIsEnterprise(t *testing.T) {
	cases := []struct {
		name string
		eid  string
		want bool
	}{
		{"普通企业号", "ent-12345", true},
		{"纯空白视为无归属", "   ", false},
		{"空串不是企业版", "", false},
		{"制表/换行也是空白", "\t\n", false},
		{"个人号（无字段）", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{EnterpriseID: c.eid}
			if got := a.IsEnterprise(); got != c.want {
				t.Errorf("EnterpriseID=%q → IsEnterprise()=%v, want %v", c.eid, got, c.want)
			}
		})
	}
}

// TestIsEnterpriseNilSafe nil 接收者不得 panic（防御性：调用方可能拿到 nil auth）。
func TestIsEnterpriseNilSafe(t *testing.T) {
	var a *Auth
	if a.IsEnterprise() {
		t.Error("nil auth 不该是企业版")
	}
}

// TestIsEnterpriseOrthogonalToGlobal 企业版与 global 是**正交**维度：
// 企业版是账号类型（有无个人成长体系），realm 是路由域。四种组合都要能表达。
func TestIsEnterpriseOrthogonalToGlobal(t *testing.T) {
	// cn + 企业
	cnEnt := &Auth{EnterpriseID: "e1", Domain: "www.workbuddy.cn"}
	// cn + 个人
	cnPer := &Auth{Domain: "www.workbuddy.cn"}
	if !cnEnt.IsEnterprise() || cnEnt.IsGlobal() {
		t.Error("cn 企业号：IsEnterprise 应为真、IsGlobal 应为假")
	}
	if cnPer.IsEnterprise() {
		t.Error("cn 个人号：IsEnterprise 应为假")
	}
}
