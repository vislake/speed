# 35. Serve 阶段与 http 组件

## 1. Serve 阶段

生命周期在 `Start` 之后增加一个阶段：

```
Prepare → Construct → Verify → Init → Start → Serve
```

`Serve` 的语义是**入口开始接受外部请求**：HTTP 监听、队列消费、调度触发，凡是把外部流量引入进程的动作都在这一阶段执行。

它必须是独立阶段，而不是 `Start` 的一部分，原因是顺序的表达能力：

- `Start` 按依赖序执行，提供方先于消费方。这对**资源**是正确的——消费方需要它依赖的东西已经在跑。
- 入口不一样：**入口的流量可以打到任何组件，而不只是它自己声明的依赖**。它需要的是"排在全体之后"，而依赖序只能表达"排在我的依赖之后"。一个不被入口依赖的组件（例如只处理后台任务的模块）与入口之间没有边，其 `Start` 完全可能晚于入口。

`Serve` 因而是全体 `Start` 完成之后才开始的一整轮，参与者在其中按依赖序执行。与其余阶段一致：描述符声明了 `Serve` 回调才参与，未声明的组件不受影响；`Serve` 失败与 `Start` 失败一样触发逆序回滚。

## 2. 入口的关停次序

入口必须**最先**停止接受新请求，让在飞请求对着一个仍然完整的系统排空，之后其余组件才被通知停止。

`Stop` 因此分两拍：先通知声明了 `Serve` 的组件（成员之间按逆依赖序），再按逆依赖序通知其余组件。`Close` 不变——逆依赖序等待排空并释放资源，恰好一次。

## 3. 阶段的只读读取

`ComponentRegistry` 导出当前阶段的只读读法。

声明面从注册表内建席位下放为组件产物之后，"某个声明只能在 `Init` 阶段写入"的门禁不再由注册表统一强制；提供声明面的组件需要自己复刻这个保证。阶段读取是它们据以拒绝越界写入的唯一依据，因此属于装配契约的一部分，而不是某个组件的内部细节。

## 4. http 组件

http 组件提供进程的 HTTP 服务：它持有路由与中间件的声明面，组装最终的 `http.Handler`，拥有监听器。

```
Provides:  (*pkgcore.RouteRegistrar)(nil), (*pkgcore.MiddlewareRegistrar)(nil)
Requires:  宿主的链路策略（非可选，*httpserve.LinkPolicy）
```

- **落位**：组件在 `go/app/httpserve`（`go/app` 的子包），提供面的单实例是 `*httpserve.Face`。它位于依赖图中 `chain` 之上：组装固定链要 import `go/app/chain`，而 `chain` 反向 import `go/app`（`PreAuthAllowlist`）——组件因而不能住在 `go/app` 根包（成环），`pkgcore` 也不能反向依赖 `chain`，所以**链路策略类型 `LinkPolicy` 定义在组件侧**（消费方定形自己的依赖契约），`chain` 保持无策略，`pkgcore` 只留契约 token 与共享类型。
- **两个 token，一个产物**：同一个实例同时实现路由注册与中间件注册两个接口，消费方按各自需要的 token 取用——只加中间件的组件（观测）不必依赖路由注册面。
- **链路策略由宿主提供，且非可选**：验签器、授权表、admin 前缀、模拟身份、租户状态门、额外的免鉴权路由、宿主自有路由的挂载钩子、宿主自有的外层包装，都是宿主策略，不是平台能替宿主决定的默认值。策略缺失时装配失败，而不是退化成一个没有鉴权的裸路由表。`LinkPolicy` 另带 `Chainless` 正声明（无 authn 模块的组合显式声明"无固定链"；携带受链半边字段或受链策略缺验签器都在组装时被拒）。
- **`Init` 阶段**：不做组装——此时其他组件仍在挂载路由。
- **`Serve` 阶段**：读出累积的路由与中间件，按宿主策略组装固定中间件链（`chain.Standard`；`Chainless` 时受保护 mux 直接成脸、累积路由由组件自行挂载），叠加声明的平台级中间件（链输出最外层），再套宿主外层包装，最后打开监听。此时全体 `Init` 与 `Start` 均已完成。平台活性路由（observability 的 `/healthz`、`/metrics`）与路由标签种子也在此处落位，顺序与宿主机时代逐字一致。
- **`Stop`**：停止接受新请求（`Stop` 的第一拍）。**`Close`**：等待在飞请求排空，释放监听。
- **配置**：监听地址、读头超时与关停超时经自身 `ConfigSchema` 解析（`components.http.*`），另有 `listen`（默认 true）声明只组装不开监听——宿主把组合好的 handler 交给自己的调用方时使用（reference-app 的 BuildServer 形态）。
- 其产物满足 `chain.RouteSource`，固定中间件链的组装沿用 `chain.Standard`，顺序契约不变。监听器的请求基上下文与 Serve 阶段上下文的取消解耦（`context.WithoutCancel`），关停信号不会先于排空取消在飞请求。

