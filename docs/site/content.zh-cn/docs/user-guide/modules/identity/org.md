---
title: org
weight: 3
description: "租户的组织树、绑在节点上的成员关系、以及创建它们的邀请——authn 成员检查与 rbac 子树范围授权背后的数据。"
---

# org

org 是 speed 的组织模块:每个租户一棵树、一张绑在节点上的成员名册、
以及创建这些成员关系的邀请。它是两个邻居模块问题背后的数据——authn
失败关闭检查要的成员关系答复、rbac 范围授权要的节点 id 与路径——却
一个也不 import。

## 它做什么

树是 `OrgNode{ParentID, Path, Depth}`——邻接表加物化路径,没有闭包表
也没有递归 CTE:每租户一个根,之下任意深度,节点带业务 `Kind`。
`TreeService` 负责创建、改名、移动与删除;`memberships` 是关联数据
——每人每租户一个席位,绑到一个节点,节点之下的子树就是他们的数据
范围;`InviteService` 签发、投递、撤回并接受那些把邮件地址变成成员的
邀请。对 `authn.user.created` 的订阅——只订阅、从不声明——给全新用
户一个工作区:幂等地确保根与一条成员关系。

刻意**不在**这里:用户表或超出 id 字符串的用户概念(`authn`);成员
关系上的角色(`rbac`);邀请邮件以外的任何消息;以及动态配置——深度
与名字上限、邀请 TTL 与限流预算都是包常量,可经
`WithMaxDepth`/`WithInvitationTTL` 按宿主覆盖,因为 org 不得 import
`config`。

## 何时选用

当成员关系有结构范围:多层组织里成员只能看、只能动自己的子树,名册
按子树读回,邮件邀请作为加入路径。它也是 `authn` 的
`MembershipReader` 与 `rbac` 的 `SubtreeResolver` 两个接缝背后的规范
实现。扁平「组织」同样适用:一个根,成员都在它之下。

## 接线与最少使用

两处接线必选,`Register` 会大声拒绝——地址无法索引的邀请永远找不到,
而 `pkgcore.Mailer` 直接拒绝空的 `From`:

```go
m := org.NewModule(db,
    org.WithEmailIndexer(emailIndexer),      // dbkit.NewBlindIndexer,键控 org.EmailIndexColumn
    org.WithMailFrom("team@example.com"),
    org.WithInvitationLinkBuilder(builder),  // 把令牌变成接受链接
    // 由别处投递邀请的宿主调用 WithInvitationEmailDisabled(),两个邮件选项都不需要
)
```

模块像任何模块一样加入 `Kernel.Bootstrap` 集;访问器是 `Tree()`、
`Members()`、`Invitations()` 与只读的 `Scope()` 视图,并且它在**调用
时**才读宿主的 registry,绝不在 `Register` 期间读。HTTP 面是
`/api/v1/org` 下的十一个操作——节点 CRUD 加移动、子树范围的成员列
表与移除、邀请的创建/列表/接受。宿主除一个操作外全部门控在四个已声
明权限上(`org:read`、`org:manage`、`org:invite_member`、
`org:remove_member`,经 rbac);`accept` 保持不门控——接受一封写给你
的邀请不需要常驻授权——并且**无租户**:刚被邀请的人没有任何租户声
明,宿主须放这一条路径穿过租户解析中间件。创建/接受操作的调用者身份
经 `SubjectResolver` 而来,未接线时失败关闭(`401
org.subject_unresolved`)。

## 核心概念与 API 要点

- **物化路径就是查询索引。** 子树成员是一次带索引的前缀扫描,路径带
  尾分隔符——`/a/` 不是 `/ab/` 的前缀——id 字母表(`[0-9a-f-]`)有
  测试钉住,保证扫描在两个方言上返回相同行。
- **删除绝不静默放宽。** 有成员的节点拒绝删除,除非调用方显式级联;
  删除从不把孤儿重新挂到祖父节点(那会静默放宽成员的数据范围),根
  永不可删;每笔写都并发安全——一行锁或一条数据库仲裁的条件语句。
- **成员关系是每人每租户一个活跃席位。** 名册站在任意节点读整棵子
  树;`Remove` 拒绝移除租户最后一个活跃成员;`TenantsOf` 是唯一的跨
  租户读,以系统上下文为门。
- **邀请就是验证类例外。** 令牌是 32 字节 `crypto/rand`,只返回一
  次,只存其 SHA-256 哈希;地址静态加密,盲索引列(索引键必须与加密
  键不同)。投递按两个维度限流——每租户、每收件人(后者按盲索引键
  控,绝不按明文)——按收件人 locale 渲染;一次新的 `Invite` 取代旧
  令牌,接受是一次创建成员关系的单次比较-交换。
- **只读的 `Scope` 视图**——`Path`、`DescendantIDs`、
  `MemberNodeIDs`——每个签名只用 stdlib 类型,所以 `rbac` 可以声明
  同一接口、无 import 收下 `*ScopeService`;「什么都看不见」是普通答
  案。
- **事件与审计。** `org.node.*`/`org.member.*` 事件是 rbac 的 reap
  订阅对象。写操作经 dbkit 自动捕获进审计:三张租户数据模型带
  `Auditable` 标记,org 以 `AuditableModels()` 导出捕获范围。
- **软删除。** `OrgNode` 与 `Membership` 实现 `dbkit.SoftDeletable`:
  删除即标记,`Restore` 撤销某一行自己的标记——逐节点、绝不级联,
  拒绝落回已死父节点。

## 边界与注意

- 绝不 import `authn` 或其他业务模块的 struct:用户是从事件学来的 id
  字符串。绝不绕过 `TreeService` 写 `OrgNode` 行——它是 `ParentID`
  与 `Path` 保持同步的保证。
- 邮件地址或邀请令牌绝不以外显形式离开模块:不进日志行、事件负载、
  限流键、错误参数或响应——盲索引是离开模块的最具标识性的东西。
- `Restore` 还没有 HTTP 面(仅服务级调用);没有查询能列出某节点已
  标记删除的后代;邀请无人清扫——过期在接受时判定,pending 行按设
  计累积。
- 结构化错误码:见[错误码表(English)](/docs/user-guide/error-codes/#org)
  ——树、名册与邀请各组。

## Source

- [go/org/AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md)——权威文档(树形、邀请、接缝、并发、规则)
- HTTP 片段:[go/org/api/openapi.yaml](https://github.com/vislake/speed/blob/main/go/org/api/openapi.yaml)
- 相关:域指南[身份与访问](/zh-cn/docs/user-guide/domains/identity-access/)与[租户与组织](/zh-cn/docs/user-guide/domains/tenancy-and-org/),以及本组页面[authn](/zh-cn/docs/user-guide/modules/identity/authn/)与[rbac](/zh-cn/docs/user-guide/modules/identity/rbac/)
