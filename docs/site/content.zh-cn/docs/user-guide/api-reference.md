---
title: API 参考
weight: 5
bookToC: false
description: "完整的平台 API 参考,由十一平台模块的合并 OpenAPI 契约渲染——speed 服务暴露的每个操作、参数、schema 与错误。"
---

# API 参考

speed 服务的完整 HTTP API,由十一平台模块(authn、notification、billing、
admin、ai-gateway、integration、org、pki、sharing、storage、config)的合并
OpenAPI 契约渲染。契约是单一真源——先契约后代码,生成面锁步——所以
你在这里看到的就是组合服务在 `/api/v1/...` 上实际应答的内容。

浏览前值得知道的几点:

- **认证走访问令牌**:请求携带 `Authorization: Bearer <token>`。没有
  tenant 请求头——租户在令牌声明内,由服务自己的中间件解析,绝不取自
  请求。
- **错误是结构化的**:错误应答携带机器码(`module.snake_case`)、
  trace id 与参数。码的约定与完整清单见
  [错误码索引](../error-codes/)。
- **操作按模块 tag 分组**;每个 tag 背后的模块在各自的
  [模块页面](../modules/) 有完整介绍,[领域指南](../domains/)
  则把整个产品流程从头走到尾。

<div id="redoc"></div>
<script src="/apidocs/redoc.standalone.js"></script>
<script>
  Redoc.init('/apidocs/speed.yaml', {
    hideLoading: true,
    requiredPropsFirst: true,
    expandDefaultServerVariables: true,
    scrollYOffset: 8
  }, document.getElementById('redoc'));
</script>