## 5. 声明面的迁移

`Routes` 与 `Middleware` 不再是 `ComponentRegistry` 的内建席位，而是 http 组件的产物（注册表席位因此从十一个降为九个）：

- `ComponentRegistry` 移除这两个席位字段及其访问器、`MountedRoutes()`/`Middlewares()` 读法，与随席位存在的两个内存注册器。
- 挂载路由的模块声明 `Requires{Token: (*pkgcore.RouteRegistrar)(nil), Optional: true}`，在 `Init` 中经 `pkgcore.GetOptional` 取到才挂载（`pkgcore.MountRoute` 是该惯用形的一行版：无提供者时是合法空操作）。可选是必需的：一个不服务 HTTP 的组合（纯后台进程）里模块照常构造，只是不挂路由；而宿主的链路策略是非可选依赖，所以任何真的对外提供 HTTP 的组合必然把 http 组件带进来。
- 声明平台级中间件的组件同理，依赖 `MiddlewareRegistrar`（observability 是首个成员）。
- 写入门禁由 http 组件自己按阶段读取强制：`Init` 之外的挂载被拒绝（`Mount` panic、`Add` 返回错误，均包 `ErrStageViolation` 并点名阶段），而不是静默写入一个已经组装完毕的路由表。
- 测试面共享的记录器在 `pkgcore/componenttest`（`NewFaceRecorder`/`FaceOf`），声明体驱动与读取走同一实例。

`RouteRegistrar`/`MiddlewareRegistrar`/`MountedRoute`/`RouteAccess`/`MountRoutes` 作为契约 token 与共享类型留在 `pkgcore`；实现它们的组件在依赖图中位于 `chain` 之上；`ErrNilMiddleware` 留在 `pkgcore`（接口自身的拒绝契约）。

## 6. 宿主的落位

宿主不再拥有监听器：它提供链路策略，并把自身的路由与外层包装表达在策略里。两个仓内宿主按同一条路径迁移：

- 宿主组件提供 `*httpserve.LinkPolicy`（构造期交付，解析 http 组件的非可选依赖），内容在**晚于全体模块 `Init` 的宿主步骤**里填充——策略内容读的是运行时服务（如 rbac 的授权服务），而计划序会被模块指向路由面的可选依赖边提前拉动，所以填充位置由一条指向宿主引导步骤产物的要求边结构化钉住，不靠种子序。reference-app 的填充与该宿主的 Init 级订阅、演示种子同住在 pre-serve 步骤里；模板五变体没有种子链，填充落在 post-bootstrap 步骤尾。
- 宿主的链参数、手工路由与 SPA 外层包装全部进策略；SPA 仍是整条组装结果的最外层（33 号文登记的边界行为原样保持，由 webdist 边界钉测试从两侧继续钉住）。
- reference-app 的演示种子改经路由面累积表构建的种子 mux 驱动（`Init` 阶段即可得；种子所依赖的寄存器路由、口令策略与限流器都在模块自己的 handler 内，链路对未鉴权的注册 POST 是透传），自服务订阅仍在种子之后安装，判别式与监听窗口前提不变。
- `RunAssembly` 的 `ServeFunc` 保持为宿主级的等待钩子，不再是监听器的归属地；`BuildServer` 形态经策略所在组件的配置把 `listen` 置 false，读 http 组件的产物 `*httpserve.Face` 取组合好的 handler。
