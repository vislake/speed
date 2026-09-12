# 34. 目录式交付契约

## 1. 两种交付，一套词汇

一个组件把自己的产物交付给装配时，只有两种语义：

- **绑定式交付**：这个 token 在一次装配里恰好由一个选中组件交付，消费方按类型单值读取（`Get[T]`/`GetOptional[T]`）。两个选中组件交付同一个绑定式 token 是歧义错误。
- **目录式交付**：同一个 token 由若干个选中组件各自交付一份，它们**按名字**被取用，从不被解析成唯一值。支付通道（`gateway.{stripe,alipay,wechat}`）、AI 厂商（`chat.*`/`image.*`）是这一类：多实现共存不是冲突，而正是这类模块存在的理由。

两种语义都在 `Requires`/`Provides` 这一套词汇里表达，消费方和提供方各自显式声明自己属于哪一种。装配器据此决定校验规则、依赖边和读取面——不依赖任何约定。

## 2. 提供方：`Provides` 与 `ProvidesMember`

`Component` 的交付声明分成两个字段：

```go
// Provides declares the bound contracts this component delivers: tokens a
// single selected component owns, read by type through Get.
Provides []any

// ProvidesMember declares the catalog contracts this component delivers a
// member of: tokens several selected components may deliver at once,
// addressed by name and never resolved to a single value.
ProvidesMember []any
```

规则：

- **一个组件只能属于一种交付类型**：同时声明 `Provides` 与 `ProvidesMember` 是非法描述符（`ErrInvalidComponent`）。一个组件要么是某些绑定式契约的唯一提供者，要么是某些目录的一名成员。
- **成员身份由类型 token 确定，不由名字确定**：`Component.Module` 保持它原本的职责——资产归属（迁移集、语言资源的 id 前缀）与诊断分组，不再承担"这个组件是哪个目录的成员"这一含义。目录成员是否真的可赋值给目录的类型，在装配期即可校验，而不是等到取用时的类型断言才暴露。
- **跨组件矛盾即拒绝**：一个 token 被某个选中组件声明为 `Provides`、又被另一个声明为 `ProvidesMember`，是不可能同时成立的两种语义，Prepare 阶段直接失败并点名两个来源。
- 单值交付的重复校验（同一绑定式 token 被两个选中组件交付）只针对 `Provides`；`ProvidesMember` 的多重交付是合法的，不进入该校验。

## 3. 消费方：`Requirement` 的目录形式

```go
type Requirement struct {
    Token    any
    Optional bool

    // Catalog declares that this requirement consumes every selected
    // member delivering Token, by name, instead of one resolved value.
    Catalog bool

    // MinMembers is the smallest member count the consumer accepts. Zero
    // accepts an empty catalog. It is meaningful only with Catalog.
    MinMembers int
}
```

`Catalog: true` 的解析规则：

- **依赖边连向全部成员**：消费方与每一个交付该 token 的选中成员之间都建立依赖边，成员在消费方之前构造，拓扑序因此正确——这是目录式依赖此前完全无法表达、因而对依赖图不可见的部分。
- **成员数下界**：选中成员数少于 `MinMembers` 时装配失败，报错点明 token、实际成员数与要求的下界。`MinMembers: 1` 就是"至少要有一条支付通道"这类约束的表达方式。
- **不做自动拉取**：自动拉取的规则是"候选唯一才拉"，对目录没有意义；目录成员一律由组合配置显式选中。
- **永远不会产生歧义错误**：多重性正是目录的语义。

## 4. 读取面

```go
// Member is one catalog member: the component name it was delivered under,
// and its product.
type Member[T any] struct {
    Name  string
    Value T
}

// Members returns every selected catalog member delivering T, in dependency
// order.
func Members[T any](r *ComponentRegistry) []Member[T]
```

- `Members[T]` 返回 Construct 阶段已经构造好的成员产物，对应"一次构建"的取用形态（装配期枚举成员、建映射表）。
- `Build[T](ctx, r, name, override)` 保持不变，对应"按调用构造"的取用形态：以本次调用解析出的配置（例如租户自己的厂商凭据）作为 `override` 现场构造一份新的成员实例。
- **目录成员的产物不进入按类型索引的单值上下文**：`Get[T]`/`GetOptional[T]` 永远看不到它们。一个目录因此可以容纳任意多个成员，而不会让任何绑定式读取变成歧义。
- `MemberNames(r, module)` 保持为按 `Module` 分组的诊断读法（回答"哪些选中组件实现了模块 X"），不是目录取用面。

## 5. 模块落位

- `ai-gateway` 的 `chat`/`image` 两个厂商目录：厂商组件声明 `ProvidesMember: []any{(*ChatProvider)(nil)}`（image 侧同理），`Gateway` 声明 `Requires: []Requirement{{Token: (*ChatProvider)(nil), Catalog: true}}`，使这条依赖第一次出现在依赖图里；按调用构造的路径（`buildSelected` → `Build[T]` 携带凭据 override）不变。
- 一个目录容纳两个及以上成员是这套契约的最小可用性：同一目录下两个厂商组件同时选中必须装配成功，并各自可被按名取用。
- `examples/reference-app` 作为强制首个消费者，与上述声明面变更同批迁移：它按名字指定模型路由与凭据的既有写法继续成立，消费侧不得留在旧声明形态上。
