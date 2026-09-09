---
title: "@speed/tokens"
weight: 1
description: "设计令牌树,无依赖的纯数据——带类型的 defaultTokens 组装与写时复制的 deepMerge 覆盖机制;@speed/ui-kit 的 createAppTheme 把它映射成 MUI v9 主题。"
---

# @speed/tokens

平台的设计令牌,以无依赖的纯数据形式存在:类型、`defaultTokens`
树,以及 `deepMerge` 覆盖机制。零运行时依赖,没有 React,没有
CSS-in-JS——消费者(首先是 ui-kit 的主题工厂,也包括需要原始值
的应用)只 import 数据与类型。

## 它做什么

`@speed/tokens` 是所有 speed 前端共用的视觉真相唯一来源:

- **语义调色板**——六个角色,各带
  `main`/`light`/`dark`/`contrastText`,外加 50 到 950 的中性 slate
  梯度与 text/background/divider 面。
- **字体排印**——拉丁优先、以可渲染中日韩的 fallback 收尾的字体
  栈,12 到 48px 字号、字重、行高与字距。
- **间距**——一个单位,8px。
- **圆角**——8px。
- **断点与 z-index**——`xs`..`xl` 取值,以及 MUI 的八个 z-index
  槽名与其 MUI 默认值。
- **阴影**——六个海拔槽(1/2/4/8/16/24)的分层 rgba 阴影。

组装好的 `defaultTokens` 树在约定**与**运行时两个层面都不可变:类型
里各段是 `readonly`,树在模块加载时被深冻结。覆盖从不靠改树——改树
靠 `deepMerge`,这个写时复制机制也归本包所有:结果与默认树按同一性
共享未动过的分支,只重建动过的分支。

它**不是**主题包:这里没有任何东西认识 MUI、React 或 CSS。它不带
任何用户可见文案,也不发布 locale——它是数据,不是 UI。

## 何时选用

任何建在 speed 前端上的宿主都用它,无论直接引用还是经
`@speed/ui-kit`(它的 `createAppTheme` 把 `defaultTokens` 当基底
层)。要在非 MUI 代码里取原始令牌值,或产品需要项目级、租户级的视
觉身份时,直接用它:每个覆盖层都是压在默认值上的一层 `deepMerge`
diff,白标租户的覆盖可以只是几个令牌,而不是一份主题分叉。

## 接线与最少使用

```ts
import { defaultTokens, deepMerge, type TokensOverride } from '@speed/tokens'

const override: TokensOverride = {
  color: { semantic: { primary: { main: '#0F766E' } } },
  zIndex: { values: { drawer: 1400 } },
}
const tokens = deepMerge(defaultTokens, override)
```

`TokensOverride` 就是 `DeepPartial<SpeedTokens>`,所以形状漂移——
多写了一段、该写十六进制的地方写了字符串——是**编译期**错误,绝
不会拖到运行时。覆盖整支与覆盖单个令牌是同一个调用;层能叠加,因
为后写的覆盖胜出。

## 核心 API 与使用要点

- **`defaultTokens`**——组装好的树,组装时深冻结。直接改这棵树,
  或改任何 `deepMerge` 结果与它按同一性共享的分支,在严格模式下会
  抛错,而不是悄悄污染每一份覆盖都从它出发的基底。
- **`deepMerge(base, ...overrides)`**——覆盖机制,语义全部被包的
  测试钉死:不改输入(写时复制:没动过的分支保持同一性,动过的分支
  重建);`undefined` 覆盖值被跳过,所以局部覆盖永远不可能抹掉一
  个令牌;普通对象递归合并,数组与其它任何值整体替换;后写覆盖胜
  出;敌意的 `__proto__` 键落成不可枚举的自有属性,对一切拷贝面不
  可见,结果的原型与下游任何展开拷贝都不会被污染。
- **MUI 对等行由测试钉死。**必须与 MUI 一致的令牌行——
  `breakpoints.values` 与 `zIndex.values`(间距单位 8 本来就是 MUI
  的)——拿安装好的 MUI 主题真实 `createTheme` 默认值对测(只作为
  dev 依赖,包自身运行时保持零依赖)。MUI 大版本改动这些默认值会
  先在这个包失败,不会等某个主题适配器带着悄悄错误的样式上线。
- **一条记录在案的分叉。**`shape.borderRadius` 发 8,MUI 默认 4
  ——刻意的产品决定,并有测试:若 MUI 默认值有一天收敛到令牌值,
  该测试失败。

## 边界与注意

- 令牌是数据;**适配决定**——树怎么映射成 MUI 主题(中性梯度到
  `palette.grey` 的别名、排印角色、压平到 25 槽的阴影梯度)——住在
  [ui-kit](/zh-cn/docs/user-guide/modules/web/ui-kit/) 的
  `createAppTheme` 里,在那里记录并由测试钉死。中性梯度的 950 号在
  MUI 没有灰槽,刻意不映射。
- 不要改合并结果的共享分支:那些分支就是冻结默认树自己的节点,写
  穿它们会抛错(严格模式),而不是弄脏基底。要新结果就再压一层覆
  盖。
- `deepMerge` 是定义在 `SpeedTokens` **之上**的;合并你自己形状的
  数据请用自己的合并函数——这里的防敌意键与防改写保证只对这一棵
  树的组装成立。
- 本包不发布 i18n 资源;没有要注册的命名空间,没有要翻译的文案。

## Source

- [web/packages/tokens/AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/tokens/AGENTS.md)——包规则与记录在案的决定
- 相关:消费这棵树的主题工厂与组件在 [ui-kit](/zh-cn/docs/user-guide/modules/web/ui-kit/);前端整体叙事见[构建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
