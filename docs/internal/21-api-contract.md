# API 契约：OpenAPI 单一真源

> **所有对外 REST API 必须有 OpenAPI 规范；前端禁止手写任何 API 调用代码，一律使用从规范生成的客户端。** 规范随版本一起发布。
>
> 目标不是"有文档"，而是**从机制上杜绝前后端接口不一致**——靠约定和 review 拦不住漂移，靠编译器可以。

## 核心决策：spec-first，而非 code-first

| 方案 | 机制 | 为什么不选 / 选它 |
|---|---|---|
| code-first（swaggo 注释） | 从 Go 注释生成 spec | **不选**。注释与代码没有强绑定，改了 handler 忘了改注释，CI 也发现不了——文档腐化的经典路径，与本项目要解决的问题正相反 |
| **spec-first（选定）** | spec 生成 Go server interface，handler 必须实现它 | **编译期强制一致**：改了 spec 不改实现，编译失败；改了实现不改 spec，实现签名对不上生成的 interface，同样编译失败 |

工具链：
- **后端**：`oapi-codegen` 从 spec 生成 server interface、请求/响应类型、参数绑定与校验
- **前端**：`orval` 从 spec 生成 TanStack Query hooks（`useXxxQuery` / `useXxxMutation`）与 TS 类型——与已选的 TanStack Query 方案直接对齐，生成即可用
- **合并与校验**：`redocly` CLI 合并各模块 spec 片段、lint 规范一致性
- **破坏性变更检测**：`oasdiff` 比对上一发布版本（尚未接线，见"与发布流程的绑定"第 2 条）

**现状总览**：spec-first 闭环的两半均已落地——后端 interface 生成、前端 sdk 生成、前端运行时与 CI 一致性 diff 全部接线；每一处再生成都跟在一致性闸门之后，handler 编译由 reference-app 的 `go build` 兜底（各环节的现状与细节见下文对应小节）。

## 规范的组织与合并

每个模块在自己目录维护 `api/openapi.yaml`，与 migrations、i18n 资源一样是模块自带资产：

```
go/billing/api/openapi.yaml     # 只描述 billing 自己的路径与 schema
go/authn/api/openapi.yaml
...
contracts/speed.yaml            # CI 合并产物，发布物之一
```

**避免合并冲突的命名规范**（CI 强制）：

| 项 | 规范 | 示例 |
|---|---|---|
| 路径前缀 | `/api/v1/<module>/...` | `/api/v1/billing/plans` |
| operationId | `<module>_<action><Resource>` | `billing_listPlans` |
| schema 名 | `<Module><Type>` | `BillingPlan`、`AuthnSession` |
| 标签 | 模块名 | `billing` |

operationId 直接决定生成的函数名与 hook 名，命名不规范会污染整个前端调用面，因此由 CI 校验而非靠自觉。

**现状：片段实例。** 这套惯例的第一个实例是 reference-app 的 notes 模块：`examples/reference-app/internal/notes/api/openapi.yaml` 片段，同目录携带生成器配置 `oapi-codegen.yaml`（钉定 oapi-codegen v2.8.0）与生成物 `notes-server.gen.go`——对应上文 `<module>/api/openapi.yaml` 的模块资产布局，只是落在 reference-app 而非 go/ 模块下。notes 的 handler 实现生成的 `api.ServerInterface`（`internal/notes/handler.go` 的 `var _` 编译期断言），并经 `api.HandlerFromMux` 从片段注册路由；spec 加了 operation 而 handler 没跟上时编译直接失败。

第二个片段实例是 `go/org`：`go/org/api/openapi.yaml`——路径全部 `/api/v1/org/...`，`operationId` 为 `org_<action><Resource>`，schema 名 `Org<Type>`，同目录携带自己的 `oapi-codegen.yaml`（同一钉定版本 v2.8.0）与生成物 `org-server.gen.go`，`Handler` 实现生成的 `api.ServerInterface`（`go/org/handler.go` 底部的 `var _` 编译期断言，与 notes 的手法完全一致）。org 的片段如今与其余平台模块片段一样加入合并文档、经 orval 生成进 `@speed/api-sdk`——合并成员由模块驱动策略决定（见下文"模块驱动的合并策略"），不看工作区里有没有现成消费者；org 早于合并机制落地、前端面长期只交付后端一半的状态随该策略关闭。

