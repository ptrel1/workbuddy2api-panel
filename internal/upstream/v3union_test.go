package upstream

import (
	"testing"
)

// mkModel 构造测试用 ModelInfo（只填并集逻辑关心的字段）。
func mkModel(id string, maxOut, maxIn int64) ModelInfo {
	return ModelInfo{ID: id, Name: id, MaxTokens: maxOut, ContextWindow: maxIn}
}

// TestUnionV3TakesUnionOfIDs 模型集合必须是两路并集：
// IDE 与 CLI 目录互不包含（CN 实测 22 vs 51，CLI 独有 31 个），
// 只取其一都会丢模型 —— 这正是用户报「面板看不到官方那些模型」的根因。
func TestUnionV3TakesUnionOfIDs(t *testing.T) {
	ide := []ModelInfo{mkModel("a", 100, 1000), mkModel("b", 200, 2000)}
	cli := []ModelInfo{mkModel("b", 50, 500), mkModel("c", 300, 3000), mkModel("d", 400, 4000)}

	out := unionV3(ide, cli)
	ids := map[string]bool{}
	for _, m := range out {
		ids[m.ID] = true
	}
	for _, want := range []string{"a", "b", "c", "d"} {
		if !ids[want] {
			t.Errorf("并集缺少模型 %q（实际 %v）", want, ids)
		}
	}
	if len(out) != 4 {
		t.Errorf("并集大小 = %d, want 4（不得重复计入 b）", len(out))
	}
}

// TestUnionV3TakesLargerBound 同 id 时上限取**较大值**：
// 实测 IDE 值从不小于 CLI 值，且上游接受大于 CLI 声称值的 max_tokens
// （kimi-k3-1 发 50000 得 200 而非报错），故取大值是安全侧。
func TestUnionV3TakesLargerBound(t *testing.T) {
	ide := []ModelInfo{mkModel("m", 393216, 1000000)}
	cli := []ModelInfo{mkModel("m", 128000, 200000)}

	out := unionV3(ide, cli)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].MaxTokens != 393216 {
		t.Errorf("MaxTokens = %d, want 393216（取较大值）", out[0].MaxTokens)
	}
	if out[0].ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d, want 1000000（取较大值）", out[0].ContextWindow)
	}
}

// TestUnionV3KeepsLargerWhenCLIBigger 反向保护：万一 CLI 值更大也要取 CLI 的
// （不能写成"无条件用 IDE 值"——那会在上游改版后静默丢上限）。
func TestUnionV3KeepsLargerWhenCLIBigger(t *testing.T) {
	ide := []ModelInfo{mkModel("m", 1000, 100)}
	cli := []ModelInfo{mkModel("m", 9000, 900)}

	out := unionV3(ide, cli)
	if out[0].MaxTokens != 9000 {
		t.Errorf("MaxTokens = %d, want 9000（CLI 更大时应取 CLI 值）", out[0].MaxTokens)
	}
	if out[0].ContextWindow != 900 {
		t.Errorf("ContextWindow = %d, want 900", out[0].ContextWindow)
	}
}

// TestUnionV3StableOrder 输出顺序必须稳定：primary 原序在前，新增项按 id 升序。
// 不稳定会让前端每次刷新看到不同顺序（同一数据不同展示 = 困惑源）。
func TestUnionV3StableOrder(t *testing.T) {
	ide := []ModelInfo{mkModel("z", 1, 1), mkModel("a", 1, 1)}
	cli := []ModelInfo{mkModel("m", 1, 1), mkModel("b", 1, 1), mkModel("y", 1, 1)}

	want := []string{"z", "a", "b", "m", "y"}
	for i := 0; i < 20; i++ { // 重复跑，防 map 迭代序污染
		out := unionV3(ide, cli)
		for j, id := range want {
			if out[j].ID != id {
				t.Fatalf("第 %d 次: out[%d] = %q, want %q（顺序必须稳定）", i, j, out[j].ID, id)
			}
		}
	}
}

// TestUnionV3EmptySecondary 只有一路时的退化行为（CLI 探测失败场景）。
func TestUnionV3EmptySecondary(t *testing.T) {
	ide := []ModelInfo{mkModel("a", 1, 1)}
	out := unionV3(ide, nil)
	if len(out) != 1 || out[0].ID != "a" {
		t.Errorf("secondary 为空时应原样返回 primary，得 %+v", out)
	}
}
