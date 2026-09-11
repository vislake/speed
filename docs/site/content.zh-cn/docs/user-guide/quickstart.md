---
title: 快速开始
weight: 2
description: 用 saasctl new 生成可启动的起始项目、迁移数据库并运行——发布之前,从本地 checkout 试用 speed 的真实现行方式。
aliases: ["/docs/quickstart"]
---

# 快速开始

从零到跑起来的项目:克隆仓库、用 `saasctl new` 生成起始项目、迁移数据库
并启动它。只需要一个 Go 工具链,别无他物。

> [!NOTE]
> **还没有可用的发布版本。** v0.0.1 的 Go 模块曾发布——21 个模块 tag 由
> Go module proxy 服务——随后 tag 被删除、该版本作废;npm 包从未发布。
> 对该版本,代理的不可改写缓存只服务到一部分模块(21 个里 17 个可解析,
> admin、ai-gateway、integration、saasctl 不可解析),完整的树则留在
> 仓库自身历史的发布提交里可达;无论从哪条来源看,该版本都已作废、
> 不受支持。下面这条本地 checkout 路径(克隆加 `go run`)是当前唯一
> 真实的方式。生成的起始项目处于过渡形态:require 的版本串由指向该
> checkout 的 `replace` 指令覆盖(零占位版本与个别真实版本并存),
> `go.sum` 要等第一次消费侧 `go mod tidy` 才会生成。

## 1. 获取 checkout

```sh
git clone https://github.com/vislake/speed
cd speed
```

仓库根目录是一个 Go workspace(`go.work`),不是单个 Go module——从根
目录运行 Go 命令要用完整 import path,见本页「在本仓库运行 Go 命令」
一节。`saasctl` 直接在 checkout 里用 `go run` 运行。

## 2. 用 `saasctl new` 生成起始项目

`saasctl` 是 speed 面向业务方的 CLI:它从一棵内嵌的真实文件模板树中
生成一个可直接启动的起始项目——所有演示专用内容都被移除,没有临时
拼凑。它的宿主角位(authn 的 `MembershipReader`、org 的
`SubjectResolver`、config 的 resolver)保持未接线、失败即关闭,每个都
在生成文件里以 doc comment 点名,是你的第一个任务。

```sh
go run ./go/saasctl new --speed-root . ../my-app
```

`--speed-root` 指定生成 `go.mod` 要指向的 checkout。省略时,saasctl
依次回退到 `SPEED_ROOT` 变量、再向上逐级查找列出 `go/pkgcore`
的 `go.work`。目标目录不能已经存在(已存在的空目录可以接受)。
目录的基名会成为 go.mod 的 module path;不能充当 module path 的名字
会在创建任何东西之前被拒绝。

### 用 `--with` 选择模块

五个模块(`pkgcore`、`dbkit`、`tenancy`、`config`、`observability`)
始终存在——没有可以移除它们的开关。另有三个可切换——`authn`、`rbac`、
`org`——`--with` 做正向选择加向下闭包校验:选 `rbac` 或 `org` 而不选
`authn` 会被拒绝,并点名隐含需要的 `authn`。没有 `--without`:不列出
某个模块就是不接入它。`go/pki` 不是第四个选项——只要选了 `authn`,它
就会作为 authn 的签名密钥来源悄悄随行;每种选择实际产生的 require
集合见[模块索引](/zh-cn/docs/user-guide/modules/)。

```sh
# 默认:完整的 {authn, rbac, org} 组合
go run ./go/saasctl new --speed-root . ../my-app

# 只要 authn——没有组织树,没有基于角色的访问控制
go run ./go/saasctl new --speed-root . --with=authn ../my-app-lite

# 只保留配置能力的裸骨架,不接入任何可切换模块
go run ./go/saasctl new --speed-root . --with="" ../my-app-bare
```

## `saasctl` 的四个命令

