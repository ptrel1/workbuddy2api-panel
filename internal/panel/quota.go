// quota.go 额度评估：从**实测用量**反推各套餐的实际能力上界。
//
// ⚠️ 定位声明（必须在 UI 上原样透出，不得省略）：
//
//	本模块产出的是「**基于实测推断的经验值**」，**不是上游公布的额度**。
//	依据是"该账号在观测窗口内单日跑到的最大用量" —— 它是**已证明能达到的下界**，
//	而非"上限"：账号可能只是没被打满。样本量小（每套餐 1~2 个账号）。
//
// 为何仍值得做（实用主义）：
//  1. 它能回答运维最实际的排期问题：「这个号大概能扛多少」；
//  2. 套餐间的**相对倍数**比绝对值更可信（同窗口、同口径对比）；
//  3. 撞限事件本身给出了**上界的反证**（撞了说明确实到顶了）。
//
// 设计纪律（对齐 cross/dev-principles.md）：
//   - §十 不得产出无依据的值：无数据的套餐显示"—"，**不猜**；
//   - §十一 口径必须标注：每项都带"样本天数 / 观测窗口"，避免被当成官方额度；
//   - §十二 控时间变量：只统计**同一观测窗口**内的数据，跨窗口不可比。
package panel

import (
	"sort"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// QuotaEstimate 单个账号的额度评估。
type QuotaEstimate struct {
	UID   string `json:"uid"`
	Plan  string `json:"plan"`  // 套餐类型（pool.PlanKind）
	Label string `json:"label"` // 中文展示名
	// PeakTokens 观测窗口内**单日最大** prompt+completion tokens。
	// 这是"已证明能达到"的量，故是**下界**（未打满则偏低），绝非上限。
	PeakTokens int64 `json:"peak_tokens"`
	// PeakReqs 同一日的请求数（辅助口径：有的号量大次少，有的相反）。
	PeakReqs int    `json:"peak_reqs"`
	PeakDay  string `json:"peak_day"`
	// HitDays 观测到"疑似撞限"的天数（当日有错误且成功数明显被压制）。
	HitDays int `json:"hit_days"`
	// ObservedDays 参与统计的天数（口径标注用 —— 让读者知道样本多大）。
	ObservedDays int `json:"observed_days"`
	// Limited 当前是否处于限流中（有 rate_limited_models）。
	Limited bool `json:"limited"`
	// RecoverAt 当前限流的恢复时刻（上游权威墙钟）。
	RecoverAt string `json:"recover_at,omitempty"`
}

// QuotaTier 按套餐聚合的评估（跨账号）。
type QuotaTier struct {
	Plan  string `json:"plan"`
	Label string `json:"label"`
	// Accounts 该套餐的账号数（**样本量**，必须展示 —— 少样本结论不可靠）。
	Accounts int `json:"accounts"`
	// PeakTokens 该套餐内**最好**账号的峰值（即"该套餐能达到的水平"）。
	PeakTokens int64 `json:"peak_tokens"`
	PeakReqs   int   `json:"peak_reqs"`
	// MedianTokens 该套餐各账号峰值的**中位数**（比最大值稳健；样本≥2 才有意义）。
	MedianTokens int64 `json:"median_tokens,omitempty"`
}

// QuotaReport 额度评估报告。
type QuotaReport struct {
	Tiers []QuotaTier     `json:"tiers"`
	Rows  []QuotaEstimate `json:"rows"`
	// Window 观测窗口（最早~最晚日期），§十一 口径标注。
	Window string `json:"window"`
	// Caveat 免责说明（UI 原样显示，不得删改）——这是本模块诚实性的载体。
	Caveat string `json:"caveat"`
}

// quotaCaveat 固定的口径说明。**写在服务端**而非前端，保证不被前端悄悄删改。
const quotaCaveat = "基于实测用量推断的经验值，非上游公布的额度。" +
	"「峰值」是观测窗口内单日跑到的最大用量，属**已证明能达到的下界**（账号可能未打满）；" +
	"套餐间**相对倍数**比绝对值更可信。样本量见各行「观测天数」。"

// BuildQuotaReport 由用量桶 + 套餐分类构建评估报告。
//
// plans: 短 uid → 套餐 kind（来自 pool.ClassifyPlan）。
// limited: 短 uid → 恢复时刻（来自 rate_limited_models.reset_at，可为空）。
func BuildQuotaReport(days []usage.Daily, plans map[string]string, labels map[string]string, limited map[string]string) QuotaReport {
	//  1. 每账号取「最好的一天」+ 统计观测天数。
	//     只纳入 Reqs >= minSampleReqs 的天：低于此的多半是"服务刚启用/账号刚加"
	//     的残天，会把峰值拉低造成误判。
	best := map[string]*QuotaEstimate{}
	obs := map[string]int{}
	var minDay, maxDay string
	for _, d := range days {
		if d.Reqs < minSampleReqs {
			continue
		}
		uid := shortUID(d.UID)
		obs[uid]++
		if minDay == "" || d.Day < minDay {
			minDay = d.Day
		}
		if maxDay == "" || d.Day > maxDay {
			maxDay = d.Day
		}
		cur, ok := best[uid]
		if !ok || d.Tokens > cur.PeakTokens {
			best[uid] = &QuotaEstimate{
				UID: uid, PeakTokens: d.Tokens, PeakReqs: int(d.Reqs), PeakDay: d.Day,
			}
		}
	}

	rows := make([]QuotaEstimate, 0, len(best))
	for uid, e := range best {
		e.Plan = plans[uid]
		if e.Plan == "" {
			e.Plan = "unknown"
		}
		e.Label = labels[e.Plan]
		if e.Label == "" {
			e.Label = "未分类"
		}
		e.ObservedDays = obs[uid]
		if at, ok := limited[uid]; ok && at != "" {
			e.Limited = true
			e.RecoverAt = at
		}
		rows = append(rows, *e)
	}
	// 稳定排序：套餐优先级 → 峰值降序 → uid（视觉权重与重要性一致）
	sort.SliceStable(rows, func(i, j int) bool {
		if planRank(rows[i].Plan) != planRank(rows[j].Plan) {
			return planRank(rows[i].Plan) < planRank(rows[j].Plan)
		}
		if rows[i].PeakTokens != rows[j].PeakTokens {
			return rows[i].PeakTokens > rows[j].PeakTokens
		}
		return rows[i].UID < rows[j].UID
	})

	// 2) 按套餐聚合。样本 <2 时不算中位数（单个值当"中位数"是伪统计）。
	byTier := map[string][]QuotaEstimate{}
	for _, r := range rows {
		byTier[r.Plan] = append(byTier[r.Plan], r)
	}
	tiers := make([]QuotaTier, 0, len(byTier))
	for plan, rs := range byTier {
		t := QuotaTier{Plan: plan, Label: rs[0].Label, Accounts: len(rs)}
		toks := make([]int64, 0, len(rs))
		for _, r := range rs {
			if r.PeakTokens > t.PeakTokens {
				t.PeakTokens, t.PeakReqs = r.PeakTokens, r.PeakReqs
			}
			toks = append(toks, r.PeakTokens)
		}
		sort.Slice(toks, func(i, j int) bool { return toks[i] < toks[j] })
		if len(toks) >= 2 {
			n := len(toks)
			if n%2 == 1 {
				t.MedianTokens = toks[n/2]
			} else {
				t.MedianTokens = (toks[n/2-1] + toks[n/2]) / 2
			}
		}
		tiers = append(tiers, t)
	}
	sort.SliceStable(tiers, func(i, j int) bool { return planRank(tiers[i].Plan) < planRank(tiers[j].Plan) })

	win := "—"
	if minDay != "" {
		win = minDay + " ~ " + maxDay
	}
	return QuotaReport{Tiers: tiers, Rows: rows, Window: win, Caveat: quotaCaveat}
}

// minSampleReqs 单日最少成功请求数才纳入统计。
// 取 20：低于此的天多半是"服务刚启用/账号刚加"的残天，会把峰值拉低造成误判。
const minSampleReqs = 20

// planRank 套餐在 UI 中的排序权重（标准版在前 —— 它是"最该指望的号"）。
func planRank(p string) int {
	switch p {
	case "standard":
		return 0
	case "trial":
		return 1
	case "activity":
		return 2
	case "free":
		return 3
	default:
		return 4
	}
}

// shortUID 取 uid 前 8 位（面板统一口径：既够区分又不必展示全量）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
