---
title: config
weight: 5
description: "schema 先行、数据库承载的设置库——各模块声明的配置项与功能开关、system 到 tenant 的值域、经事件加防丢轮询的热更新,以及两个预认证端点。"
---

# config

speed 的动态配置模块:schema 先行、数据库承载的设置库,值可运行时
变更并热生效——功能开关、租户品牌项、默认限额、AI 模型默认值。
值存在 `configs` 表里,分两层作用域,`system`(平台级)→ `tenant`
(逐租户覆盖);读取从窄层回落到宽层,再落到 schema 默认值。配置的
另一层——进程启动的引导输入(旗标、环境变量与文件)——不在本模块
职责内:声明在 `pkgcore` 的引导声明面上(组件描述符的 `BootstrapKeys`)、
由通用加载器
`pkgcore/config` 解析(宿主自己的目标结构体驱动),该零依赖包本模块
绝不导入;声明键材料的派生约定——每个键路径一个 purpose 串
(`pkgcore.BootstrapKeyPurpose`),配合 `dbkit.DeriveBootstrapKey`——
也在声明与工具箱侧。本模块在该声明上只声明一枚自己的键(`config.cipher_key`),
不拥有该层任何机制。它拥有运行时层,对多租户宿主是必需的:任何其他
模块的行为都可能受它服务的开关或限额支配。

每个模块在 `Register` 期间把它的配置项与开关声明到注册器上
(`reg.ConfigSeat().Add(pkgcore.ConfigItem{Key, Type, Default, ...})`,
功能开关带各自的 `DependsOn` 链)。本模块把这些声明折进一张
schema,运行时解析开关依赖,并服务有效值。

## 何时选用

永远——它属于常开模块,没有关闭开关。它的接线与普通模块有一
个承重差异:光注册不够。`Register` 声明不需要已组装注册器的东西;
`Attach`——恰好一次,在每个组件的 `Init` 轮次都跑完之后——把注册器
*合并*后的配置项与开关声明折进 schema,并要求注册期不得触碰的
运行时部件:迁移好的 `configs` 表、任何已注册项为 `Sensitive` 时
的 `dbkit.Cipher`(否则 `ErrCipherRequired`)、以及轮询间隔。
`Attach` 返回的 `*Service` 才是宿主该持有的东西。

## 接线与最少使用

```go
// 组合选中 config 组件;装配构造模块并驱动它——Register 在它的 Init
// 回合声明,附着的 *Service 在每个组件的声明都就位后,由模块自己的
// Start 回合发布。
reg := pkgcore.NewComponentRegistry()
// 在 reg 上注册宿主自己的组件(或让加载器替你选)
if err := app.Assemble(ctx, reg, app.LoadSpec{Host: &hostConfig, Options: loaderOpts}); err != nil { /* handle err */ }

svc, err := pkgcore.Get[*config.Service](reg) // 装配已附着的服务
// handle err

name, err := config.GetTyped[string](svc, ctx, "brand.site_name") // 平台默认值
// handle err

tenantCtx := pkgcore.WithTenant(ctx, "acme")
if err := svc.Set(tenantCtx, config.ScopeTenant, "brand.site_name",
    config.Value{Data: "Acme Dental"}, "alice"); err != nil {
    // handle err
}
enabled, err := svc.IsEnabled(tenantCtx, "brand.custom_theme")
// handle err
```

组件是被选中的,不是手工构造的:装配构造模块,它的 `Init` 回合经
`Register` 声明,它的 `Start` 回合附着 schema 快照并发布 `*Service`,
宿主用 `pkgcore.Get` 读回。被选的 db 组件已在 `Verify` 阶段应用每
个组件声明的迁移集,所以服务附着前 `configs` 表已存在。需要在
`Init` 期间就用服务的宿主——早于 `Start` 发布它——由自己的步骤组件
在 `Init` 里调用 `configModule.Attach(reg)` 并发布返回的服务;模块
的 `Start` 回合随后补全那份快照,而不是再附着第二个服务。

## 核心概念与 API 要点