第三个片段实例是 `go/authn`：`go/authn/api/authn-server.gen.go`、`go/authn/handler.go` 的 `var _ api.ServerInterface` 编译期断言与 notes 走的是同一套机制；它与 notes、org 片段各自独立生成（各自的生成 leg 互不依赖，改一个片段不会触发另一个片段重新生成）。再之后 `go/storage`、`go/notification`、`go/sharing`、`go/pki`、`go/admin`、`go/integration`、`go/ai-gateway`、`go/billing`、`go/config` 与 reference-app 的 cases、smilesim 片段以同一形态相继落地——每个片段携带自己的 `oapi-codegen.yaml` 与生成物 `<name>-server.gen.go`，每个 handler 都在各模块 `handler.go` 底部以 `var _ api.ServerInterface` 编译期断言实现生成接口。平台模块的后端片段现为 org、storage、notification、sharing、pki、admin、integration、ai-gateway、billing、authn、config 十一个，是 api-contract 流水线的再生成面；reference-app 自有的 notes、cases、smilesim 是应用片段，归应用自有生成流（下文专节），两类的漂移闸门同为 `tools/check_api_fragments.py`（`fragments` 与 `app_owned` 两个清单、同一 `tools/api_fragments.json` 单一来源）。

**notification 片段的形状。** `go/notification/api/openapi.yaml` 同目录携带生成配置（同一钉定 oapi-codegen v2.8.0）与生成物 `notification-server.gen.go`，`Handler` 在 `go/notification/handler.go` 底部以 `var _ api.ServerInterface` 编译期断言实现之；十一个操作全部在 `/api/v1/notifications` 下，`operationId` 为 `notification_<action><Resource>`：收件箱消息列表、未读数、标记已读两操作、类型目录、偏好读与单键更新、外部联系人花名册的 list/create/verify/resend。契约侧的例外不在操作而在媒体类型：`GET /api/v1/notifications/stream`（SSE 长连接）不是 OpenAPI 3.0 能表达的，由 `NewHandler` 手挂载、片段头部注释记录省略——设计行"站内信 SSE 实时推送"所预言的"单独文档化事件格式"即以此形态兑现。

**合并与 lint。** `task api:merge`（`Taskfile.yml`）与 `.github/workflows/api-contract.yml` 用钉定的 `@redocly/cli@2.51.1` 的 `join` 命令把片段合并进 `contracts/speed.yaml` 并 lint——不是 `bundle`：各片段是对等的完整文档，不是一个根文档 `$ref` 到另一个。合并成员以 `tools/api_fragments.json` 的 merge_rank 为单一来源（join 输入顺序即 merge_rank 顺序），现为十一个平台模块片段：admin、ai-gateway、authn、billing、config、integration、notification、org、pki、sharing、storage。reference-app 自有的 notes、cases、smilesim 不是成员——应用自有生成流（下文专节）把它们并入应用自己的合并文档，平台合并文档不含任何 demo 表面。`task api:gen` 的前端 leg 依赖 `api:merge`，钉定的 orval（8.17.0，经 `pnpm dlx` 运行、永不进入 lockfile）从合并后的 `contracts/speed.yaml` 生成 `@speed/api-sdk` 的 `src/index.ts`，即 api-sdk 覆盖全部十一个平台合并片段；前端 leg 与 api-contract.yml 的再生成步骤执行同一对命令。

**模块驱动的合并策略（记录）。** 合并成员由模块归属决定，不由工作区里现成的 web 消费者决定：凡带 HTTP 片段的平台模块一律加入合并文档与 `@speed/api-sdk`。理由分两层。其一，speed 是库不是应用——对外发布的 SDK 必须覆盖整个平台面，不能只导出工作区 demo 应用（reference-app）碰巧消费的那部分；凡"等一个工作区消费者再进合并"的排除在模块片段上一律作废。其二，生成的类型覆盖面不会因无人调用而腐化：api-contract 流水线对每个片段做再生成 + 一致性闸门 + handler 编译兜底，无人调用的操作与有人调用的操作受同一组闸门钉住。因此"无消费者"纪律约束的对象是**业务 API 的消费证明**——哪个工作区页面真实调用哪些操作、由哪些 Go 测试驱动真实路由（reference-app 的 mandatory-first-consumer 规则）——而不是生成的 SDK 覆盖面；一个没有工作区消费者的平台片段照常进合并、照常被 orval 生成、照常被闸门钉住。org、storage 早期"不进合并"的排除记录、以及片段进合并文档需随前端消费面落地的旧表述，均随该策略关闭，`Taskfile.yml` 的 api:merge 注释与各模块 AGENTS.md 的片段段落已改写为现状。**应用片段不在本策略的射程内**：reference-app 自有的 notes、cases、smilesim 属于应用而不属于平台，平台 SDK 不覆盖 demo——它们由应用自有生成流（下文专节）承担，与本策略无关。

