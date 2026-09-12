# 32. 组件配置契约

## 1. 契约的形状

一个组件对配置的需求分两类，只由 `Component.ConfigSchema` 一个字段表达：

- **进程启动期配置**：命令行 flag、环境变量、可选配置文件、root-key 派生这四源合并出的值，解析一次，装配完成后不再变化。
- **运行时动态配置**：`configs` 表里的值，按租户/平台维度存储，运维在线编辑，组件通过 `config.Module` 的 handle 在请求期自己读取，从不通过 `ConfigSchema`/构造参数传入。这一层与本文档无关，保持现状不变。

`ConfigSchema` 描述的是第一类。组件在注册时声明一个指向 struct 的类型化 nil 指针作为 `ConfigSchema`；引擎在 `New` 被调用之前，把这个 struct 的每个字段按其声明解析完毕、合并好优先级，`New` 收到的 `ComponentConfig` 解码出来就是最终值——组件不关心某个字段的值到底来自 flag、环境变量还是配置文件。

这是一份双向契约：组件声明"我需要什么形状、什么来源类别的配置"，引擎负责"解析、合并、交还成品"。组件永远不必自己判断优先级，也永远不必到 `Prepare` 回调里手工查一张按 key path 寻址的材料表。

## 2. 字段声明词汇

`ConfigSchema` 的字段用 `config` struct tag 声明行为，复用一套五源解析管线（flag > env > 可选配置文件 > root-key 派生 > 声明默认值）：

- 没有 `config` tag 的字段：只被配置文件覆盖，行为等价于过去朴素的 `ComponentConfig.Decode`。这是默认状态——大多数配置项不需要 flag/env 入口。
- `config:"expose"`：该字段额外参与 flag/env 解析。
- `config:"derive"`：该字段是 `[]byte` 类型的 32 字节 key material，走派生/声明默认值这一档；隐含 `expose`。
- `config:"required"`：五源都未提供值时，装配在字段解析阶段直接失败，不留到组件内部才发现零值。
- `config:"env=NAME"`：钉死环境变量名，不使用按 key path 派生的默认拼写。
- `config:"-"`：字段完全不参与任何来源解析（既不属于 flag/env，也不属于配置文件），组件自己在构造之外的路径填充它。
- `config:"sensitive"`：纯文档标记，供 `--help`/生成的配置参考文档打码；一个标了 `sensitive` 的字段必须同时提供对应的 `ConfigDocs()` 说明，否则装配失败。
- `config:"group=NAME"`：纯文档分组标记。

字段的人话说明（`Description`/`Default`/`Example`，操作层面的文字，不是可运行的 Go 值）不塞进 tag 字符串，由 `ConfigSchema` 目标类型可选实现：

```go
type FieldDoc struct {
    Description string
    Default     string
    Example     string
}

type Documented interface {
    ConfigDocs() map[string]FieldDoc // key 是字段本地名，不含命名空间前缀
}
```

## 3. 命名空间

一个组件的 `ConfigSchema` 字段解析出的 key path，默认取 `components.<组件名>.<字段名>`——这与该字段在配置文件里本来就占据的地址完全一致，只是现在同一地址也向 flag/env 开放。

`Component` 用 `ConfigNamespace string` 字段控制这个前缀：

- 留空：使用默认的 `components.<Name>.` 前缀。
- `pkgcore.NoConfigNamespace`（`"-"`）：不加前缀，字段暴露成裸路径。
- 其他非空字符串：整体替换默认前缀（规范化为以 `.` 结尾）。

装配阶段对所有已选中组件展开出的最终 key path 做一次全集去重校验——任意两个组件（不论各自命名空间取值如何）产出同一个 key path 视为装配错误，报错点名两个来源，不允许运行期悄悄互相覆盖。

## 4. 装配期流程与包边界

`ComponentConfig` 传给 `New` 之前的处理分两层，分别居于不同的包：

- `go/pkgcore`（根包）只定义一个零依赖接口：

  ```go
  type ComponentConfigResolver interface {
      ResolveComponentConfig(componentName string, schema any, fileConfig ComponentConfig) (ComponentConfig, error)
  }
  ```

  `Prepare` 阶段为每个已选中组件算出配置文件片段后，若注册表里存在通过 `reg.Put` 放入的 `ComponentConfigResolver`（`GetOptional[ComponentConfigResolver]`），调用它取得五源合并后的结果；否则维持"仅配置文件"的朴素行为。根包不引入解析器依赖——`koanf` 等解析工具链只存在于 `go/pkgcore/config` 子包。

