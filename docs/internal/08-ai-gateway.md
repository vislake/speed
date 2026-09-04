# AI 网关

> 多厂商 LLM 与图像生成的统一抽象层，默认打通计量计费与可观测性。

## 统一抽象层
- 统一 `ChatProvider` 接口（Chat / ChatStream），`Params map[string]any` 透传厂商特有参数，避免抽象僵化。
- `Gateway` 门面内部串起：**自动配额检查 → 凭证解析 → 模型路由 → Provider 调用 → 自动用量上报** → 自动埋点。业务代码只调 `gateway.Chat()`，AI 计量是内置行为不需手动上报（AI token 是最高频的计量场景，值得做成自动挡）。**配额检查排在最前面，先于凭证解析与真正的 Provider 调用**——这样一个被拒绝的调用者不会先产生真实的厂商调用费用再被拒绝；流式响应在最后一个 chunk 处理真实用量。
- 凭证：支持平台统一 Key 与租户自带 Key（BYOK）两种模式，AES-GCM 加密存储，master key 从环境变量注入（Compose 场景不引入 Vault/KMS，后续上云再升级）。
- 模型访问控制复用 `billing.Entitlements.Check(ctx, featureKey, requested)` 的 boolean entitlement，不另造一套开关——**接口签名里没有 tenant 参数**，租户从 `ctx` 里取（与仓库"租户来自上下文，不做显式参数"的一贯做法一致），`featureKey` 形如 `"model:xxx"`。
- MVP 不做自建推理服务，但 `ChatProvider` 接口对本地/远程无感知，后续接 Ollama/vLLM 只是新增实现。

## 多模态扩展：从 Chat 到图像生成

- 新增 `ImageProvider` 接口：文生图、**图生图（image-to-image）**、图像编辑/inpainting（传入原图 + 掩膜 + 提示词）。参数同样用 `Params map[string]any` 透传厂商差异。
- **所有图像任务默认异步**（走 `jobs`），返回 JobID；输入输出图像统一经 `storage` 存取。**"接口传递的是对象引用而非字节流"这条边界画在 `Gateway`/job handler 这一层，不在 `ImageProvider` 内部**——`ImageProvider` 自己的三个方法实际交换的是原始字节（`ImageBytes`：内容 + MIME），与 `ChatProvider` 交换 `ChatMessage` 内容而非存储引用是同一个道理（`pkgcore.SeamRegistry[T].Build` 的 `Config` 是扁平 `map[string]string`，装不下一个存活的 `go/storage` 句柄，且把存储读写压给每一个 `ImageProvider` 实现——包括未来的第三方实现——会不必要地把简单的厂商适配绑死在 `go/storage` 上）；只有 job handler 这一处真正做存储 I/O：调用 Provider 前把引用翻译成字节，拿到结果后再把字节写回存储、翻译回引用。业务代码touch 到的 `ImageRequest`/`ImageJobResult` 因此确实只携带对象引用（`InputObjectID`/`MaskObjectID`/`OutputObjectID`），从不携带一个字节——这是本条规则真正生效的边界。
- 用量计量按厂商实际计费维度上报（张数 / 步数），不再只有 token。**分辨率档位不是第三个独立计量维度**——它是分类信息而非数量，走 `UsageEvent.Metadata` 而不是自己的 Feature，不参与配额扣减或按量计费的加总。
- Provider 可插拔，业务方可以接任意商业图像 API 或自建推理服务；脚手架不绑定特定模型。

