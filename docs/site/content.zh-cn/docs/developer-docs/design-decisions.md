---
title: 设计决策档案
weight: 7
description: "仓库的决策记录:架构决策记录(ADR)与本栏设计页的关系,以及每条 ADR 的公开叙述。"
---

# 设计决策档案

本栏的逐模块设计页从仓库设计文档与各模块 `AGENTS.md` 提炼而来。
本页覆盖的是另一类记录:**架构决策记录(ADR)**——把设计文档变成
代码的过程中,设计撞上现实、必须有取舍时作出的决策。

## ADR 在这里怎么运作

每条 ADR 一个文件、一个决策,按 Context → Decision → Consequences
写成英文,放在 `docs/adr/` 下——刻意与设计文档语料分开,因为
ADR 记录的是仓库的公开形态,不只被人读,也被工具读:许可证扫描
器就拒绝放行弱版权(copyleft)依赖,直到依赖清单指名一条对它做出
裁定的 ADR。

产生 ADR 的典型情形有三种:

- 设计文档内部自相矛盾,实现接口时暴露出来(0001、0002);
- 设计文档从未作出的实现层面决定,落到代码时被迫拍板(0003)。

ADR 与修复本身同一次改动落地,原始设计文档也在同一次改动里被
改正——记录永远反映代码所体现的决策,而不是文档曾经描述过的那
个。

## ADR 0001:`Module.Migrations()` 返回 `embed.FS`

`Module` 接口是每个模块都要实现的接线契约,它活在 `pkgcore`——
依赖地基里。设计文档写的是 `Migrations() dbkit.MigrationSet`,那
会迫使 `pkgcore` import `dbkit`;而 `dbkit` 已经 import `pkgcore`
(为租户上下文原语与模块契约本身),于是按文档实现的接口无法编译:
一个 import 环。

决策:`Migrations()` 直接返回标准库的 `embed.FS`——刻意选择能
成立的最薄类型。模块迁移如何被解释(方言选择、版本方案)是
`dbkit.MigrationRegistry` 的事,发生在接口边界之后。后果:`pkgcore`
对 `dbkit` 保持零 import 依赖;方言逻辑不进 `Module` 接口;设计
文档在同一次改动中被改正。

[读 ADR 0001 全文](https://github.com/vislake/speed/blob/main/docs/adr/0001-module-migrations-return-embed-fs.md)

## ADR 0002:租户上下文原语活在 `pkgcore`

早期草稿把租户上下文原语——`WithTenant`、`TenantFromContext`、
`WithSystemContext` 等——放在 `tenancy` 包里。但 `dbkit.Repository[T]`
每次读取都必须从上下文解析租户、拿不到就 fail closed,而 `tenancy`
依赖 `dbkit`(为它的 GORM 租户隔离插件)。这同样是编译不过的双包
import 环:`dbkit -> tenancy -> dbkit`。

决策:裸原语放在 `pkgcore`——`dbkit` 与 `tenancy` 共同依赖的那
一个包。`tenancy` 在其上叠更丰富的包装:它的 `WithSystemContext`
额外发布一条审计事件,那是没有理由活在 `pkgcore` 里的机制。后果:
`dbkit` 无需 import `tenancy` 就实现 fail-closed 的租户级仓库;
`tenancy` 保持依赖 `dbkit` 的自由;在 `tenancy` 存在之前写下的
业务代码直接调用 `pkgcore` 原语。

[读 ADR 0002 全文](https://github.com/vislake/speed/blob/main/docs/adr/0002-tenant-context-primitives-live-in-pkgcore.md)

## ADR 0003:为 `go/pki/signer/vault` 接受 MPL-2.0

仓库的许可证政策禁止 GPL/AGPL 系依赖,并要求任何 MPL/LGPL 系
("弱 copyleft")依赖入树之前先有 ADR 裁定。`go/pki/signer/vault`
——可选的 HashiCorp Vault Transit 签名后端——建立在
`github.com/hashicorp/vault/api` 之上时,这条政策从未被咨询过;
一次依赖清单再生成把该依赖明确无疑的 MPL-2.0 许可证摆上了台面。

决策:为这一个依赖、在这一个可选子包里接受 MPL-2.0。MPL-2.0 是
文件级的弱 copyleft——调用该库不会让仓库自己的代码落入 MPL,没有
任何 `vault/api` 文件被修改,子包之外无人 import 它,也不存在
宽松许可的 Vault 客户端可作替代,而删掉后端只会移除已交付的能力
而非消解风险。这不是一揽子放行:未来任何 MPL/LGPL 依赖都需要自己
的裁定。后果:依赖清单条目带上扫描器要求的 ADR 引用;import 该
子包的消费者连带继承这份推理,其余人不受影响。

[读 ADR 0003 全文](https://github.com/vislake/speed/blob/main/docs/adr/0003-accept-mpl2-for-pki-signer-vault.md)

## 一条新 ADR 长什么样

需要记录的决策与上面三条同形:它改变了已交付代码的行为,它是在
有文档可循的替代方案对照下作出的,而且日后一定会有人问为什么。
ADR 写明背景、陈述决策、列出后果——并与体现该决策的代码在同一
次改动里落地。

## Source

- [docs/adr/0001-module-migrations-return-embed-fs.md](https://github.com/vislake/speed/blob/main/docs/adr/0001-module-migrations-return-embed-fs.md)
- [docs/adr/0002-tenant-context-primitives-live-in-pkgcore.md](https://github.com/vislake/speed/blob/main/docs/adr/0002-tenant-context-primitives-live-in-pkgcore.md)
- [docs/adr/0003-accept-mpl2-for-pki-signer-vault.md](https://github.com/vislake/speed/blob/main/docs/adr/0003-accept-mpl2-for-pki-signer-vault.md)