- `go/app`（已经依赖 `go/pkgcore/config` 的五源解析器）实现 `ComponentConfigResolver` 的具体版本，在 `reg.Prepare()` 之前把它 `reg.Put` 进注册表。`New`/`Prepare` 回调签名不变（`New(ctx, reg, cfg ComponentConfig) (any, error)`，组件内部仍然 `cfg.Decode(&c)`）——变化的只是 `cfg` 在传入前经过的合并处理。

`go/pkgcore/config` 包新导出一个只读反射投影，供解析器实现（`go/app`）与工具侧复用：

```go
func Describe(target any) ([]FieldSummary, error)
```

投影给出每个可解析字段的本地 key path（json tag 名，或没有 json tag 时的小写 Go 名，嵌套 struct 以点连接）、Go 字段名、类型与全部 tag 选项（`expose`/`derive`/`required`/`sensitive`/`env`/`group`/`-`），不含命名空间前缀——组件名与 `ConfigNamespace` 不属于这个包的知识，由调用方叠加；`"-"` 字段以 Skip 标志保留在投影里，因为排除配置文件来源正是解析器要按它执行的动作。

根包一侧的 `pkgcore.DescribeComponentSchema(componentName string, schema any) ([]FieldDescriptor, error)` 与 `Describe` 同口径、但实现各自独立：根包不得 import `go/pkgcore/config`（依赖地板约束），因此根包自携分析器，而不是在 `Describe` 之上拼出；两份投影对同一 schema 的字段判定、键拼写与选项词汇由测试对齐（同 schema 经两条投影面产出逐字段一致的本地键与选项）。解析器侧另用 `Declaration` 的钉死环境变量名承载 `env=NAME`（声明管线的字段读取本就有 pin 席位，声明结构补上这一字段）。

命令行 `--help` 渲染与配置参考文档生成器共享根包这一份 `FieldDescriptor` 收集逻辑。

## 5. 平台级密钥材料的归属

一个 key 只有在同时满足以下三条时，才不落在任何组件的 `ConfigSchema` 里，而是作为独立于组件构造参数存在的平台级材料声明：

1. 密钥身份不属于任何具体组件实例；
2. 没有一个组件的构造天然拥有它——它是被注入的秘密，不是某组件的构造参数；
3. key path 本身承担派生稳定性契约（重命名即轮换），必须保持全局扁平，不随组件重命名或多实例化而变化。

绝大多数模块自己的密钥材料（例如一个模块自身消费的对称密钥、盲索引 HMAC 密钥）不满足这三条中的第二条——它们是该模块 `ConfigSchema` 的 `derive` 字段，直接落在该模块自己的命名空间下。真正符合三条判据、没有天然组件归属的极少数平台密钥，走独立于 `ConfigSchema` 的声明路径,由装配阶段统一解析,同样纳入第 3 节的全局 key path 去重校验。

**当前无成员**：本仓库六个既有平台密钥（`authn.pii_cipher_key`、`authn.blind_index_key`、`config.cipher_key`、`notification.contact_index_key`、`org.invitation_email_index_key`、`pki.local_key_cipher_key`）逐一对照三条判据，无一满足第 2 条——每一个都是所在组件构造（`New`）或 Prepare 的输入——已全部迁入各自组件的 `ConfigSchema` `derive` 字段（五个组件以模块名作 `ConfigNamespace`，六条 key path 逐字保持，派生 purpose 不变），独立声明路径（`Component.BootstrapKeys`）本体保留但仓内无成员。够格标准即三条判据本身，逐条实例化：① 密钥身份不属于任何具体组件实例——例如一个被多个模块或有宿主侧消费者共享、且其身份不随任何单一组件命名的密钥；② 没有任何组件的构造天然拥有它——它不是被某组件的 `New`/`Prepare` 消费的构造输入；③ key path 承担派生稳定性契约且必须全局扁平——不随组件重命名或多实例化改变。三者同时成立才回到独立声明路径；届时该 path 同样纳入第 3 节全局去重，且任何迁移都不得改动 path（重命名即轮换）。

## 6. 实施收尾记录（终态核查）

本节是"§1–§5 契约条目 ↔ 实现与文档终态"的对照清单，供终态验收逐条核对：每行给出实现位置（现树中的符号）与可复跑的证据（测试名或命令）。本节只登记终态事实，不引入新契约；实现所涉的宿主可见面在 31 号文 §5.27–§5.30 另有面向发布的对账登记。

### 6.1 §1–§5 逐条对照

