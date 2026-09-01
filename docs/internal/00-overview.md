# 总览：speed SaaS 脚手架

> 本文档是整套设计的入口。先读这一篇，再按需跳转到具体主题。

## 背景与目标

### 为什么做这件事
团队要持续对外交付多个 SaaS 项目，每次都从零搭建认证、多租户、组织成员、计费、后台、可观测性这些"每个 SaaS 都一样"的部分，重复成本极高。目标是建立一套可复用的 SaaS 底座，让新项目把这些能力**以模块方式引入**（`go get` / `npm install`），而不是 fork 一整个仓库后各自漂移——fork 意味着底座升级无法回流到已交付项目，这是脚手架失效的根本原因。只有最小的启动骨架（main.go、Provider 装配、docker-compose 等胶水代码）允许由 CLI 生成并被项目自由修改。

### 已确认的产品决策
| 决策项 | 结论 |
|---|---|
| 复用方式 | 模块引入为主，仅最小启动骨架由 CLI 生成（可自由改） |
| 后端 | Golang |
| 前端 | React + MUI |
| 多租户 | 共享数据库 + `tenant_id` 隔离 |
| 数据库 | 生产 PostgreSQL / 开发 SQLite |
| 部署 | Docker Compose，单机或小集群（非 K8s） |
| 仓库策略 | 单 monorepo 开发 + 各模块独立发布 |
| 支付渠道 | 国内（支付宝/微信）+ 国际（Stripe）均为 MVP 必须 |
| 交付节奏 | 内部分里程碑推进，对外一次性发布 v1.0 |
| 试点项目 | 暂无 → 用内建 `reference-app` 作为强制验证消费者 |

### 预期结果
一套 v1.0 底座：新 SaaS 项目用 CLI 生成骨架后，开箱即有多租户隔离、认证与 RBAC、SSO、组织成员、订阅计费与计量、AI 网关、可观测性、运营后台，前端有配套的设计系统与业务组件包，`docker compose up` 即可跑起完整环境。

---

## 一句话概括

`speed` 是一套面向 Golang + React 的 SaaS 底座：**20 个 Go module（19 个可独立引入 + `saasctl` CLI）、16 个 npm 包（15 个可独立引入 + `create-saas-app` CLI）**。业务项目通过 `go get` / `npm install` 引入能力，而不是 fork 整个仓库。

## 文档导航

### 先读这三篇（约 20 分钟，建立整体认知）

| 文档 | 内容 |
|---|---|
| [01 整体架构](01-architecture.md) | 模块依赖图、架构纪律、模块接入契约 |
| [03 运行形态](03-runtime-profiles.md) | 生产 / 演示双模式，贯穿全局的核心原则 |
| [15 里程碑](15-roadmap.md) | 分阶段交付计划与出口条件 |

### 工程与交付

| 文档 | 内容 |
|---|---|
| [02 仓库结构与发布](02-repo-and-release.md) | monorepo 布局、统一版本号策略、CLI 分工 |
| [13 文档规范](13-documentation-standards.md) | 文档随代码发布、面向 AI Agent 的可验收要求 |
| [21 API 契约](21-api-contract.md) | OpenAPI 单一真源、前端禁止手写调用、随版本发布 |
| [18 CI/CD 流水线](18-cicd.md) | GitHub Actions 编排、架构纪律的自动化检查、lockstep 发布流程 |
| [19 开发工作流](19-dev-workflow.md) | 本地环境、分支与提交规范、PR checklist |
| [20 质量与安全工程](20-quality-and-security.md) | 测试分层、质量门槛、供应链与安全测试 |
| [16 验证方式](16-verification.md) | 各能力的验收标准与 CI 强制项 |
| [17 风险登记](17-risks.md) | 已识别风险与缓解措施 |

### 后端能力设计

| 文档 | 内容 |
|---|---|
| [04 数据层与多租户](04-data-and-tenancy.md) | ORM 选型、双方言兼容、租户隔离三重防护 |
| [05 身份与访问](05-identity-and-access.md) | RBAC、企业 SSO、社交登录、组织树、登录日志与会话撤销 |
| [06 计费与计量](06-billing-and-metering.md) | 订阅、信用点、国内外支付、用量计量 |
| [07 平台服务](07-platform-services.md) | 异步任务、媒体存储、分享链接、通知、API 开放与外发 Webhook |
| [08 AI 网关](08-ai-gateway.md) | 多厂商 LLM 与图像生成抽象 |
| [09 可观测性](09-observability.md) | OTel + LGTM 栈，租户维度的高基数处理 |
| [10 合规与审计](10-compliance-and-audit.md) | 字段加密、操作审计、数据保留与删除 |
| [11 横切能力](11-cross-cutting.md) | 国际化、配置管理、功能开关 |

### 前端与示例

| 文档 | 内容 |
|---|---|
| [12 前端架构](12-frontend.md) | npm 包分层、主题与多品牌、状态管理选型 |
| [14 示例应用](14-reference-app.md) | AI 微笑模拟平台：脚手架的强制第一消费者 |

## 阅读这套文档的两条提醒

1. **架构纪律不是建议**。[01 整体架构](01-architecture.md) 与仓库根 [CLAUDE.md](../../CLAUDE.md) 中标注为"禁止/必须"的条目由 CI 与 code review 强制执行，不是风格偏好。
2. **每个设计决策都写了被否决的方案及原因**。改动某个决策前，先确认原因是否仍然成立。
