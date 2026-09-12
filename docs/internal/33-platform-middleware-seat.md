# 33. 平台级中间件声明面

## 1. 契约

平台级中间件是 http 组件（`go/app/httpserve`）产物的两个声明面之一（另一个是路由面 `RouteRegistrar`；两面由同一实例实现，消费方按各自需要的 token 取用）。它解决的是组件路由声明覆盖不到的一类需求——一个组件需要包住**每一个**请求，包括被鉴权或租户中间件拒绝的请求，而不是只包自己挂载的路由子树。

组件自己路由子树的中间件不需要这一面：`net/http.Handler` 本身可组合，组件在调用 `RouteRegistrar.Mount(path, handler)` 之前自己套装饰器即可。中间件面只服务"必须站在整条固定链最外层"的场景。

```go
// MiddlewareRegistrar collects the platform-wide handlers that must wrap
// the assembled chain's outermost layer -- outside authn.Middleware,
// seeing every request including one authn or tenancy will refuse.
type MiddlewareRegistrar interface {
    // Add registers middleware, applied in registration order: the first
    // added wraps outermost.
    Add(mw ...func(http.Handler) http.Handler) error
    // Middlewares returns every registered middleware, in registration order.
    Middlewares() []func(http.Handler) http.Handler
}
```

`MiddlewareRegistrar` 是契约 token，定义在 `pkgcore`，由 http 组件作为产物提供；http 组件的注册器实现按装配的阶段读数（`(*ComponentRegistry).Stage()`）拒绝 `Init` 之外的写入，跟注册表自持席位时的门禁逐字同形。

## 2. 边界：不改变固定链顺序

`go/app/chain` 的固定顺序（`authn.Middleware` 最外层 → `AdminRoutes`/`AuthnRoutes` 结构性豁免分支 → `tenancy.Middleware`+白名单 → `Protected`）保持不变，是平台定死的顺序，中间件面不能、也不提供任何方式插入到这条链的内部——它只能包在链输出结果的最外层（有链策略下由 `chain.Standard` 应用；无链策略下由 http 组件直接包在受保护 mux 之外）。

站在这一层的中间件天然运行在鉴权之外：它拿到的是未鉴权的原始请求，不能依赖 `authn.Principal` 或租户上下文，只能做无状态的、对请求内容不敏感的旁路工作（追踪、指标、panic 恢复这一类）。一个试图借这一席读取租户/身份信息的组件是误用，代码评审应当拒绝。

## 3. 装配期消费

http 组件在 `Serve` 阶段读出累积声明并组装 face：有链策略时 `chain.Standard`（唯一"从累积声明读取组装"的入口）在 `Chain(cfg)` 产出固定链的最终 `http.Handler` 之后，按声明顺序叠加这一层；无链策略时组件自己把同一层包在受保护 mux 之外：

```go
for _, mw := range slices.Backward(middlewares) {
    handler = mw(handler)
}
```

先注册的包最外层。

## 4. 组件侧的声明方式

一个需要这一面的组件声明可选依赖并在自己的 `Init` 回调里取用：

```go
Requires: []pkgcore.Requirement{{Token: (*pkgcore.MiddlewareRegistrar)(nil), Optional: true}},

Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
    registrar, ok, err := pkgcore.GetOptional[pkgcore.MiddlewareRegistrar](reg)
    if err != nil || !ok {
        return err // 纯后台组合没有 http 组件：跳过声明
    }
    return registrar.Add(Middleware)
},
```

`go/observability` 的组件（`go/observability/component.go`）是这一面的首个、也是当前唯一的成员：它的 tracing/metrics 中间件必须看到每一个请求，包括鉴权失败的请求，因此声明在这一面上，而不是由宿主在组装 HTTP 服务器时手写调用。
