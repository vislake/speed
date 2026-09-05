---
title: 快速开始
weight: 2
---

# 快速开始

> [!NOTE]
> **目前还没有任何发布物。** speed 的第一个正式 tag 要到里程碑 M4 才会
> 出现（见[实现状态](../status/)）；在那之前不存在
> `go get github.com/vislake/speed/...`，也不存在从任何 registry
> `npm install @speed/...`。下面这条路径——本地 checkout 加
> `go run`——就是当下试用 speed 真实、现行的方式，不是对某个已发布流程
> 的简化版本。

## 1. 克隆仓库

```sh
git clone https://github.com/vislake/speed
cd speed
```

仓库根目录是一个 Go workspace（`go.work`），不是一个 Go module——运行
Go 命令时的注意事项见下文。

## 2. 用 `saasctl new` 生成一个起始项目

`saasctl` 是 speed 面向业务方的 CLI：它从一棵内嵌的模板树中生成一个
可直接启动的起始项目，而不是临时拼出一个骨架。可以直接在 checkout 里
用 `go run` 运行：

```sh
go run ./go/saasctl new ../my-app --speed-root .
```

目标目录不能已经存在（或者必须是空目录）。生成项目的 `go.mod` 里带有
指向 `--speed-root` 的 `replace` 指令——这是每一个生成项目在第一个正式
发布之前都会带有的过渡态形态，因为目前还没有任何按版本号可依赖的
registry 条目。

### `--with`：选择接入哪些业务模块

默认情况下 `new` 会接入完整的可选模块集合。五个模块（`pkgcore`、
`dbkit`、`tenancy`、`config`、`observability`）始终存在——没有可以
移除它们的开关。另外三个是可选的，`--with` 做的是正向选择加向下闭包
校验：选择 `rbac` 或 `org` 而不选 `authn` 会被拒绝，并明确指出隐含
需要 `authn`（两者都需要一个认证层）。没有 `--without`——不列出某个
模块就等于不接入它。

```sh
# 默认：完整的 {authn, rbac, org} 组合
go run ./go/saasctl new ../my-app --speed-root .

# 只要 authn，不要组织树或基于角色的访问控制
go run ./go/saasctl new ../my-app-lite --speed-root . --with=authn

# 只保留配置能力的裸骨架，不接入任何可选模块
go run ./go/saasctl new ../my-app-bare --speed-root . --with=""
```

`go/pki` 不是第四个可选项：只要选择了 `authn`，生成的项目就会默默把
`pki` 接入作为其签名密钥来源。业务方项目实际会接入哪些内容的具体清单
见[模块索引](../modules/)。

## `saasctl` 的四个命令

| 命令 | 作用 |
|---|---|
| `saasctl new [flags] <target-directory>` | 从 `saasctl` 内嵌的模板树把项目骨架生成到一个新目录，替换掉应用的 module path 与解析出的 speed checkout 路径。退出码：0 成功/帮助，2 用法错误（错误的参数、非法的目标名），1 执行错误（无法解析 speed root、目标目录非空、I/O 失败）。 |
| `saasctl upgrade --version vX.Y.Z [go.mod]` | 就地重写业务方 `go.mod` 里 `github.com/vislake/speed/go/*` 的 require 到目标版本，逐字节保留其余一切（第三方 require、`replace` 块、注释、格式）——这正是新版本发布时锁步版本管理所需要的重写。`--version` 是必填项，会对照发布版本号的语法校验；对已经重写过的文件再运行一次是幂等的。 |
| `saasctl db migrate [go.mod]` | 把项目 `go.mod` 所要求的、恰好带迁移文件的那些模块（目前是 `authn`、`config`、`org`、`pki`、`rbac`，按字母顺序）的 SQL 迁移应用到项目的 SQLite 数据库，每个模块一个事务，每个文件落地时都记录进 `schema_migrations`。当部署模式不是单进程模式时拒绝执行；面对一个已存在但没有 `schema_migrations` 台账的数据库文件时同样拒绝，而不是去猜。 |
| `saasctl config print [go.mod]` | 展示生成项目的启动配置具体是怎么解析出来的——五个 `SPEED_*`/`PORT` 环境变量，每一行都显示其取值与来源，两个关键行无论环境变量里实际是什么都会被脱敏显示。 |

## 3. 迁移数据库并启动

`saasctl` 本身还没有安装到任何地方，所以当你不在目标项目自己的目录里
运行时，它的子命令需要一个显式的 `go.mod` 参数来指明目标项目：

```sh
# 仍然在 speed checkout 内部
go run ./go/saasctl db migrate ../my-app/go.mod
go run ./go/saasctl config print ../my-app/go.mod

# 然后从生成项目自己的目录里启动它
cd ../my-app
go run .
```

生成的应用自己在启动时也会应用其迁移（`Kernel.Bootstrap` 的 `Apply`
步骤）——先运行 `db migrate` 是同一步骤的运维方手动版本，适合想在第一次
启动前就把 schema 准备好的场景；用这种方式先迁移过的数据库会让启动时的
`Apply` 变成空操作。

## 在本仓库里运行 Go 命令

仓库根目录是一个 `go.work` workspace，不是单个 Go module——从根目录
直接运行 `go test ./...` 不会像在普通 module 里那样按预期解析。要么
从目标模块自己的目录里运行 module 范围的命令，要么从仓库根目录用完整
的 import path：

```sh
# 从仓库根目录
go build github.com/vislake/speed/go/authn/...
go vet github.com/vislake/speed/go/authn/...

# 等价地，从模块目录内部
cd go/authn && go build ./... && go vet ./...
```

## 接下来

- [模块索引](../modules/) —— 业务方项目可以接入的 Go module 与 npm
  包的具体清单，各自链接到自己的 `AGENTS.md`/`README.md`。
- [面向 AI Agent](../ai-agents/) —— 应该先读什么，以及当 Agent 负责
  接入时最容易踩坑的架构纪律。
- [实现状态](../status/) —— 当前实现逐模块的真实进展。
