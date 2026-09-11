# 30 BootstrapKey.Format 升格为 schema 合成源

> 本文是 26 号文（bootstrap 键声明机制）与 29 号文（配置驱动的组件装配）之后的一轮收口设计：把 `Component.BootstrapKeys` 声明从"文档 + 事后绑定校验"升格为"直接驱动 loader 生成 CLI flag / 环境变量 / 配置文件 schema 并解析出值"，消灭引擎里手写的 `PlatformConfig` 镜像结构体。全文为终态设计；实施以本文为准。

## 1 现状核对

- `go/pkgcore/bootstrap_key.go` 的 `BootstrapKey` 字段集（`Key`/`Format`/`Default`/`Sensitive`/`Description`/`Group`/`Example`）本身已经完整、可渲染成文档（26 号文 §3.2）。`Format` 是闭集：`string`/`int`/`bool`/`hexkey`。`validateBootstrapKey`（同文件）校验 Format 闭集与 `Sensitive`+`Description` 配对，`BootstrapRegistrar.Add` 校验重复键——但这套校验只在旧 `Registry.Bootstrap` 席位路径（legacy module 世界）触发。
- `go/app/legacy_bridge.go` 明确写"the component model has no bootstrap-key seat (components declare BootstrapKeys statically)"：29 号文的 `Component.BootstrapKeys` 是纯静态字段，从未经过 `validateBootstrapKey`——Format 闭集、`Sensitive`+`Description` 配对、跨组件重复键，在 Component 世界里今天完全没有校验。
- `go/app/loader.go` 的 `resolveBootstrapMaterial`/`lookupDeclaredKey`：遍历全部已注册组件的 `BootstrapKeys`，对每个 `key.Key` 只做两件事——校验路径形状（`pkgcore.BootstrapKeyPurpose`）、以及在宿主的 `LoadSpec.Host`/`LoadSpec.Platform` 结构体上用 `pkgconfig.Verify`+`pkgconfig.Lookup` 找到一个已经被同一个 `pkgconfig.Loader` 解析过的字段值。它不解析任何东西，只做绑定校验和取值。真正的 flag/env/file 解析，由 `go/pkgcore/config` 对 `LoadSpec.Host`/`Platform` 字面 struct 的反射（`describe()`/`walk()`）完成。
- `go/app/config.go` 的 `PlatformConfig`：引擎手写的镜像结构体，5 个子结构体各带一个 `[]byte` 字段、打 `config:"derive"` tag，字段路径必须与 `authn`/`config`/`notification`/`org`/`pki` 五个模块各自声明的 `BootstrapKey.Key` 逐字对应；`platformKeyPaths` 是这六个路径的第二份硬编码清单；`verifyBinding` 用它们做绑定校验。这是本文要删除的对象。
- `go/pkgcore/bootstrap_material.go` 的 `BootstrapMaterial`：已经是通用的、按键路径（或按 `BootstrapKeyPurpose` 推导的 purpose）寻址的解析结果容器，不关心值从哪来。本文改动的输出目标不变，改的只是"值从哪来"这一段。
- 五个声明模块（`go/authn`、`go/config`、`go/pki`、`go/org`、`go/notification`）的 `component.go`/`module.go` 里的 `bootstrapKeyDecls` 已经是完整声明，本文不要求它们做任何改动。
- reference-app 现状（已核实）：`examples/reference-app/internal/app/server.go` 的 `ServerConfig`（108-121 行）以 `speedapp.PlatformConfig`（tag `config:"-"`）内嵌；`internal/app/bootstrap.go` 的 `hostConfig`（311-321 行）以未打 skip tag 的同类型内嵌，527 行 `loader.Load(&hc.PlatformConfig)` 实际加载，475 行 `DevPlatformConfig()` 给出开发期默认值，820 行 `config.Verify(&hc.PlatformConfig, []string{key.Key})` 做绑定校验，553/667/693 行做字段搬运与 `ConfigSpec{Platform: ...}` 传参。reference-app 自身**没有**重复声明这六个字段——六枚密钥完全经由内嵌 `PlatformConfig` 引入，是本文要删除的引擎镜像的唯一消费形态。
- saasctl 生成骨架现状（已核实，纳入范围）：`go/saasctl/internal/template/project/cmd/server/config.go` 第 192 行同样内嵌 `speedapp.PlatformConfig`（`config:"-"`），323-328 行以 `speedapp.PlatformConfig{...}`/`speedapp.PlatformConfigKeyMaterial{Cipher_Key: devConfigKey}` 字面量给开发期默认值，357 行 `loader.Load(&hc.PlatformConfig)`；五个 `selection/*/server.go` 变体（`none`、`authn`、`authn+org`、`authn+rbac`、`authn+org+rbac`）里至少 `authn` 变体（132/161/169/207 行）直接引用 `b.hostConfig.PlatformConfig.Authn.PII_Cipher_Key` 等字段构造 cipher/`authn.WithBlindIndexKey`；`go/saasctl/internal/appconfig/appconfig.go` 的 twin 对账面提到 `PlatformConfig` 声明的键路径（用于生成骨架的 env 名一致性核对，不是字段引用，需要在实施时确认它是否需要跟着改，还是只是描述性注释）。**结论：模板侧同样是本文范围**，§5 的删除清单同样适用于这五个 `server.go` 变体与 `cmd/server/config.go` 模板；金标 `go.mod.txt`/物化-tidy-构建验收链需要随之重新走一遍（参照 26 号文 §9.7 的验收方式）。configrefgen（`tools/configrefgen`）按 26 号文 D2b 已经改为读描述符而非字面 struct，本文改动预期对它免疫，但实施时仍需以树为准跑一次 `--check` 复核，不能只凭历史记录假设。

