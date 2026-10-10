// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// blackcatNow 供 InNightWindow 判定用的当前时刻，测试可替换以固定窗口。
// 生产路径恒为 time.Now（零行为变化）；与 travelAccountDelay 同款测试接缝，
// 避免夜猫子这条放行路径只能在 23:00–08:00 才测得到。
var blackcatNow = time.Now

// RunBlackcatNow 对池内账号执行夜猫子对话补足（窗口外跳过）。
//
// ⚠️ **本排程是「禁用/暂停量只停选号」这条通则的唯一例外**：
// paused（暂停选号）与 disabled 账号**都跳过**。理由见 disabled.go 第五节 ——
// RunNightChats 逐条 ChatStream 发**真实 glm-5.2 对话**，是全任务体系中唯一
// 「整任务都是真实模型对话」的，与「让位防风控」的意图正面冲突：
// 让位的号若在 23:00 又被拉出来发消息，等于没让位。
// （对照：其余 7 类排程走 billing/report/growth 的 RPC 通道，不含模型对话，照常参与。）
//
// 凭证检查与 IsGlobal() 门控保留不动：它们是「不发无效请求」的天然防线。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(blackcatNow()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		// paused/disabled 都跳过：夜猫子发真实模型对话，与「让位」语义冲突（见 disabled.go 第五节）。
		if st.Disabled || st.Paused {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", a.UID, err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", a.UID, ok, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", a.UID, ok)
		time.Sleep(activityAccountDelay)
	}
}
