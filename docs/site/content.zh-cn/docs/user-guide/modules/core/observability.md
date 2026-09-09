---
title: observability
weight: 4
description: "由选项而非部署模式决定的 OpenTelemetry 接线;默认开启 PII/密钥脱敏的上下文结构化日志器;以及标签基数受限的通用 HTTP 埋点。"
---

# observability

speed 服务的可观测性地基:OpenTelemetry 初始化——导出器接线由你的
选项决定,从不咨询部署模式;上下文感知的结构化日志器——PII/密钥
脱敏默认开启、无逐调用关闭途径;以及 HTTP 中间件——每次请求一个
span,外加标签基数受限的请求计数/耗时指标。各领域的「必须埋点指标」
(队列深度、投递率、支付结果)属于拥有那些领域的模块;本包提供它们
赖以记录的接线。

## 何时选用

每个二进制都用它:`obs.Init` 在进程启动时跑一次,请求或任务作用域
内的每一行日志都走 `obs.FromContext(ctx)`(绝不自己造日志器),HTTP
`Middleware` 包住你的顶层 mux。唯一真正的决定是选导出器。不传
`WithOTLPEndpoint`(默认)接本地导出器——trace 与指标进 stdout,
兼作开发默认;要 Prometheus 抓取端点的宿主空导入
`go/observability/exporter/prometheus`,它注册 `MetricsHandler`
服务的本地拉取 reader。传 `WithOTLPEndpoint` 则两条信号都经
OTLP/gRPC 推送——但需要空导入 `go/observability/exporter/otlp`,
否则 `Init` 以点名该导入的错误失败。两个导出器家族都住在子包里,
只记日志的消费方永远不会继承 gRPC 或 Prometheus 依赖树;depguard
让根包对两个 SDK 都保持干净。

```go
shutdown, err := obs.Init(ctx, obs.WithServiceName("my-service"))
if err != nil {
    return err
}
defer shutdown(ctx) // 优雅停机

obs.RegisterMountedRoutes(reg.Routes.Routes()) // 流量到达前先喂路由限制器

mux := http.NewServeMux()
mux.Handle("/", obs.Middleware(appHandler)) // 在 tenancy.Middleware 之外,按固定链序
```

`Init` 可跑多次——每次成功调用都会先拆掉上一对 provider——显式空
服务名在任何 provider 构建前就被拒绝。

## 核心概念与 API 要点

- **`FromContext` / `WithLogger`**——日志器来自上下文;`FromContext`
  从活动 span 附 `trace_id`/`span_id`、从租户上下文附 `tenant_id`,
  各自独立可选,缺省回落到 `slog.Default()`。属性键是平台各层共用
  的 `snake_case` 常量(`TraceIDKey`、`TenantIDKey`……)。
- **脱敏,默认开启**——`FromContext` 返回的每个日志器都被包装,
  没有逐调用关闭途径。属性层两条规则:键含敏感词干(`token`、
  `secret`、`password`、`authorization`、`credential`、`key`;
  `token` 用词边界规则,`prompt_tokens` 得以幸存)的,整个值替换为
  `[REDACTED]`;良性键下的秘密形状值(Bearer/Basic 凭据、JWT、
  供应商前缀密钥、URL 查询里的密钥参数)就地打码。关联字段
  (`trace_id`、`user_id`、`job_id`……)绝不脱敏。刻意的边界:良性
  键下的 PII 与提示词形状内容由日志调用点负责——本层是凭据类的
  兜底,绝不是自由文本的主防线。
- **`Middleware`**——经 otelhttp 起 span,记录
  `http.server.request.count`/`duration`,标签只有 method、route 与
  status。两个攻击者可控维度在到达 instrument 前都被限界:method
  标签坍缩到九个标准方法加 `_OTHER`;route 标签上限
  `MaxRouteLabelValues` 个不同值、每个 `MaxRouteLabelLength` 字节、
  仅合法 UTF-8,溢出进 `{overflow}`——垃圾路径永远铸不出序列。
  **`tenant_id` 绝不成为指标标签**;租户关联走 span 属性
  (`AnnotateTenant(ctx)`,租户解析后调用)与日志字段。
  `RegisterMountedRoutes` 用宿主真实挂载前缀喂饱限制器,启动后的
  垃圾洪泛无法把真实路由全部挤进溢出标签。
- **Span 表面**——span 名与 route 属性携带受限的 route 值,绝不含
  原始路径(其分段会把租户与资源 id 带进 trace 后端);查询串永不
  成为 span 属性。`url.path` 是唯一保留真实路径的表面,限长且
  UTF-8 消毒——一个非法字节不能废掉整批 OTLP 导出。

## 边界与注意

- 把中间件挂在 **`tenancy.Middleware` 之外**(固定链序是
  `recover -> request-id/log-context -> observability ->
  tenancy.Middleware -> ...`),业务代码在租户已知后调
  `AnnotateTenant`——span 能越过上下文分叉存活,普通上下文值不能。
- `MetricsHandler` 在空导入 `exporter/prometheus` 之前答 404——
  这是可选加入的契约,生成的消费项目也继承它。
- 手工 `WithLogger` 附上的日志器必须是裸 `slog.New(handler)`,不得
  带 `With` 属性——它们会绕过脱敏层;静态属性放在 `FromContext`
  的结果上。
- 日志消息是常量字符串;一切变量进属性(`duration_ms`、`job_id`
  ……),这是平台日志规则。
- 任何模块都不用 `fmt.Println`/`log.Printf` 打结构化日志——
  `FromContext` 才是通道。

## Source

- [observability AGENTS.md](https://github.com/vislake/speed/blob/main/go/observability/AGENTS.md)