## 2 边界重申：不推翻 26 号文 §3.2

26 号文 §3.2 的核心判断——"声明层不放 Go 类型的 Default/Required/Min/Max，因为宿主结构体是唯一的类型/默认值真源，声明层再放一份必然与之漂移"——对**宿主自己拥有、按部署环境定制**的键继续成立，本文不改这一类：reference-app 自己的 29 个宿主键（端口、DSN、开关……）仍然写在 reference-app 自己的 loader 形状结构体上，各自决定类型、默认值、`required`、`env=` 钉法，本文不动它们，也不为它们提供任何组件声明的替代路径。

本文改动的是一个更窄、且此前未被 26 号文单独讨论过的场景：**没有独立宿主定制意愿的声明键**。`PlatformConfig` 覆盖的六枚平台密钥就是这个场景——它们的类型永远是 `hexkey`（`[]byte` + `derive`），永远没有宿主想给的默认值（26 号文 §3.2 自己写明"密钥材料无 Go 默认值"），唯一"决定形状"的角色是声明它的模块，而不是某个宿主。`PlatformConfig` 结构体不是"宿主的定制层"，它是引擎（`go/app`）对模块声明的手抄副本——手抄副本不是第二个独立真源，只是同一个真源的复制品，复制品必然漂移（新模块要素每次都要去 `go/app/config.go` 手工加一段）。

**判据**：一个 `BootstrapKey` 是否适合由引擎合成 schema，看它是否存在一个**独立于模块声明意愿**的宿主定制层。有——继续走宿主手写结构体（26 号文机制不变）。没有——模块声明本身就是唯一需要的信息，引擎应该直接消费它，而不是要求某处再抄一遍。

## 3 机制形状

### 3.1 `go/pkgcore/config` 新增声明驱动的解析入口

新增一个不经过字面 Go struct 反射、直接由声明列表驱动解析的入口。精确函数签名与内部实现留给实施轮决定（比照 26 号文 §4.4 的先例："改写是内部实现选择，本节定义语义与测试契约"），但必须满足：

- 输入是一组 `(Key string, Format string)`（或直接接受 `[]pkgcore.BootstrapKey`；签名选择需要避免在 `pkgcore/config` 引入对 `pkgcore` 根包的导入依赖——`pkgcore/config` 是依赖地板的一部分，不得反向依赖 `pkgcore`。若 `BootstrapKey` 类型本身值得跨这条边界传递，签名可以取 `[]string`/一个不依赖 `pkgcore` 的最小声明结构体，仿 26 号文 §3.4 `Verify(target any, declared []string) error` 的先例）。
- 复用现有 `collect`/`coerceTextValues`/`applyKeyMaterial`/`checkEmpty`/`sourcesFor` 的解析逻辑：五源优先级（flag > env > file > 派生 > 声明默认）、`hexkey` → `[]byte` + `derive`（含显式值/根密钥派生/默认三段优先级）、`string`/`int`/`bool` 的文本转换规则，必须与今天字面 struct 反射路径**逐条同构**，不得产生第二套解析或校验语义。
- 输出是按声明 `Key` 路径寻址的解析结果（`map[string]any`），不写回任何 Go struct 字段——可以直接喂给 `pkgcore.NewBootstrapMaterial`。
- **Format 闭集校验、`Sensitive`+`Description` 配对、跨组件重复 `Key`**：这条新路径必须真正执行这三项校验（今天在 Component 世界完全空缺，见 §1），失败时给出装配错误目录的四要素（阶段/组件/原因/补救），不复用 legacy `validateBootstrapKey` 的实现（两边的错误呈现面不同：一个是 `Add` 时的库内错误，一个是 Prepare 拍①的装配错误），但语义对齐。

