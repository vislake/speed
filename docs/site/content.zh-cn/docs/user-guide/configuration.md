---
title: 配置参考
weight: 98
---

# 配置参考

一个基于 speed 的应用从两层解析配置,两层的键互不重叠:启动层(bootstrap)
是进程启动输入,一次解析、进程生命周期内固定;运行时层是 configs 表里的
动态值,按租户或平台维度存储、运维在线编辑、即时生效。每层的完整清单
(每个键的环境变量名、格式、默认回退、是否敏感、保护什么)是英文页面:

[查看完整配置参考(English)](/docs/user-guide/configuration/)

## 启动层

启动层的键由平台模块在注册时声明(`reg.Bootstrap.Add`):模块声明它消费什么,
宿主负责解析值并注入,模块自己从不读环境变量。宿主用 `go/pkgcore/config`
加载器解析,优先级从高到低为命令行旗标、环境变量、可选的 YAML **或 JSON**
配置文件、根密钥派生(仅限打 `derive` 标记的密钥材料字段)、目标结构体上
已设的默认值。键名由目标结构体字段派生:字段
`Database.DSN` 对应键 `database.dsn`、旗标 `--database.dsn` 与环境变量
`SPEED_DATABASE__DSN`(默认前缀 `SPEED_`,键大写,每层嵌套一个双下划线;
单下划线不是嵌套标记)。`WithEnvPrefix` 可为变量已带别的前缀的宿主替换前缀,
字段也可以用 `config:"env=PORT"` 钉住精确的变量名。

启动层文件源的成对完整示例随仓库提交:`docs/config.example.yaml` 与其派生的
`docs/config.example.json`(平台键,两种格式等价),可直接作为复制模板。

六枚密钥材料(authn、org、notification、pki、config 各自声明)有文档化的
**非密钥**开发默认值,真实部署必须从密钥库覆盖。平台为派生定下两件稳定
契约:声明的键路径在引导席位固定其 purpose 串(`pkgcore.BootstrapKeyPurpose`,
目的串内嵌键路径,如 `speed.config.cipher_key.v1`),`dbkit.DeriveKey`
(HKDF-SHA256)再把根密钥与该 purpose 变成该键的 32 字节材料——一枚 32 字节
根密钥即可服务全部六枚,`dbkit.DeriveBootstrapKey` 把两步合成一次调用。
**派生由加载器施加**:宿主把密钥材料字段打成 `derive`(仅限 `[]byte` 字段),
用 `WithRootKey` 或 `WithRootKeyEnv`(后者在同一次 Load 内读变量,宿主自己
无须读环境)指明根密钥来源,并装入 `WithKeyDerivation(dbkit.DeriveBootstrapKey)`;
该字段随后按"显式 64 位十六进制值 > 根密钥派生 > 结构体默认"解析,空值一律
按未设处理。宿主自行为根密钥与每个单键选择变量名,平台只接收字节。重命名
声明的键路径即轮换,必须按轮换流程发布。零声明是诚实状态:不消费启动输入的
模块不声明任何键。

## 运行时层

运行时层由 `go/config` 模块提供:每个模块在 `Register` 时声明自己的配置项与
feature flag,`Attach` 把它们折叠成一份 schema。值存在 `configs` 表,分
system(平台级)与 tenant(租户级)两档,读取按 tenant → system → schema 默认
逐级回退。敏感项在存储时加密(configs 表只存密文),永不出现在公开端点上。

## 分层规则

一个 dotted 键只能属于一层。同一键在两层的声明会被启动与生成器同时拒绝:
一个标识符承载两种含义、两种默认值、两种编辑面,运维改"那个键"时无从知道
改的是哪一层。
