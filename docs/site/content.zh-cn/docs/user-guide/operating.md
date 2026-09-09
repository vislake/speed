---
title: 生成项目的运维
weight: 4
description: saasctl 生成项目的日常维护——它可以运行的两种部署形态,以及维持它的 upgrade、db migrate、config print 三个命令。
---

# 生成项目的运维

[起始项目](../quickstart/)生成并跑起来之后,有三种操作会反复出现:在启动
前先准备好数据库 schema、把项目迁到新的 speed 发布版本、弄清它启动时
会用什么配置以及为什么。三者都走 `saasctl`,从 speed checkout 以
`go run ./go/saasctl <command>` 运行;每个命令都以目标项目的
`go.mod` 为参数——在项目目录内部运行时 `[go.mod]` 默认为
`./go.mod`。

## 部署形态简述

`APP_DEPLOYMENT_MODE` 声明生成项目运行在两种部署形态中的哪一种;
默认是 `standalone`。

- **Standalone(单进程)**——一个进程、一个 SQLite 文件,每条基础设施
  接缝(事件总线、KV 存储、邮件、对象存储)都用进程内实现。零外部
  依赖;本地开发与小规模单机部署跑的就是它。
- **Distributed(分布式)**——同一个二进制,以多个副本运行。需要跨副本
  共享的接缝从环境变量组装真实实现:`APP_REDIS_ADDR` 接出 Redis 支撑
  的事件总线与 KV 存储,`APP_S3_*` 组接 S3 兼容对象存储,`APP_SMTP_*`
  一对接真实邮件,`APP_SMS_GATEWAY_URL` 接 authn 的短信通道。

形态只做约束,从不替你做选择:这些变量一个都不设时,
`APP_DEPLOYMENT_MODE=distributed` 会让启动以 `ErrCapabilityUnsatisfied`
失败,点名仍落在进程内实现上的那条接缝。项目自己的数据库在两种形态下
都是 SQLite。

## `db migrate`——在启动前备好 schema

生成的应用每次启动都会自己应用迁移(`Kernel.Bootstrap` 的 `Apply`,
经 `schema_migrations` 台账幂等)。`saasctl db migrate` 是这一步的
运维方手动版本,用于让 schema 在任何进程运行前就位——预置好 schema
的首次启动、脚本化的开通步骤、CI 运行。

```sh
saasctl db migrate                 # 迁移 ./go.mod 所属项目
saasctl db migrate ../my-app/go.mod
```

它应用项目 `go.mod` 所要求、恰好带迁移文件的那些模块——`authn`、
`config`、`org`、`pki`、`rbac`,按字母序——的 SQL 迁移,每个模块一个
事务,每个文件落地时都记录进 `schema_migrations`。完整的
`authn+org+rbac` 组合应用 33 个文件(`authn` 11、`config` 1、`org` 9、
`pki` 9、`rbac` 3),数目随各模块自己的迁移集合变动。重跑会报告数据库
已是最新,对着迁移过的文件启动则让启动 `Apply` 空转——先 CLI 后启动
与只靠启动两条路结果一致。

拒绝都点名原因:没有 speed require 的 `go.mod` 多半不是生成项目;
已存在但没有 `schema_migrations` 台账的数据库文件会被拒绝,而不是去
猜。默认数据库路径 `app.db` 落在 `[go.mod]` 参数旁边——也就是应用
文档规定的运行目录;显式 `APP_DB_PATH` 永远优先,相对路径按同一方式
锚定。任何副本启动前先把命令跑完:共享的 SQLite 文件经不起并发写入。

## `config print`——这个应用会用什么启动,以及为什么

```sh
saasctl config print ../my-app/go.mod
```

`config print` 展示生成项目启动配置的解析结果:部署形态、端口、生效的
SQLite 路径、五份密钥材料与可选的基础设施变量——生成
`cmd/server/config.go` 解析的每个变量一行,显示解析出的值与来源(是哪
个变量带来的,还是回退到了哪个默认值)。SQLite 路径行显示生效文件,
所以锚定到项目目录的相对路径会按应用实际会打开的文件来报告。密钥行
——五份密钥材料、S3 密钥、SMTP 密码、SMS 网关 URL——无论环境里是什么
都显示 `[redacted]`。输入非法时的拒绝方式与应用自身拒绝启动的方式
完全一致。改密钥之前、排查起不来的启动之前,先跑它。

## `upgrade`——把项目迁到一个发布版本上

speed 的发布是锁步的:每个 Go 模块与 npm 包共享同一个版本号,只支持
同版本组合。一个发布落地时,一条命令就能重写业务方项目的整个兼容面:

```sh
saasctl upgrade --version v1.0.0 ../my-app/go.mod
```

重写只碰每条 `github.com/vislake/speed/go/*` require 行的版本号——
第三方 require 及其 `// indirect` 标记、`replace` 块、注释与格式逐
字节存活,被重写的模块集合来自文件自己的 require 行。
`--version` 必填并按发布版本号语法校验;对已重写过的文件再运行一次
是无操作。该命令只重写 Go 的 `go.mod` 文件。

快速开始页的现状说明在这里同样适用:里程碑 M4 之前没有任何发布物,
今天没有真实发布版本可迁,生成 `go.mod` 里的 `replace` 指令也让本地
checkout 对构建保持权威;这条命令就是第一个发布被消费时走的那条机制。

## 接下来

- [参考应用演练](../walkthrough-reference-app/)——完全接好线、预置演示
  数据的组装体,用真实 HTTP 驱动。
- [错误码索引](../error-codes/)——运行中的服务拒绝请求时可能应答的
  错误码。
- [模块索引](/zh-cn/docs/modules/)——项目可以 require 的每个 Go
  模块与 npm 包,各自链接自己的 `AGENTS.md`/`README.md`。
- [快速开始](../quickstart/)——生成本页所运维的那个项目。

## Source

- [saasctl AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md)——
  `upgrade`、`db migrate`、`config print` 的权威契约,含退出码与拒绝
  形态。
- [reference-app README](https://github.com/vislake/speed/blob/main/examples/reference-app/README.md)——
  在跑遍每个模块的应用上演示部署形态与基础设施接线。