**应用自有生成流（reference-app 的 app-owned 生成；记录）。** speed 是库不是应用，reference-app 是"消费方形状"的模板；一个消费方项目的自有 API 片段归它自己生成管理，不混进平台流水线。本轮落地这一形态的在库种子：reference-app 的三个应用片段经 `task api:gen:app`（`Taskfile.yml`）——钉定 oapi-codegen v2.8.0 各自再生成后端 `notes-server.gen.go`/`cases-server.gen.go`/`smilesim-server.gen.go`；钉定 redocly 2.51.1 的 `join` 把三个片段并入应用自己的合并文档 `examples/reference-app/web/app-openapi.yaml`（`web/scripts/redocly-join-fixup.mjs` 以显式 title/description 参数盖章 info 块，同一脚本服务两条腿）；钉定 orval 8.17.0 从该文档生成应用自有 SDK——`examples/reference-app/web/src/app-api/index.ts`，随后的 nodenext fixup 对应用生成入口与接缝以显式路径参数运行同一 `web/scripts/orval-nodenext-fixup.mjs`。应用 web 宿主对 notes/cases/smilesim 操作一律 import 该应用 SDK（`src/app-api`），平台操作仍 import `@speed/api-sdk`；两个 SDK 骑同一 `bindRequestFn` 接缝与同一 QueryClient——应用 SDK 的接缝文件（`src/app-api/runtime.ts`，全目录唯一手写文件）再导出 `@speed/api-sdk/runtime`，宿主启动时的一次绑定同时服务两面。生成物全部提交，与平台先例一致；一致性门禁是应用自己的最小诚实等价物——full-check.yml 的 reference-app job 跑上述同一组命令并做 porcelain 闸门（应用是消费方，消费方侧的门禁归消费方自己的 CI；api-contract.yml 不承载应用片段）。该流即 `saasctl openapi generate`（第 4 条）的在库种子，产品化时以生成项目的形态重定接缝与门禁决策。

`redocly.yaml`（仓库根目录）定义了合并规则与命名规范 lint 规则：operationId 格式、component schema 名称格式、tag 格式三条按 error 强制。tag 规则按 error 的前提是每个合并片段的每个 operation 都带模块标签——无标签 operation 会被 join 归入合成的 `openapi_other` 标签，多个片段同时贡献该标签时 join 直接失败，所以标签约定是合并本身的前置条件，规则只是把它钉在结果上（`redocly.yaml` 自己的注释）。路径前缀（`/api/v1/<module>`）这一条命名规范暂时没有对应的 redocly 规则——`redocly.yaml` 的结尾注释记录了原因：redocly 找不到能把 PathItem 的 map key 暴露给断言的 subject 类型，而不是没去做；这条留给代码评审。

## 前端：禁止手写 API 调用

- **`@speed/api-sdk`**：由 orval 从合并文档生成（orval 钉定 8.17.0，经 `pnpm dlx` 从 `web/` 运行、永不进入 lockfile，即 orval 不是工作区依赖），每次重新生成整体覆盖 `src/index.ts`，禁止手工修改——文件头带 `// Code generated by orval. DO NOT EDIT.` 与钉定版本的 DO-NOT-EDIT 标记。生成代码不直接触碰网络：mutator 指向包内唯一手写源文件 `src/runtime.ts` 接缝——`bindRequestFn(createClient(...))` 由 host 启动时绑定一次、last-bind-wins，`speedRequest` 把 orval 的 axios 形态调用适配到 `@speed/api-client` 的 `RequestFn` 契约——并以 exports map 的 `./runtime` 子路径暴露，整体再生成不会覆盖它。orval 的 mutator 发射为无扩展名相对导入，nodenext 构建无法编译（TS2835），由确定性再生成脚本 `web/scripts/orval-nodenext-fixup.mjs` 改写为显式 `.js`——工具缺口用工具补（脚本在 orval 改变发射形态时非零退出），手写面收敛到那一个接缝文件。生成代码无租户概念：无租户头（tenant 只存在于 access-token claims，前端从不把租户放进请求头，见下条），query key 为裸 spec 路径；租户 query-key 命名空间化由消费壳在查询键上执行（`['tenant', tenantId, …]`），是宿主纪律而非生成物。覆盖范围是十一个平台合并片段——reference-app 的 notes/cases/smilesim 操作不经由此包（平台 SDK 只覆盖平台面），它们由应用自有 SDK 承担（"应用自有生成流"节）。钉定与延期细节以包内 README/AGENTS.md 为准。
- **`@speed/api-client`**：手写的运行时基建——fetch 实例、认证头注入、401 静默刷新、错误归一化、重试策略。`createClient` 交付可注入 fetch、内存 access-token store（包内无任何 storage API）、401 静默单飞刷新、超时、幂等方法限定的瞬态重试、`ApiError` 归一化与结构化 reporter。`api-sdk` 生成的代码调用它作为 HTTP 层。没有"租户上下文头"这种东西：tenant 只存在于 access-token claims 里，由后端从 token 解析，前端从不把租户放进请求头。

