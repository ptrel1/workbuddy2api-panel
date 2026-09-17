// timing.go 上游调用的分段耗时观测（纯观测，不改变任何控制流）。
//
// 背景：面板日志长期只有单一 TTFB 字段，语义为「首个 SSE data 帧到达 − 请求进
// handler」，把选号、建连、请求体准备、上游首字节、SSE 解析全部揉进一个数里。
// 实测复盘时该口径不可归因：curl 量到 time_starttransfer 0.88~1.29s，而面板同
// 批请求记 1668~3050ms，差异无法解释（见 .claude/reports/transport-hardening.md
// 与本次排查）。根因不是数值错误，而是**语义未标注**——值对，但拿它当「上游
// 首字节」用就会误判。
//
// 本文件只做一件事：把 ChatStreamContext 内部可归因的几段显式计时并返回，
// 供 handler 写入日志。**零行为变更**——不参与任何判定，不触发任何重试/熔断。
//
// 设计纪律（对齐 cross/dev-principles.md）：
//   - §十「留白不得产出物理上不可能的值」：各分段必须满足 sum ≤ total，
//     且任一分段不得为负。Prepare/Do 的计时点用单调时钟（time.Since），
//     不依赖 wall clock，时钟回拨不会产生负值。
//   - 所有字段零值 = 「未测量」（而非「0ms」），由调用方区分展示。
package upstream

import "time"

// ChatTiming 单次上游聊天调用的分段耗时。
// 各字段零值表示该段未测量（如未发生 fallback 重试、请求未发出即失败）。
type ChatTiming struct {
// Prepare 本地请求体准备耗时（prepareBody：effort 降级 / 指纹脱敏 /
// cache key 注入）。纯 CPU，不含 IO；异常大值提示 body 过大或锁竞争。
Prepare time.Duration
// Do 从调用 http.Client.Do 到响应头 + 首字节到达的耗时。三段合一：
//   连接获取（池内复用 ≈0，新建则含 TCP+TLS 握手）+ 请求写出 + 上游首字节。
// 说明：Go 的 http.Transport 不暴露「建连 vs 首字节」的分离计时点，
// 强行拆需自定义 RoundTripper 包装并牺牲连接池复用语义，故此处合并观测；
// 若需进一步归因，用独立实验（curl time_connect）而非生产路径改动。
Do time.Duration
// Attempts 实际发出的上游请求次数（>1 表示 global 路径 404/405 发生了
// fallback 重试）。用于识别「重试导致的慢」而非「单次慢」。
Attempts int
}

// Add 累加另一次尝试的耗时（fallback 重试路径用），并递增 Attempts。
// 仅累加已测量的分段：零值段（未测量）不参与累加，避免把「未测量」摊成「0ms」
// 而虚高 Attempts 对应的分母。
func (t *ChatTiming) Add(o ChatTiming) {
if o.Prepare > 0 || o.Do > 0 {
t.Prepare += o.Prepare
t.Do += o.Do
}
if o.Attempts > 0 {
t.Attempts += o.Attempts
}
}

// Total 返回本结构已测量的分段合计。恒 ≥0；用于自证 sum ≤ 外层 total。
// 不变量（可由测试断言）：Total() <= handler 侧 total，差值即 handler 自身开销
// （选号 / 租约 / 日志 / SSE 解析），是分段观测的正确性判据。
func (t ChatTiming) Total() time.Duration {
return t.Prepare + t.Do
}
