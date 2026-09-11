---
title: saasctl
weight: 1
description: "面向业务方的 CLI:new、upgrade、db migrate 与 config print——每个命令的用法、选项、校验与退出码,以及 new 所生成项目的形态。"
---

# saasctl

saasctl 是 speed 面向业务方的 CLI——塑造业务方实际运行的那个应用的
工具。speed 以库的形式分发,不是一个应用;saasctl 管理项目与它拉入
的模块之间的边界:生成项目骨架(`new`)、把项目的 speed-module
require 重写到同一个锁步版本(`upgrade`)、把各模块的 SQL 迁移应用
到生成项目的数据库(`db migrate`)、展示它的启动配置如何解析
(`config print`)。

与本节参考里的每个模块都不同,没有任何东西会把 saasctl 组装进
二进制:它不实现 `pkgcore.Module`,业务方代码从不 import 它,参考
应用也刻意不接它——它的消费方是生成的项目。它在开发期作用于项目
边界,从不进入运行中的内核。目前还没有可用的发布物,所以它从
checkout 运行(`go run ./go/saasctl <command>`,在仓库根目录)。
四个命令共享同一套退出码契约——**0** 成功与帮助,**2** 用法错误,
**1** 执行错误——都接受可选的 `[go.mod]` 参数(项目目录内默认
`./go.mod`);没有 speed require 的 `go.mod` 会被点名拒绝。

## 何时使用

从零开始一个项目时用 `new`;schema 必须在任何进程运行前就位时
(预置好 schema 的首次启动、脚本化开通、CI)用 `db migrate`;
启动起不来、或密钥材料即将轮换时用 `config print`;发布落地时用
`upgrade`。[快速开始](/zh-cn/docs/user-guide/quickstart/)把前两个
命令在一个项目上完整走了一遍。

## `new`——生成起始项目

```
saasctl new [flags] <target-directory>
```

从内嵌模板树把骨架生成到目标目录——目标目录不能已经存在(已存在
的空目录可以接受并会被填入)。目录基名会成为 go.mod 的 module
path。创建任何东西之前,两者都必须通过 go 命令自己的校验器:基名
要过 `module.CheckImportPath`,生成的 go.mod 文档要过
`modfile.Parse`——`replace` 指令把 speed-root 路径原样嵌进去,所以
一个被 go.mod 语法拒绝的 checkout 路径会送出一个没有 `go mod tidy`
能接受的骨架。失败的运行会把已创建的东西全部清掉。

`--speed-root` 指定生成的 `go.mod` 所指向的 checkout:先取该 flag,
再取 `SPEED_ROOT` 变量,最后向上逐级查找列出 `go/pkgcore` 的
`go.work`。`--with` 做逗号分隔的正向选择。五个模块(`pkgcore`、
`dbkit`、`tenancy`、`config`、`observability`)始终存在;可切换的
集合是 `{authn, rbac, org}`,默认是完整集合。选择做向下闭包校验——
选 `rbac` 或 `org` 而不选 `authn` 会被拒绝并点名隐含需要的
`authn`,未知名字会被拒绝并列出合法集合,没有 `--without`。
`go/pki` 从来不是选项:选了 `authn` 它就作为签名密钥来源悄悄随行。

```sh
go run ./go/saasctl new ../my-app --speed-root .            # 默认:authn,org,rbac
go run ./go/saasctl new ../my-app-lite --speed-root . --with=authn,org
go run ./go/saasctl new ../my-app-bare --speed-root . --with=""   # 只留配置能力的裸骨架
```

## `upgrade`——把项目迁到一个发布版本上

```
saasctl upgrade --version vX.Y.Z [go.mod]
```

只重写文件里每条 speed require 行的版本号,其余一律不碰:第三方
require 及其 `// indirect` 标记、`replace` 块、注释与格式逐字节
存活——因为被改写的是解析后的语法树,再经 `golang.org/x/mod/modfile`
(go 命令自己的解析器)打印回去。被重写的集合来自文件自己的
require 行。`--version` 必填(目前没有可用发布物,版本发现
尚未实现;目标版本由调用方显式给出)并按发布版本号语法校验。
结果在写回前会从字节重新解析并做结构自检;自检还会拒绝用
`replace`/`exclude` 抵消重写结果
的文件——那样的文件上报告「干净」就是在项目实际构建的版本上说谎。
对已重写过的文件再运行一次会报告没有变化并以 0 退出。只处理 Go
的 `go.mod` 文件。

## `db migrate`——在启动前备好 schema

```
saasctl db migrate [go.mod]
```

应用项目 `go.mod` 所要求、恰好带迁移文件的那些模块——`authn`、
`config`、`org`、`pki`、`rbac`,按字母序——的 SQL 迁移,每个模块
一个事务,每个文件落地时都记录进 `schema_migrations`;`pki` 的表
只在 go.mod 确实 require 了 `go/pki` 时才迁移。完整的
`authn+org+rbac` 组合应用 33 个文件,各模块的数目随它们自己的
迁移树变动。重跑只应用未记录的部分并报告数据库已是最新;对着迁移
过的文件启动时,应用本会执行的启动 `Apply` 变成空转——本命令正是
那一步的运维方手动版本。