两者分离的原因：生成物会被反复覆盖，手写基建不能被覆盖。混在一个包里，每次重新生成都会冲突。

**token 传输的现实形状（契约事实）。** authn 的 token 签发响应把 refresh token 放在**响应体**（`AuthnTokenPair.refresh_token`），在 tenant-switch 与 step-up 响应中缺席——这两个操作轮换既有 token 家族；**不存在 refresh cookie**——authn 设置的唯一 HttpOnly cookie 是 social 绑定预授权路径的那个（`Path /api/v1/authn/social`）；refresh 端点在请求体里读取调用方持有的 token。前端运行时按此现实设计：内存 store 只持有 access token，refresh token 只存在于会话闭包、永不被写入任何存储。

**CI 强制规则**：前端包与业务项目中，除 `@speed/api-client` 内部外，**任何直接 `fetch(`/`axios.` 调用后端路径的代码一律拒绝合入**（ESLint 自定义规则 + 配置级白名单）；要调接口，只能用生成 SDK 导出的 hook——平台面用 `@speed/api-sdk`，消费方自有的片段面用该应用的应用自有 SDK（reference-app 壳的 `src/app-api` 即此形态）。执行件是 `web/eslint-rules/no-direct-http.js`——规则与其单测在 `web/eslint-rules/no-direct-http.js` / `no-direct-http.test.mjs`，由 fast-check 的 repo-checks 任务逐 PR 与 `no-literal-text` 单测同命令运行。它拒绝前端各包 `src` 中的直接 HTTP——裸全局 `fetch(...)`（带遮蔽检查：标识符能解析到局部或导入绑定时不算全局）、`window.fetch`/`globalThis.fetch`、`new XMLHttpRequest()`，以及任何 `axios`/`node-fetch` 的 import/require（规则不认路径）。配置级白名单只有一处：`web/eslint.config.mjs` 把 `packages/api-client/**` 整体豁免——api-client 就是手写 HTTP 的家，扩大白名单属于架构变更。规则的语义是"除 api-client 外禁止直接触碰网络"，与"只用 api-sdk 的 hook"不冲突：api-sdk 的生成代码同样经手写接缝调用 api-client 作 HTTP 层，每次重新生成后其 src 都必须通过本规则，规则继续把 HTTP 收口在一个包内。reference-app 的 consumer 壳（见"生成面的消费形态"）同受此纪律约束。

## 统一的错误响应

所有接口的错误响应共用一个 schema，与国际化结构化错误码方案对齐（结构化错误码由后端返回、展示文案由前端按 `code` 查 i18n 资源）：

```yaml
ApiError:
  type: object
  required: [code, traceId]
  properties:
    code:    { type: string, example: "billing.quota_exceeded" }
    params:  { type: object, additionalProperties: true }
    message: { type: string, description: "英文兜底文案，仅用于日志排查，前端不得直接展示" }
    traceId: { type: string }
    details: { type: array, items: { $ref: "#/components/schemas/FieldError" } }
```

统一 schema 让生成的客户端能做统一错误处理（401 刷新、429 退避、错误码 → i18n 文案映射），而不是每个接口各写一套。**`message` 字段明确标注不得直接展示**——展示文案一律由前端按 `code` 查 i18n 资源。