### 3.2 `go/pkgcore`：统一解析入口

`go/pkgcore/component_assembly.go`（或 loader 侧，实施轮按耦合关系判断哪边更合适）用 §3.1 的新入口统一解析全部已注册组件的 `BootstrapKeys`，产出 `pkgcore.BootstrapMaterial`——`bootstrap_material.go` 本身不需要改动，输出契约不变。

### 3.3 `go/app`：`resolveBootstrapMaterial` 改道，`PlatformConfig` 删除

- `go/app/loader.go` 的 `resolveBootstrapMaterial`/`lookupDeclaredKey` 改为直接调用 §3.1/§3.2 的新入口取值，不再做"在 Host/Platform 结构体上找字段"这一步。`LoadSpec.Platform` 字段随之失去存在理由（六枚平台密钥不再需要任何宿主/引擎结构体字段）。
- `go/app/config.go` 删除：`PlatformConfig`、`PlatformAuthnKeyMaterial`、`PlatformConfigKeyMaterial`、`PlatformNotificationKeyMaterial`、`PlatformOrgKeyMaterial`、`PlatformPKIKeyMaterial`、`platformKeyPaths`、`platformCipherKeyPath`、`verifyBinding`；`ConfigSpec.Platform` 字段删除，`loadConfiguration` 相应简化（不再单独 `Load` Platform target，不再跑 `pkgconfig.Verify(spec.Platform, platformKeyPaths)`）。
- 引擎构建平台 cipher（消费 `config.cipher_key`，即原 `platformCipherKeyPath` 消费点）的逻辑，改为从新解析出的 `BootstrapMaterial` 按键路径查询取值，而不是从 `PlatformConfig.Config.Cipher_Key` 字段取值。
- **`DevPlatformConfig()` 的落点（已裁定，不留给实施轮）**：CLAUDE.md 的 `task dev`/`go run ./cmd/server` 零外部依赖启动纪律，以及多条 flowtests 对六密钥开发默认值的依赖，要求这条便利路径必须存续，不能因为结构体消失就悄悄丢掉。**裁定：Option A——开发期默认值不进 `BootstrapKey`/模块声明**，继续保持声明层零 Go 类型信息（与 26 号文 §3.2 的边界逐字自洽，不重开"声明层要不要放 Go 默认"这个已经裁定过的问题）。零配置开发启动改由**装配器（引擎或宿主）侧的非声明性 dev-default 表**承担：一个按声明键路径寻址的 `map[string][]byte`（或等价形状），作为 §3.1 新入口里"声明默认值"这一优先级层的实际提供者——它占据的正是今天 `PlatformConfig` 字段被 `DevPlatformConfig()` 预填之后，在五源链里充当"struct 默认值"（最低优先级，低于根密钥派生）这个位置，只是载体从"预填的结构体字段"换成"装配器传入的表"，优先级语义逐字不变。这个表由**想要零配置开发体验的装配方**提供（reference-app 自己的 dev 引导路径、saasctl 生成骨架各自的 `Dev*` 常量），不是模块，也不是 `go/pkgcore/config` 包本身内置——`go/pkgcore/config` 只提供"接受一个可选 dev-default 表"这个机制位，不预置任何值。`DevPlatformConfig()`/`Dev*` 常量本身作为 Go 值继续存在，只是从"预填一个结构体"改成"构造这张表"，函数改名与迁移细节留给实施轮，但形态（表、最低优先级、非声明字段）不再是开放问题。

### 3.4 env 变量拼写不变

`PlatformConfig` 的六个字段今天没有任何 `config:"env=…"` pin，走的是"前缀 + 键路径大写、点转双下划线"的通用派生（`EnvName(prefix, key)`）。新入口对没有 pin 的声明键，同样应该走这条通用派生路径——这是纯 Go API 层面的重构，`APP_AUTHN__PII_CIPHER_KEY`、`APP_CONFIG__CIPHER_KEY` 等既有环境变量拼写逐字保留，不改变任何已发布/已文档化的名字。复核方法：跑现有 `flowtests`/`server_config_equivalence_test.go` 一类的等价性测试（或新增一条），断言迁移前后同一份 env 注入集产出完全相同的解析结果。

