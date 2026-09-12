---
title: ai-gateway
description: "厂商无关的对话与图像生成网关:OpenAI 兼容默认实现之上的 Provider 注册表、分作用域的 BYOK 凭据,以及经 jobs 与 storage 的纯异步图像管线。"
weight: 2
---

# ai-gateway

ai-gateway 是 speed 的 AI 网关:覆盖每个 LLM 与图像生成厂商的一层抽
象。业务代码只调 `Gateway` 门面——同步 `Chat`/`ChatStream`,或纯异
步的 `GenerateImage`——绝不直接碰厂商 SDK;provider 注册在
`ChatProvider`/`ImageProvider` 模块接口之后,租户经分作用域的凭据库自带
密钥。

## 它做什么

**对话面。** `ChatProvider`(`Chat`/`ChatStream`)是厂商无关接口;
`OpenAICompatibleProvider` 是零依赖默认实现,直接用标准库对着
OpenAI 兼容线协议写;第三方 provider 就是 `chat` 目录里多一个组件。`Gateway.Chat` 跑固定管线:校验 → 检查
`Entitlements`(若接线)*先于*任何解析,被拒的调用方绝不会被计费 →
解析凭据 → 路由 → provider → 调用 → 上报用量。
`WithModelRoute(logicalKey, provider, vendorModel)` 在构造期把调用方
面对的抽象逻辑键(如 `"chat:default"`)映射到具体厂商模型;未路由的
键答 `ErrUnroutedModel`,绝无静默回退。凭据集中在一张
`ai_gateway_credentials` 表——`CredentialScopeSystem` 或
`CredentialScopeTenant`,租户覆盖、回落平台默认——API key 列加密落
库。**图像面。** `ImageProvider`(`TextToImage`/`ImageToImage`/
`Inpaint`)镜像对话模块,自带默认实现与注册表;`Gateway.GenerateImage`
*没有*同步对应物——它校验、查权益、把恰好一个 `jobs` 任务入队并
立刻返回其 `JobID`。job handler 是存储 I/O 唯一发生的地方,调用方
经 `jobs.Queue.Get` 读回结果:成功 job 的 `Result.Data` 反序列化成
`ImageJobResult{OutputObjectID, Usage}`。

它**不是**什么:不是推理服务——模块是推理端点的客户端,绝不是端点
本身(自托管 OpenAI 兼容主机只是一次凭据配置,不是新注册)。无动态
模型路由(`go/config` 承载的路由刻意缺席;路由就是构造期的
`WithModelRoute` 决策)。凭据面只写:任何响应不回显 key,也没有轮换
或过期生命周期。按用收费刻意不是本模块的事——ai-gateway 与 billing
同层,谁也不 import 谁;收费是宿主经 `go/billing` 先预留后结算的
工作,参考应用的 smilesim 服务就是那个形态。

## 何时选用

你的产品要调 LLM 做对话或补全,或异步生成图像,并且想把厂商代码收
在同一个门面后——带按租户 BYOK 凭据、权益门、用量上报与防 SSRF 的
租户端点。内建 OpenAI 兼容实现已够到 OpenAI、Azure OpenAI、DeepSeek
与多数自托管网关;别的厂商协议就是组合里多一个 provider 组件。图像任务要同步返回就不选本模块——异
步管线就是交付形态。

## 怎么接线

```go
m := aigateway.NewModule(db,
    aigateway.WithModelRoute("chat:default", "chat.openai-compatible", "gpt-4o-mini"),
    aigateway.WithModelRoute("image:smile", "image.openai-compatible", "gpt-image-1"),
    aigateway.WithImageGeneration(queue, objectService), // 武装异步管线
    aigateway.WithEntitlements(billingEntitlements),     // 可选:计费前先判定
    aigateway.WithUsageRecorder(meteringRecorder),       // 可选:上报用量
)
g := m.Gateway()

// 对话——req.Model 是抽象键,调用时才解析:
resp, err := g.Chat(ctx, req)

// 图像生成——纯异步;经你接线的队列轮询:
jobID, err := g.GenerateImage(ctx, imageReq)
// ... 之后,在 worker 或轮询方:
job, err := queue.Get(ctx, jobID) // StatusSucceeded 后反序列化 Result.Data
var res aigateway.ImageJobResult // OutputObjectID、Usage
_ = json.Unmarshal(job.Result.Data, &res)
```

