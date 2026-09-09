---
title: 错误码索引
weight: 99
---

# 错误码索引

API 应答你的结构化错误码才是客户端要处理的契约——绝不是一句散文式消息。
本页说明 speed 错误码的约定与处理方式;每个码的完整清单(HTTP 状态、
locale 消息、触发条件、Go 源码出处,按模块分组)是英文页面:

[查看完整错误码表(English)](/docs/user-guide/error-codes/)

## 约定

- 每个码形如 `module.snake_case`:`authn.invalid_credentials`、
  `rbac.permission_denied`、`tenancy.tenant_unresolved`。
- HTTP 状态与码一一对应:400/401/403/404/409/500 由 `apperr` 的六个
  构造器映射,429 与个别非常规状态以 `&apperr.Error` 字面量或模块内
  辅助函数构造。
- 码的语义由 Go 源码里**构造它的那一处**决定(声明或内联构造);完整
  表的「触发条件」列直接取自该处上方的文档注释,与实现零漂移。

## 客户端处理

- 按码分支处理,不按状态码或消息文本:状态码区分大类,消息文本会随
  locale 变化,码才是稳定标识。
- 需要展示给用户的文案来自**客户端自己的 i18n 资源**,由码索引到双语
  文本——完整表的「Message」列只显示该码在 `en-US` locale 目录中的
  条目,供对照语义;无条目(`_(no locale message ...)_`)表示该码是
  启动期接线拒绝之类永不抵达终端用户的错误,或抵达时客户端以自己的
  fallback 文本兜底的请求期拒绝。
- 建议做法:维护本应用可达码的 whitelist,每个码给双语文本与兜底文案;
  参考应用前端即如此(codes-alignment 套件把 whitelist 钉在服务端码上)。
