// v3union.go /v3/config 的「双 UA 并集」策略。
//
// 背景（20260917 实测）：同一账号、同一端点 /v3/config，**只换 User-Agent**
// 会返回**不同的模型目录**：
//
//	CN 域            global 域
//
// IDE UA   22 个（上限高）     13 个（上限高）
// CLI UA   51 个（上限低）     22 个（上限低）
// 并集     53 个               30 个
//
// 两者**互不包含**（CN 域 CLI 独有 31 个、IDE 独有 2 个），故只取其一都会丢模型。
// 症状：用户在官方客户端看到 gpt-5.6 / deepseek-v4.1-flash 等，面板却一个都没有。
//
// 策略：**模型集合取并集，maxOutputTokens 取两者较大值**。
//
// 取较大值的依据（已实测，非推测）：
//   - 上游**接受**大于 CLI 声称值的 max_tokens：对 kimi-k3-1（IDE=1048576/CLI=32000）
//     发 max_tokens=50000 得到 200 而非报错；glm-5.3-flash（IDE=131072/CLI=32000）
//     发 100000 同样接受。若上游真硬限 32K，超限应被拒而非静默接受。
//   - 实测 IDE 值**从不小于** CLI 值（CN 20 个共有模型：IDE 更大 10、相同 10、
//     CLI 更大 0），故「取较大值」等价于「IDE 值优先，CLI 补齐 IDE 没有的模型」。
//
// ⚠️ 保留不确定性声明：上游对 max_tokens 只是「接受」，未证实长输出不会在某个
// 内部阈值被截断（实测最长仅到 1815 tokens 就自然结束）。故 UI 必须保留
// 「官方声称值仅供参考」的提示，不得据此宣称"一定能输出这么多"。
package upstream

import "sort"

// unionV3 catalogs 把两路 /v3/config 目录（primary=IDE UA、secondary=CLI UA）
// 合并为并集：
//   - 模型集合 = 两路 id 的并集（互不包含，都要）；
//   - 同 id 时字段取**更优者**：maxOutputTokens / maxInputTokens 取较大值，
//     credits/name 等非数值字段 primary 优先（两路一致，仅防御性取舍）。
//
// 输出顺序 = primary 原序在前，secondary 补充项按 id 升序在后 —— 稳定输出，
// 不依赖 map 迭代序（与 mergeModelInfos 同一纪律）。
func unionV3(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}
	byID := make(map[string]int, len(primary)) // id -> primary 下标
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for _, m := range primary {
		byID[m.ID] = len(out)
		out = append(out, m)
	}
	// secondary 补充：同 id 时把「更大的上限」并进去，否则追加为新模型。
	var extra []ModelInfo
	for _, m := range secondary {
		if i, ok := byID[m.ID]; ok {
			out[i] = mergeUpperBound(out[i], m)
			continue
		}
		byID[m.ID] = -1 // 占位：避免 secondary 内部重复
		extra = append(extra, m)
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].ID < extra[j].ID })
	return append(out, extra...)
}

// mergeUpperBound 同 id 合并：数值上限取较大值，其余以 primary 为准。
//
// 为何只取 maxOutputTokens / maxInputTokens 而不整体替换：两路对同一模型的
// name/credits 等描述字段一致（实测），差异只在数值上限；整体替换会引入
// 「哪个 UA 的 credits 更权威」这种无依据的判断，故只并公认"取大更安全"的两项。
func mergeUpperBound(primary, secondary ModelInfo) ModelInfo {
	if secondary.MaxTokens > primary.MaxTokens {
		primary.MaxTokens = secondary.MaxTokens
	}
	if secondary.ContextWindow > primary.ContextWindow {
		primary.ContextWindow = secondary.ContextWindow
	}
	if secondary.MaxAllowedSize > primary.MaxAllowedSize {
		primary.MaxAllowedSize = secondary.MaxAllowedSize
	}
	return primary
}
