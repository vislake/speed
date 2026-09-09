---
title: account-ui
weight: 9
description: "已登录的账号管理组件族——为什么读走生成式 react-query hooks 而非会话操作、为什么 MFA 状态靠行为发现且 step-up 提升从不活过一枚访问令牌、为什么恢复码只出现一次。"
---

# account-ui

`@speed/account-ui` 是基于 speed 的前端的账号管理组件族:`@speed/auth-ui`
家族结束之处的账号故事已登录一半。四个区块组成宿主账号页——会话与设
备,含单会话与批量撤销;登录历史;社交绑定,含在宿主回调路由完成绑定
的回调处理器;step-up 门控的双因子开通。[用户指南的 account-ui
页](/zh-cn/docs/user-guide/modules/web/account-ui/)讲怎么用;本页讲
为什么。

## 职责与边界

这些表面是区块,不是路由页:拥有它们的页面与包外的一切(资料字段、
密码设置)都是宿主内容。边界沿用的纪律与同层家族一致:

- **读走生成进 `@speed/api-sdk` 的 react-query hooks、经宿主的
  QueryClient**;写走同一接缝的生成 mutation。这里
  不读存储、不 attach 会话、不导航、不直连网络。
- **session 以 prop 出现,恰好只在生成面表达不出的会话操作处**——
  添加区的授权 URL 请求、挑战对话框的 step-up 验证。两个只读区块
  完全无 prop:身份来自调用方绑定的客户端。
- **不 import `@speed/auth-ui`。** 同层包互不 import:provider 词
  汇是复制的、形态逐字一致,由 authn spec 保持同步。
- **表面只能做 spec 有的事。** 没有 factor-status、disable 或改密
  操作,所以 `MfaSection` 靠行为发现状态,而非声明状态。

## 为什么读走生成 hooks 层

账号面读的是列表——会话、历史、身份——它们是**可缓存的共享状态**,
在各自 mutation 后失效重取:这正是 api-sdk 契约的生成 hooks 层,也
是 auth-ui 零-hooks 表单的刻意对照——登录回答是一次性的,不是缓
存。这个选择解释了其余形态:宿主树在 auth-ui 所需 provider 之外多
一个 `QueryClientProvider`,`@speed/api-sdk` 在此是运行时依赖(不
是 type-only),失效走导出的 query-key 构造器而非手写 key。

## 各区块为何如此成形

**会话列表就是服务端的列表。** `SessionsSection` 原样渲染 authn
模块的回答:当前会话按服务端自己的 `is_current` 标记,已撤销的行留
在列表灰显。当前会话永远不能从列表撤销。撤掉别的单条会话损失低、
无需二次确认;"登出其他设备"是更重的动作,置于双重确认的危险对话
框后,`revoked_count` 回答经 `role="status"` 通知播报。

**历史只经白名单渲染服务端词汇。** 最新二十条窗口冻结、刻意不分页:
登录历史是最长尾表面,而本区块是只读账号页,不是搜索工具。方法值与
失败原因值只有在其已知 token 清单上才过 `t()`——未知一律渲染通用
文案,绝不显示裸值;会话 `amr` 值渲染为不透明引用,刻意不译。

**解绑不可逆;绑定是纯请求。** 行尾解绑置于危险 `ConfirmDialog`
后;`authn.last_login_method` 这类拒绝留在页面、显示码文本。添加
区向会话要每个渠道的授权 URL——经 `onAuthorizeUrl` 上报,绝不导
航。`BindingCallbackHandler` 用普通生成调用完成流程,绝不是会话操
作:绑定是给调用者自己的账号加身份,不把任何人登入。其 effect 以
`(code, state)` 对为键,StrictMode 只发起一次交换;回答形态分派结
果:绑定形态的回答失效身份列表并触发一次 `onBound`;登录形态的回答
意味着调用者的登录已死、交换把另一个账号登了进来——处理器渲染"已
在别处登录"面板,什么都不触发。

**MFA 没有状态,只有行为。** 因为 spec 没有 factor-status 操作,
状态靠行为发现:调用者令牌没有新鲜的第二因子证明时,门控动作答 403
`authn.step_up_required`;开通的 200 打开向导。这个 200 是首次开通
还是更换,由调用者自己的提升决定、绝不由状态码决定——所以带着温热
令牌重进开通流程,仍会在确认(会作废上一批恢复码)前看到更换警告。
step-up 对话框驱动 `session.verifyStepUp`:成功落定一枚 `amr` 已带
因子的新访问令牌,而提升只活在那枚令牌的生命期里——对话框从不承诺
之后不再询问。恢复码明文只发一次:一次性面板是它们唯一出现处,想看
再见只能重新生成。被服务端报为已用的码——输掉的竞争也在内,码已在
服务端验证过、即使提升从未落定也已作废——绝不供重新提交;错码则是
字段级错误、可重试。开通是手工录入,一项有记录的决策:无 QR 与剪贴
板依赖,向导以文本展示 secret 与 provisioning URI。

## 错误文案:可达码白名单

每个失败归一到单个码,经一个 `role="alert"` 横幅渲染(例外:错 MFA
码是字段级错误)。白名单覆盖会话生命周期族、社交绑定码、双因子与
step-up 码、`authn.rate_limited` 与 `client.*` 传输码;其余一律渲
染 `errors.unknown` 兜底,绝不显示裸 key。失败语境与登录面相同的
码逐字复用 auth-ui bundle 的文案。

## 对外稳定面

五个导出组件、prop 表都小——两个只读区块无 prop;`SocialBindingsSection`
多 provider 清单与 `onAuthorizeUrl`;`BindingCallbackHandler` 多
`code`/`state` 对与 `onBound`;`MfaSection` 收 session——加上
`SocialProvider`/`SocialProviderConfig` 与双语
`ACCOUNT_UI_NAMESPACE`/`accountUiResources` 对。

## Source

- [web/packages/account-ui/README.md](https://github.com/vislake/speed/blob/main/web/packages/account-ui/README.md)——契约、prop 表、错误白名单与 Known limitations
- [docs/internal/12-frontend.md](https://github.com/vislake/speed/blob/main/docs/internal/12-frontend.md)——account-ui 实现注记(生成 hooks 层、按行为的 MFA)
- [docs/internal/05-identity-and-access.md](https://github.com/vislake/speed/blob/main/docs/internal/05-identity-and-access.md)——表面背后的身份设计:会话与撤销、MFA 与 step-up、社交绑定规则

## 相关页

- [前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)——分层与两种数据层
- [auth-ui 设计](/zh-cn/docs/developer-docs/modules/web/auth-ui/)——本家族是其已登录延续的登录一半;[authn 设计](/zh-cn/docs/developer-docs/modules/identity/authn/)——两者共同驱动的后端表面
- 用户指南:[account-ui 模块](/zh-cn/docs/user-guide/modules/web/account-ui/)、[authn 模块](/zh-cn/docs/user-guide/modules/identity/authn/)
