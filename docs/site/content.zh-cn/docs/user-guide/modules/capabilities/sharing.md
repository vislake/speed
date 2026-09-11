---
title: sharing
description: "公开分享链接:一个受控、全量记录、可即时撤销的入口,让未认证的外部访客查看一个内部资源——五条强制安全规则全部在代码里落实。"
weight: 3
---

# sharing

sharing 是 speed 的公开分享模块:一个受控入口,让未认证的外部访客
查看一个内部资源——病人查看自己的模拟结果、客户查看报告、匿名的
一次性结果页。模块不持任何字节:分享命名一个 `ResourceRef`,宿主的
`ResourceResolver` 模块把一次获准的分享变成真正的内容。

## 它做什么

`Service.Create` 铸造一份分享——抽出 256 位 `crypto/rand` bearer
token、哈希可选密码、解析过期时间——并把原始 token **恰好一次**
交还。`Access`(认证宿主调用)与 `AccessPublic`(真正匿名的访客,租
户单凭 token 解析)把 token 解析成它命名的分享,或拒绝;`Revoke`
即时撤回;`Get`/`List`/`ListAccessLog` 服务所有者自己的视图,含谁
看过、看过几次。过期清扫(`Module.EnqueueExpirySweep`,宿主排程的
`jobs` 任务)标记过期或看次耗尽、已中断的分享,退还中断的查看预
留。一个片段里出两个 HTTP 面:`PathAccess`(`GET /api/v1/sharing/access`
——真正公开,每个响应都带 `Cache-Control: no-store`,密码走
`X-Sharing-Password` 头)与 `PathShares`(五个所有者操作:创建、列
表、获取、撤回、访问日志)。

模块强制五条规则,每条都有通过的测试:tokens 密码学随机;任何分享
都不可能永不过期(`Forever` 请求被拒,显式过期受上限校验,nil 解析
到租户配置的默认、未配置时 30 天);撤销在紧接着的下一次访问检查
即生效,**模块内任何地方都没有缓存**;每次访问——准或拒——都落入
访问日志;每个拒绝理由(未知 token、已撤销、已过期、看次耗尽、密
码缺失或错误)都答同一个外型一致的 `sharing.not_accessible`。

它**不是**什么:无浏览器端密码输入页(路由收密码头;渲染收码并重
发的 HTML 提示是宿主前端的工作);无泛化敏感分类
(`CreateParams.Sensitive` 是调用方提供的布尔,触发
`sharing.share.create_sensitive` 审计动作);自身也守不住 CDN——撤
销要即时,分享页与资源必须声明 `no-store` 且绝不坐在 CDN 后面,这
是模块管不到的部署拓扑决策,只能警告。

## 何时选用

产品内部的一个资源必须让没有账户的人够到:病人的结果、客户的报
告、一次性下载。五条规则存在是因为这个面是 SaaS 最易泄漏的地方—
—强制过期、即时撤销、全量日志与一致外型的拒绝,是分享链接该有的
最低限度。要持有字节的资源就配 [storage](/zh-cn/docs/user-guide/modules/services/storage/);
分享的东西是数据导出就配 [compliance](/zh-cn/docs/user-guide/modules/capabilities/compliance/)。

## 怎么接线

```go
m := sharing.NewModule(db,
    sharing.WithResourceResolver(storageResolver), // 把 ResourceRef 变成字节
    sharing.WithQueue(queue),                      // 武装过期清扫
    // 可选:sharing.WithTenantConfigReader(cfg),让默认过期可按租户调
)
svc := m.Service()

one := 1 // MaxViews 是 *int;nil = 不限次
res, err := svc.Create(ctx, sharing.CreateParams{
    ResourceRef: objectID, // 对 sharing 不透明;是你 resolver 的词汇
    MaxViews:    &one,
    Sensitive:   true,     // 触发敏感创建审计动作
})
// res.Token 恰好返回一次——把它发给查看者:
// https://your.host/api/v1/sharing/access?token=<res.Token>
```

宿主的 `ResourceResolver` 实现通常是 `storage.ObjectService.OpenContent`
的适配(参考应用的 resolver 正是这组合)。公开路由必须在你的
`tenancy.Middleware` 里 allowlist(`sharing.PathAccess`,仅 GET),所有
者路径则像任何模块表面一样上门禁——五个操作上模块自己不判授权。
匿名入口在**投递时**才算消费查看:访问路由只在完整响应体到达查看
者后才记一次受限分享的查看,跑信用账本同款的先预留→确认/退款形
态,一次坏掉的首试绝不耗掉单次分享。

## 核心概念与 API 面

- **库里只有 token 的 SHA-256 哈希。** 泄漏的数据库备份换不来可用
  链接;查找全是哈希比较。
- **`AccessPublic` 单凭 token 解析租户**——经一张刻意很窄的平台数
  据表(`shareTokenIndex`,`AssertNotTenantScoped`),匿名调用方唯一
  拿不出手的东西——然后重入普通 `Access`,每条规则的执行点原地不
  动。全程不涉系统上下文逃生口。
- **行上的 `Share.ExpiresAt` 永不为 nil**:nil 请求解析到默认,显式
  值必须落在有效上限(`MaxExplicitShareLifetime`,30 天;租户配置的
  默认更长时抬到该默认)之内,租户自己的默认原样尊重。
- **未知拒绝便宜,已识别拒绝均等。** 未知 token 一次索引查找即拒、
  不做 argon2id 燃烧(防扫描放大器,有时序测试钉死);每条已识别
  token 的拒绝路径都付同一次恒定时间密码检查。唯一的外向差异:每
  token 错误猜测预算耗尽答 `sharing.rate_limited`,披露了该 token
  存在——作为给猜谜者限速的代价被如实记录。
- **访问日志与查看计数同写原子。** 已准访问的日志行落不了库就把这
  次访问判失败(`sharing.internal_error`),绝不留下无痕的准予;过长
  的日志元数据在写边界截断。
- **结构化错误码**——`sharing.not_accessible`、
  `sharing.expiry_out_of_range`、`sharing.rate_limited`、
  `sharing.resource_unavailable` 等——索引在[错误码索引(English)](/docs/user-guide/error-codes/#sharing)。

## 已知限制与链接

- `clientIP` 只信直连的 `RemoteAddr`,绝不信 `X-Forwarded-For` 等任
  何调用方可给的头;坐在可信反代后面的宿主自己归一化地址。
- 受限 `MaxViews` 分享同一时刻只服务一个查看者:一次投递在途时,
  并发第二次抓取会被拒——多人同时看用不限次分享(或每收件人一
  份)。
- 访问日志的只增纪律是模块约定,不是数据库后盾:表必须保持可擦,
  好让合规制度的保留与擦除路径能走。模块注册保留参与者
  (`sharing.access_log`),宿主的合规清扫因此能收掉旧条目。
- 不交付分享管理前端(`@speed/sharing-ui` 或宿主页面);参考应用直
  接驱动两个 HTTP 面。

### 出处

- [go/sharing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/sharing/AGENTS.md)——权威文档(五条规则、服务协议、租户解析、限制)
- 相关页面:[storage](/zh-cn/docs/user-guide/modules/services/storage/)、[compliance](/zh-cn/docs/user-guide/modules/capabilities/compliance/)、域指南[存储、分享与 AI](/zh-cn/docs/user-guide/domains/storage-sharing-and-ai/)