**前端文案的来源与生成**：`code` 是唯一查找键，文案来自两层——各 UI 包自己的 `locales/*.json`（覆盖本包白名单码，未知码落 `errors.unknown`），以及由后端模块目录机械生成的平台错误文案包（`tools/gen_platform_error_bundle.py` 取 apperr census ∩ `locales/*.toml` 生成 `web/packages/i18n/src/platform-errors/locales/*.json`，随 `@speed/i18n` 的 `./platform-errors` 子路径发布，作为宿主实例的兜底命名空间；见 11-cross-cutting 的国际化一节）。生成器把 Go 模板占位符 `{{.name}}` 规范化为 i18next 的 `{{name}}`，并在语言间补齐复数形式；把 `ApiError.params` 的键透传给 `t()` 以填写这些占位符是纯前端的后续工作，当前解析器不传参。

## OpenAPI 覆盖不到的部分

必须显式列出并单独文档化，避免"以为都覆盖了"：

| 场景 | 为什么不在 spec 里 | 如何处理 |
|---|---|---|
| 站内信 SSE 推送 | 长连接流式，生成器支持差 | 事件格式单独文档化——SSE 端点由 notification 的 `NewHandler` 手挂载、片段头部注释记录省略（见上文 notification 片段形状）；前端唯一一处 EventSource 调用设计封装在 `@speed/notification-ui` 内 |
| 文件上传 | 不存在 spec 表达不了的形态：上传是**服务端中转流式**（Create→Upload→Complete 均为普通端点），全程可由 OpenAPI 表达 | wire 契约权威是 `go/storage/api/openapi.yaml` 的七个操作；直传 S3/OSS 的形态未落地（见 `go/storage/AGENTS.md` 的延期清单），storage 片段照常加入合并文档与 SDK（见上文合并段落）；`FileUploader` 是受控队列组件——队列是 host 的 `rows` 状态、交互经回调上报，上传传输是 host 自己的代码，组件不封装任何上传 HTTP |
| 外发 Webhook | 是本系统**发出**的请求，不是提供的接口 | 用独立的 AsyncAPI 风格文档描述事件负载 |
| 支付渠道回调 | 由第三方按各自格式回调 | 内部实现细节，不进公开 spec |

## 与发布流程的绑定

1. **spec 与代码同版本发布**：合并后的 `speed.yaml` 作为 Release 附件，同时打包进 `@speed/api-sdk` 与文档站的对应版本目录。
2. **破坏性变更闸门**：`oasdiff` 检测到 breaking change（删路径、删字段、改必填、改类型、改 operationId）时，CI 拒绝合入，除非 PR 显式标记 `breaking-change` 标签并附升级指南条目。**现状**：此闸门尚未接线——`oasdiff` 需要一个发布基线作比对对象，而 v0.0.1 发布后作废（21 个模块 tag 已从远端与本地删除；npm 半从未发布）；可比对的基线取自仓库自身历史：发布提交 `fbaaaf98` 是 main 的祖先，其树带着完整的合并文档 `contracts/speed.yaml` 与全部 spec 片段，Go module proxy 只解析 21 个模块中的 17 个（admin、ai-gateway、integration、saasctl 返回 404），单靠代理拼不出完整基线。下一个发布版本成为新基线。这里记录的是机制决策而非假闸门，权威清单见 `@speed/api-sdk` 的 AGENTS.md。
3. **生成物一致性检查**：CI 重新生成一遍前后端代码，与仓库内产物做比对，不一致即失败——防止有人改了 spec 却没提交重新生成的代码。**现状（平台面）**：`.github/workflows/api-contract.yml` 在改动平台片段 / 生成器配置（含 `web/orval.config.ts` 与 `web/scripts/**`）/ `Taskfile.yml` / 流水线自身的 PR 上触发；触发后先重新生成——十一个平台后端片段各自跑 oapi-codegen，前端 orval + nodenext-fixup 从 `web/` 跑，合并文档单独再生成——每个再生成步骤后各跑一次一致性闸门：全部后端片段、合并文档与前端 sdk 各一份（片段清单以 `tools/api_fragments.json` 的 `fragments` 为单一来源，闸门数量随再生成面扩展），实现为 `git status --porcelain --untracked-files=all`，绝不 `git diff --exit-code`——再生成新建文件时 diff 闸门会静默通过，工作流头部注记写明该陷阱；最后 `go build` reference-app 兜底 handler 编译：reference-app import 全部平台模块，平台片段的生成接口若超出其模块 handler 的编译期断言，该兜底编译失败。authn 另设自己的再生成 + `go build ./...` leg，理由是 `go/authn` 是每次 fast-check 都会构建的真实模块，其编译强制不必等 full-ci 才得到回答——与 reference-app 是否 import 它无关。**现状（应用自有面）**：应用片段不进该流水线——`task api:gen:app` 的再生成与 porcelain 闸门在 full-check.yml 的 reference-app job（平台片段改动触发的是另一条路径筛选；应用片段产物的一致性由全检门禁与 fast-check repo-checks 的应用构建共同兜底），见"应用自有生成流"节。
4. **业务项目同样适用**：`saasctl openapi generate` 让业务项目用同一套工具链管理自己的 API，生成的客户端与脚手架的 sdk 在同一个 QueryClient 下工作。**现状**：该命令不在 saasctl 的已交付命令集（`new`、`upgrade`、`db migrate`、`config print`）里——业务项目自持 spec 片段的生成管理没有官方入口（当前已知局限；延期的权威清单与理由见 `go/saasctl/AGENTS.md` 的 Known limitations）。参考实现先在库内以 reference-app 的应用自有生成流落地（上文"应用自有生成流"节），即该命令的在库种子：三片段并入应用自有合并文档、orval 出应用自有 SDK、宿主双 SDK 同接缝同 QueryClient——产品化时按生成项目的形状复刻并重定接缝决策。CLI 的分工是：后端骨架生成归 saasctl，前端脚手架归 create-saas-app。