## 4 模块侧：零改动

`go/authn`、`go/config`、`go/pki`、`go/org`、`go/notification` 的 `component.go`/`module.go` 里的 `bootstrapKeyDecls` 不需要任何改动——它们已经是完整声明。这是本文对"模块只需要维护现有声明，不需要配合引擎改动"承诺的直接验证点：实施完成后，这五个文件的 diff 应该是零。

## 5 reference-app 影响

- `server.go`：删除 `ServerConfig` 里的 `speedapp.PlatformConfig` 内嵌字段（108-121 行）、`b.hostConfig.PlatformConfig = cfg.PlatformConfig`（553 行）、`speedapp.ConfigSpec{..., Platform: &b.hostConfig.PlatformConfig}` 里的 `Platform` 传参（667 行）。
- `bootstrap.go`：删除 `hostConfig` 里的 `speedapp.PlatformConfig` 内嵌字段（311-321 行）、`loader.Load(&hc.PlatformConfig)`（527 行）、`DevPlatformConfig()` 调用点（475 行，落点按 §3.3 决定）、`config.Verify(&hc.PlatformConfig, []string{key.Key})` 这一步宿主侧绑定校验（820 行——六枚密钥的校验改由引擎新入口的 Format 闭集校验承担，宿主不需要再验一遍）、693 行的字段搬运。
- 迁移后，reference-app 编译通过、既有 flowtests/等价性测试全绿，即证明六枚平台键的实际解析结果（值、env 名）逐字未变。

## 6 测试面

1. 新入口对四种 Format 的 flag/env/file 解析、优先级、`hexkey` 的显式值/派生/默认三段优先级——逐条对照今天字面 struct 反射路径的既有测试断言，确保语义不漂移。
2. Format 闭集校验、`Sensitive`+`Description` 配对、跨组件重复 `Key`：三条校验各自的错误四要素（阶段/组件/原因/补救）。
3. `go/app` 端到端：一个只声明 `BootstrapKeys`（不匹配任何 `PlatformConfig` 式宿主结构体字段）的测试组件，验证装配能正确解析并发布 `BootstrapMaterial`。
4. reference-app 编译 + 既有 flowtests/等价性测试全绿。
5. `PlatformConfig` 删除是公开 Go 符号变更：登记宿主可见变更清单或带 `!BREAKING` footer。

## 7 风险与开放问题

| 风险/问题 | 说明 |
|---|---|
| 新入口被误用到"宿主定制键"场景 | 必须在 godoc 与本文写清使用边界：只用于没有独立宿主定制意愿的声明键（即今天 `PlatformConfig` 覆盖的场景），不是 host 自有 bootstrap 结构体的替代品——29 个 reference-app 宿主键继续走 26 号文机制 |
| 新入口与 `describe()`/`walk()` 反射路径共享多少内部实现 | 实现层面的选择；约束是语义必须与反射路径逐条一致，不允许出现第二套优先级/校验规则 |
| 零配置开发启动的存续 | 已裁定（§3.3 Option A：装配器侧非声明性 dev-default 表，最低优先级，逐字复刻今天"预填结构体字段"的语义）；实施验收项：`task dev`/`go run ./cmd/server` 零外部依赖起、既有依赖六密钥开发默认值的 flowtests 全绿 |
| `PlatformConfig` 删除影响的下游 | reference-app 与 saasctl 生成骨架（五个 `selection/*/server.go` 变体 + `cmd/server/config.go`）是仓内已核实的消费者（§1）；configrefgen 预期免疫（26 号文 D2b 已改读描述符），仍需实施时以树为准跑 `--check` 复核，不能只凭历史记录假设 |

## 8 与 26/29 号文的关系

- 本文不改写 26 号文 §3.2 原文；26 号文 §9.9 追加一条落地记录指向本文，记录"声明层不放 Go 类型信息"的判断对宿主定制键继续成立、对引擎自有镜像场景不再成立。
- 本文是 29 号文 `BootstrapKeys` 字段消费契约的精确化，不改变 29 号文的七阶段模型、`ComponentRegistry` 核心结构或声明席语义。
