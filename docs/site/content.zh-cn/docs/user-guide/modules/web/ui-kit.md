---
title: "@speed/ui-kit"
weight: 3
description: "平台第一个渲染 DOM 的包——createAppTheme 与 AppThemeProvider 把 @speed/tokens 树映射成 MUI v9 主题,七个受控组件(PageHeader、EmptyState、ConfirmDialog、FormField、FormLayout、DataTable、FileUploader)只渲染宿主给的状态。"
---

# @speed/ui-kit

平台第一个渲染 DOM 的包:把 `@speed/tokens` 变成 MUI v9 主题的主题
工厂(`createAppTheme`、`AppThemeProvider`),外加七个受控组件——
`PageHeader`、`EmptyState`、`ConfirmDialog`、`FileUploader`、
`FormField`、`FormLayout`、`DataTable`——它们只渲染宿主给的状态。
一切内置文案住在双语 `ui-kit` 命名空间里,经 `@speed/i18n` 注册
——绝不把文本写进代码。

## 它做什么

- **主题工厂。**`createAppTheme(projectTokens, tenantOverrides)` 把
  每层可选的令牌层以写时复制的 `deepMerge` diff 压在
  `defaultTokens` 上合并,再把结果映射成 MUI v9 主题:调色板角色逐
  键对应,中性梯度进 `palette.grey`(带 MUI 自己的 A 槽别名),排印
  字号映射到各 variant 角色,间距/断点/z-index 直通,六个海拔阴影
  压平进 MUI 的 25 槽梯度。结果刻意不带 locale:`AppThemeProvider`
  在渲染期把 `i18n.language` 对应的 MUI locale 合并进去,订阅
  `languageChanged`,渲染一次 `CssBaseline`,不渲染
  `I18nextProvider`(那属于宿主启动)。
- **七个受控组件。**状态经 props 流动;组件把它回显出来、触发变更
  回调——从不取数、从不改数据、从不存业务状态。全族唯一一处交互
  局部细节——`ConfirmDialog` 的双重确认武装——弹窗一关就复位。

## 何时选用

当你要在 MUI v9 上搭 speed 前端、想要现成的骨架而不是自己手搓等
价物时——会话族包与外壳包就是叠在这些组件上的。MUI 被假定为宿主
的组件库:`react`/`react-dom`、`@mui/material`(^9)、
`@emotion/react`/`@emotion/styled` 与 `react-hook-form` 都是必选
peer。

## 接线与最少使用

```tsx
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'

const i18n = createI18n()
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>{/* 页面 */}</AppThemeProvider>
    </I18nextProvider>
  )
}
```

`registerNamespace` 在任何变更前校验两种语言的叶子键集对等,每个
实例只跑一次,在启动时——绝不在组件里。想改措辞的宿主注册自己的
双语资源对到 `UI_KIT_NAMESPACE` 名下;`uiKitResources` 只是随包发
布的默认值。

## 核心 API 与使用要点

- **`PageHeader`**——页面的语义 `h1`、可选的面包屑跟在带标签的
  `nav` landmark 里(末级 `aria-current="page"`;没有 `href` 的级
  永不交互),外加可选的描述与尾部操作区。
- **`EmptyState`**——现成的 `empty`/`noPermission`/`error` 三种占
  位,标题与描述来自命名空间的双语文案;
  `title`/`description`/`action`/`icon` 可覆盖它们,图标按契约纯
  装饰。标题是真实标题元素,级别由 `headingLevel` 选(默认
  `'h6'`):只有宿主知道它前面是什么,请传能续上页面顺序的级别。
- **`ConfirmDialog`**——受控:`open` 显示,`onConfirm`/`onCancel`
  上报两条出路——Escape 与点背景永远走 `onCancel`。`'danger'` 变
  体把确认钮涂成错误角色,配合 `doubleConfirm` 只在第二次点击时触
  发 `onConfirm`(第一次点击把按钮改标);武装点击还会让按钮在
  `CONFIRM_ARM_LOCKOUT_MS`(600ms)内保持惰性——双击不可能跳过这
  道闸。
- **表单族。**页面自持一个 react-hook-form `useForm`;`FormLayout`
  安装 `FormProvider` 上下文,给了 `onSubmit` 就渲染接好
  `handleSubmit` 的 `<form>`,字段流带统一 `spacing`(默认 2)与右
  对齐的 `actions`,另有可选 `columns={2}`——`sm` 断点以下退回单
  列。`FormField` 把一个 `Controller` 绑到 `name` 上,经 `render`
  prop 把绑定好的字段状态与解析好的错误文案交给宿主的控件
  (`field`、`invalid`、`isTouched`、`required`、`errorMessage`、
  `errorText`);`required` 在 `rules` 没定义时注入 `form.required`
  规则。ui-kit 命名空间键消息渲染成它的翻译,其它一律原样;
  `REQUIRED_ERROR_KEY` 导出内置键。
- **`DataTable`**——完全受控:`rows` 就是宿主此刻想显示的行,绝不
  被重排或重切。排序与过滤是状态回显加回调
  (`sort`/`onSortChange` 带 `aria-sort`;`filter` 逻辑留在宿主);
  传 `onSelectionChange` 开启选择,行键由 `rowKey` 决定;
  `pagination` 渲染页脚(`count: -1` 表示未知总数);行非空时加载只
  显示状态行。表格永远渲染在可横向滚动的 `TableContainer` 里;列
  上可选的 `priority`(`high`/`medium`/`low`)让该列在该档断点以下
  隐藏——纯 CSS,未设的列永不隐藏。
- **`FileUploader`**——同一套完全受控契约用在上传上:队列从宿主自
  己的 `rows` 状态渲染(状态 `uploading`/`succeeded`/`failed`,可
  选 `progress` 分数,可选原样显示的 `error`),每次选择、取消、重
  试、移除都经 `onSelectFiles`/`onCancel`/`onRetry`/`onRemove` 上
  报。传输——校验、往返、进度、中止——是宿主代码;`File` 活不过
  上报它的那个处理器。

## 边界与注意

- **不取数、不存业务状态、不写校验规则。**上传校验与传输是宿主代
  码——ui-kit 不提供任何上传端点;真实宿主的传输通常经 api-sdk
  接缝调一个生成的 storage 操作。后端错误码除非是 ui-kit 键,否则
  原样渲染;没有码到文案的解析器,也没有生成类型推导的校验——错
  误文案契约就是接缝。
- **标题层级归宿主。**默认 `'h6'` 只是保住老行为——真实页面上请
  显式传 `headingLevel`(以及 DataTable 的 `emptyHeadingLevel`)。
- 命名空间注册之前不要渲染组件:缺键会渲染成键本身——启动时注册
  一次,绝不在组件里注册。
- 语言落在 MUI locale 表之外不会在渲染中途抛错:MUI 内置文案退回
  它自己的 en-US locale,你自己的翻译继续按活动语言渲染。

## Source

- [web/packages/ui-kit/README.md](https://github.com/vislake/speed/blob/main/web/packages/ui-kit/README.md)——权威文档(逐组件契约、文案键表、无障碍说明)
- [web/packages/ui-kit/AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/ui-kit/AGENTS.md)——包规则与记录在案的决定
- 相关:它映射的令牌树在 [tokens](/zh-cn/docs/user-guide/modules/web/tokens/);复用其 `EmptyState` 的骨架在 [layout-kit](/zh-cn/docs/user-guide/modules/web/layout-kit/);领域叙事见[构建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
