# 管理员用量请求诊断

## 查看与启用

管理员在「使用记录」的每行点击「查看诊断」，按需调用 `GET /api/v1/admin/usage/:id/diagnostic`。详情包含权威用量元数据、客户端入站 JSON、每次实际 HTTP 发送的上游 JSON、账号/平台/模型/端点/耗时，以及有限响应摘要。普通用户、列表与导出接口不提供正文；详情响应设置 `Cache-Control: no-store`。没有请求重放功能。

默认关闭。使用 `deploy/config.example.yaml` 的 `request_diagnostics` 段，或环境变量：

| 环境变量 | 默认值 | 含义 |
| --- | ---: | --- |
| `REQUEST_DIAGNOSTICS_ENABLED` | `false` | 是否采集新请求 |
| `REQUEST_DIAGNOSTICS_PRESERVE_RAW` | `false` | 按原文保存请求正文和上游响应，不脱敏、不重新编码正文 |
| `REQUEST_DIAGNOSTICS_RETENTION_HOURS` | `24` | 正文保留小时，启用时允许 1–168 |
| `REQUEST_DIAGNOSTICS_MAX_ENTRY_BYTES` | `8388608` | 单条存储字节预算，最多 128 MiB；响应按实际 gzip 字节计，请求和元数据按 UTF-8 字节计 |
| `REQUEST_DIAGNOSTICS_MAX_ACTIVE` | `4` | 同时采集数，最多 32；`MAX_ENTRY_BYTES × MAX_ACTIVE` 不得超过 2 GiB，另有每个 gzip writer 的固定工作内存和临时编码开销 |
| `REQUEST_DIAGNOSTICS_MAX_PENDING_BYTES` | `33554432` | 后台队列预算，至少一条、最多 1 GiB；按每条实际保存大小预占（最少 64 KiB） |
| `REQUEST_DIAGNOSTICS_MAX_STORAGE_BYTES` | `268435456` | `0` 表示仅按保留期清理；正数为全表压缩正文预算，至少一条、最多 10 GiB |

响应在 transport 的调用者 `Read` 路径上使用标准库 `gzip.BestSpeed` 增量压缩，不长期保留整份明文。常驻内容包括压缩响应、有界响应摘要、最多三个待补齐 UTF-8 字节、请求副本和 gzip 工作区。持久化仍需 JSON 编码（内部 gzip 字段有 base64 开销）与外层 gzip；采集交接和编码存在临时副本，所以预算不是整个进程 RSS 的严格上限。写库超时按外层压缩后大小放宽（5s 起，每 4 MiB 加 1s，最多 60s）。管理员按需读取时才展开正文；每条快照共享 512 MiB 展开资源保护，响应优先于请求，触发保护明确标记 `partial` / `decompression_limit`，不静默显示完整。

六小时且不提前容量淘汰需设置 `REQUEST_DIAGNOSTICS_RETENTION_HOURS=6` 和 `REQUEST_DIAGNOSTICS_MAX_STORAGE_BYTES=0`。这里记录配置语义，未表示已修改生产配置。零容量模式跳过全局容量 advisory trylock 和容量窗口；正数模式继续沿用容量淘汰及锁忙丢弃行为。仍可能因采集并发、队列过载、存储故障或资源保护无法保存诊断，不能将 retention-only 解读为无限资源保证。

正文可能仍包含业务数据和个人信息。启用前确认访问权限、采集授权与保留政策；不要把详情作为公开日志或外部附件。脱敏会隐藏敏感字段、常见密钥格式、Auth/Cookie、代理凭据及 URL 查询参数，不能语义识别任意无已知模式的自定义秘密。前端只以文本渲染正文，不执行 HTML；复制由管理员主动触发。

## 采集边界

