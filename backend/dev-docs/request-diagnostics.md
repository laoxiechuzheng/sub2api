# 管理员用量请求诊断

## 查看与启用

管理员在「使用记录」的每行点击「查看诊断」，按需调用 `GET /api/v1/admin/usage/:id/diagnostic`。详情包含权威用量元数据、客户端入站 JSON、每次实际 HTTP 发送的上游 JSON、账号/平台/模型/端点/耗时，以及有限响应摘要。普通用户、列表与导出接口不提供正文；详情响应设置 `Cache-Control: no-store`。没有请求重放功能。

默认关闭。使用 `deploy/config.example.yaml` 的 `request_diagnostics` 段，或环境变量：

| 环境变量 | 默认值 | 含义 |
| --- | ---: | --- |
| `REQUEST_DIAGNOSTICS_ENABLED` | `false` | 是否采集新请求 |
| `REQUEST_DIAGNOSTICS_RETENTION_HOURS` | `24` | 正文保留小时，启用时允许 1–168 |
| `REQUEST_DIAGNOSTICS_MAX_ENTRY_BYTES` | `8388608` | 单条已保存字符串预算，最多 8 MiB |
| `REQUEST_DIAGNOSTICS_MAX_ACTIVE` | `4` | 同时采集数，最多 32 |
| `REQUEST_DIAGNOSTICS_MAX_PENDING_BYTES` | `33554432` | 后台队列预算，至少一条、最多 256 MiB |
| `REQUEST_DIAGNOSTICS_MAX_STORAGE_BYTES` | `268435456` | 全表压缩正文预算，至少一条、最多 10 GiB |

正文可能仍包含业务数据和个人信息。启用前确认访问权限、采集授权与保留政策；不要把详情作为公开日志或外部附件。脱敏会隐藏敏感字段、常见密钥格式、Auth/Cookie、代理凭据及 URL 查询参数，不能语义识别任意无已知模式的自定义秘密。前端只以文本渲染正文，不执行 HTML；复制由管理员主动触发。

## 采集边界

- 在 API Key 鉴权后、Composite 路由/模型/正文改写前被动采集入站正文，不提前消费 Body。压缩 JSON 在共享 reader 解压后、lenient JSON 修正前采集。
- transport 通过 `GetBody` 读取有限副本，不消费真正发送的 Body；Grok 兼容重试与 HTTP 重定向分别记录实际 attempt。平台来自已解析分组或强制平台，不按模型名称推断。
- 只保存可安全处理的 JSON。multipart、二进制/图片/base64、无法重读的上游 Body、无效 JSON/UTF-8 和超大正文明确标为省略；超大正文不会保存原始截断片段。
- 入站和最新 attempt 优先。最多保留最近 16 个 attempts；预算不足时旧正文被淘汰或安全截断，并有明确标记。截断字符串可能不是有效 JSON，但外层响应仍是合法 JSON。
- SSE 响应只保存有限 usage/stop/error/terminal/EOF 摘要，不保存完整输出流。完整、小于 64 KiB 的 JSON 响应可保留脱敏正文；过大、未读完或编码响应只留摘要元数据。摘要不能证明客户端收到或保存了输出。
- WebSocket 多轮推理不做连接级正文绑定；这些用量可能显示未采集。历史请求没有原文，不能事后补回。
- 若关闭、过载、异步绑定未完成或存储失败，计费继续，不为诊断阻塞业务。没有行时统一显示 `not_captured`，不能从该状态区分所有未采集原因。

## 关联、存储与生命周期

独立表 `usage_request_diagnostics` 由迁移 `242_usage_request_diagnostics.sql` 创建。主键是 `(api_key_id, usage_request_id)`，不是 ops UUID 或客户端裸 UUID。成功记录用量后，capture 使用本次请求的原始 `created_at` 绑定；即使去重路径回填历史行，也不能将当前正文误挂到旧用量。插入时 PostgreSQL 精确核验 canonical 键与时间；读取还会验证快照 schema/key/time。

`Finish` 与有效 `BindUsage` 都完成后只交接一次。单条预算统计正文和元数据字符串，不包含外层 JSON 转义开销；后台再执行 16 MiB 编码硬上限。队列按每条最大预算预留，默认最多 32 MiB；异步 usage 等待仍占采集名额，HTTP 完成后 30 秒未绑定则释放。后台 gzip 保存与计费线程分离，错误日志不包含正文。

存储容量按实际压缩正文字节计算，超限保留最新记录并将旧正文置空。此预算不是 PostgreSQL 文件/索引总大小上限；备份也不属于它。跨副本容量锁忙时丢弃诊断，不等待业务请求。

到期正文在读取时立即隐藏；后台每分钟批量清空过期正文、清理没有精确用量关联的 orphan，并清理超过七天的到期墓碑。关闭采集仍执行清理。物理清理是批处理，数据库与备份的实际回收遵循运维保留策略；普通用量删除不是即时 FK cascade。

关闭服务时先停 usage worker，再停诊断 writer，最后关闭基础数据库。诊断队列有六秒排空预算；到期取消正在执行的数据库操作、丢弃剩余诊断并归还队列额度，等 writer 退出后才关闭数据库。取消依赖 repository/driver 响应 context；这不是强制终止线程。诊断本身不延长生产请求的等待时间。

## 流式修复的时间边界

通用 Anthropic 流在第一个真实 text/client tool 输出前暂存语义事件；thinking、server tool 和心跳不算可见输出。首输出缓冲超过 8 MiB 或最终没有可见输出，返回可重试的账号 failover 错误；只写过 transport comment 不妨碍安全切换，已经输出真实内容则不得重放。

Anthropic→Responses/Chat Completions 转换流遇到客户端写入失败后停止后续写入，但继续读取到终止、闲置超时或错误以取得 usage。`message_stop` 后保留固定 **500 ms** 兼容尾窗口，收到 ping/重复 stop 不续期，并关闭、等待读协程退出。窗口内迟到 usage 会合并；窗口外迟到 usage 不保证采集。因此不能把 `message_stop` 等同于任何兼容上游都已送完 usage，也不能恢复无限等待 EOF。

## 验证方式

本机可运行 unit、前端测试、vet 与嵌入构建。Windows 工具链没有 CGO 时无法运行 race；没有 Docker 时 integration harness 会跳过，跳过不代表真实数据库验证通过。个人镜像发布工作流先在 Linux 跑完整后端 unit、诊断/流式 race、真实 PostgreSQL 集成测试，以及前端类型检查和四组诊断回归，全部通过才推镜像。
