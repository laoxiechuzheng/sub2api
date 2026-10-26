# Composite 路由备用模型

最近核实：2026-10-27（本地 unit 验证；Linux race 由 CI 执行）。

## 配置

Composite 分组的每条显式路由可配置最多 8 个 `fallback_models`（迁移 `247_composite_route_fallback_models.sql`，JSONB 数组）。每项：

- `upstream_model`：备用上游模型，必填，≤200 字节。
- `target_platform`：留空表示沿用主目标平台（之后修改主平台时跟随）；填写则固定为该具体平台（不能是 `composite`），即使与当前主平台相同也原样保存，不会因以后修改主平台被连带改变。

保存时去空白，按"有效平台 + 模型"去重，并丢弃与主目标（同平台同模型）相同的项。更新接口未传 `fallback_models` 时保留原值，传空数组则清空。

## 回退时机

入口：`backend/internal/server/routes/gateway.go` 的 Composite 中间件，命中显式路由时进入 `runCompositeRouteWithFallback`（`composite_fallback.go`）。只有一个目标时直接分发，但仍执行原生压缩协议的目标准入检查。

在 handler 改写请求前，使用 `IsOpenAICompactionRequest` 按原始路径和原始正文识别原生 Responses 压缩，包括 `/responses/compact`、remote compaction v2 和会提升为 legacy compact 的非流式 trigger。每次分发前跳过不使用 OpenAI 网关的平台，避免经 Anthropic 等普通转换丢失压缩语义；兼容 OpenAI handler 继续通过 `CanForwardOpenAICompaction` 检查账号的实际协议。无保真目标时返回 `compact_not_supported`。普通正文中的会话摘要请求仍可跨平台转换。

多目标时用 `compositeFallbackWriter` 包裹响应：

- 非流式：错误状态码（≥400）响应先暂存，不写给客户端。
- SSE：注释帧原样透传；`response.created`/`message_start`/`ping`/仅 role 的 chunk 等前导帧暂存，暂存期间每 15s 发 `: keepalive` 注释防止客户端超时；出现第一帧可见输出即提交，此后不再回退；输出前出现 error / `response.failed` 事件视为失败。
- 暂存上限 4 MiB，超过即提交并透传。

每次尝试都从原始请求重建：独立的路由决策、请求体按目标模型从原始字节重写、请求头克隆（handler 对请求头的修改不会带入下一次尝试）、gin Keys 恢复到路由前快照。重放只调用最终路由 handler（`c.Handler()`）；夹在 Composite 中间件与 handler 之间的中间件只在首次尝试运行（目前只有对 Composite 分组恒放行的 `RequireGroupAssignment` 和已按同一请求体校验过的体积限制），新增此类中间件必须可重放或放到 Composite 中间件之前。

### 判定是否切到下一个目标（`compositeFallbackRetryable`）

不切换，直接返回当前结果：

- 已产生可见输出、客户端已断开；
- 下游写入失败，包括 `io.ErrClosedPipe` 和规范化为 `io.ErrShortWrite` 的短写。状态在整个备用链内持久保存；首注释、首可见输出的写入错误逐层返回，异步 heartbeat 失败立即停拍，此后不再向下游 Write/Flush 或切备；
- `GetOpsCyberPolicy` 已存在标记：专用 cyber 计费路径拥有该次尝试，即使后台账单 worker 尚未执行，也不能重放或重复普通计费；
- 本次尝试已把用量交给计费：`wrapUsageRecordTaskContext` 在请求 goroutine 内同步标记，不依赖后台 worker 落库。失败尝试若已记账（如带部分用量的错误、`response.failed` 自带 input tokens），重放会与同一 request_id 的计费冲突，成功的那次可能被丢账，因此直接返回；
- 413、499；
- 调用方原因的业务限制：IP 限制、API Key 分组不可用/未分配、本地内容策略拒绝（`local_policy_denied`）；
- 请求级失败（`MarkOpsRequestScopedError`，或带内错误 `RequestScoped`）；
- 没有任何上游失败登记、也不是无可用账号、状态 <500 的失败：用户/Key 并发或限流的 429、余额/配额的 402/403、本地参数校验的 400 等，换哪个目标都一样。