- 在 API Key 鉴权后、Composite 路由/模型/正文改写前被动采集入站正文，不提前消费 Body。压缩 JSON 在共享 reader 解压后、lenient JSON 修正前采集。
- transport 通过 `GetBody` 读取有限副本，不消费真正发送的 Body；Grok 兼容重试与 HTTP 重定向分别记录实际 attempt。平台来自已解析分组或强制平台，不按模型名称推断。
- 只保存可安全处理的 JSON。multipart、二进制/图片/base64、无法重读的上游 Body、无效 JSON/UTF-8 和超大正文明确标为省略；超大正文不会保存原始截断片段。
- 最新响应优先使用剩余预算，两份请求再分配余量。最多保留最近 16 个 attempts；预算不足时旧正文被淘汰或安全截断，并有明确标记。压缩 payload 不截取无效 gzip 前缀；压缩后仍超过实际存储预算则省略并标记 `budget_exceeded`。截断字符串可能不是有效 JSON，但外层响应仍是合法 JSON。
- 默认模式下，SSE 响应只保存有限 usage/stop/error/terminal/EOF 摘要；完整、小于 64 KiB 的 JSON 响应可保留脱敏正文。启用 `preserve_raw` 后，请求正文按原文保存，上游 JSON/SSE 响应以增量 gzip 保存调用者实际读取的字节。没有旧 64 MiB 明文截断点；响应超过 512 MiB 展开资源保护时明确省略并标记 `decompression_limit`，压缩字节预算仍独立生效。前端 API 读取还原后可分别复制模型输出正文与完整响应原文。响应中途结束标记不完整；大型终帧以有界增量 JSON 观察识别，完整终帧后 Close 不要求额外读 EOF。编码响应仍只留摘要。采集不预读上游，也不能证明客户端收到或保存了输出。
- WebSocket 多轮推理不做连接级正文绑定；这些用量可能显示未采集。历史请求没有原文，不能事后补回。
- 若关闭、过载、异步绑定未完成或存储失败，计费继续，不为诊断阻塞业务。没有行时统一显示 `not_captured`，不能从该状态区分所有未采集原因。

## 关联、存储与生命周期

独立表 `usage_request_diagnostics` 由迁移 `242_usage_request_diagnostics.sql` 创建，`248_relax_usage_request_diagnostics_payload_check.sql` 放宽 `payload_bytes` 约束。主键是 `(api_key_id, usage_request_id)`，不是 ops UUID 或客户端裸 UUID。成功记录用量后，capture 使用本次请求的原始 `created_at` 绑定；即使去重路径回填历史行，也不能将当前正文误挂到旧用量。插入时 PostgreSQL 精确核验 canonical 键与时间；读取还会验证快照 schema/key/time。

`Finish` 与有效 `BindUsage` 都完成后只交接一次。单条预算统计请求、元数据字符串和实际压缩响应字节，不包含外层 JSON 转义及 base64 开销；后台继续执行 512 MiB 编码硬上限（迁移 `248_relax_usage_request_diagnostics_payload_check.sql` 的 `payload_bytes` 上限同为 512 MiB，本次不修改迁移）。队列按每条实际保存字节预留（最少 64 KiB、最多单条预算），默认总预算 32 MiB；异步 usage 等待仍占采集名额，HTTP 完成后 30 秒未绑定则释放。后台 gzip 保存与计费线程分离，错误日志不包含正文。

纯文本 snapshot 继续采用 schema v1；含压缩响应的 snapshot 采用 v2，响应 payload 包含内部 `content_gzip` 和 `uncompressed_bytes`，其 `StoredBytes` 为实际 gzip 长度。服务读取同时接受 v1/v2，校验 gzip 校验和、声明长度、压缩长度和展开后的 UTF-8；拒绝混合明文/压缩、旧 schema 携带 gzip、无效数据和未知版本。响应返回前恢复原有 `Content` / `StoredBytes=len(Content)` 接口并清除内部压缩字段，快照的存储字节统计随展开调整。所有 payload 共用 512 MiB 展开保护，声明大小还会限制实际解压读取量，防止伪造小长度绕过保护。

