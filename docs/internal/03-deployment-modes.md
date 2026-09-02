# 部署模式：单进程 / 分布式双模式

> 这是贯穿全局的架构原则，不是某个模块的局部设计。任何新增能力都必须同时满足两种部署模式。

## 两种部署模式的定位

脚手架必须同时服务两种截然不同的场景，且两者的基础设施重量不能是同一个量级：

| | **单进程部署模式**（本地开发 / 演示 / CI） | **分布式部署模式** |
|---|---|---|
| 目标 | 一个二进制 + 一个数据文件即可跑起全部功能，**零外部依赖** | 完整能力、可水平扩展、数据可靠 |
| 启动 | `./app` 或 `docker compose -f docker-compose.standalone.yml up`（单容器） | `docker compose up`（app + pg + redis，可叠加观测栈） |
| 冷启动 | 秒级 | 分钟级 |

## 能力降级矩阵

**核心原则：所有基础设施依赖都定义为 `pkgcore` 中的接口，每个接口至少有两套实现。** 由 `SPEED_DEPLOYMENT_MODE=standalone|distributed` 一个开关在 Kernel 装配阶段选择实现，业务代码完全无感知。

| 能力 | 单进程实现（零依赖） | 分布式实现 |
|---|---|---|
| 数据库 | SQLite 文件（`glebarez/sqlite`，纯 Go） | PostgreSQL（+ 可选 TimescaleDB 扩展） |
| 缓存 / 计数器 | 进程内 `sync.Map` + TTL（`KVStore` 接口） | Redis |
| 事件总线 | 进程内 channel | Redis Streams（消费者组） |
| 计量缓冲 | 内存 channel 直接聚合写 SQLite 汇总表 | Redis Streams → aggregator → 汇总表 |
| 任务队列 | 进程内 worker pool + SQLite 持久化任务表 | Redis（`hibiken/asynq`） |
| 定时调度 | 进程内 goroutine + ticker | 同左（小集群无需独立调度器），加 Redis 分布式锁防重复执行 |
| 分布式锁 | 无需（单进程，`sync.Mutex` 足够） | Redis 锁 |
| 对象存储 | 本地文件系统目录 | S3 兼容（MinIO / 云 OSS） |
| 邮件发送 | 打印到 stdout（`ConsoleMailer`） | SMTP |
| 短信发送 | 打印到 stdout（`ConsoleSMS`） | 短信网关 |
| 站内信实时推送 | 单进程直接推给 SSE 连接 | Redis Pub/Sub 扇出到所有实例 |
| 全文检索 | SQLite `LIKE` 降级 | PostgreSQL `tsvector` |
| 支付渠道 | `MockGateway`（立即成功 / 可模拟失败与延迟回调） | Stripe / 支付宝 / 微信 |
| AI Provider | `EchoProvider` + 可录制回放的 fixture | OpenAI / Anthropic / 国内厂商 |
| 可观测性 | OTel SDK 输出到 stdout + 进程内 `/metrics` 端点 | OTel Collector → Prometheus / Tempo / Loki / Grafana |
| 认证 | 密码登录 + 内置种子账号 | 全量（含 OIDC SSO、MFA） |

## 必须遵守的约束

1. **业务代码只依赖接口，永远不 import 具体实现**。`billing` 依赖 `pkgcore.KVStore`，不 import `go-redis`。
2. **接口选型以能力最弱的一方为准**。例如 `KVStore` 不暴露 Redis 特有的 Lua 脚本能力；确需原子操作时，接口层定义 `IncrByFloat`/`CompareAndSwap` 这类可被两种实现满足的语义。
3. **启动时 fail-fast 校验**：单进程部署模式的内存实现不支持多实例（配额计数、事件总线会各算各的）。进程无法自知被部署了几个副本，因此靠**独占锁**来保证——启动时对 SQLite 数据文件取排他文件锁并写入实例标识，第二个进程会立即失败退出并打印明确原因。这比"检测副本数"可靠得多，也顺带防止了两个进程同时写坏同一个 SQLite 文件。
4. **单进程部署模式的语义降级必须显式声明**：文档中写明单进程部署模式下计量数据、事件投递不保证进程重启后存活，仅供演示与开发，禁止用于任何真实计费场景。启动日志打印醒目横幅。
5. **两种部署模式共用同一套业务代码路径**，不允许出现 `if mode == "standalone"` 散落在业务逻辑里——这类分支只允许存在于 Kernel 的装配代码中。

## Compose 分层

按需组合，而非一个大文件：

```
docker-compose.standalone.yml    # 仅 app 一个容器 + SQLite 卷
docker-compose.yml               # app + postgres + redis
docker-compose.observability.yml # 叠加 otel-collector/prometheus/tempo/loki/grafana
docker-compose.dev-tools.yml     # 可选：MinIO、MailHog、支付沙箱代理
```

## 对可测试性的额外收益

单进程部署模式的这套内存实现同时就是单元测试的 test double，不需要为测试再造一套 mock，也让 CI 无需拉起 testcontainers 就能跑绝大多数测试（只有双方言 SQL 兼容性测试、以及 `jobs` 与 `pkgcore` 需要真实 Redis 的集成测试，才需要拉起 testcontainers）。这是这套设计除"单进程部署轻量"之外的第二个正收益，值得为它多付出接口抽象的成本。

