// plan.go 账号套餐分类：从上游积分包名判定「免费版 / 活动号 / 标准版」。
//
// 为何要分类（用户提「能否判断是不是免费账号给标记出来」）：
// 账号的"来头"决定了它的**稳定性与可持续性** ——
//   - 免费版：额度最小，最容易撞 6004（实测 ptrel1 = Free Plan，余额仅 30/410）
//   - 活动号：积分全靠赠送包（拉新/裂变/体验），**额度用完就没了**
//   - 标准版：有正式订阅，最稳定
// 面板标出来，运维一眼知道"该指望哪个号"。
//
// ⚠️ 判定依据是**上游下发的套餐名文本**，故：
//   1. 用**关键词宽松匹配**，不写死完整名（上游随时可能改文案）；
//   2. 同时保留**原始套餐名**透出（不吞信息），匹配不上时用户仍能看到实情。
package pool

import (
"strings"
)

// PlanKind 账号套餐类型。
type PlanKind string

const (
PlanFree      PlanKind = "free"      // 免费版（Free Plan）
PlanTrial     PlanKind = "trial"     // 体验版 / 试用
PlanActivity  PlanKind = "activity"  // 积分全部来自活动赠送包
PlanStandard  PlanKind = "standard"  // 有正式订阅（标准版/专业版等）
PlanUnknown   PlanKind = "unknown"   // 无包信息 / 无法判定
)

// planKeywords 关键词表（宽松匹配，上游改文案时只需改这里）。
//
// 顺序即优先级：先判"最强信号"（Free/标准版），再判活动包 ——
// 因为一个账号可能**同时**有标准版和活动包（实测汪羽杰 = 标准版 + 62 个活动包），
// 那时应判为标准版（订阅才是它的真实身份）。
var planStrong = []struct {
kind PlanKind
keys []string
}{
// 免费版：最明确的信号
{PlanFree, []string{"free plan", "免费版", "free tier", "免费套餐"}},
// 试用/体验
{PlanTrial, []string{"trial", "体验版", "试用", "体验套餐"}},
// 正式订阅（付费）：标准版/专业版/团队版等
{PlanStandard, []string{"标准版", "专业版", "团队版", "企业版", "standard", "pro plan", "premium", "plus", "订阅"}},
}

// planActivityKeys 活动赠送包关键词：这些包名共同点是"赠送/权益/裂变/拉新"，
// 代表积分是**活动得来**而非订阅额度。全为活动包 → 判为活动号。
var planActivityKeys = []string{
"拉新", "裂变", "权益包", "赠送", "活动", "bonus pack", "promo", "reward",
"邀请", "新人",
}

// ClassifyPlan 由套餐名列表判定账号类型，并返回命中的证据（供面板展示）。
//
// packages 为空 = 无数据（查询失败或真的没有包）→ PlanUnknown，
// **不猜**（§十：不得产出无依据的值）。
func ClassifyPlan(packages []string) (PlanKind, string) {
if len(packages) == 0 {
return PlanUnknown, ""
}
low := make([]string, 0, len(packages))
for _, p := range packages {
low = append(low, strings.ToLower(p))
}
// 第一轮：强信号（免费/试用/标准版）—— 按优先级顺序
for _, g := range planStrong {
for _, l := range low {
for _, k := range g.keys {
if strings.Contains(l, strings.ToLower(k)) {
return g.kind, k
}
}
}
}
// 第二轮：全部是活动包 → 活动号
allActivity := true
for _, l := range low {
hit := false
for _, k := range planActivityKeys {
if strings.Contains(l, strings.ToLower(k)) {
hit = true
break
}
}
if !hit {
allActivity = false
break
}
}
if allActivity {
return PlanActivity, "全部为活动/赠送包"
}
return PlanUnknown, ""
}
