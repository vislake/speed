---
title: compliance
description: "治理:保留窗口清扫、被遗忘权编排、导出收集与投递、只读审计查询——一个不发明任何删除或审计机制自身的编排层。"
weight: 4
---

# compliance

compliance 是 speed 的治理层:保留窗口清扫、被遗忘权编排、数据导出
的收集*与*投递、以及只读审计查询。它不发明任何机制——它引起的每
一次物理删除都经业务模块自己的 `dbkit.Repository[T].HardDelete`
(在 `go/dbkit` 里租户绑定、系统上下文门禁),它产生的每一条审计记
录都走 `go/dbkit/audit` 的 `Emit`。它是纯 Go 级 API:无 HTTP 面、
无自己的表。

## 它做什么

三个编排加一个查询 API,全部建在业务模块经 `reg.RetentionSeat()`
向注册表保留席位注册的 `pkgcore.RetentionParticipant` 之上:

- **`RetentionService`**——`SweepTenant(tenant)` 把各注册参与者过了
  保留窗口的软删行物理删除;`EnqueueRetentionSweep`(一个 `jobs` 任
  务,窗口作用域幂等 key)是宿主驱动的排程点;`SweepAllTenants` 经
  宿主提供的 `TenantLister` 覆盖每个租户。
- **`ErasureService.Erase(subject)`**——对一个主体的行立即、绕过保
  留窗口的擦除:无租户上下文、或 `SubjectRef` 的租户与上下文租户不
  符,都会在任何参与者运行、任何系统上下文进入之前被拒。重跑收
  敛:参与者对自己已完成的工作报 `(0, nil)`。
- **`ExportService.Export(tenant)`**——把每个参与者的 `Export` 数据
  收进一份可存储的 `ExportManifest`,经 `ObjectStore` 模块存下,再
  铸造一份 24 小时、单次查看、无密码的 `go/sharing` 分享
  (`Sensitive: true`)完成投递,返回对象 key、清单与分享 token。
- **`AuditQuery`**——租户作用域 `Query`(或系统上下文门禁的
  `QueryAcrossTenants`)、按 id `Get`:对 `dbkit/audit` 既有表的只读
  过滤,维度为 actor、代办管理员、资源、动作、时间范围与结果;
  `RenderAuditReport` 把任意事件切片渲染成 CSV 或 JSON。

它**不是**什么:无表无迁移(持久记录是 `audit_events` 加各参与者自
己已迁移的表);无 HTTP 面(它的字节正是将来某 handler 会设为响应
体的东西);无哈希链、无按时间分区归档;也没有自己的排程——保留清
扫只在宿主入队或直接调用时才跑。

## 何时选用

你的产品存着监管者或合同可能问起的数据:会软删、终须物理清除的
行;可能主张被遗忘权的主体;可能主张把数据带走的租户;必须可查的
审计轨迹。compliance 是编排者——每个参与模块仍然拥有自己的行,而
没注册参与者的清单就是擦除的边界,模块 `AGENTS.md` 里精确写明。只
要审计轨迹本身(不要治理操作),`dbkit/audit` 的仓库是更底层的座
位;compliance 在其上加查询与编排。

## 怎么接线

```go
c := compliance.NewModule(auditRepo, // 与你审计接线共用的同一个 *audit.Repository
    compliance.WithQueue(queue),                 // 武装保留清扫任务
    compliance.WithSharing(sharingModule.Service()), // 武装导出投递
    // 可选:compliance.WithTenantLister(lister)、WithConfigService(cfg)
)
// 放进你的组合选出的组件集。拥有该参与者的业务模块在其组件的
// Init 窗口内注册——参考应用的 notes 模块注册的正是这个形态,回
// 调由它自己的仓库的 HardDelete 与读方法背书:
if err := reg.Retention.Add(
    notes.NewRetentionParticipant(notes.NewRepository(db)),
); err != nil { /* 处理 */ }

// 宿主排程点(周期 tick,按租户):
if err := c.Retention().EnqueueRetentionSweep(ctx); err != nil { /* 处理 */ }

// 被遗忘权,由运营或合规工作流发起:
_, err := c.Erasure().Erase(ctx, pkgcore.SubjectRef{
    TenantID: tenantID, SubjectID: userID,
}, requestedBy)
```

