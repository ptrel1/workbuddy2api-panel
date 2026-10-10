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

// RunBlackcatNow 对**所有**账号执行夜猫子对话补足（窗口外跳过）。
// 禁用账号同样参与（禁用只停选号，见 disabled.go）；无 AccessToken 的跳过。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(blackcatNow()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		// 不按 st.Disabled 跳过：夜猫子属增长任务，走 growth/chat 任务通道而非选号。见 disabled.go。
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
