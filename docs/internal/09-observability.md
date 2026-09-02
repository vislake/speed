# 可观测性

> 埋点标准、存储选型、以及多租户场景下必须回避的高基数陷阱。

## 技术选型：OTel + LGTM 栈

**埋点层与后端存储层解耦是这里的关键**——OTel SDK 在两种部署模式下都照常埋点，只是导出目标不同：单进程部署模式下导出到 stdout（结构化日志直接可读）并在进程内暴露 `/metrics` 端点（需要时用浏览器直接看，或临时挂一个 Prometheus），完全不需要 Collector 与任何存储组件；分布式部署模式下导出到 OTel Collector。业务代码与中间件零改动。

| 维度 | 选型（分布式部署模式） | 理由 |
|---|---|---|
| 埋点 | OpenTelemetry Go SDK | 厂商中立，换后端不改业务代码 |
| Metrics | Prometheus | 单二进制、Compose 一键起 |
| Traces | Grafana Tempo（非 Jaeger） | 只依赖本地磁盘，资源占用远低于 Jaeger+ES |
| Logs | Grafana Loki | 索引只做标签，成本远低于 ELK |
| 采集层 | OTel Collector | 业务只发本地 Collector，后端可替换 |
| 告警 | Grafana Alerting（内置，非独立 Alertmanager） | 少一个容器 |

全部是普通容器，无 Operator/CRD 依赖。

**tenant_id 高基数问题（必须遵守的规则）**：`tenant_id` **禁止**作为 Prometheus metric label——租户上千即炸出百万级时间序列，是 Prometheus 最典型的事故源。租户维度信息统一放在 **span attribute 和结构化日志字段**（Tempo/Loki 对高基数字段容忍度高得多）。确需大客户 SLA 看板时，只对 Top N 客户走独立 metric name 打标，长尾归入 `other`，并用 exemplars 从聚合指标下钻到具体 trace。

**两条数据路径的边界要写进文档**：Prometheus/Grafana 服务"运维看系统健康度"；`metering` 的汇总表服务"租户/财务看用量和账单"。即便同源于 `UsageEvent`，也不能互相替代。


## 结构化日志

日志是给机器查询的，不是给人读故事的。Loki 按标签与字段检索、与 trace 关联，一句拼接出来的话既无法过滤也无法聚合。

**规则**（后端由 `observability` 包封装 `log/slog` 提供，前端对应物是 `api-client` 的 reporter）：

- **消息是常量字符串，变量一律进 key-value 属性**。`log.Info("user " + id + " logged in")` 是错的；`log.Info("user logged in", "user_id", id)` 是对的——前者每条消息都不同，无法分组统计。
- **logger 从 context 取**（`obs.FromContext(ctx)`），自动带上 `trace_id`、`span_id`、`tenant_id`。请求路径里新建 logger 会丢失关联，排查时无法从一条日志跳到整条链路。
- **字段名 `snake_case` 且全栈统一**：`tenant_id`、`user_id`、`job_id`、`trace_id`、`duration_ms`、`error`。一处写 `userId`、另一处写 `uid`，日志就查不动了。
- **消息小写、无句尾标点、描述已发生的事**：`"payment webhook received"`，不是 `"Received the payment webhook!"`。
- **不要既记录错误又向上返回**——二选一，否则一次失败会在调用栈上出现三遍。
- 级别语义：`Debug` 开发细节（生产关闭）、`Info` 值得运维留痕的状态变化、`Warn` 已恢复的异常、`Error` 需要人介入的失败。
- 明文 PII、密钥、令牌、完整 prompt 一律不进日志，脱敏默认开启且不得关闭。

前端没有结构化日志后端，等价纪律是**结构化上报**：生产相关信号走 `reporter`（常量消息 + 属性对象），并带上错误信封里的 `traceId`，这是把用户反馈接回服务端日志的唯一途径。

## 必须埋点的关键指标

光有技术栈不等于有可观测性——下面这些是各模块必须产出的指标，缺一项就意味着对应的故障只能靠用户投诉发现。全部为低基数标签（不含 `tenant_id`）。

| 领域 | 指标 | 为什么必须有 |
|---|---|---|
| HTTP | 请求量、延迟分位、错误率（按路由 × 状态码） | 基础健康度 |
| 数据库 | 连接池使用率、慢查询计数、事务时长 | 连接池耗尽是最常见的雪崩起点 |
| **任务队列** | **队列积压深度**、执行时长分位、失败率、重试次数、死信计数（按任务类型） | 积压是异步系统最重要的先行指标；死信堆积意味着有任务永久失败 |
| **计量管道** | 事件采集速率、outbox 待投递积压、聚合延迟 | outbox 积压直接对应"钱算不准" |
| **通知** | 各渠道投递成功率、延迟、退信率 | 退信率突增通常意味着通道信誉受损 |
| **支付** | 回调处理成功率、中间态订单数量与滞留时长 | 中间态订单滞留是资金问题的早期信号 |
| **AI 网关** | 各 provider 调用量、延迟、错误率、限流命中 | 第三方不稳定需要能快速定位到具体 provider |
| **站内信 SSE** | 活跃连接数、扇出延迟、断连率 | 长连接泄漏会耗尽文件描述符 |
| 认证 | 登录成功/失败率、MFA 挑战量、token 刷新失败率 | 失败率突增可能是撞库攻击 |
| 缓存 / KV | 命中率、延迟 | 命中率骤降会引发数据库过载 |

**告警只对少数指标设置**，其余留作排查用。首批告警：HTTP 5xx 率、任务队列积压超阈值、outbox 积压、支付回调失败率、死信堆积、数据库连接池接近上限。告警过多等于没有告警。