| 契约条目 | 实现位置 | 证据 |
|---|---|---|
| §1 `Component.ConfigSchema` 是进程启动期配置的唯一声明位；`New` 收到五源合并后的成品 | `pkgcore/component.go`（`Component.ConfigSchema`）、`pkgcore/component_assembly.go`（`analyzeComponentSchema`/`resolveComponentConfigs`）、`pkgcore/config_resolver.go`（`ComponentConfigResolver` 接口）、`go/app/component_config.go`（引擎实现） | `pkgcore/config_schema_test.go`、`pkgcore/component_assembly_test.go`；`go/app/component_config_test.go` 的五源阶梯 |
| §2 `config` tag 词表：`expose`/`derive`/`required`/`env=NAME`/`-`/`sensitive`/`group=NAME`（derive 隐含 expose；`-` 独占）；无 tag 字段仅配置文件 | `pkgcore/config_schema.go`（`parseSchemaTag`，装配分析与根投影共用）；子包等价读法 `pkgcore/config/describe.go`（`parseFieldTag`） | `pkgcore/config_schema_test.go` 与 `go/app/component_config_test.go` 的 `TestResolverComponentConfig_KeyPathsMatchTheRootProjection`（两投影逐字段对齐） |
| §2 `FieldDoc`/`Documented`；sensitive 字段必须带非空 Description 说明，否则装配拒绝 | `pkgcore/config_schema.go`（`Documented`/`FieldDoc`）、`pkgcore/component_assembly.go`（`validateSensitiveDocs`，`ErrInvalidComponent`） | `pkgcore/component_assembly_test.go` |
| §3 命名空间：默认 `components.<Name>.`、`pkgcore.NoConfigNamespace` 去前缀、自定义串规范化；最终 key path 全集去重（`ErrConfigKeyConflict`） | `pkgcore/config_schema.go`（`configKeyPrefix`）、`pkgcore/component_assembly.go`（`validateConfigKeyPaths`/`claimConfigKeyPath`）；引擎镜像于 `go/app/component_config.go`（`componentConfigPrefix`） | `pkgcore/component_assembly_test.go`；六平台键以平铺路径渲染（`go run ./cmd/server --help` 输出） |
| §4 根包只定义零依赖接口；解析器实现与 `koanf` 工具链只在 `go/app` / `pkgcore/config` | `pkgcore/config_resolver.go`、`go/app/component_config.go`；`go/app/loader.go`（`Load` 在 Prepare 前 `reg.Put`，宿主自带解析器时尊重既有者） | `go/app/component_config_test.go` 的 `TestLoad_PublishesTheComponentConfigResolver`、`TestResolverComponentConfig_NoDeclarationsKeepTheBlock` |
| §4 `pkgcore/config.Describe` 只读投影，`"-"` 字段以 `Skip` 保留、json `"-"` 字段不列 | `pkgcore/config/describe.go` | `pkgcore/config/describe_test.go` |
| §4 根包 `DescribeComponentSchema` 自携分析器（不 import 子包），与 `Describe` 同口径 | `pkgcore/config_schema.go`（`DescribeComponentSchema`/`analyzeConfigSchema`） | 对齐测试（上）+ `pkgcore/config_schema_example_test.go` |
| §4 `Declaration.Env` 承载钉死变量名；声明驱动入口跑同一五源管线 | `pkgcore/config/declaration.go`（`Declaration`/`ResolveDeclarations`/`WithDevDefaults`）；`go/app/loader.go`（`declaredBootstrapKeys` 折叠 `BootstrapKeys` 席位与 schema `derive` 两面） | `pkgcore/config/declaration_test.go`、`go/app/loader_test.go` |
| §4 命令行 `--help` 渲染与配置参考生成器共享同一份 `FieldDescriptor` 收集逻辑 | 收集：`pkgcore.DescribeComponentSchema`（唯一实现，两方各自调用、无拷贝）；帮助渲染：`go/app/config_help.go`（`CollectComponentConfig`/`RenderComponentConfigHelp`，sensitive 字段文档格打码 `[redacted]`）；参考生成器：`tools/configrefgen/host.go`（`schemaDeclaredKeys`）；真实入口：`examples/reference-app/cmd/server/main.go`（`runHelp`，`--help`/`-h`/`help`，经 `pkgcore.GlobalComponents()` 只读声明、不组装） | `go/app/config_help_test.go`、`go/app/example_test.go` 的 `ExampleRenderComponentConfigHelp`、`examples/reference-app/cmd/server/main_test.go`（`TestRunHelp_*`）；实跑 `go run ./cmd/server --help` |
| §5 独立声明路径本体保留（`BootstrapKeys` 席位、`BootstrapMaterial`、`BootstrapKeyPurpose`），仓内无成员 | `pkgcore/bootstrap_key.go`、`pkgcore/bootstrap_material.go`、`pkgcore/bootstrap_key_purpose.go`；两面折叠与冲突拒绝在 `go/app/loader.go` | `pkgcore/bootstrap_key_purpose_test.go`、`pkgcore/component_assembly_test.go`、`go/app/loader_test.go` |
| §5 一键一层校验只管密钥材料（普通 schema 字段与同名 runtime item 共存不拒） | `pkgcore/component_registry.go`（`validateOneLayerPerKey`） | `pkgcore/component_registry_test.go` |
| §5 六平台键逐字保持（path 即派生身份，迁移零轮换） | 五组件 schema：`go/authn/component.go`、`go/config/component.go`、`go/notification/component.go`、`go/org/component.go`、`go/pki/component.go`（`ConfigNamespace` 各取模块名）；导出路径常量 `authn.PIICipherKeyPath` 等 | `examples/reference-app/flowtests/server_config_test.go` 的 `TestDeclaredMaterial_RootKey_DerivesAllSixKeys` |