默认数据库路径——固定的字面量 `app.db`,绝不从 module path 派生——
落在 `[go.mod]` 参数旁边,也就是应用文档规定的运行目录;显式
`APP_DB_PATH` 永远优先,相对路径按同一方式锚定。拒绝都点名原因:
没有 speed require、文件没有 `schema_migrations` 台账、路径存在但
不是普通文件。两种部署形态都接受——生成项目的数据库在两者之下都
说 SQLite;形态改变的是接缝,从来不是方言。任何副本启动前先把
命令跑完:共享的 SQLite 文件经不起并发写入。

## `config print`——预览一次启动会用到的配置

```
saasctl config print [go.mod]
```

展示生成项目启动配置的解析结果,解析生成 `cmd/server/config.go` 的
每个环境变量一行:部署形态、`PORT`、`APP_DB_PATH`、六份密钥材料
(按声明键路径的派生拼写:`APP_CONFIG__CIPHER_KEY`、
`APP_ORG__INVITATION_EMAIL_INDEX_KEY`、`APP_AUTHN__BLIND_INDEX_KEY`、
`APP_AUTHN__PII_CIPHER_KEY`、`APP_PKI__LOCAL_KEY_CIPHER_KEY`、
`APP_NOTIFICATION__CONTACT_INDEX_KEY`)、
`APP_REDIS_ADDR`/`APP_S3_*`/`APP_SMTP_*` 基础设施变量组以及
`APP_OTLP_ENDPOINT`。每行显示
解析出的值与来源——是哪个变量带来的,还是回退到了哪个默认值。
SQLite 路径行报告生效文件,相对路径解析出不同结果时原始值仍然
可见。密钥行——六份密钥材料、S3 密钥、SMTP 密码、SMS 网关 URL——
无论环境里是什么都显示 `[redacted]`。只设了一部分的
`APP_S3_*`/`APP_SMTP_*` 组与非法输入,都会像应用自己的启动解析
那样被拒绝。本命令不写任何东西。

## `new` 生成什么

模板树是构造上就可工作的应用:先对真实的 speed checkout 做生成、
`go mod tidy` 与构建,再把路径转回 token 而成。它镜像参考应用的
`cmd/server` 形态,去掉了一切演示专用内容(没有 notes 模块,没有
演示租户、授权、成员库或种子数据)。宿主角位(authn 的
`MembershipReader`、org 的 `SubjectResolver`、config 的 resolver)
保持未接线、失败即关闭,每个都在生成文件的 doc comment 里被点名
为所有者的第一个任务。不同组合只差两个文件——go.mod 的 require
集合与 `server.go` 的接线;`config.go` 逐字共享——并在生成时替换
两个 token:`__APP_NAME__`(module path)与 `__SPEED_ROOT__`
(checkout 路径)。宿主中性的组装本身(装配引擎:配置装载、组合配置
解析与七阶段组件驱动;以及 authn 的挂载路径、服务超时、预认证允许
列表与固定中间件链——存活探针端点归 `go/observability`、路由挂载
规则归 `pkgcore`)只有一份,位于平台模块
`github.com/vislake/speed/go/app`,每个生成项目与参考应用一样直接
import——各宿主自己的 `server.go` 把组件注册到
`pkgcore.ComponentRegistry`,经引擎的 `Assemble` 驱动,并由宿主自己
的监听器对外服务;仓库有一道门禁止任一宿主自行重新声明这套内核或
重抛出引擎掌管的装配步骤。

启动默认值:standalone 形态、SQLite `app.db`、端口 8080。
`/healthz` 与 `/api/v1/config/public` 应答 200,注册应答 201;但骨架
出厂状态下密码登录不可能成功:没有成员库,authn 每次登录都经
宿主注入的 `MembershipReader` 复核成员身份,空 reader 失败即关闭
——错密码与对密码都应答 401 `authn.invalid_credentials`。接好那条
接缝是点名给所有者的第一个任务。在下一个发布落地之前,生成项目带着
过渡态——require 的版本串由指向该 checkout 的 `replace` 指令覆盖
(零占位版本与个别真实版本并存)、`go.sum` 要等第一次消费方
`go mod tidy` 才会生成——而 `upgrade` 就是发布被消费时走的那条机制。

## 示例会话

```sh
# 在 speed checkout 内部:生成完整的默认组合。
go run ./go/saasctl new ../my-app --speed-root .
cd ../my-app

# 有了发布之后,在项目目录内:
saasctl upgrade --version v1.0.0     # 重写 ./go.mod 的 speed require
saasctl db migrate                   # 在 ./go.mod 旁备好 app.db
saasctl config print                 # 预览这次启动会用到的配置
go run .                             # 启动;启动 Apply 空转
```

## 边界与注意

- 命令都安全可重跑,第二次运行本身就充当检查:`db migrate` 对着
  已是最新的数据库、`upgrade` 对着已重写过的文件,都报告无事可做
  并以 0 退出;`config print` 则根本不写任何东西。
- `db migrate` 的迁移集合是 go.mod require 里带迁移文件的*根*模块;
  子包迁移(如 `go/dbkit/audit` 的)刻意留在外面——接入时由应用
  自己的启动 `Apply` 应用,两条路径结果一致。

## 相关阅读

- [快速开始](/zh-cn/docs/user-guide/quickstart/)——一个项目的生成、
  迁移与启动,从头到尾。
- [生成项目的运维](/zh-cn/docs/user-guide/operating/)——日常命令
  与两种部署形态简述。
- [模块参考](/zh-cn/docs/user-guide/modules/)——生成项目接线的各
  模块,各自一页。

## Source

- [saasctl AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md)——
  全部四个命令的权威契约:选项、退出码、校验与拒绝形态。
