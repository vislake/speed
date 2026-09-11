---
title: web 组——设计
weight: 7
description: "web 组设计导览——十二个 @speed 包分五层:每包一句话职责、前端如何消费后端契约,以及贯穿每个包的两条设计线索。"
bookCollapseSection: true
---

# web 组——设计

`web/packages` 下的十二个 `@speed/*` 包是 speed 产品的前端半边,而且重复了后端同款形态决策:不是一个应用,而是同一锁步版本下、由产品宿主组装进自己应用的库。[前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)页画了完整的依赖图;本组导览用每包一句话划分职责、说明本组与仓库 Go 一侧的关系,并点出下面每个包页都会回到的两条设计线索。按层分工:

- **地基,与认证无关**——每个渲染包所坐的地板。`@speed/tokens`——设计令牌树,零依赖纯数据,默认树深冻结,只许经类型化 copy-on-write 差异覆盖。`@speed/i18n`——每宿主一个 react-i18next 实例、协商确定的起始语言、注册前先校验覆盖与键集对等的命名空间,跨语言回退被构造性地设为不可能。`@speed/ui-kit`——把合并后的令牌树映射成 MUI v9 主题的主题工厂,外加七个只渲染宿主所给状态的受控组件。`@speed/layout-kit`——共享应用外壳(`AppShell`、`RouteGuard`),放行/拒绝/待定的裁决以宿主注入的值抵达,包自身不携带任何导航或认证逻辑。
- **唯一 HTTP 客户端**——`@speed/api-client`,手写 HTTP 的唯一归宿:可注入 `fetch`、纯内存令牌存储、静默单飞 401 刷新、保守的瞬时重试、一切失败归一为一个 `ApiError`。以及 `@speed/api-sdk`,合并 API 文档的生成类型面,自身不发任何 HTTP——每次调用都经 `bindRequestFn` 这一条接缝适配到那个客户端上。
- **会话与身份**——`@speed/auth-core`,基于生成 authn 面的纯内存会话状态机;`@speed/auth-ui`,登录组件族;`@speed/account-ui`,登录后的账户页;`@speed/tenancy-ui`,租户切换控件。
- **组装**——`@speed/product-shell`,三分支视图机(登出、登入、会话已死),把外壳、登录族与会话钩子组装在一起。
- **业务只读面**——`@speed/billing-ui`,基于生成 billing 操作的账单文档只读面——领域只读面的范式。

## web 组与 Go 一侧的关系

前端永远看不到 Go 模块——它只消费后端的契约,而且只有两种形态。绝大多数流量走合并后的 OpenAPI 文档:十一个平台模块的片段并入一份 spec,`@speed/api-sdk`(平台操作)与每个产品自己的 app-owned SDK(产品自有操作)都由它生成——[API 契约](/zh-cn/docs/developer-docs/api-contract/)页解释这条分工与生成机制。类型化手写封装不是与生成 hooks 并列的另一条路,而是垫在它下面的一层:config 的两个 pre-auth 端点与其它平台面一样走生成操作,`fetchPublicConfig`/`useFeature` 仍是端点动态配置键之上的逐键映射层——生成类型只能把该 body 记成动态映射。包到模块的对应:authn 模块的操作支撑会话族,config 的端点支撑 `fetchPublicConfig`/`useFeature`,billing 的读操作支撑 `billing-ui`——逐面的映射在[前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)页。

不存在后端契约之处,Go 一侧的纪律被镜像而非消费:`go/pkgcore` 的双语消息目录——语言文件不一致的模块注册即失败——被 `@speed/i18n` 的命名空间注册及其同款键集对等规则镜像,CI 里同一把工具直接查原始文件。令牌树与主题没有 Go 对应物;那些层纯属前端,各页如是说。

## 贯穿全组的两条线索

- **状态流入、事件流出。** 每个组件只渲染宿主经 props 给它的状态,变更经回调上报;包代码里没有任何东西取数、存储、导航或裁决租户。理由是结构性的:路由、身份与数据流是宿主的组装决定,包代码不可能知道——只渲染给定状态,组件才能在任意宿主中保持正确、无服务器也可测。
- **文本要么双语,要么不进代码。** 每个渲染包以自有命名空间携带一对键集相同的 `zh-CN` 与 `en-US` 包,工作区 `speed/no-literal-text` ESLint 规则拒绝包 `src` 里的内联用户可见文本——镜像后端"用户可见文本绝不进代码"的规则。

## 阅读顺序

按自底向上读地基页——依赖序是 [tokens](/zh-cn/docs/developer-docs/modules/web/tokens/)、[i18n](/zh-cn/docs/developer-docs/modules/web/i18n/)、[ui-kit](/zh-cn/docs/developer-docs/modules/web/ui-kit/)、[layout-kit](/zh-cn/docs/developer-docs/modules/web/layout-kit/)——再读随落地而至的 HTTP、会话与组装页。[用户指南对应页](/zh-cn/docs/user-guide/modules/web/)从消费者一侧看同样的十二个包。
