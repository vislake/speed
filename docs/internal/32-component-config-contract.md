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