SSE 的 HTTP 状态可能已被 heartbeat 固化为 200。重试判定优先使用 `OpsStreamError.IntendedStatus`，其次使用登记的上游状态，最后才使用原始上游错误帧的推断状态，避免把本地余额不足的 403 猜成 502，或把上游 413 猜成 400 后误切备。带上游归因的真正 429/5xx 仍可重试。

切换：

- 登记过上游失败（`OpsUpstreamStatusCodeKey` 或 `OpsUpstreamErrorsKey`），无论状态码（上游 429/5xx/529、账号侧 401/403、上游 400 参数不被该模型支持等）；
- 无可用账号（`OpsRoutingCapacityLimitedKey`，空池或可服务该模型的账号全部限流）；
- 模型/平台维度的本地拒绝（`local_model_configuration`、`local_feature_gate`）；
- 其它 5xx；
- 网关未自己登记的带内 SSE 错误（即上游流里透传来的 error 事件）。网关自己写的带内错误（如等待并发槽超时）会登记 `OpsStreamError`，不切换。

全部失败时返回最后一个目标的错误。最终 SSE 错误无论是否已有 heartbeat，均按入口协议收尾：Responses → 唯一 `event: response.failed`（Codex 只把它当终止事件，普通 error 帧会导致盲重连），Messages → Anthropic `event: error`，Chat Completions → `data: {"error":…}`。已有合法 `response.failed` 保留其 payload，包括 response ID 和 usage；裸 error 归一为正确终态，并丢弃失败尝试的前导帧。若已被 keepalive 提交但最终尝试什么也没写，补发此前错误的协议终态，都没有则合成通用失败事件。最终尝试只设置状态码与响应头而无正文时，状态码与响应头按原样交给 gin 收尾写出。

OpenAI 普通转发和 passthrough 的流解析已经观测到 tokens 或图片时，`Forward` 同时返回部分 `OpenAIForwardResult` 与原始错误，并复用成功出口构造模型、request/response ID、tier、reasoning、缓存、图片等计费元数据。handler 在账号 failover 前将该次尝试视为终局并提交用量；零用量、零图片的安全失败继续返回 nil result。

`ServerTiming` 提交时只读取自身内层 writer 的响应头，不经 `c.Writer.Header()` 回调外层 Composite 锁；公开 `ServerTimingHeaderValue` 的授权、缓存状态和 WebSocket 调用方式保持兼容。

## 使用记录标记

迁移 `247_usage_log_composite_route.sql` 为 `usage_logs` 增加 `composite_route_id`、`composite_route_kind`（`compaction`/`conversation`）、`composite_fallback_chain`（如 `gemini-3.8-flash-high (429) → gemini-3.8-flash`）。每次尝试有自己的 `CompositeRouteTrace`，链路在该尝试开始时固定，经 `usageRecordContext` 传给后台计费 worker，因此每行用量只标注产生它的那次尝试。

管理员接口 `GET /api/v1/admin/usage`、`/stats` 以及 dashboard 的 trend / models / groups / snapshot-v2 支持 `composite_route=routed|compaction|fallback|none` 筛选；前端使用记录页提供同名筛选（列表、统计卡与图表一致），"路由·压缩会话 #id"/"Composite 路由 #id" 与"备用模型"徽标，导出含 Composite 路由列。图表钻取的用户明细（`/admin/dashboard/user-breakdown`）同样支持该筛选；用户排行 tab 不显示该筛选，也不带该参数。清理对话框的清理接口不支持该维度，因此对话框隐藏该筛选（以及压缩、计费模式、上游模型审计），删除范围与所见一致。

## 已知覆盖范围

- Gemini 原生端点（`/v1beta`）的 Composite 中间件未接入回退，也不写使用记录标记。
- OpenAI Live（`/v1/live`）用量在 `context.Background()` 中记录，不带 Composite 标记。
