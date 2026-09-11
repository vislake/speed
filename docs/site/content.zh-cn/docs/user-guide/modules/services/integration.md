---
title: integration
description: "租户对外的 API 面:发给脚本与伙伴的 API key、三层限流,以及把内部事件作为签名 HTTP 调用投出的外发 webhook。"
weight: 4
---

# integration

integration 是租户对外的 API 面:租户发给自家脚本与第三方系统的
API key,保护平台、租户与单个 key 互不挤占的三层限流,以及把业务
模块的内部领域事件变成 HMAC 签名 HTTP 投递的外发 webhook。

## 它做什么

**API key。** `Service.Create` 生成一把原始 key 并恰有一次返回给调
用方;存下来的只有它的 SHA-256 哈希与一段明文展示前缀。每把 key
都带强制的到期(`WithMaxAPIKeyLifetime`,默认一年——永不过期的 key
被拒绝而非钳制)。`Rotate` 签发替换并撤销前任;`Revoke` 打标;
`Authenticate` 只凭哈希把出示的原始 key 解析到它的属主租户,以同
一个对外无差别的 `ErrAuthenticationFailed` 拒绝。作用域在签发时冻
结(经 `PermissionLister` 接缝对照创建者当前权限校验)且永不重推。
`LayeredLimiter` 组合全局/租户/key 限流层;`HTTPGuard` 把拒绝翻译
成带 `Retry-After` 与 `X-RateLimit-*` 头的 429。`AuthMiddleware`
在 HTTP 上运输 `Authenticate`:承载凭据走 `X-API-Key` 头(绝不走
`Authorization`,会话层先占着它)。

**外发 webhook。** 租户订阅(`WebhookSubscription`)公开事件类型;
`EventMapping` 声明由宿主在构造时接入,把业务模块的内部事件映射
到带版本的公开 schema——内部事件绝不裸转。命中时模块向租户的
active 订阅扇出,渲染体只算一次,每个订阅入队一个投递 job(6 次重
试,然后死信)。每次投递都签
`v1=HMAC-SHA256(secret, "<timestamp>.<body>")`,时间戳签在被覆盖
内容之内;SSRF 防两次:创建时一次,每次尝试拨号时再一次——后者才
是击破 DNS rebinding 的所在。`DeleteWebhookSubscription` 打标而
非删除;恢复后的订阅**总是暂停**(`Active = false`)——恢复对第三方
URL 的外发 HTTP 必须是显式动作。`RedeliverWebhookDelivery` 把死信
投递重新入队(Service 级)。

它**不是**什么:不是生产入站网关——`AuthenticatedAPIKey.Scopes`
之上的作用域强制刻意留给你的宿主;webhook 半边没有按租户的投递量
限流;CRUD 面是会话认证的租户管理,刻意不挂在 `HTTPGuard` 后面
(它守护的是 key 认证的流量,不是 key 管理)。

## 何时选用

你的租户需要机器通道:脚本、伙伴系统、CI——任何用长寿秘密而非用
户会话认证、需要按 key 限流与撤销的调用方。你需要把你平台的事件
投到客户端点、签名可验,并希望内部事件 schema 保持带版本与私有。

## 怎么接线

```go
m := integration.NewModule(db,
    integration.WithWebhookQueue(queue), // 投递 handler 排空的 jobs.Queue
    integration.WithEventMapping(integration.EventMapping{
        InternalType:  "org.member.joined",          // 宿主发布过的业务事件
        PublicType:    "org.member.joined",          // 订阅方在 EventTypes 里指名的
        PublicVersion: "v1",                         // 破坏性变更以新映射发布
        Transform:     transformMemberJoined,        // func(ctx, pkgcore.Event) (json.RawMessage, error)——宿主所有
    }),
    // 校验请求作用域是否创建者当前权限的子集
    // (rbac 的 Authorizer 实现同形):
    integration.WithPermissionLister(func(ctx context.Context, tenantID, userID string) ([]string, error) {
        return az.ListPermissions(ctx, rbac.Subject{TenantID: pkgcore.TenantID(tenantID), UserID: userID})
    }),
    // 更多可选接缝:WithMaxAPIKeyLifetime、WithSubjectResolver、WithMembershipChecker、WithAuthenticationGuard
)
```

HTTP 面——`/api/v1/integration` 下十个操作(apikey
创建/列表/轮换/撤销;webhook 列表/创建/更新/删除/恢复加投递日
志)——是对上面 `Service` 方法的薄翻译。时序:`Register` 订阅映射
并认领投递 handler;`Service` 稍后在装配返回后的 `Attach`
里构建。

## 核心概念与 API 面

- **密钥材料永不存储**——哈希查找、前缀展示;webhook `Secret` 是
  唯一的可逆例外(每次投递都要从它重推 HMAC)。
- **对外无差别的拒绝。** `Authenticate` 从不区分 key 为何失败;撤
  销、轮换与 webhook CRUD 把「从未存在」与「属于别的租户」折叠成
  同一个 not-found。
- **扇出幂等。** 投递行带派生的幂等键(订阅、公开类型与版本、渲
  染体、发生时刻),队列任务带同一键,数据库有 UNIQUE 兜底——至
  少一次的总线绝不会对一次发生双投。
- **限流有序。** 全局、再租户、再 key,首个拒绝即短路;零值层等于
  关闭;限流器错误是 500,绝不静默放行。
- **权限切分**是 `integration:apikey:*` 对 `integration:webhook:*`
  (各 read/manage);审计动作覆盖 key 创建/撤销与 webhook CRUD 四
  重奏。

## 已知限制与链接

- `Rotate` 是两次写、不是一笔事务——中途失败留下两把活 key(安全
  方向的盈余,报告给调用方,绝不是锁死)。
- API key 过期清扫(`EnqueueAPIKeyExpirySweep`)是宿主调度的可选工
  作——正确性从不依赖它(`Authenticate` 每次调用查到期;清扫只回
  收磁盘)。
- 发送侧不是恰好一次:传输接受后崩溃的窗口会重发;投递的 payload
  在扇出时只算一次,所以重试永远捡不到映射修复。
- 两个覆写接缝(`WithWebhookURLValidator`、`WithWebhookHTTPClient`)
  只供离线测试——生产宿主绝不能接,也绝不能只接一个。
- 结构化错误码:见[错误码索引(English)](/docs/user-guide/error-codes/#integration)——
  `integration.authentication_failed`、`integration.rate_limited`、
  `integration.webhook_url_blocked`、`integration.scope_not_held_by_creator`
  等。

### 出处

- [go/integration/AGENTS.md](https://github.com/vislake/speed/blob/main/go/integration/AGENTS.md)——权威文档(key 生命周期、接缝、webhook 流水线、裁定、限制)
- 相关页面:[平台服务](../)、[pki](../pki/)、[notification](../notification/)