## 契约变更的正确顺序

改接口时必须按这个顺序，不能反过来：

1. 改模块的 `api/openapi.yaml`
2. 重新生成后端 interface → 编译失败暴露所有待改的 handler
3. 补实现直到编译通过
4. 重新生成前端 sdk → 类型错误暴露所有待改的调用点
5. 补前端直到类型检查通过
6. 同一个 PR 提交 spec、实现、生成物

**先改实现再补 spec 是被禁止的**——那等于回到 code-first，失去了编译期约束的全部价值。

这两步的机制真实生效：第 2 步——spec 加了 operation 而 handler 没跟上时，生成的 interface 让编译直接失败，暴露所有待改的 handler（每个 handler 都以 `var _ api.ServerInterface` 编译期断言实现生成接口，见上文"现状：片段实例"）；第 4 步——重新生成前端 sdk 后，类型错误暴露所有待改的调用点，补前端直到类型检查通过。第 6 步——spec、实现、生成物在同一个 PR 提交，由第 3 条的一致性闸门钉住。

## 生成面的消费形态

生成面被层层消费：`@speed/auth-core` 的 in-workspace 编译消费、auth-ui / account-ui / tenancy-ui / product-shell 的 in-form 运行时消费、reference-app consumer 壳的真实宿主组合消费——每一层都以自己的 `usage-example.test.tsx` 或壳套件钉住 README 写下的旅程，防止生成面与文档脱节。测试名的细节即证据所在：`src/usage-example.test.tsx` 编译并执行各包 README 的 quick start，作为包的 compilable example。

**编译消费。** `@speed/auth-core` 是 api-sdk 生成面的首个 in-workspace 编译消费者：其单元套件经包内手写接缝 `bindRequestFn` 绑定 scripted `RequestFn`（与 host 的 `createClient` 绑定的是同一个接缝，last-bind-wins），驱动密码登录、登出、刷新等生成操作并做类型检查；`src/usage-example.test.tsx` 把 README 的会话-hooks 流程编译执行，作为包的 compilable example。`@speed/auth-ui` 的公开类型增加 api-sdk 的第二个编译消费方向：`RegisterForm` 的 `onRegistered` 回调携带生成的 `AuthnUser`（api-sdk 相应列为该包的 dependency）。

**运行时 in-form 消费。** `@speed/auth-ui` 的 `src/usage-example.test.tsx` 编译并执行 README quick start 的组合——真实 `@speed/api-client`（`createClient` + 内存 access-token store + 可注入 fetch；fetch 替身以真正的 `Response` 对象作答，并逐条记录 method/path/authorization）经同一 `bindRequestFn` 接缝绑定，`attachSession` 后以 host-gate fixture 驱动组合的登录家族，旅程钉死六次请求的顺序与形状：密码登录（store 持有签发 token）→ 受保护请求（过期的 access token）以 `authn.token_expired` 被拒 → api-client 静默刷新（刷新请求凭声明不带凭据；轮换后重试携带新 token，store 翻新）→ 服务端会话死亡（该次 /me 拒绝为 `authn.session_revoked`，刷新自身被 `authn.refresh_token_invalid` 拒绝）→ `refresh()` 解析 `false` 收敛匿名 → 再次登录 → `switchLanguage` 到 en-US 断言同一表面的英文文案。