| 命令 | 作用 |
|---|---|
| `saasctl new [flags] <target-directory>` | 从 saasctl 内嵌的模板树把项目骨架生成到一个新目录,替换应用的 module path 与解析出的 speed checkout 路径。退出码:0 成功/帮助,2 用法错误,1 执行错误。 |
| `saasctl upgrade --version vX.Y.Z [go.mod]` | 把业务方 `go.mod` 里 `github.com/vislake/speed/go/*` 的 require 就地重写到同一个目标版本,逐字节保留其余一切(第三方 require 及其 `// indirect` 标记、`replace` 块、注释、格式)。`--version` 必填,并按发布版本号语法校验;对已重写过的文件再运行一次是无操作。只重写 Go 的 `go.mod` 文件。 |
| `saasctl db migrate [go.mod]` | 把项目 `go.mod` 所要求、恰好带迁移文件的模块——`authn`、`config`、`org`、`pki`、`rbac`,按字母序——的 SQL 迁移应用到项目的 SQLite 数据库,每个模块一个事务,每个文件落地时都记录进 `schema_migrations`;完整组合共 34 个文件(`authn` 12、`config` 1、`org` 9、`pki` 9、`rbac` 3)。幂等;没有 `schema_migrations` 台账的既有数据库文件会被拒绝。 |
| `saasctl config print [go.mod]` | 展示生成项目启动配置的解析结果——生成 `cmd/server/config.go` 解析的每个 `APP_*`/`PORT` 变量一行,显示解析出的值与来源,密钥行无论环境变量里是什么都显示 `[redacted]`;输入非法时的拒绝方式与应用自身拒绝启动的方式完全一致。 |

## 3. 迁移数据库并运行

`saasctl` 还没有安装到任何地方,当你不位于项目自己的目录里时,它的
子命令需要一个显式的 `go.mod` 参数:

```sh
# 仍然在 speed checkout 内部
go run ./go/saasctl db migrate ../my-app/go.mod
go run ./go/saasctl config print ../my-app/go.mod

# 然后从生成项目自己的目录里启动它
cd ../my-app
go run .
```

启动默认值:standalone 部署形态、当前目录下名为 `app.db` 的 SQLite
数据库(`APP_DB_PATH` 可覆盖)、8080 端口(`PORT` 可覆盖)。生成的应用
自己会在每次启动时应用迁移——被选的 db 组件在装配的 `Verify` 阶段
运行它们,经 `schema_migrations` 台账幂等——`db
migrate` 是同一步骤由操作者手动执行的版本,适合想在第一次启动前就把 schema
准备好的场景;先 CLI 后启动与只靠启动两条路结果一致。

骨架一启动就有两个可依赖的应答:`/healthz` 与 `/api/v1/config/public`
返回 200,经 `/api/v1/authn/register` 注册账号返回 201。密码登录在
生成的骨架上不可能成功:它没有成员存储,而 authn 契约在每次登录时都
通过宿主注入的 `MembershipReader` 复核租户成员关系——reader 为 nil
就失败关闭,所以无论密码对错,登录都只答 401
`authn.invalid_credentials`。接上这个接缝就是生成代码点名的第一个任务。

## 在本仓库运行 Go 命令

从仓库根目录运行 module 范围的命令要用完整 import path;在模块内部
则用 `./...`:

```sh
# 从仓库根目录
go build github.com/vislake/speed/go/authn/...

# 等价地,从模块目录内部
cd go/authn && go build ./... && go vet ./...
```

## 接下来

- [参考应用演练](../walkthrough-reference-app/)——驱动一个完全接好线、
  预置了演示数据的应用走真实 HTTP,看登录、权限与租户隔离的实际行为。
- [生成项目的操作](../operating/)——部署形态简述,以及日常维护项目的
  `upgrade`/`db migrate`/`config print` 命令。
- [用户指南](/zh-cn/docs/user-guide/)——按领域阅读:身份与访问、多租户
  与组织、计费、通知等。
- [面向 AI Agent](/zh-cn/docs/ai-agents/)——该先读什么,以及 Agent
  负责接入时最容易踩坑的架构纪律。

## Source

- [saasctl AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md)——
  四个命令的权威契约。