文档面与上表同行：根包的 `Documented`/`FieldDoc`/`FieldDescriptor`/`DescribeComponentSchema` 作为宿主可见面记在 `go/pkgcore/AGENTS.md` 的组件配置契约一节，`pkgcore/config` 的 `Describe`/`FieldSummary`（含 `Skip`）与 `Declaration`（含 `Env`）/`ResolveDeclarations`/`WithDevDefaults` 记在同文件的 `pkgcore/config` 一节；`--help` 收集与渲染记在 `go/app/AGENTS.md` 的引擎与测试两节；宿主接入（`--help`/`-h`/`help`、`EnvPrefix` 的导出缘由）记在 `examples/reference-app/README.md` 的 Running it 一节。

### 6.2 六键全链（声明→解析→消费）复核

- **声明**：六键分别是五组件 `ConfigSchema` 的 `derive` 字段（`authn.pii_cipher_key`、`authn.blind_index_key`、`config.cipher_key`、`notification.contact_index_key`、`org.invitation_email_index_key`、`pki.local_key_cipher_key`），在模块名命名空间下取平铺平台路径；`configrefgen` 的 bootstrap 层与上文 `--help` 面从同一收集读出同一组路径，与 `go/saasctl/internal/config` 的 `config print` 变量清单逐字一致。
- **解析**：引擎 loader 在 Prepare 前把两面声明的键经 `pkgcore/config` 的声明驱动入口解析，按路径发布 `BootstrapMaterial`（`go/app/loader.go` 的 `resolveBootstrapMaterial`）。证据：`examples/reference-app/internal/app/bootstrap_material_test.go` 的 `TestDeclaredMaterial_EnvSpellingAndThreeTierPrecedence`（开发默认表 / `APP_ROOT_KEY` 派生 / 显式 `APP_<模块>__<键>` 变量三档，逐键核 env 拼写与优先级）、`flowtests/server_config_test.go` 的 `TestDeclaredMaterial_RootKey_DerivesAllSixKeys` 与 `TestDeclaredMaterial_RootKey_IndividualOverrideWins`。
- **消费**：真实二进制整机启动实测（`go run ./cmd/server`，仅开发默认表供键）——pki 以本地密钥密文键创建根 CA 与中间 CA，authn/config/notification/org 各以其声明键完成构造（PII 密文键、盲索引键、configs 表密文键、联系人索引键、邀请邮箱索引键），`/healthz` 与 `/api/v1/config/public` 均答 200，SIGTERM 后干净关闭。构造侧逐项交付证据另见 `examples/reference-app/internal/app/descriptor_delivery_test.go`，HTTP 流证据见 `examples/reference-app/flowtests/`。

### 6.3 披露（终态事实，非行动项）

- **命名空间的两处读法**：`pkgcore.DescribeComponentSchema` 按进程级注册读 `ConfigNamespace`（供渲染），装配期 key path 去重与引擎解析器按传入注册表的描述符读（供解析）。仓内两个宿主都以全局注册为种子、覆盖只换回调不改命名空间，两面一致；仅当宿主在自有注册表上以「进程级已注册名 + 不同 `ConfigNamespace`」注册时才会分歧——仓内无此形状，如实登记。
- **打码边界**：`--help` 面把 sensitive 字段的文档格（Default/Example）渲染为 `[redacted]`；配置参考文档的 bootstrap 表把声明文本（如 "documented non-secret development default"）原样渲染在 Unset fallback 列。两者面向不同的读者（运行期运维输出 vs 提交物），共同契约是"从不呈现任何值本身"；本清单登记该边界，避免把两处文案差异误读为漂移。
- **未标 sensitive 的既有字段**：如 `pkgcore/objectstore/s3` 的 `secret_key`/`access_key` 等 schema 字段未带 `config` tag（既无 expose 也无 sensitive，无文档格），`--help` 相应只列路径、不列任何值。marker 与否是声明内容问题，不影响渲染行为；此处只登记现状。