`@speed/tenancy-ui` 是 `authn_switchTenant` 端点的 in-form 运行时消费者：`src/usage-example.test.tsx` 编译并执行 README quick start——同一真实 client 与接缝，`attachSession` 后先密码登录，再驱动三次切换尝试并钉死请求顺序与形状：成功切换的请求携带 `authorization`（新签发的 access token）与 `{tenant_id}` body，store 随签发翻新；被拒切换（非成员租户）的答案经该包 9-code 白名单渲染码文本、会话状态不变、控件可重试。切换响应只含 access token 不含 refresh token（token 传输现实见上文），旅程中无刷新腿出现。

`@speed/product-shell` 的 `ProductShell` 把该旅程组合到尽头：三分支视图机（认证 → `AppShell` 框架、匿名且到达过 app → sessionEnded 槽或默认 `SessionEndedScreen`、匿名新访客 → signIn 槽或空），只读 auth-core 快照；其 `src/usage-example.test.tsx` 执行 README 组合，旅程为登录 → 框架 → userMenu（`TenantSwitcher` 与 `SignOutButton` 并列）切租户 → 登出 → 会话结束屏 → 再登录。`RouteGuard` 门禁的形态以宿主组合在套件内证明：`src/gated-journey.test.tsx` fixture 在 `children` 里以 view-id mini-router 组合 `RouteGuard`（status 由 `usePermission` 在 host attach 的列表上派生，切租户 commit 后按存活规则 re-attach）；shell 包代码本身不消费 `RouteGuard`/`usePermission`/`setPermissionSet`，权限的真实获取（从 /me 派生列表或 rbac 端点）由宿主组合承担，fixture 以 role-load 替身扮演。

auth-ui 的 in-form 消费走的是 **session 契约**（登录操作经 auth-core 的生成调用）；`@speed/account-ui` 证明的是同一生成面的另一条腿——**组件树里的生成 hooks 与 mutation 直呼**，它是第一个把生成 hooks 真正渲染进组件树的包：会话/登录历史/绑定身份三个列表分别经 `useAuthnListSessions`/`useAuthnListLoginHistory`/`useAuthnListIdentities` 读取，写经生成的 mutation（下线、解绑、enroll/confirm/regenerate）与 `authnSocialCallback` 直呼，失效一律走导出的 query-key 构造器（`getAuthnListIdentitiesQueryKey` 等），不经手写 query key；hooks 消费意味着宿主树需要 `QueryClientProvider`（shared-QueryClient 契约的第二个消费方，也是第一个渲染进组件树的消费方）。随之 `@speed/api-sdk` 在 account-ui 里是运行时 dependency（hooks、query-key 构造器、`authnSocialCallback` 都在这里执行），而相对 auth-ui 保持 type-only 依赖。其 README quick start 由 `src/usage-example.test.tsx` 编译执行——真实 `@speed/api-client` 经同一 `bindRequestFn` 接缝绑定、fetch 替身答真实 `Response` 并逐条记录 method/path/query/authorization，旅程钉死十八次请求的顺序：社交交换登入 → 列表读取 → 下线 → step-up 403 → 验证经 `session.verifyStepUp` 轮换 access token → 组件重试携带新 token → 绑定回调交换 → remount 后列表收敛，逐请求断言 authorization 头：登入前无凭据、轮换前 `access-1`、轮换后 `access-2`。

