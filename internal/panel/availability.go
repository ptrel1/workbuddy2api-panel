// availability.go 模型可用性探测的面板接口与持久化。
//
// 定位：**手动触发**（用户点按钮）+ **持久化**（结果落盘，下次打开页面直接看）。
// 不做定时探测 —— 虽然单次成本很低（每模型 1~数 token），但"自动烧额度"应是
// 显式选择而非默认行为（对齐 cross/dev-principles.md「会产生写操作的通道，
// 开关只能放配置文件」的谨慎精神）。
//
// 与 output_probes.json（输出上限探测）**分开存**：
//   - model_avail.json   ← 本文件：能不能用（轻、快、可频繁手动跑）
//   - output_probes.json ← probe_max_tokens.py：真实上限（重、慢、烧额度）
//
// 两者语义不同（可用 ≠ 上限已测），混存会让前端无法区分"没测过"与"测了不可用"。
package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// availProbeConcurrency 探测并发上限。
// 取 2 而非更高：并发过大触发上游限流，会把"测可用性"变成"制造 429"，
// 测出的失败是自造的假象。宁可慢一点也要测得准。
const availProbeConcurrency = 2

// modelAvailability 返回已持久化的可用性探测结果（只读，无需触发）。
func (p *Panel) modelAvailability(w http.ResponseWriter, r *http.Request) {
	rep, err := p.loadAvailability()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read availability: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"exists":     rep != nil,
		"results":    availabilityResults(rep),
		"updated_at": availabilityUpdatedAt(rep),
	})
}

// probeAvailability 手动触发一次可用性探测：对指定域的全部模型发最小请求，
// 合并写入 model_avail.json（未重测的模型保留旧记录）。
//
// 查询参数：
//
// realm=cn|global  要探测哪个域（缺省 cn）
// models=a,b,c     只测指定模型（缺省：该域全部）；用于"只重测几个失败的"
func (p *Panel) probeAvailability(w http.ResponseWriter, r *http.Request) {
	realm := r.URL.Query().Get("realm")
	if realm == "" {
		realm = "cn"
	}
	acct := p.cfg.Pool.PickExcludingForRealm(nil, "", realm)
	if acct == nil {
		writeErr(w, http.StatusServiceUnavailable, "该域没有可用账号（realm="+realm+"）")
		return
	}

	// 待测模型列表：默认取该域目录全量；models 参数可指定子集。
	models := splitCSV(r.URL.Query().Get("models"))
	if len(models) == 0 {
		infos, err := p.cfg.Upstream.FetchModels(acct)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "fetch models: "+err.Error())
			return
		}
		for _, mi := range infos {
			models = append(models, realm+":"+mi.ID)
		}
	}
	if len(models) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可探测的模型")
		return
	}
	sort.Strings(models) // 稳定顺序，便于日志比对

	res, err := p.cfg.Upstream.ProbeAvailability(r.Context(), acct, models, availProbeConcurrency)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "probe: "+err.Error())
		return
	}

	// 合并旧结果后落盘：未重测的模型保留（与 probe_max_tokens.py 同语义）。
	old, _ := p.loadAvailability()
	merged := upstream.AvailabilityReport{Version: 1, UpdatedAt: time.Now(), Results: map[string]upstream.Availability{}}
	if old != nil && old.Results != nil {
		for k, v := range old.Results {
			merged.Results[k] = v
		}
	}
	for k, v := range res {
		merged.Results[k] = v
	}
	if err := p.saveAvailability(merged); err != nil {
		// 落盘失败不影响本次结果返回（用户至少能看到结果），但要显式告知
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "results": merged.Results, "realm": realm,
			"warn": "结果未能落盘：" + err.Error(),
		})
		return
	}

	okN := 0
	for _, v := range res {
		if v.OK {
			okN++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"realm":      realm,
		"tested":     len(res),
		"available":  okN,
		"results":    merged.Results,
		"updated_at": merged.UpdatedAt.Format(time.RFC3339),
	})
}

// availFilePath 可用性结果文件路径（与 ProbeFile 同目录）。
func (p *Panel) availFilePath() string {
	if p.cfg.ProbeFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.cfg.ProbeFile), "model_avail.json")
}

func (p *Panel) loadAvailability() (*upstream.AvailabilityReport, error) {
	fp := p.availFilePath()
	if fp == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 从未探测过：返回 nil 而非错误（面板显示"未测"）
		}
		return nil, err
	}
	var rep upstream.AvailabilityReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// saveAvailability 原子写入（tmp + rename）：避免崩在写一半留下损坏文件。
func (p *Panel) saveAvailability(rep upstream.AvailabilityReport) error {
	fp := p.availFilePath()
	if fp == "" {
		return nil // 未配置探测文件路径：静默跳过（与 modelProbes 的降级口径一致）
	}
	raw, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		return err
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fp)
}

func availabilityResults(rep *upstream.AvailabilityReport) map[string]upstream.Availability {
	if rep == nil || rep.Results == nil {
		return map[string]upstream.Availability{}
	}
	return rep.Results
}

func availabilityUpdatedAt(rep *upstream.AvailabilityReport) string {
	if rep == nil || rep.UpdatedAt.IsZero() {
		return ""
	}
	return rep.UpdatedAt.Format(time.RFC3339)
}

// splitCSV 解析逗号分隔参数并去空白（空串返回 nil）。
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