`WithImageGeneration(queue, objects)` 直接收 `go/jobs` 的队列与
`go/storage` 的 `ObjectService`(两者都在 ai-gateway 之下);不带它
构建的纯对话网关不注册任何 job handler,`GenerateImage` 答
`ErrImageGenerationUnavailable`。路由与凭据命名空间两半共用——逻辑
前缀别撞(`chat:`、`image:`)。`Entitlements` 与 `UsageRecorder` 都是
结构化类型、可选的模块接口,由 `billing.EntitlementsService` 与
`metering.Recorder` 无需 import 边即可满足。租户 BYOK 凭据走 HTTP
写:`PUT /api/v1/ai-gateway/credentials/{provider}/tenant`(门禁
`ai-gateway:write`)或 `.../platform`(`ai-gateway:manage_platform`);
读只答 provider/scope/baseUrl 元数据,绝无 key。

## 核心概念与 API 面

- **凭据表是平台数据。** 仿 `go/config` 的 `configs` 表:平台行用空
  串租户哨兵,租户覆盖的解析序,`AssertNotTenantScoped`。key 列经宿
  主注册的 `dbkit` 加密序列化器加密落库。
- **对象引用边界画在 job handler。** `ImageProvider` 只经手原始字节
  (将来的厂商集成无需自带存储管道);`Gateway` 侧的形状只带
  `go/storage` 对象 id,handler 在两者之间翻译。完成 job 的输出是请
  求租户名下的全新对象。
- **租户可写的 base URL 有两道 SSRF 防线。** `ValidateBaseURL` 写入
  时拒掉被禁地址;调用时守卫 HTTP 客户端只解析一次主机、拒绝任何
  被禁候选、按已验 IP 拨号——绝不再解析一次主机名,DNS rebinding
  失败关闭。被禁的拒绝不回显解析出的地址(不做内网 DNS 侦察洞);
  平台级凭据刻意在守卫之外——内网网关是合法的运营默认。
- **重试绝不重跑厂商调用、绝无双计用量。** job 在调用厂商前先认领
  一行 pending 记录,输出写入前就记下「厂商已作答」;用量以该标记
  为闸,重叠的 `Handle` 运行收敛到同一个结果。
- **结构化错误码**——SSRF 三件套 `aigateway.base_url_invalid` /
  `aigateway.base_url_unresolvable` / `aigateway.base_url_blocked`、
  `aigateway.entitlement_denied`(403,带 model 与 reason 参数)及其
  余——索引在[错误码索引(English)](/docs/user-guide/error-codes/#aigateway)。

## 已知限制与链接

- 凭据写面只验形状:写时不与 provider 往返、不查 key 有效性。
- 背不起守卫 HTTP 客户端的第三方 provider,对租户级凭据会被拒
  (`ErrProviderNotSSRFGuardable`)——宿主把它路由到可守卫的
  provider,或让它解析在平台级,即为修复。
- 图像 job 无进度上报(单次厂商调用没有天然中间点),除尝试调用外
  也没有表面能查哪些逻辑键已路由。
- `UsageRecorder` 是分析级、失败开放的模块;要给 AI 用量收费的宿主
  照参考应用 smilesim 的形态做(入队前预留积分、job 到终态时结算,
  下面垫着持久预留记账)。

### 出处

- [go/ai-gateway/AGENTS.md](https://github.com/vislake/speed/blob/main/go/ai-gateway/AGENTS.md)——权威文档(provider 模块、凭据库、异步管线、SSRF 姿态、限制)
- 相关页面:[billing](/zh-cn/docs/user-guide/modules/capabilities/billing/)、[metering](/zh-cn/docs/user-guide/modules/services/metering/)、[storage](/zh-cn/docs/user-guide/modules/services/storage/)、域指南[存储、分享与 AI](/zh-cn/docs/user-guide/domains/storage-sharing-and-ai/)