**reference-app consumer 壳：真实宿主组合。** 壳是第一个真实宿主组合下的运行时端到端消费者：其 bootstrap 以同一 `bindRequestFn` 接缝绑定一个真实 `@speed/api-client`（fetch 由宿主环境供给——`createClient` 在构造时捕获 `globalThis.fetch`，包内注入 seam 在此是依赖注入点而非测试替身点），绑定恰好一次；从此壳内全部 API 流量都是生成代码流量，app 目录与包一样受 `speed/no-direct-http` 纪律约束。壳的表面按 API 归属分两个生成来源：壳宿主（reference-app）自己的 notes、cases、smilesim 半边经**应用自有 SDK**（`src/app-api`，见"应用自有生成流"节）行使——notes 的 `useNotesListNotes`/`useNotesCreateNote` 经生成的 react-query hooks（失效经导出的 query-key 构造器，键以 `['tenant', tenantId, …]` 命名空间化——命名空间是壳纪律不是生成物）；平台半边经 `@speed/api-sdk` 行使——authn 经 auth-core 会话与 auth-ui/account-ui/tenancy-ui 组件面、billing 经 billing-ui 家族，登录、切租户与账号面全部经同一 bound client 而行，其 `refreshAccessToken` 接会话的静默刷新（壳的 demo server 不答 401，该腿在壳层从未触发，机制的在形态行使属包级 usage-example 证据）；step-up 面相反，壳层真实驱动：owner 账号的账号管理腿经组合后的 account 面把 discover-by-acting 的 403 门走通——第二次 MFA setup 经 403 `authn.step_up_required` 发现 active factor（spec 无 factor-status 端点，setup 即探测），错误 step-up 码以 400 `authn.mfa_invalid_code` 的 field text 作答，正确码把 bearer 轮换升高（step-up 后为 `access-4`），重试的 setup 确认并替换 factor、亮出新一轮恢复码（只亮一次），29 次请求 trace 钉住两次 `POST /api/v1/authn/mfa/step-up`。生成面的消费随之闭合为一段：生成半边被真实壳消费，既有 in-workspace 编译消费保留为包级证据。壳不触碰生成物边界：壳不改 `@speed/api-sdk` 的 DO-NOT-EDIT 源、不改应用自有 SDK 的 DO-NOT-EDIT 源、不在 `bindRequestFn`/`runtime.ts` 接缝之外出现任何手写 HTTP——平台生成物的门禁是 api-contract 流水线，应用自有生成物的门禁是应用自有生成流的 porcelain 闸门（full-check 的 reference-app job），两者按归属各管一段。壳的真实宿主组合（其 bootstrap 与门禁）由 vitest 套件以脚本化 fetch 形态行使（`src/test-utils/demo-server.ts` 按真实服务器的方式作答，镜像事实逐条引用 Go 侧的钉住测试——文件与测试符号）；真实服务器形态由同侧的 Go 套件行使（`cmd/server/demo_users_test.go`、`internal/app/demo/demo_subject_test.go` 驱动真实组成的服务器，钉住真实注册/登录/授权与策略）。浏览器页面：壳的 `index.html` 把 bootstrap 挂进 vite 生产构建，reference-app 服务器经 `APP_WEB_DIST` 伺服该构建，Dockerfile 把它打进镜像；当前已知局限是驱动该页的浏览器自动化尚未落地（html runner 挂载）。

**错误码契约在应用边界的对齐。** "错误以 code 作为契约"的行使从 spec 结构延伸到"服务器实际可答的码集合"。壳的 `src/codes-alignment.test.ts` 把十个消费面的 reachable-error 白名单——auth-ui/account-ui/tenancy-ui 各自包内 `internal/error-text.js` 的清单（deep-import、绝不复制），与其余七个来自应用自有表面模块的清单（notes-view、cases-errors、smile-sim-errors、share-errors 两份、team-view、credits-view）——双向对齐到逐条带源码引用（`go/authn/errors.go`、`go/rbac/errors.go` 与各业务模块 sentinel；引用只带文件与 sentinel 标识符——标识符由套件以文件级 exactly-once 断言钉住，行号由机器普查 JSON 承载；枚举自身的 80 尺寸由套件的守卫钉住）的服务器码集合。方向一：服务器答得出的码至少一个表面有白名单文案（漏了即渲染裸 key，失败指名该码与其引用）；方向二：白名单里没有服务器在此答不出的码（死拷贝，失败指名表面）。外加两条精确规则——`client.*` 各面只白名单保留三件套（`client.network`/`client.timeout`/`client.protocol`，`client.http.<status>` 系列按 api-client 契约保持动态、落各面 unknown 兜底）；tenancy-ui 相对 auth-ui 家族恰好多出的两个 token 验证码（`authn.authentication_required`/`authn.token_invalid`，各有 GO_PINNED 引用或登记豁免，套件对该集合有专门断言）。GO_PINNED 枚举为手写且逐条引用其 sentinel；机器提取的服务端码普查已落地——`tools/gen_error_code_index.py` 生成 `docs/error-codes.json`，套件的机器腿逐条核验所引（码, 文件）是普查中的真实构造、并核对所绑标识符——留给人手的只有 reachability 判定（表面自身流能真正引出的码集合，行级提取器推不出来）。两个方向的断言使手写清单保持诚实：服务器新增码而白名单未覆盖、或白名单出现无引用条目，都失败于此。