- **作用域、回落、资格**——值以三元组 `(key, scope, tenant_id)`
  寻址;由服务——绝不是调用方、绝不是 HTTP 层——在 `Set` 上强制
  每层资格:租户写要求上下文里有租户并归属该租户;系统写要求带审计
  的系统上下文(否则 `ErrSystemScopeRequiresSystemContext`),因此
  任何租户作用域的请求路径都扩宽不了平台设置。`user` 层保留未实现:
  对它写答 `ErrUserScopeUnavailable`,绝不落错层。
- **规范值与边界**——每类值以规范字符串存取(int 用十进制,
  duration 用 `time.Duration.String()`);解码是唯一卡点,损坏的行以
  错误浮现,绝不会解码成错类型的值。`GetTyped` 支持 `string`、
  `bool`、`int64`、`time.Duration`。写时强制边界;校验错误绝不把
  违规值回显到任何地方。
- **Sensitive 项**——`Sensitive` 项在宿主 cipher 下 AES-GCM 密封
  后入库;明文只存在于服务缓存与有资格的 `Get` 应答里。变更事件、
  watcher 投递、日志与错误里一律以 `[redacted]` 标记替代;
  `Public`/`Sensitive` 互斥由校验保证,预认证端点结构上就漏不出
  敏感值。
- **热更新**——`Set` 先推进进程自身缓存再发布 `config.item.changed`
  (携带 actor、旧→新,敏感键打码)——该事件同时充当写入的审计记录。
  发布失败不回滚写入(`ErrAuditPublishFailed` 是审计为必选项的宿主
  的信号)。订阅方与对端副本经事件收敛,外加一条防丢轮询(间隔由
  宿主选择,默认 30 秒;`0` 对单实例宿主关闭)——一条丢掉的变更
  事件绝不能把副本留在无限期陈旧配置上。`Watch` 按事件所见逐条
  投递。
- **功能开关**——开关仅当它自己**及其依赖的每个开关**(传递闭包)
  都开时才算开,所以关掉某个依赖就逐租户关掉它上面的一切,无需数据
  迁移;环在 Attach 时被拒(`ErrFeatureFlagDependencyCycle`)。
  `EnabledFlags` 把开启列表喂给前端。消费语义(「被禁功能答 404,
  不答 403」)是各消费模块的职责——本模块只回答什么开着,别无其他。
- **端点**——两个预认证 GET/HEAD 端点挂在导出的 `PathPublic`
  (`/api/v1/config/public`)与 `PathSystemFeatures`(`/api/v1/config/features`)
  常量上,宿主在租户中间件 allowlist 里指名它们;两者由模块自带的
  OpenAPI 片段声明(`config_getPublicConfig` / `config_getSystemFeatures`,
  生成进 `@speed/api-sdk`)。两者经宿主接线的
  `tenancy.Resolver` 解析请求租户,查不到即回落平台默认值——绝不
  报错,因为渲染不出来的登录页才是最糟的失败模式。一个未设值或损坏
  的 Public 项只会被快照跳过,绝不允许拖垮端点(连带登录页)。

## 边界与注意

- 别从其他模块直接读 `configs` 表:作用域回落、解密、缓存与开关
  语义都在 `Service` 里;绕过它的人实现的只是错误子集。
- `configs` 表是平台数据——刻意不是 `dbkit.TenantScoped`——它是
  仓库规则的成文例外,不是租户数据该抄的样板。
- 两个端点的线上契约就是模块自带的 OpenAPI 片段
  (`api/openapi.yaml`);前端的逐键读取与开关查询仍走
  `@speed/api-client` 的类型化封装——它是生成操作之下的映射层。
  也没有配置*编辑*界面。
- 持久审计行只在组装了 `compliance` 模块时存在(它订阅变更事件);
  没有它时,审计对你为必选就把发布失败当回事。
- 配置项描述是代码里的单语言散文,尚未成为目录消息 id。

## Source

- [config AGENTS.md](https://github.com/vislake/speed/blob/main/go/config/AGENTS.md)
- [config `example_test.go`](https://github.com/vislake/speed/blob/main/go/config/example_test.go)