回滚旧二进制后，v1 记录仍可读，v2 记录会因旧服务不认识 schema 而读取失败；它们仍按既有过期时间清理。不能把回滚解释为 v2 已丢失或伪造空正文；需要回读 v2 时恢复本版本或更新的兼容 reader。数据库表结构未因内部格式变化而新增列。

正数存储容量按外层实际压缩正文字节计算，超限保留最新记录并将旧正文置空；`0` 模式不执行此容量窗口和容量锁。此预算不是 PostgreSQL 文件/索引总大小上限；备份也不属于它。

到期正文在读取时立即隐藏；后台每分钟批量清空过期正文、清理没有精确用量关联的 orphan，并清理超过七天的到期墓碑。关闭采集仍执行清理。物理清理是批处理，数据库与备份的实际回收遵循运维保留策略；普通用量删除不是即时 FK cascade。

保留期按持久化记录的 `captured_at`（后台完成编码后生成）起算，`expires_at=captured_at+retention_hours`；snapshot 内 `captured_at` 仍记录 HTTP capture 的 Finish 时间。读取过期判断使用前者。每轮最多清空 500 条过期正文，积压可能需要多轮；未测量大量积压的最大清理延迟，不能承诺所有到期记录都在一分钟内物理清空。

关闭服务时先停 usage worker，再停诊断 writer，最后关闭基础数据库。诊断队列有六秒排空预算；到期取消正在执行的数据库操作、丢弃剩余诊断并归还队列额度，等 writer 退出后才关闭数据库。取消依赖 repository/driver 响应 context；这不是强制终止线程。诊断本身不延长生产请求的等待时间。

## 流式修复的时间边界

通用 Anthropic 流在第一个真实 text/client tool 输出前暂存语义事件；thinking、server tool 和心跳不算可见输出。首输出缓冲超过 8 MiB 或最终没有可见输出，返回可重试的账号 failover 错误；只写过 transport comment 不妨碍安全切换，已经输出真实内容则不得重放。

Anthropic→Responses/Chat Completions 转换流遇到客户端写入失败后停止后续写入，但继续读取到终止、闲置超时或错误以取得 usage。`message_stop` 后保留固定 **500 ms** 兼容尾窗口，收到 ping/重复 stop 不续期，并关闭、等待读协程退出。窗口内迟到 usage 会合并；窗口外迟到 usage 不保证采集。因此不能把 `message_stop` 等同于任何兼容上游都已送完 usage，也不能恢复无限等待 EOF。

## 验证方式

本机可运行 unit、前端测试、vet 与嵌入构建。Windows 工具链没有 CGO 时无法运行 race；没有 Docker 时 integration harness 会跳过，跳过不代表真实数据库验证通过。个人镜像发布工作流先在 Linux 跑完整后端 unit、诊断/流式 race、真实 PostgreSQL 集成测试，以及前端类型检查和四组诊断回归，全部通过才推镜像。

2026-10-27 新增定向回归包括：65 MiB SSE 的完整长度/hash/尾标记及无 EOF 大型终帧、单字节多字节字符边界、响应优先、压缩队列计量、关闭/取消/Drop 释放、v1/v2 持久化往返及坏 gzip/长度/UTF-8/资源保护。真实 PostgreSQL 测试入口为 `TestRequestDiagnosticRepositoryIntegrationRetentionOnlyKeepsUnexpiredBodies`、`TestRequestDiagnosticRepositoryIntegrationRetentionOnlySixHourBoundaryAndCleanup` 和 `TestRequestDiagnosticRepositoryIntegrationCompressedSnapshotRoundTrip`，验证持锁时零容量写入、不提前淘汰、六小时边界遮蔽、清理、普通用量保留及真实压缩快照往返；本机执行结果以任务报告为准，CI 真实 PG 通过前不宣称已验证。
