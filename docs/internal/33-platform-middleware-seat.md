# 33. 平台级中间件声明席

## 1. 契约

`ComponentRegistry` 的十个声明席之外新增第十一个：`Middleware`。它解决的是组件路由声明覆盖不到的一类需求——一个组件需要包住**每一个**请求，包括被鉴权或租户中间件拒绝的请求，而不是只包自己挂载的路由子树。

组件自己路由子树的中间件不需要这一席：`net/http.Handler` 本身可组合，组件在调用 `reg.Routes.Mount(path, handler)` 之前自己套装饰器即可。`Middleware` 只服务"必须站在整条固定链最外层"的场景。

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

`ComponentRegistry.Middleware MiddlewareRegistrar` 字段只在 `Init` 阶段开放写入，跟其余十席一样，`Init` 收尾后冻结。

## 2. 边界：不改变固定链顺序

`go/app/chain` 的固定顺序（`authn.Middleware` 最外层 → `AdminRoutes`/`AuthnRoutes` 结构性豁免分支 → `tenancy.Middleware`+白名单 → `Protected`）保持不变，是平台定死的顺序，`Middleware` 声明席不能、也不提供任何方式插入到这条链的内部——它只能包在 `chain.Chain` 输出结果的最外层。

站在这一层的中间件天然运行在鉴权之外：它拿到的是未鉴权的原始请求，不能依赖 `authn.Principal` 或租户上下文，只能做无状态的、对请求内容不敏感的旁路工作（追踪、指标、panic 恢复这一类）。一个试图借这一席读取租户/身份信息的组件是误用，代码评审应当拒绝。

## 3. 装配期消费

`go/app/chain.Standard`（唯一"从已装配 registry 读取组装"的入口）在 `Chain(cfg)` 产出固定链的最终 `http.Handler` 之后，按声明顺序叠加这一层：

```go
handler, err := Chain(cfg)
...
for _, mw := range slices.Backward(reg.Middleware.Middlewares()) {
    handler = mw(handler)
}
return handler, nil
```

先注册的包最外层。直接调用 `Chain(cfg)`（不经过 registry）的调用方，如果需要这层能力，自行读取 `reg.Middleware.Middlewares()` 手动叠加。

## 4. 组件侧的声明方式

一个需要这一席的组件在自己的 `Init` 回调里声明：

```go
Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
    return reg.Middleware.Add(Middleware)
},
```

`go/observability` 的组件（`go/observability/component.go`）是这一席的首个、也是当前唯一的成员：它的 tracing/metrics 中间件必须看到每一个请求，包括鉴权失败的请求，因此声明在这一席上，而不是由宿主在组装 HTTP 服务器时手写调用。
