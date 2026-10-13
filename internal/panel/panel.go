// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/balance"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
	"strings"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage   *usage.Recorder
	Balance *balance.Recorder // 余额趋势采样记录器（nil = 该视图返回 501）

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// RequestLog 请求级指标与脱敏归档记录器（reqlog 包；nil = 该视图返回 501）。
	RequestLog *reqlog.Recorder

	// ModelFallback 返回「模型降级路由表」（pool.model_fallback 的当前值），
	// 供面板在限流提示里标注「已路由至 <等价模型>」——让运维一眼看出
	// "这个模型虽然限流了，但请求已被自动改道，服务没断"。
	//
	// 为什么用回调而非直接传 map：配置**可热更**（面板改完即生效），回调每次读
	// 最新值，避免面板持有启动时的旧快照。nil = 未启用降级（面板不显示该标注）。
	ModelFallback func() map[string]string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	// Logo 候选对比页（只读展示，无需鉴权：页面不含账号/密钥）
	p.mux.HandleFunc("GET /panel/logo", p.logoPreview)
	p.mux.HandleFunc("GET /panel/logo.js", p.logoPreviewJS)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/errors", p.withAuth(p.errorsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/status", p.withAuth(p.schoolStatus))
	p.mux.HandleFunc("POST /panel/api/school/run_all", p.withAuth(p.schoolRunAll))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	// 请求级指标与脱敏归档（reqlog）。
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))

	// 余额趋势：窗口由 days 参数控制（默认 30 天，上限 30 天=detailedKeep）。
	p.mux.HandleFunc("GET /panel/api/balance_history", p.withAuth(p.balanceHistory))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	// 模型可用性探测（手动触发 + 结果持久化，见 availability.go）
	p.mux.HandleFunc("GET /panel/api/model_availability", p.withAuth(p.modelAvailability))
	p.mux.HandleFunc("GET /panel/api/plans", p.withAuth(p.plans))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/models/off", p.withAuth(p.modelsOff))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/models/toggle", p.withAuth(p.modelToggle))
	p.mux.HandleFunc("GET /panel/api/quota", p.withAuth(p.quota))
	p.mux.HandleFunc("GET /panel/api/usage_series", p.withAuth(p.usageSeries))
	p.mux.HandleFunc("POST /panel/api/model_availability/probe", p.withAuth(p.probeAvailability))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// plans 返回各账号的套餐分类（供账号池标记「免费版/活动号/标准版」）。
