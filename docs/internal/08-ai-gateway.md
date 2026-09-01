# AI 网关

> 多厂商 LLM 与图像生成的统一抽象层，默认打通计量计费与可观测性。

## 统一抽象层
- 统一 `ChatProvider` 接口（Chat / ChatStream），`Params map[string]any` 透传厂商特有参数，避免抽象僵化。
- `Gateway` 门面内部串起：凭证解析 → 模型路由 → Provider 调用 → **自动配额检查与用量上报** → 自动埋点。业务代码只调 `gateway.Chat()`，AI 计量是内置行为不需手动上报（AI token 是最高频的计量场景，值得做成自动挡）。流式响应在最后一个 chunk 处理真实用量。
- 凭证：支持平台统一 Key 与租户自带 Key（BYOK）两种模式，AES-GCM 加密存储，master key 从环境变量注入（Compose 场景不引入 Vault/KMS，后续上云再升级）。
- 模型访问控制复用 `billing.Entitlements.Check(ctx, tenant, "model:xxx", 1)` 的 boolean entitlement，不另造一套开关。
- MVP 不做自建推理服务，但 `ChatProvider` 接口对本地/远程无感知，后续接 Ollama/vLLM 只是新增实现。

## 多模态扩展：从 Chat 到图像生成

- 新增 `ImageProvider` 接口：文生图、**图生图（image-to-image）**、图像编辑/inpainting（传入原图 + 掩膜 + 提示词）。参数同样用 `Params map[string]any` 透传厂商差异。
- **所有图像任务默认异步**（走 `jobs`），返回 JobID；输入输出图像统一经 `storage` 存取，接口传递的是对象引用而非字节流。
- 用量计量按厂商实际计费维度上报（张数 / 步数 / 分辨率档位），不再只有 token。
- Provider 可插拔，业务方可以接任意商业图像 API 或自建推理服务；脚手架不绑定特定模型。