`RetentionParticipant`(声明在 `pkgcore`,经 `Retention` 席位注
册)是 `Name` 加 `Sweep`/`Erase`/`Export` 三个回调;注册时 `Sweep`
与 `Erase` 必填、`Export` 可选,每个回调都被期望调参与者自己的
`dbkit.Repository[T]` 方法——compliance 从不 import、也从不直查业
务模块的表。参考应用的 notes 模块是真实消费方;模块自己的
`AGENTS.md` 枚举了尚未注册参与者的租户作用域所有者。

## 核心概念与 API 面

- **部分失败一等公民,绝不静默。** 一个参与者的回调失败不挡其它
  人;失败按参与者记进返回的 `Errors` 映射与审计记录——只记分类,
  绝不记错误文本(它可能带主体标识符,会被刻进那张什么都删不掉
  的表)。调用照样给自己记审计,并在完整结果旁返回结构化的部分失
  败错误(`compliance.sweep_partial_failure` 等)。恢复是重试,不是
  回滚。
- **`Erase` 绝不跨租户擦除**——上下文门禁加上底下的租户绑定
  `HardDelete`,让跨租户不可擦属性可证;参与者报出的计数即使自己报
  错也保留,审计绝不少报一次不可逆操作真正毁掉的东西。
- **一次导出是一整个租户,绝不是单个主体**,以带凭据的一次性交接
  投递:单次查看、无密码、24 小时过期(可经导出投递读取器模块按租
  户调,钳在 `go/sharing` 上限内)。投递失败删掉已存的清单;投递成
  功的由模块自己的 `compliance.export_manifests` 参与者在过了保留
  窗口后回收。每次导出留两条轨迹:compliance 的请求与投递记录,加
  sharing 的敏感创建记录。
- **`AuditQuery` 在 Go 里过滤,不在 SQL**——刻意如此,过滤
  `ListByTenant` 返回的行,排序确定(occurred_at 再 id,降序);诚实
  的代价是每次查询 O(该租户全行)。`OnBehalfOf` 过滤维度正是冒充问
  责可查的关键。
- **结构化错误码**——`compliance.erasure_partial_failure`、
  `compliance.export_delivery_failed`、`compliance.audit_record_failed`、
  `compliance.erasure_tenant_mismatch` 等——索引在[错误码索引(English)](/docs/user-guide/error-codes/#compliance)。

## 已知限制与链接

- 保留清扫的排程点就是模块自己的声明:`Module.Register` 在注册表的
  `Schedules` 座席上声明按租户的周期任务,宿主起一个
  `jobs.Scheduler` 即按租户清扫,不必自己加排程点(也可手动入队某
  租户的清扫)。
- `SweepAllTenants` 需要宿主提供 `TenantLister`——compliance 坐在
  `org` 之上,不会 import 它;没有内建租户目录。
- 一次 `Erase` 只删运行时刻已注册参与者的行——绝不是某主体租户数
  据的全集。没注册参与者的所有者(org、rbac、storage、notification、
  billing 等)行照留;边界只靠一次注册来闭合。
- compliance 自己没有 PostgreSQL 集成层:它的逻辑按设计对方言不敏
  感(参与者的仓库与 `dbkit/audit` 各自带着双方言证明),而
  `audit_events` 上的只增触发器强制住在 `go/dbkit/audit`,在那里对
  真实服务器证明过。

### 出处

- [go/compliance/AGENTS.md](https://github.com/vislake/speed/blob/main/go/compliance/AGENTS.md)——权威文档(参与者契约、部分失败语义、导出投递、限制)
- 相关页面:[sharing](/zh-cn/docs/user-guide/modules/capabilities/sharing/)、[admin](/zh-cn/docs/user-guide/modules/capabilities/admin/)、[dbkit](/zh-cn/docs/user-guide/modules/core/dbkit/)(审计轨迹与硬删除)