//
// 为何独立接口而非并入 overview：套餐信息要**逐个账号向上游查询**（billing 接口），
// 单账号 ~300ms；并入 overview 会让主页面（每 5 秒轮询）变慢。
// 故拆成独立接口，前端**按需调用一次**（不轮询），并在面板上显示"查询于 XX"。
//
// 查询失败/无数据的账号返回 unknown —— **不猜**（§十）。
// quota 额度评估：由实测用量 + 套餐分类反推「各套餐能扛多少」。
//
// ⚠️ 产出是**实测推断值**，非上游官方额度 —— 口径说明随响应一起返回
// （quota.Caveat），前端必须原样展示（见 quota.go 的定位声明）。
// usageSeries 「账号 × 模型」用量时序（供折线图）。
//
// 返回全部有点的组合，每条带 Enough 标志（点数是否够画折线）——
// 实测 30 个组合中仅 3 个 ≥7 点，其余画不成线；前端据此**降级为数字**，
// 避免产出"看起来像故障"的单点折线。
func (p *Panel) usageSeries(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	series := p.cfg.Usage.AccountModelSeries()
	// 昵称仅用于展示，取自池快照（不含任何凭证）——与 usage 端点同口径。
	// 为什么在 panel 层拼而非改 usage.Series：昵称属**池状态**（会变、来源是 auth），
	// 而 usage 包只管用量聚合；把池状态混进聚合结构会让该包反向依赖账号池。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	// 匿名嵌入 usage.Series：JSON 输出是原有字段 + name，前端零破坏性改动。
	// name 为空时前端自行回退到 uid 短哈希（见 app.js renderAms）。
	type seriesRow struct {
		usage.Series
		Name string `json:"name,omitempty"`
	}
	rows := make([]seriesRow, 0, len(series))
	enough := 0
	for _, s := range series {
		if s.Enough {
			enough++
		}
		rows = append(rows, seriesRow{Series: s, Name: nicks[s.UID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "series": rows,
		"total": len(rows), "plottable": enough,
	})
}

func (p *Panel) quota(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	// 套餐分类：查上游 billing（与 /panel/api/plans 同一来源）。
	// 为何不缓存到池状态：套餐名会变（新活动包随时发放），且本接口不是高频调用；
	// 多打一次上游换取"始终最新"是划算的（单账号 ~300ms，并发 3）。
	plans := map[string]string{}
	labels := map[string]string{
		"standard": "标准版", "trial": "体验版", "activity": "活动号",
		"free": "免费版", "unknown": "未分类",
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for _, st := range p.cfg.Pool.List() {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			packs, _, _, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				return // 查不到 → 保持 unknown（不猜）
			}
			names := make([]string, 0, len(packs))
			for _, pk := range packs {
				names = append(names, pk.Name)
			}
			kind, _ := pool.ClassifyPlan(names)
			short := uid
			if len(short) > 8 {
				short = short[:8]
			}
			mu.Lock()
			plans[short] = string(kind)
			mu.Unlock()
		}(st.UID)
	}
	wg.Wait()

	// 当前限流状态：取自池快照（已是内存数据，无需上游调用）
	limited := map[string]string{}
	for _, st := range p.cfg.Pool.List() {
		if len(st.RateLimitedModels) == 0 {
			continue
		}
		short := st.UID
		if len(short) > 8 {
			short = short[:8]
		}
		at := st.RateLimitedModels[0].ResetAt
		if at.IsZero() {
			at = st.RateLimitedModels[0].Until
		}
		if !at.IsZero() {
			limited[short] = at.Format("01-02 15:04")
		}
	}
	rep := BuildQuotaReport(p.cfg.Usage.DailyUsage(), plans, labels, limited)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

// modelToggle 手动开关某 (账号, 模型) 组合。
//
// 请求：POST /panel/api/accounts/{uid}/models/toggle
//
// {"model": "glm-5.3", "off": true, "note": "质量差"}
//
// 语义（用户选定）：**永久 + 精确到组合 + 手动优先**。
//   - 关闭后该账号不再被选中执行该模型（healthyForModel 拒绝）；
//   - 自动机制（6004/11102 到期）**不能**把它打开；
//   - 仅手动 `off:false` 才恢复。
//
// model 用**裸名**：前缀（cn:/global:）是网关侧路由协议，同账号域固定，
// 带前缀存会在模型改名/换域时对不上。
// modelsOff 返回某账号**手动关闭**的模型清单（供开关抽屉渲染初始状态）。
func (p *Panel) modelsOff(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	list := p.cfg.Pool.ModelOffOf(uid)
	if list == nil {
		list = []pool.ModelOffEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "models": list})
}

func (p *Panel) modelToggle(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req struct {
		Model string `json:"model"`
		Off   bool   `json:"off"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	// 剥 realm 前缀（与出站口径一致：上游只认裸名）
	if i := strings.Index(model, ":"); i >= 0 {
		model = model[i+1:]
	}
	if req.Off {
		if !p.cfg.Pool.SetModelOff(uid, model, req.Note) {
			writeErr(w, http.StatusBadRequest, "set model off failed")
			return
		}
	} else {
		p.cfg.Pool.SetModelOn(uid, model)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "model": model, "off": req.Off,
		"list": p.cfg.Pool.ModelOffOf(uid),
	})
}

func (p *Panel) plans(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	out := make(map[string]any, len(accts))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 3) // 并发上限：避免瞬时打满上游限流
	for _, st := range accts {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			names := make([]string, 0, len(packs))
			for _, pk := range packs {
				names = append(names, pk.Name)
			}
			kind, evidence := pool.ClassifyPlan(names)
			rec := map[string]any{
				"kind":     string(kind),
				"evidence": evidence,
				"packages": names,
				"remain":   remain,
				"size":     size,
			}
			if err != nil {
				rec["error"] = err.Error()
			}
			mu.Lock()
			out[uid] = rec
			mu.Unlock()
		}(st.UID)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"plans":      out,
		"updated_at": time.Now().Format("15:04:05"),
	})
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"accounts":        p.cfg.Pool.List(),
		// 模型降级路由表：前端据此在限流提示里标注「已路由至 <等价模型>」。
		// 空/未配置 = 不显示该标注（老行为）。
		"model_fallback": p.modelFallback(),
	})
}

// modelFallback 返回降级路由表的当前快照（未注入回调时返回空 map，前端自然不显示）。
func (p *Panel) modelFallback() map[string]string {
	if p.cfg.ModelFallback == nil {
		return map[string]string{}
	}
	if m := p.cfg.ModelFallback(); m != nil {
		return m
	}
	return map[string]string{}
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// errorsHandler 错误聚合：按「账号 × 错误类型」统计 WARN/ERR 行。
//
// 与 /panel/api/logs（原始行）的分工：日志页适合"逐行追因"，
// 这里适合"一眼看谁有问题"。两者并存而非替代 ——
// 汇总回答"是不是有问题"，明细回答"具体怎么了"（见 errors.go 的设计说明）。
func (p *Panel) errorsHandler(w http.ResponseWriter, r *http.Request) {
	entries := p.logs.Snapshot()
	// 传结构化条目（带 TS）——Ring 的 Text 已剥时间戳，只有 TS 才是真实时刻；
	// 首版只传 Text，「最近」列恒空（实测被用户发现）。
	lines := make([]LogLine, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, LogLine{Text: e.Text, TS: e.TS})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"stats":      AggregateErrors(lines),
		"scanned":    len(lines),
		"updated_at": errorsUpdatedAt(),
	})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 无可用账号 503（先添加账号）；上游失败 502。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	// realm 选择按哪个域取模型目录，缺省 cn（与历史行为一致）。
	//
	// 修（20260917）：此前用 Pool.Pick() 取随机一个账号查目录，该账号可能属 cn
	// 也可能属 global，而两域的上游 base 与模型目录都不同，导致同一页面刷新两次
	// 可能看到不同列表（同一输入不同输出，属不确定性缺陷）。现按指定域取号。
	// 见 cross/dev-principles.md 第十四节。
	realm := r.URL.Query().Get("realm")
	if realm == "" {
		realm = "cn"
	}
	acct := p.cfg.Pool.PickExcludingForRealm(nil, "", realm)
	if acct == nil {
		writeErr(w, http.StatusServiceUnavailable, "该域没有可用账号（realm="+realm+"）：请先添加账号或切换域")
		return
	}
	infos, err := p.cfg.Upstream.FetchModels(acct)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch models: "+err.Error())
		return
	}
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		entry := map[string]any{
			"id":                   mi.ID,
			"name":                 mi.Name,
			"default_effort":       mi.DefaultEffort,
			"supported_efforts":    mi.Efforts,
			"can_disable_thinking": mi.CanDisableThinking,
			"supports_reasoning":   mi.SupportsReasoning,
			"supports_images":      mi.SupportsImages,
			"credits":              mi.Credits,
			"description":          mi.Description,
			"tags":                 mi.Tags,
			"vendor":               mi.Vendor,
			"is_default":           mi.IsDefault,
			"supports_tool_call":   mi.SupportsToolCall,
			"only_reasoning":       mi.OnlyReasoning,
			"reasoning_effort":     mi.ReasoningEffort,
			"reasoning_summary":    mi.ReasoningSummary,
		}
		if mi.MaxAllowedSize > 0 {
			entry["max_allowed_size"] = mi.MaxAllowedSize
		}
		// 与 /v1/models 同口径：context_length / max_output_tokens 走四级查找链
		// （上游动态值 → 静态知识表 → model.json → models.dev → 1M 兜底），
		// effort 档位走 EffortListing（远端权威 ∪ CN 静态兜底表）——面板展示的
		// 数值即客户端实际拿到的数值，两侧不再漂移。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, p.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, p.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 按账号**实际**域取 effort 档位（此前写死 "cn"，global 模型会误用 cn 表）。
		if efforts, def := upstream.EffortListing(realm, mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["supported_efforts"] = efforts
			if def != "" {
				entry["default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// 回显 realm：前端据此标注"当前看的是哪个域"，避免把某个域的数字误当全局（§十四）。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "realm": realm, "models": out})
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新 / 旅行 / 开学季 / 连登（唯夜猫子例外——它发真实模型对话）。
//
// 与 disable 的区别（面板两个按钮分开正是为了让这个区别可见）：不写 reason、
// 不清冷却域、不重置计数 —— 账号是「临时让位」而非「判死」，点「恢复选号」即可
// 立刻回到池子，无需重登或解冻。面板标签据此显示「已暂停选号」而非「需重新登录」。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（恢复参与选号）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
	}
	resp := map[string]any{"ok": true}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：UserResource → SetCredits（不触碰冷却状态）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCredits(uid, remain, total)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCheckinNow()
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunActivityNow()
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunKeepaliveNow()
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。hours 查询参数控制小时粒度时序窗口（默认 72，
// 上限 1440=60 天）；更早的数据自动折叠为日点，因此长期趋势不会丢。
// requestMetrics 返回进程内请求指标（计数/成功率/最近 100 条）+ 归档存储状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持 outcome/account/model 三个筛选项（对应 reqlog.Filter）。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome: r.URL.Query().Get("outcome"),
		Account: r.URL.Query().Get("account"),
		Model:   r.URL.Query().Get("model"),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, nicks))
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// balanceHistory 返回余额采样序列，供「余额趋势」视图画折线。
// days 查询参数控制窗口（默认 30，上限 30 = detailedKeep；更早的数据已折叠为日粒度，
// 传更大值也只能拿到日粒度点）。
func (p *Panel) balanceHistory(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Balance == nil {
		writeErr(w, http.StatusNotImplemented, "balance recorder not available")
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 30 {
		days = 30
	}
	since := time.Now().AddDate(0, 0, -days)
	// 昵称仅用于展示，取自池快照（不含任何凭证）；实时值优先，历史值兜底。
	// 昵称与域仅用于展示，取自池快照（不含任何凭证）；实时值优先，历史值兜底。
	// realm 必须在 Snapshot 前注入：cn 与 global 的积分不同源、不可互换，
	// 跨域求和语义不成立，前端需按域分组渲染。
	for _, st := range p.cfg.Pool.List() {
		if st.Nickname != "" {
			p.cfg.Balance.SetName(st.UID, st.Nickname)
		}
		if st.Realm != "" {
			p.cfg.Balance.SetRealm(st.UID, st.Realm)
		}
	}
	series, now := p.cfg.Balance.Snapshot(since)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"now":    now.Format(time.RFC3339),
		"days":   days,
		"series": series,
	})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
