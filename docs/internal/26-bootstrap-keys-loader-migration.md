# 26 bootstrap 键声明机制与 reference-app loader 迁移:机制、边界与两阶段落地

本设计把两个议题合并为一个设计轮:平台机制(模块级 bootstrap 键声明席位 + loader 前缀选项)与 reference-app 的 loader 迁移形态。二者互相形塑——声明的字段集必须能承载 reference-app 现存 curated 目录的全部事实(类型、回退表述、密钥标记、分组、示例),而迁移后的宿主结构体与模块声明必须能通过同一套一致性门;分开设计意味着字段集与键归属各猜一次。三个已裁定决策作为本设计的约束,不再重开:

1. 本设计轮与 reference-app loader 迁移设计合并为一轮。
2. `APP_` 前缀族保留:loader 新增 `WithEnvPrefix` 选项(增量,默认 `SPEED_` 不变),reference-app 以其现有 `APP_` 前缀驱动 loader;`APP_` 链上的环境变量名一个不改。
3. 设计批准后分两步实现:步骤一为机制 + 配置文档机制改造;步骤二为 reference-app 迁移 + 全链文档同步。本轮只设计,不写实现。

文中的"现状"陈述于 2026-09-10 对照工作树逐条复核,引用处符号名优先(步骤一/二落地后早期行号已失效,§9.2 末条记录该改写)。

## 1 现状核对

### 1.1 loader 机制现状(go/pkgcore/config)

`go/pkgcore/config` 是平台唯一的 bootstrap 配置加载器:

- 四源优先级,包注释(config.go 头部)与 `config.example.yaml` 均明示:命令行旗标 > `SPEED_*` 环境变量 > 可选 YAML/JSON 文件(`WithConfigFile`,文件缺席静默跳过)> 宿主目标结构体上已设的默认值。第四个源是隐式的:Load 只写有来源供值的字段。
- 命名面常量:`EnvPrefix = "SPEED_"`(config.go 的包级常量)、`EnvSeparator = "__"`(嵌套层级标记)、`KeyDelimiter = "."`(键路径段)、`TagName = "config"`(struct tag,当时仅 `required` 与 `-` 两个选项,config.go 的 tag 选项常量)。结构体字段名小写化、点号连接成键路径(如字段 `Database.DSN` → 键 `database.dsn`),同一键派生旗标 `--database.dsn` 与环境变量 `SPEED_DATABASE__DSN`(envVarFor,config.go 的命名派生)。单下划线不是嵌套标记(`SPEED_DATABASE_DSN` 解析不到 `database.dsn`),匹配到任何字段的键一律忽略,所以无关的 `SPEED_` 前缀变量无害。
- 校验纪律:未知键丢弃;`required` 键未供值报 `ErrMissingValue`;空值是值——string 字段可持有,无表示类型的字段(数字、bool、嵌套结构)对空串报错(`checkEmpty`,config.go 的空值校验),"shell 变量设了空值"绝不允许静默变成零值;错误信息逐键点名每个被查过的来源(`sourcesFor`,其中环境变量名按前缀推导)。
- 包注释已有一段层边界表述:"Bootstrap configuration is deliberately narrow……Values that operations needs to tune at runtime, and values a tenant may override, belong to the separate dynamic configuration module; they are not resolved here."——只到"另一个模块负责"为止,没有键级两层互斥规则(§2 补)。
- 自证无人使用:`config.example.yaml`(仓库根)头部明写 "IMPORTANT: this file is the FILE FORMAT'S example, not a file any process in this repository currently reads.";`go/pkgcore/config` 包内无宿主。

### 1.2 reference-app 直读面

reference-app 完全绕过 loader,`ConfigFromEnv`(internal/app/server.go,返回 `(ServerConfig, error)`)用 `os.Getenv` 直读 35 个键:34 个 `APP_*` + 1 个无前缀的 `PORT`。configrefgen 的 curated 目录(bootstrap.go 的 `bootstrapTable`)是这份面最完整的权威清单,按 7 组划分(组名、键数、键):

| 组 | 数 | 键 |
|---|---|---|
| core | 3 | `APP_DEPLOYMENT_MODE`、`APP_DB_PATH`、`PORT` |
| seams | 13 | `APP_REDIS_ADDR`、`APP_S3_ENDPOINT`、`APP_S3_BUCKET`、`APP_S3_ACCESS_KEY`、`APP_S3_SECRET_KEY`、`APP_S3_REGION`、`APP_S3_USE_SSL`、`APP_OBJECT_STORE_ROOT`、`APP_SMTP_HOST`、`APP_SMTP_PORT`、`APP_SMTP_USERNAME`、`APP_SMTP_PASSWORD`、`APP_SMS_GATEWAY_URL` |
| keys | 7 | `APP_ROOT_KEY` 与六枚派生键 `APP_CONFIG_KEY`、`APP_ORG_INDEX_KEY`、`APP_NOTIFICATION_INDEX_KEY`、`APP_PKI_LOCAL_KEY_CIPHER_KEY`、`APP_AUTHN_BLIND_INDEX_KEY`、`APP_AUTHN_PII_CIPHER_KEY` |
| network | 2 | `APP_TRUSTED_PROXIES`、`APP_READ_FLY_CLIENT_IP` |
| observability | 2 | `APP_OTLP_ENDPOINT`、`APP_PUBLIC_ORIGIN` |
| serving | 1 | `APP_WEB_DIST` |
| demo | 7 | `APP_DEMO_USERS_PASSWORD`、`APP_DEMO_PLATFORM_STAFF_PASSWORD`、`APP_AI_GATEWAY_IMAGE_BASE_URL`、`APP_AI_GATEWAY_IMAGE_API_KEY`、`APP_DISABLE_DEMO_USER_HEADER`、`APP_DISABLE_QUEUE_WORKER`、`APP_FAIL_SELF_SERVICE_PROVISION` |

curated 目录给每键注 kind(`string` 24 / `hexkey` 7 / `bool` 2 / `int` 2)、unset 回退表述、secret 标记(10 枚:keys 组 7 + `APP_S3_SECRET_KEY` + `APP_SMTP_PASSWORD` + `APP_SMS_GATEWAY_URL`,按表内 `secret: true` 计)、一行"配置什么"、example 建议值。

几个与迁移形态直接相关的语义事实:

- 密钥三段优先(`resolveKey`,internal/app/server.go):单独设的派生键 env(individual env,`parseHexKeyEnv` 解析)> `APP_ROOT_KEY` 设了之后按 HKDF-SHA256 按 purpose 派生(`dbkit.DeriveKey`)> 硬编码 dev default。任何一枚派生键都可单独覆盖、单独轮转。
- 存在式开关:若干键的语义是"非空即真"而非布尔解析——`APP_DISABLE_DEMO_USER_HEADER`、`APP_DISABLE_QUEUE_WORKER`(读 `os.Getenv(...) != ""`),curated kind 为 string 并注明 "any non-empty value disables them";`APP_READ_FLY_CLIENT_IP`、`APP_S3_USE_SSL` 是 bool kind;`APP_FAIL_SELF_SERVICE_PROVISION` 与 `APP_SMTP_PORT` 是 int kind。
- 深注释栖于 env 常量本身(server.go 的每个 `*Env` 常量一条),curated 表注释自述"每个声明点(常量注释)是表的深层来源"。`ServerConfig`(server.go)字段注释同样是权威叙述(如六密钥字段的注释块、`PublicOrigin` 的完整人口切分说明)。装配产物还含非 env 字段(`HostTenants`、`TrustedProxies` 拆分等)。
- 读点:`cmd/server/main.go` 两处 `PORT`(healthcheck 分支的 `runHealthcheck(ctx, os.Getenv("PORT"))`、`run` 内的 `ConfigFromEnv()`);`internal/app/server.go` 的 `ConfigFromEnv` 主体与 `resolveKey`;demo 密码常量定义在 `internal/app/demo_users.go`、`demo_admin.go`、`frontend.go`(webDist)、`ai-gateway` 两键亦在 server.go。`cmd/server/self_service_test.go` 的 `selfServiceJourneyConfigFromEnv` 也调用 `ConfigFromEnv()`。

### 1.3 运行时动态配置层现状(对比面)

- 运行时层的注册席是 `pkgcore.Registry.Config`(`ConfigSchemaRegistrar`,registry.go 的 Config 席位),项类型 `ConfigItem`(registry.go):dotted Key、闭集 Type(string/int/bool/duration)、Default(Go 值)、Sensitive、Description、Group、Public、Min/Max。`Add` 全量校验后才入册,重复键报 `ErrDuplicateConfigKey`。Feature 旗标是独立席位 `Registry.Features`。
- `Register(reg *pkgcore.Registry)`(如 authn 模块的 `Register`,module.go)内以 `reg.Config.Add(configItems()...)`、`reg.Features.Add(...)` 收口(configrefgen/host.go 头注所称 "census of reg.Config.Add and reg.Features.Add declaration sites")。现行 census:**authn、metering、compliance、sharing、pki 五模块声明 ConfigItem,org 不声明 items、只声明两枚 feature flag**(org 的模块测试显式断言 `reg.Config.Items()` 为空,org 的 module_test.go)。configrefgen 的 schema host 正是这五模块 + org + config 模块的冻结组合。
- 运行时项最终进 configs 表、经 `config.Service` 的 Describe 枚举、可由运维在线编辑、经 `config.item.changed` 等事件传播——与启动层在生命周期、作用域、编辑面上完全正交(§2)。

### 1.4 configrefgen 与文档链现状

- configrefgen(tools/configrefgen;原位于 examples/reference-app/cmd/configrefgen,后归位为独立 Go 工具模块——go.work use 条目在 go/ 之外,锁步发布不产 tag,见 §5.2 迁移注记)是唯一同时接触两层的工具:动态层由它自组的 schema host(host.go)经 Describe 枚举;bootstrap 层用两个机制(文件头注自述):(1) AST 扫描 `internal/app` 与 `cmd/server`,提取全部 `"APP_…"`/`"PORT"` 字面量成总账——总账派生自代码,绝不手列;(2) curated 事实表(键的 kind/回退/secret/summary/group/example)配**双向覆盖门**:总账键必须恰好各出现一次,表行必须都是真实读键,否则生成器失败。
- 输出与字节幂等(main.go 的 `renderOutputs`):仓库根 `docs/config-reference.md`、`docs/config-reference.json`、根 `.env.example`、`config.example.json` 与站点页 `docs/site/content.en/docs/user-guide/configuration.md`(根 `.env.example` 头注明为 configrefgen 所生成、键缺漏会触发 drift 门;步骤二落地后该产物随宿主面一起移除,产物集为四件,§9)。`--check` 接 docs-check.yml 的 Config reference drift check 步(归位后为工作目录 `tools/configrefgen` 下 `go run . --check`;configrefgen 自身测试由 fast-check 与 full-check 的 `tools/configrefgen` 矩阵行跑)。
- `docs/config-reference.md` 导言目前自述 "The generalized loader mechanism behind this surface is `go/pkgcore/config`: …… This app's own bootstrap does not drive the loader (its values carry app-specific resolution rules, key derivation among them)……"。这句话在迁移后必须翻转。
- `config.example.yaml`(仓库根,手写)配 `config_example.go`(configrefgen 内的 loader 形状结构体,子集:deploymentmode/port/dbpath/redis.addr/smtp.*)与一个单测——用真实 loader 加载真实文件,验证格式与四源优先序。该示例维持 `SPEED_` 拼写族。

  > **落地注记(模板化):** 该示例已从"格式演示集"改写为面向用户的启动层文件模板:主体为六枚声明键(authn.blind_index_key、authn.pii_cipher_key、config.cipher_key、notification.contact_index_key、org.invitation_email_index_key、pki.local_key_cipher_key),占位值 `REPLACE_WITH_64_HEX_CHARS`,逐键注明保护什么、`SPEED_` 拼写与旗标等价写法、部署提醒;上列宿主自有键降为明确标注的第二小节(演示类型、嵌套与四源)。`config_example.go` 的 loader 形状结构体随之扩为对应六枚嵌套字段,load 测试断言六键解析(JSON 孪生测试经逐键相等自动覆盖)。同时 `docs/config-row-examples.json`(configs 表行导出)删除:该文件源于一次需求误读,用户要求的是配置文件示例(JSON+YAML 成对,帮助用户设置配置),与表行导出无关;生成产物 footer 中的引用句随之移除,`bootstrapIntro`(MD 与站点 EN 页共享)与站点 zh 手写页各补一句指路,指向仓库根成对示例。
- 站点与文档链:docs/site 下 content.en 与 content.zh-cn 对称,2026-09-10 按 `APP_|config-reference` 对两语区实跑 grep 各命中 8 页——`config-reference` 字面在站点零命中,8 页全部因 `APP_` env 字面命中(`SPEED_|os.Getenv|ConfigFromEnv` 复查面仅另见 i18n 页的 `SPEED_LOCALE_STORAGE_KEY` 与 observability-and-ops 页的 `OTLP_ENDPOINT` 直读示例,均与引导链无关,不入清单)。命中页按步骤二是否需人工核对分两类:

  - **需核对(五页,均 user-guide 域,入 §5.3 清单与 §7 评审第 4 路):**`operating.md`、`walkthrough-reference-app.md`、`domains/frontend-building.md`、`modules/tools/saasctl.md`(原清单四页,部署/启动/引导的 env 叙述),以及本轮补入的 `modules/core/pkgcore.md`——其宿主引导示例直读 `os.Getenv("APP_DEPLOYMENT_MODE")`,步骤二迁移后将成为站点里把 APP_ 键教成直读的唯一示例,必须改口;
  - **可存活(三页,不进清单):**`quickstart.md`、`modules/web/_index.md` 仅引 env 名(名不改即真);developer-docs 域 `modules/tools/saasctl.md` 是生成骨架 twin 的模块文档,骨架按 §1.5/Q1 维持直读。

  站点外的按名消费载体:`examples/reference-app/README.md`、`DEPLOY.md` 是操作文案载体;`docker-compose.yml`、`docker-compose.distributed.yml`、根 `fly.toml` 与 CI(full-check 双副本用 `APP_DEPLOYMENT_MODE=distributed`、`APP_DISABLE_QUEUE_WORKER=true`;scaffold-verify 以 distributed 模式起生成骨架)按名消费 env。
- 双重遗留:`examples/reference-app/.env.example`(191 行,手写,最后实质内容提交 9e3efa28)与根生成 `.env.example`(100 行)并存、主题相同,是迁移文档链时要清理的重复载体(§5.2)。

### 1.5 saasctl 一侧

`go/saasctl/internal/appconfig` 是**生成骨架**的 env 面 twin:骨架模板 `internal/template/project/cmd/server/config.go`(20 键、同款缺省、同款完备性规则与错误文案)由测试 pin 防止漂移;`saasctl config print`、`db` 命令共享该解析以"看到应用会看到的东西"。即生成消费者骨架同样直读 env、不驱动 loader——它是"可自由编辑的启动骨架",维持现状(§7 开放问题 Q2)。

### 1.6 缺口:为什么需要这个机制

- 平台没有任何模块级渠道声明 bootstrap 键,与"逐模块列 bootstrap 键"的裁定相悖:模块的启动期契约(它需要宿主在启动时喂什么密钥、什么格式、是否敏感)只能写在应用侧 env 常量的注释里,模块自己无从声明。
- reference-app 作为"强制第一消费者"完全绕过 loader:平台机制零使用、零验证、零演示,`config.example.yaml` 的 "no process reads it" 是这条纪律洞的实锤。curated 表 + 覆盖门其实已经是一个"声明载体"的雏形——但它挂在 configrefgen 这个生成器私有实现里,模块无法参与、宿主无法复用、与 loader 绑定面没有关系。
- 运行时层与启动层之间只有一句话的边界,没有键级规则(§2)。

## 2 边界陈述:bootstrap 键与运行时配置项

以下陈述是**持久边界陈述**,落四处:本文件、`go/pkgcore/config` 包注释、registry.go 中 Config 与 Bootstrap 两个 registrar 的文档注释(英文对应句,互相指认)。陈述文本(中文版;代码注释里写英文对应):

> 一个配置键只能属于一层。bootstrap 键是进程启动输入:在 flag > env > 可选配置文件 > 宿主结构体默认 的四源链上解析一次,之后不再读取;没有租户维度,不随运行期编辑变化,改动只在下一次启动生效。运行时配置项是 configs 表里的动态值:按租户(或平台)维度存储,运维在线编辑,经 config 模块的服务与事件传播,即时生效。同一 dotted 键在两层同时声明被禁止——同一标识符承载两种含义、两种默认、两种编辑面,运维在后台改"那个键"时无法知道改的是哪一层,而两层各自的校验体系(configs 表约束 vs loader 校验)会让双声明必然有一处是复制的、终将漂移。

机器防线两条:

1. **Kernel.Bootstrap 汇总双席**:模块注册环结束后,对比 `registry.Config.Items()` 与 `registry.Bootstrap` 项的键集,交叠即启动失败(报错点名键与声明它的两层)。注册时两席互不可见,双键冲突只有到汇总点才能抓,而 Kernel.Bootstrap 正是两席都汇聚的地方(步骤一落地)。
2. **configrefgen 双面对账**:生成参考面本就同时枚举两层,`--check` 发现同一键同现两层即红。这是永久防线——参考面不退役,对账不退役。

键形似不是冲突:运行时 `authn.*` 项与 bootstrap 键 `authn.pii_cipher_key` 字符串不同即合法。惯例上 bootstrap 键沿用其语义归属模块的前缀,让两层键在文档里天然可分(§3.5)。

## 3 机制形状:注册席、声明项与绑定关系

### 3.1 席位命名与位置

- 新席位:`Registry.Bootstrap`,类型 `BootstrapRegistrar`,项类型 `pkgcore.BootstrapKey`。`NewRegistry`(registry.go)注入内存实现 `memoryBootstrapRegistrar`。`Register(reg *pkgcore.Registry)` 不变;新席位是 Registry 结构体的新字段——这正是 registry.go 中 `Registry` 结构体字段注释规定的跨切机制扩展方式("added as a field here rather than as a method on Module"),lockstep 下所有模块同版本发布,无需改 Module 接口。
- 不叫 `ConfigEnv`/`Env`/`EnvKeys`:bootstrap 面含旗标、文件、结构体默认,env 只是其中一源;叫 Env 会把机制窄化成"环境变量清单"。叫 `Bootstrap` 与 `pkgcore/config` 包内 "bootstrap configuration" 术语一致,并与运行时 `Config` 席位在字面上就分属两层。
- Config 席位注释补一句指向 Bootstrap 席位("items editable at runtime——process-start keys belong to Bootstrap"),Bootstrap 席位注释写反向句,并把 §2 的互斥规则写进两边(或共享一段同一措辞)。

### 3.2 `BootstrapKey` 字段集

| 字段 | 含义 | 与现存面的关系 |
|---|---|---|
| `Key` | dotted 键路径,与 loader 键同一命名面(点键、旗标与配置文件用) | 对齐 `ConfigItem.Key` |
| `Format` | 值格式,闭集 `string`/`int`/`bool`/`hexkey` | 即 curated 目录的 kind 全集;`hexkey` 是 32 字节十六进制密钥材料。闭集先例 = `ConfigItem.Type`;扩展格式需平台轮 |
| `Default` | 文档性回退表述(string;无则空) | 对齐 curated 的 fallback 列("standalone"、"documented non-secret development default"……) |
| `Sensitive` | 是否密钥材料 | 对齐 curated 的 secret 列;输出永不打印值,标记只决定呈现与部署指引 |
| `Description` | 英文契约文本:键保护什么、为什么单独成键、与相邻键的隔离规则 | 对齐 curated 的 summary + 常量深注释的要点 |
| `Group` | 渲染分组;平台模块按惯例用自己模块名 | 对齐 `ConfigItem.Group` 惯例 |
| `Example` | `.env.example` 建议值(空 = 建议留空) | 对齐 curated 的 example 列 |

**为什么 Default 是文档性字符串而不是 Go 值**:绑定的默认真源是宿主 loader 目标结构体(loader 的第四源就是结构体上已设的值);若声明层再放一份 Go 默认,同一键就有了两份运行语义默认,必然漂移。声明层只做"回退时行为是什么"的文档表述——正好是 curated fallback 列今天的内容。

**为什么没有 `required`/`Min`/`Max`/`Public`**:required 是绑定面结构体 tag 的事(宿主自己决定哪个键不许缺);Min/Max/Public 是动态层语义(可编辑值需要约束与公开端点),启动键无对应物。字段集刻意保持与 curated 目录一一映射——合并裁定(设计输入 1)的直接体现:声明面一步接住生成器今天需要的全部事实,curated 表才有退役之日。

### 3.3 Add 校验

`BootstrapRegistrar.Add(keys ...BootstrapKey) error`,风格对齐 `ConfigSchemaRegistrar.Add`(registry.go:273-289):

- 空 Key、未知 Format、Description 为空的敏感键、`Example` 与 `Default` 之外无其它约束;
- 重复 Key(同次或跨次)拒绝,包新哨兵 `ErrDuplicateBootstrapKey`——两个模块声明同一键是 bug 不是合并,先例 `ErrDuplicateConfigKey`;
- 单次调用任一项非法则整体不入册。

### 3.4 声明、绑定与一致性校验的关系

三层职责分明:

1. **模块声明层**(`reg.Bootstrap.Add`):模块在 Register 时声明它消费的启动键——契约/文档/校验面。声明不读任何东西:模块永不自行读 env 的纪律不变,键值仍由宿主解析后注入(与今天的 KeySource 式注入同构)。
2. **宿主绑定层**(loader 目标结构体):默认值真源、tag 选项、required、类型解析。宿主决定键的取值,声明只决定键的语义。
3. **一致性校验层**:loader 把声明键与绑定结构体对账。新增导出函数于 `go/pkgcore/config`:

```
func Verify(target any, declared []string) error   // 每个 declared 键必须能在 target 上找到字段
```

签名取 `[]string` 而非声明项类型,是为了不引入 `pkgcore/config` → `pkgcore` 根的导入环(注册席类型在根包,加载器在子包;宿主/生成器把注册键投影成 []string 即可)。严格双向校验(结构体每个字段都有声明出处)由引用应用自备:它在 Verify 之外把宿主键清单(模块声明之外的 29 枚)也并进 declared 集合,两次 Verify 即得全等。**一致性校验归属裁定:入步骤一**——Verify 是 configrefgen 改造与模块声明的共同底座,等不到步骤二;引用应用的两向严格接入随步骤二的结构体迁移一起落。

### 3.5 键归属裁定:平台模块声明什么

平台模块只声明**平台语义键**,即密钥组七枚中六枚派生键(它们的格式、敏感性、密钥隔离规则是模块的契约,不是宿主的):

| 模块 | 键(点键形,以现有 env 常量注释语义为准) | env 名(不改) |
|---|---|---|
| authn | `authn.pii_cipher_key`、`authn.blind_index_key` | `APP_AUTHN_PII_CIPHER_KEY`、`APP_AUTHN_BLIND_INDEX_KEY` |
| org | `org.*`(邀请邮箱盲索引键) | `APP_ORG_INDEX_KEY` |
| notification | `notification.*`(联系方式盲索引键) | `APP_NOTIFICATION_INDEX_KEY` |
| pki | `pki.local_key_cipher_key` | `APP_PKI_LOCAL_KEY_CIPHER_KEY` |
| config | `config.*`(configs 表敏感值密封主键) | `APP_CONFIG_KEY` |

`APP_ROOT_KEY` 是**宿主键**:宿主根变量的命名与"显式单键胜过派生"的优先级应用归宿主;派生链的归属已被 §9.4 取代——原判"目的串、dev default、HKDF 调用属参考应用装配逻辑,不入任何平台模块"作废,最终归属为**席位 + 工具箱**:目的串约定(每个声明键路径一个 `"speed." + keyPath + ".v1"`)随 BootstrapKey 席位落在 `pkgcore`,材料派生由 `dbkit.DeriveKey` 原语承担;dev default 与根变量名仍归宿主。六派生键声明后,"每枚密钥为何单独存在、为何 AES 不当 HMAC 用"这类理由由模块声明承载(参考应用现有常量深注释是文案来源),参考应用侧常量注释在步骤二退役。

其余 29 枚(core/seams/network/observability/serving/demo + root)是宿主键:接线地址、demo rig、故障注入、端口、部署形态——装配面的东西,归宿主。它们不会因为没有模块声明而失去文档:步骤二起由 loader 形状结构体的字段注释承载(§5.1)。

### 3.6 lockstep 姿态

全部新增是纯增量公开 API:Registry 新字段与新接口、`BootstrapKey` 类型、`config.WithEnvPrefix`、`config.Verify`、新 tag 选项、`ErrDuplicateBootstrapKey` 哨兵。既有调用零改动;lockstep 意味着平台与模块同版发布,不存在"旧消费者配新平台"的矩阵,增量即安全。文档纪律(新公开 API 同 PR 配用法文档、可编译示例、模块 AGENTS.md 条目)在步骤一适用:包文档补 §2 边界句与钉/前缀说明、`pkgcore/config` 的 godoc 示例区补前缀用例、受声明影响的模块 AGENTS 行列入步骤一 checklist。声明内容(description 文案)的修订不构成 API 变更,同版本随时可改。

## 4 WithEnvPrefix 与 env 直名钉

### 4.1 为什么仅前缀不够:钉的必要性

裁定 2("`APP_` 链不改名")与 loader 现有 env 命名规则存在两处不可调和:

1. 参考应用的 env 名是**平铺单下划线**(`APP_AUTHN_PII_CIPHER_KEY`、`APP_DB_PATH`),loader 派生名 = 前缀 + 键大写 + 点转双下划线:键 `authn.pii_cipher_key` 在 `APP_` 前缀下派生为 `APP_AUTHN__PII_CIPHER_KEY`(双下划线),对不上;
2. `PORT` 无前缀,前缀过滤直接把它挡在门外——而它是 Fly 等平台自动注入的惯例端口变量,不能改叫 `APP_PORT`。

所以 loader 必须支持**每字段精确 env 名**:struct tag 新选项 `env=`(`config:"env=APP_AUTHN_PII_CIPHER_KEY"`)。有了钉,前缀只对未钉字段生效,`APP_` 链 35 个名字一字不动(§5),`SPEED_` 默认族也一字不动。

### 4.2 WithEnvPrefix 语义

```
func WithEnvPrefix(prefix string) Option
```

- 默认:`SPEED_`(`EnvPrefix` 常量保留,注释改为"默认前缀")。`New` 先落默认再应用选项,与现 Option 机制同构。
- 覆盖是**每 Loader 一次**:前缀是 Loader 字段(现有 `Loader` 结构加一成员),影响(1) env 读取的过滤前缀、(2) 未钉字段的 env 名派生(`envVarFor` 从包级函数改 Loader 方法,内部不再引用常量)、(3) 错误信息 `sourcesFor`/`checkEmpty` 里点名的环境变量名。已钉字段完全不受前缀影响(它们进入过滤包含集,派生与错误文案用钉名)。
- 与 `WithEnviron`/`WithArgs` 正交:注入 env 的测试照旧;`WithEnviron([]string{})` 依旧整体关闭 env 源,前缀随之无关。
- 校验:空前缀或不以 `_` 结尾的前缀在 `New` 应用选项时 panic——装配期编程错误,先例是 `NewRegistry` 对 nil bus/kv/mailer 的 panic(registry.go 的 `NewRegistry`)。理由:空前缀等于"读取进程全部环境变量",会把无关变量卷进解析面;不以 `_` 结尾会拼出 `FOOPORT` 这类错位名。默认值与 `APP_` 都满足尾 `_` 惯例。
- 文件名/旗标键与 env 名解耦:前缀只改 env 拼写,点键与旗标不变——`WithEnvPrefix("APP_")` 的宿主,键 `port` 的旗标仍是 `--port`。

### 4.3 env 钉语义

- 钉在结构体 tag:`config:"env=APP_X"`。`parseTag`(config.go)已有"未知选项即报错"的机制,新增选项自然被旧 loader 拒绝(向后兼容:旧代码无此 tag,不受影响)。
- 钉名必须与字段键共存:env 源过滤集 = 前缀匹配 ∪ 钉名精确匹配;钉名查表直接映射回点键,不再做分隔符翻译(钉名里的下划线就是下划线)。
- 冲突检测(describe 期,报 `ErrInvalidTarget` 并点名双方字段):两字段钉同一 env 名;某字段的钉名与另一字段的派生名相同。env 名匹配大小写敏感(进程环境本来区分大小写),钉写错大小写 = 读不到,由一致性校验与测试兜底。
- 允许空钉名吗:不允许(`env=` 无值视为未知选项报错)。
- 一个钉的键可以是任意前缀匹配不到的变量(`PORT` 即此情形)——钉名进入包含集,前缀过滤不再拦截。

### 4.4 readEnv 实现契约

`readEnv`(config.go)目前把 koanf 的 env provider 当过滤与变换器用。步骤一把它改写为直接的环境遍历(或等价语义):

1. 取环境(os.Environ 或 `WithEnviron` 注入),逐条拆 `KEY=VALUE`;
2. 接受条件:`KEY` 以本次前缀开头(派生),或 `KEY` ∈ 钉名集(查表);
3. 派生变换:去前缀、`__` 转 `.`、小写——与现行为一致;钉变换:查表给点键,键去前缀不适用;
4. 其余变量(无关前缀、无钉名)保持被忽略——"无关 `SPEED_` 变量无害"的既有承诺不变(accepts() 本就再滤一遍)。

改写是内部实现选择,本节点定义语义与测试契约;唯一必须保留的对外性质:合并确定性、单下划线不嵌套、未知键忽略、空串纪律。

### 4.5 文本值到非字符串字段的弱类型现状与补强

env 与旗标产出的一律是字符串;loader 现 `decode`(config.go)用默认 `UnmarshalConf`,string→int/bool 是否成立**目前没有被测试 pin 过**(config_test.go 无 ParseBool/WeaklyTyped 痕迹;config.example.yaml 里的数字来自 YAML 文件原生类型,不经过文本转换)。步骤一必须补强并 pin:文本源到 int/bool 按 strconv 语义转换(测试钉住可接受值集合,建议 `strconv.ParseBool` 全集:1/t/TRUE/true/True/0/f/FALSE/false/False),空串对无表示类型维持报错(`checkEmpty` 不松)。这是步骤二 `APP_SMTP_PORT`(int)、`APP_S3_USE_SSL`/`APP_READ_FLY_CLIENT_IP`(bool)迁移的前置条件。

### 4.6 测试面(步骤一)

| # | 用例 |
|---|---|
| 1 | 回归:无选项时 `SPEED_` 语义与现有全部测试逐字不变(默认前缀 pin) |
| 2 | `WithEnvPrefix("APP_")` 下未钉键从 `APP_PORT` 读、`SPEED_PORT` 不再被读 |
| 3 | 钉键 `env=PORT` 在 `APP_` 前缀下被读,且前缀过滤不拦无前缀钉名 |
| 4 | 钉键在默认前缀下同样被读(钉与前缀无关) |
| 5 | 两字段同钉 / 钉与派生撞名 → describe 报 `ErrInvalidTarget`,点名双方 |
| 6 | 错误信息(`sourcesFor`、`checkEmpty`)点名实际前缀派生名或钉名,不写死 `SPEED_` |
| 7 | `WithEnviron([]string{})` 关闭 env 源后前缀与钉均无效果 |
| 8 | 前缀不影响旗标名与配置文件键(点键不变) |
| 9 | 空前缀、非 `_` 结尾前缀 → `New` panic |
| 10 | 文本 string→int/bool 转换语义与空串拒绝(补强后 pin,§4.5) |
| 11 | 无关前缀变量(`OTHER_*`)仍被忽略 |

### 4.7 拒绝的替代方案

- 改 `EnvSeparator` 为单 `_`、让单下划线兼任嵌套标记:破坏已文档化、config.example.yaml 与既有测试 pin 的 `SPEED_DATABASE__DSN` 格式,并让含下划线的平铺键名与嵌套无法区分——把歧义引回机制本身。
- 要求参考应用把结构体字段名写成字面含下划线的标识符以蹭派生:Go 允许但键路径(旗标/文件键)不可读,且与嵌套段的区分问题依旧。
- 宿主侧"别名翻译表"(加载后把派生名翻译回旧名):等于把钉藏在装配代码里,文档、校验、错误信息三处失真。
- 不迁移 `PORT`(留一处直读):破坏"loader 绑定结构体 = 全部启动输入"的单一事实面,一致性扫描门必须为它开白名单,演示价值打折。

## 5 reference-app 迁移形态

### 5.1 分档与表达决策

**35 键全部进入 loader 绑定结构体**,不设旁路;demo rig 与 kill 开关不靠"留在宿主直读"表达(那等于承认启动面可以有两套),而靠字段分组与语义类型表达。逐档决策:

| 档 | 键 | 表达 |
|---|---|---|
| 部署与接线(core/seams/network/observability/serving,21 枚) | 见 §1.2 表 | 普通字段 + `env=` 钉 + 结构体默认;kind 随 curated(int/bool/string)转字段类型 |
| 密钥组(keys,7 枚) | `APP_ROOT_KEY` + 六派生键 | 全字段入结构体,类型为 hex 文本(string);三段优先派生(`resolveKey`)移到"加载后推导步",目的串与 dev default 常量随迁;individual env 显式(非空)胜出、派生次之、dev default 兜底的语义逐字保留;loader 的 string 空值按"未设"进入推导,与现在 `os.Getenv` 空串视同未设一致。六派生键同时是模块声明键(§3.5),结构体字段与声明的一致性由 Verify 保证 |
| demo rig 与 kill 开关(demo,7 枚) | `APP_DEMO_USERS_PASSWORD`、`APP_DEMO_PLATFORM_STAFF_PASSWORD`、`APP_AI_GATEWAY_IMAGE_BASE_URL`、`APP_AI_GATEWAY_IMAGE_API_KEY`、`APP_DISABLE_DEMO_USER_HEADER`、`APP_DISABLE_QUEUE_WORKER`、`APP_FAIL_SELF_SERVICE_PROVISION` | 存在式开关(`DISABLE_DEMO_USER_HEADER`、`DISABLE_QUEUE_WORKER`)保持 string 型与"非空即真"语义——零行为变化;`FAIL_SELF_SERVICE_PROVISION` 为 int;AI 网关 demo 两键为 string。demo 组不因"只是演示"而降低载体一致性 |
| `PORT` | 无前缀惯例变量 | `env=PORT` 钉;healthcheck 分支与主进程共用同一份 loader 结果(`runHealthcheck` 的 `os.Getenv("PORT")` 换成 cfg 值),cmd/server 内直读清零 |

**结构体形态裁定**:`ServerConfig`(server.go)保持装配面不变([]byte 密钥、`pkgcore.DeploymentMode`、拆分后的切片等字段都不是 loader 的弱类型能直接填的形状);新增一个"loader 形状结构体"承载 35 键(字段类型 = string/bool/int 的 loader 形态,字段注释从 env 常量深注释与 ServerConfig 字段注释搬家,`env=` 钉齐 35 名)。`ConfigFromEnv` 退役为"transform":loader 填充形状结构体后,hex 解析、密钥派生、`DeploymentMode` 解析、`TrustedProxies` 拆分在 transform 内完成,产出 `ServerConfig` 不变。env 常量(server.go 的 `*Env` 常量组)与 `internal/app`、`cmd/server` 全部 `os.Getenv` 删除——`os.Getenv` 从参考应用可执行代码归零(§5.2 的扫描门因此可达)。

**迁移等价性验证**:单测把"同一份 env 注入集"分别喂迁移前 `ConfigFromEnv` 与迁移后 loader+transform,断言产出同一 `ServerConfig`(含 dev default 路径、root 派生路径、individual 覆盖路径、空串路径);main_test、self_service_test.go 的引导辅助、flowtests 的环境注入点按"值语义核对清单"过一遍(任何把开关设空串当"关"的用法在 loader 下会变成启动报错——bool/int 键的空串收紧是有意行为,清单要找出所有此类注入点改成显式值或留空不设)。

### 5.2 configrefgen 后果

> **实施注记(2026-09-10,用户裁定 A):** 本节原案的"宿主 29 键进平台产物、根 `.env.example` 重生成、旧 `examples/reference-app/.env.example` 删除"三点已由裁定 A 取代——平台参考面不含宿主变量节,curated 表与宿主渲染整体删除而非推迟,根 `.env.example` 产物移除,`examples/reference-app/.env.example` 保留为宿主键的文档载体之一。落地形态与替代门见 §9。

> **迁移注记(归位):** 生成器已从 `examples/reference-app/cmd/configrefgen` 归位为独立 Go 工具模块 `tools/configrefgen`(go.work use 条目在 go/ 之外,consumer-module 形态,锁步发布不产 tag、不进 changesets)。产物路径集不变(仍为 md/json/`config.example.json`/站点页四件);生成器内与产物内的宿主指涉同轮清除——产物与生成器内不再出现任何宿主变量名或 reference-app 指涉,`config.example.yaml` 的演示值中性化为 `app.db`。命令契约:在 `tools/configrefgen` 下 `go run .` / `go run . --check`;漂移门(docs-check.yml)与单测矩阵(fast-check/full-check 的 `tools/configrefgen` 行)按新路径接线。本节与 §7 描述的"表退役与换源"工作即在该新路径发生。

- curated 表退役:35 行事实的宿主侧 29 枚迁入 loader 形状结构体字段注释,六枚平台键迁入模块声明;`bootstrap.go` 的 AST 扫描器与 `bootstrapTable` 删除。
- 覆盖门重定义:结构体字段集 ≡ 模块声明集 ∪ 宿主键清单(双向,零差零漏),维持生成器"键缺漏即红"的既有承诺;另加"残留直读即失败"扫描:`internal/app` 与 `cmd/server` 出现 `os.Getenv` 即红(白名单空,healthcheck 迁移后无豁免)。
- 导言翻转:`docs/config-reference.md` 的 "This app's own bootstrap does not drive the loader" 删除,改写为 loader 驱动叙述(前缀 `APP_`、四源链、派生键三段语义一句话带过);"no process in this repository currently reads it" 的 `config.example.yaml` 注记同步翻转(§5.3)。
- per-module 键清单:生成的 bootstrap 节新增按模块分组的平台键小节(声明键的直接呈现),这是"逐模块列 bootstrap 键"裁定的文档兑现。

### 5.3 文档链同步清单(步骤二)

| 载体 | 动作 |
|---|---|
| `docs/config-reference.md` / `.json` | 重生成(源 = 模块声明;裁定 A 后宿主变量不进产物) |
| 根 `.env.example` | 裁定 A:内容天然是宿主变量表,随宿主面整体移出平台产物并删除文件(§9) |
| `config.example.yaml`(仓库根) | 头部注记翻转;"无进程读取它"的限制改为"无进程对它接 WithConfigFile";键集/示例值不动(它演示的是格式本身) |
| `examples/reference-app/.env.example`(旧载体) | 裁定 A:保留,与 DEPLOY.md 一起作为宿主键的文档载体之一 |
| `README.md` / `DEPLOY.md`(reference-app) | 操作文案与叙述更新 |
| `docker-compose.yml` / `.distributed.yml` / 根 `fly.toml` | env 名不变 → 只需值语义核对(空串/存在式用法) |
| CI(full-check、scaffold-verify、docs-check) | env 名不变 → 值语义核对 |
| docs/site en/zh 四页(operating、walkthrough-reference-app、frontend-building、user-guide 的 modules/tools/saasctl) | 叙述核对:凡写"应用直读 os.Getenv/loader 无人驱动"处翻转;键表若有引用与生成件对齐 |
| docs/site en/zh pkgcore 页(user-guide/modules/core/pkgcore.md,本轮补入) | 宿主引导示例改写:页内 `os.Getenv("APP_DEPLOYMENT_MODE")` 直读在步骤二后陈旧——平台文档不再把 APP_ 键教成直读,示例改 loader 驱动叙述或换非 APP_ 键的通用写法 |
| `go/pkgcore/config` 包注释 | §2 边界句、钉与前缀说明、示例 |
| 六模块声明模块自身文档/AGENTS 行 | 按文档纪律随步骤一(§3.6) |

## 6 零声明诚实条款

任何平台模块都可以声明零个 bootstrap 键。平台模块的启动键契约来自它实际消费的启动输入;不消费就是零,零声明是诚实状态,不是缺陷,也不是待办。机制不设最少声明数、不要求占位声明、不为凑数而把宿主键塞进模块;configrefgen 的 per-module 清单为空时渲染"本模块不消费启动键"或直接省略该模块行,与现有 host.go census("哪五模块真正声明 items、org 只声明旗标")同源。评审与 CI 都不把零声明当告警。这一条同样约束步骤二:reference-app 迁移不要求六平台键之外的任何键进入平台模块。

## 7 两阶段实施范围与验证

### 步骤一:机制 + 配置文档机制改造

交付物与文件域:

- `go/pkgcore/config`:`WithEnvPrefix`、`env=` 钉、`Verify`、文本弱类型补强、readEnv 语义改写、包注释(边界句 + 新机制说明),配 §4.6 测试面与 godoc 示例;
- `go/pkgcore`:Registry.Bootstrap 席位、`BootstrapKey`、`BootstrapRegistrar`、memory 实现、Add 校验、`ErrDuplicateBootstrapKey`、`NewRegistry` 接线,配席位级测试;Kernel.Bootstrap 双席键冲突检查(§2);
- 平台模块声明:authn(2)、org(1)、notification(1)、pki(1)、config(1)六键声明(文案源 = 参考应用现有常量深注释);metering、compliance、sharing 记录零声明(§6 条款适用);声明模块的 AGENTS.md 行按文档纪律补齐;
- configrefgen(现于 `tools/configrefgen`,见 §5.2 迁移注记):渲染改造(模块声明 → per-module 键清单;宿主 29 键过渡期仍走 curated 表,两源对账门:键/名/分组不重不漏,表头注明过渡态;同时实现 §2 防线 2——运行时项键与 bootstrap 键交叠即红)、`.env.example`/JSON 对齐、站点页与 `config.example.json` 同入产物集(步骤一即已渲染)、docs-check 配置不动(--check 即门);
- 明确不做:reference-app 任何运行面改动、env 名、站点页文案(§2 的机器防线 1 在 kernel 落、防线 2 在 configrefgen 落,都不依赖参考应用迁移)。

排序理由:声明面先行,六平台键在过渡态有"模块声明 + curated 行"双源对账,任何漂移在步骤一就被门抓住;渲染底座先行,步骤二只换源不换管线。步骤一不触碰参考应用运行语义,因此独立可绿。

验证期望:受影响模块 `task test` 全绿;`configrefgen --check` 通过且输出重生成提交;repo-checks/docs-check/lint 绿;golangci 零新告警。

### 步骤二:reference-app 迁移 + 全链同步

- loader 形状结构体 + transform(§5.1)、env 常量与 `os.Getenv` 退役、healthcheck 复用 cfg、等价性 pin 测试、值语义核对清单(flowtests/CI/compose 注入点);
- configrefgen(现于 `tools/configrefgen`)表退役与换源、输出重生成(§5.2;含站点页与 `.json` 两份产物——裁定 A 后产物集为 md/json/`config.example.json`/站点页四件);
- 文档链同步清单整表执行(§5.3),按裁定 A 执行(旧 `examples/reference-app/.env.example` 保留,根生成件删除);
- 两向 Verify 接入(宿主键清单并册,严格全等)。

验证期望:步骤一全部项 + flowtests 全绿;`go run ./cmd/server` 零 env 手动冒烟(standalone/SQLite 照常起);双副本 flowtests(CI full-check 同款)过;站点构建零告警。

### 评审四路一致检查(两步骤共同的验收定义)

1. 模块声明(六模块 `reg.Bootstrap` 项)
2. 生成文档(`docs/config-reference.md` bootstrap 节 + `.json`;裁定 A 后不含任何宿主变量节)
3. 示例文件(`config.example.yaml`、`config_example.go`)
4. 站点页(docs/site en/zh §5.3 清单五页:operating、walkthrough-reference-app、frontend-building、user-guide 的 modules/tools/saasctl,及本轮补入的 modules/core/pkgcore)

机器门:`configrefgen --check` 在步骤一覆盖 1↔2,在步骤二经换源后覆盖 1(声明)↔2(生成文档)↔3(示例文件的加载验证)——裁定 A 后宿主键不在产物内,门收窄为平台面:声明键集与渲染键集双向全等(由生成器自带测试承担)、产物字节与现场渲染一致(`--check`);站点页是静态叙述,无自动对账,归手工核对——评审清单固定四行:键名全集一致、分组一致、敏感标记一致、叙述与宿主示例均与当前阶段一致(宿主示例陈旧是独立核点:页内嵌的宿主引导示例,如 pkgcore 页的 `os.Getenv("APP_DEPLOYMENT_MODE")` 直读,在步骤二结束后不得残留)。步骤一结束时站点页允许停留在"过渡态前"叙述(应用还没迁移,旧叙述仍为真);步骤二结束时四路必须同时为真。

## 8 风险与工作量

| 风险 | 缓解 |
|---|---|
| bool/int 键空串收紧(现 "" 视同未设 → loader 报错)在 flowtests/CI/compose 里有注入点未被清单找出 | 步骤二的值语义核对清单先于结构体迁移执行(grep 全部 `APP_` 设置点),等价性测试含空串路径 |
| 过渡期(步骤一)六平台键有"模块声明 + curated 行"双源,文案漂移 | 双源对账门(键/名/分组一致) + 步骤二表退役消灭双源 |
| 表退役后新 `os.Getenv` 潜入参考应用 | "残留直读即失败"扫描门(白名单空) |
| 旧 `examples/reference-app/.env.example` 删除后引用残留 | 删除与 README/DEPLOY 重定向同一提交,评审 grep 复核 |
| loader 文本→int/bool 弱类型现状未 pin,实现时可能发现缺口 | 步骤一先补测试后修实现(风险集中在步骤一,修不动语义:空串纪律不松) |
| hex 文本 → 密钥的解析路径二选一(string+transform 已裁定,不引入 TextUnmarshaler 类型以免波及 ServerConfig 消费面) | 形态裁定已闭环,transform 单测 pin |
| Kernel 双席冲突检查插入位置依赖注册环实现细节 | 行为契约(交叠即启动失败)先行,位置实现时定,席位测试覆盖 |
| 渲染顺序/字节幂等被改造破坏 | 既有 `--check` 幂等回归 + 重生成提交 |

工作量(以现有轮次粒度估,每轮独立小提交、可单独绿):步骤一约 2-3 轮(loader 机制 + 测试、席位 + kernel 检查 + 模块声明、configrefgen 改造 + 对齐收尾);步骤二约 2-3 轮(结构体迁移 + transform + 等价性测试、configrefgen 换源 + 输出重生成、文档链与站点同步)。两步骤顺序必要,但任一步骤结束后仓库都处于可发布绿态。

**开放问题(需要用户/后续排期裁定,不在本设计内解决)**:

- Q1 步骤二之后,`saasctl` 生成骨架(模板 `cmd/server/config.go`,20 键)与 `appconfig` twin 是否另起一轮跟进 loader?骨架是"可自由编辑"的生成产物,维持直读 env 有可辩护性;但消费者骨架若永远直读,loader 机制在参考应用之外的第二个证明面就缺失。建议:独立后续轮评估,本两阶段不动。
- Q2 六平台键的点键拼写与声明文案(Default/Description/Example 逐字)以模块声明落盘为准,本设计不定死逐字文本;落盘时若发现键名与运行时项同层撞名等新问题,按 §2 规则处理并回写本文件。
- Q3 `config.example.yaml` 是否值得在步骤二后补一份 `APP_` 拼写族示例(现只有 `SPEED_` 族):现状已能表达格式,补一份是文档体验增强,不阻塞迁移,随站点文案轮评估。
- Q4 **步骤一落地时的版本钉子**:普通语义下"模块同轮消费新 pkgcore API"需要该 API 已发布——`Registry.Bootstrap`/`BootstrapKey` 在两阶段设计写就时尚未进入任何已发布版本,而 v0.0.1 之后的模块 `go.mod` 已按发布后的形态钉真实 tag(`go-module-ci` 第 5 腿的 `GOWORK=off go build` 因此会红)。伪版本不是出路:v0.0.1 已在代理上,v0.0.1 之后的提交在 MVS 里排在 `v0.0.1` 之下,`go mod tidy` 会把钉子升回已发布版本,模块拿不到新 API。因此本轮对五个声明模块(authn、org、notification、pki、config)采用仓库此前的过渡形态——`replace github.com/vislake/speed/go/pkgcore => ../pkgcore`(依赖方的 replace 对消费者无效,只影响模块自身的独立构建)。下一次 lockstep 发布需执行同类清理:删除五行 replace,把 require 提升到与新平台同版发布的 tag(与 `first_release_replace_cleanup` 同一清点方式);漏做则该模块的新代码在本仓库之外不可用,因为消费者会按 require 里的旧 tag 解析 pkgcore。这个清理随**下一个版本号的发布**执行,不能寄望 v0.0.1 重发:v0.0.1 的 21 个模块 tag 已从远端与本地删除,而 Go module proxy 对已服务过的版本不可撤回——缓存仍在且内容不可改写,删 tag 只让 v0.0.1 在仓库里不可再生,代理上那份依旧按原样解析,因此该版本号已经作废,既不能再发布也不能被新内容复用;下一次发布必须换一个新版本号,五行 replace 的删除与 require 的同版抬升随之落在该版本上。

## 9 步骤二落地记录(2026-09-10)

### 9.1 用户裁定 A:平台产物不含宿主变量节

用户裁定(2026-09-10)选定"方案 A":平台配置参考**不含任何宿主变量节**——curated 表(`bootstrapTable`)与宿主变量渲染从生成器整体删除,**不是**推迟;29 枚宿主键不再进入平台产物。连带后果,本节即最终形态记录:

- 根 `.env.example` 产物(内容天然是宿主变量表)随宿主面一起移出平台产物,文件从仓库删除;生成器产物集为四件:`docs/config-reference.md`、`docs/config-reference.json`、`config.example.json`、站点页 `docs/site/content.en/docs/user-guide/configuration.md`。
- §5.3 表中原"删除 `examples/reference-app/.env.example` 并重定向到根生成件"一行**作废**:该文件保留,与 reference-app 的 `DEPLOY.md` 和 `README.md` 一起构成宿主键的操作文档载体;宿主键的完整契约文本(含装配规则、拒绝条件、密钥隔离理由)则由 loader 形状结构体的字段注释承载(§3.5 的"29 枚宿主键由结构体字段注释承载"按此落)。
- 站点页职责随之变为:机制陈述 + 六枚已声明键 + 零声明条款;不再有任何宿主变量清单。
- 界面不变项:`APP_` 环境变量名一个不改;`PORT` 仍无前缀(钉名)。

### 9.2 步骤二的落地形态

- **宿主结构体与 transform**(`internal/app/bootstrap.go`):`hostConfig` 承载 35 键,字段类型随 curated kind(string/bool/int);每个字段都以 `config:"env=…"` 钉住自己的确切变量名(应用变量是平铺单下划线拼写,派生名对不上,所以 35 名全钉;`PORT` 同样钉名)。loader 以 `config.New(config.WithEnvPrefix("APP_"))` 驱动,第四源默认值(`DeploymentMode`/`Port`/`DBPath`)落在结构体上。`ConfigFromEnv` 保留原名与签名,成为"loader 加载 + transform"的入口;transform(`serverConfigFrom`)完成 hex 解析、三段式密钥派生、部署形态解析、`TrustedProxies` 拆分与全部跨变量拒绝规则。env 名称常量(`*Env`)退役,名字改由钉与文档承载。
- **六枚平台键的键路径**:authn/org/notification/pki/config 五组以嵌套结构体表达,子字段名(下划线形式)小写后与声明键逐字相同——`config.Verify` 按点键路径字面比较,这是唯一能让"声明键 ↔ 目标字段"直接对上的拼写方式(nolint 说明随字段)。
- **两向 Verify 接入**:`BuildServer` 在 `Kernel.Bootstrap` 之后调用 `verifyBootstrapBinding`,用现场注册表的声明键与宿主自有键清单(29 枚)各跑一次 `config.Verify`;严格全等的另一半(结构体没有无出处的字段)由 `internal/app/bootstrap_test.go` 的形态测试承担——反射走出的键集必须恰好等于"宿主键清单 ∪ 六枚声明键",且每个叶字段都带钉。
- **残留直读扫描**:生成器不再触宿主面(见 §9.1),"`os.Getenv` 即红"的扫描落在应用内:`examples/reference-app/unittest` 的模块形态测试扫描 `internal/app` 与 `cmd/server` 的可执行代码。
- **healthcheck**:`cmd/server` 的 healthcheck 分支与主进程共用同一份 loader 结果(`app.ConfigFromEnv` 的 `cfg.Port`),`cmd/server` 直读清零。
- **等价性 pin**:`flowtests/server_config_equivalence_test.go` 以"迁移前的直读实现"为 oracle,对同一份 env 注入集逐一断言新旧两条路径产出同一 `ServerConfig`(dev default、root 派生、individual 覆盖、空串、各类拒绝);有意的行为差异(见 §9.3)单独 pin,不进等价表。
- **值语义核对**:`APP_S3_USE_SSL`、`APP_READ_FLY_CLIENT_IP`、`APP_SMTP_PORT`、`APP_FAIL_SELF_SERVICE_PROVISION` 四枚 int/bool 键的空串注入点(测试/CI/compose)改为"显式值或不设";测试面用 `internal/testutil.ClearBootstrapEnv` 统一清理。附带一条同族语义:SMTP 端口现在以 0 为"未设"(`0` 本就不是可用 SMTP 端口)。
- **生成器换源**:`bootstrap.go` 只保留两层互斥规则(`overlappingKeys`);`document.go` 的 bootstrap 节渲染"机制陈述 + per-module 声明表 + 零声明条款";`--check` 产物四件;新增渲染键集 ↔ 声明集双向测试。`docs-check.yml` 的覆盖说明随参考面改写(配置参考只看平台面),而引用应用源码的触发条目保留并加宽为 `examples/reference-app/internal/**`(错误码索引扫描面需要,同时补上 `internal/cases`、`internal/notes`、`internal/smilesim` 的既有缺口),`docs/site/static/llms.txt` 的配置参考描述同步。
- **设计文档维护**:§1、§4、§5 内凡"文件:行号"式引用一律改为符号名优先(行号随步骤一/二落地已失效)。

### 9.3 有意行为差异与本轮未做

- 四枚 int/bool 键的空串收紧(§9.2)是设计裁定的有意行为;此外无行为差异。
- 未做(与本设计无关或按既定排期):saasctl 生成骨架与 `appconfig` twin 维持直读(Q1);五个声明模块的过渡 `replace` 随下一个版本号的发布清理(Q4);`examples/reference-app/.env.example` 目前只覆盖部分宿主键(其余键的装配文本以 `bootstrap.go` 字段注释 + README/DEPLOY 叙述为准),扩到 35 键留作文档体验增强。

### 9.4 根密钥派生上收为平台能力(pkgcore 席位 + dbkit 原语)

用户要求"config 模块须支持从一枚根密钥派生其他密钥"并裁定"纳入本次:设计+实现";§3.5 的"派生链不入任何平台模块"裁定由本节取代(宿主根变量名与优先级应用仍归宿主)。能力先在 `go/config` 落地,随后按"席位 + 工具箱"归属迁移定稿并记录于此:`go/config` 撤出全部派生代码,最终归属为席位(`pkgcore` 的 `BootstrapKeyPurpose`,声明语义)与工具箱(`dbkit.DeriveKey` 原语),宿主按两调用组合派生。

- **API**(最终归属:席位 `go/pkgcore/bootstrap_key_purpose.go` + 工具箱 `dbkit.DeriveKey` 原语,无新依赖):`BootstrapKeyPurpose(keyPath)` 返回声明键路径的目的串——字面拼接 `"speed." + keyPath + ".v1"`,不做任何字符串变形;材料由宿主组合派生:`dbkit.DeriveKey(rootKey, purpose)` 从 32 字节根密钥产出该键的 32 字节材料。目的串六枚冻结:`speed.authn.blind_index_key.v1`、`speed.authn.pii_cipher_key.v1`、`speed.config.cipher_key.v1`、`speed.notification.contact_index_key.v1`、`speed.org.invitation_email_index_key.v1`、`speed.pki.local_key_cipher_key.v1`(其中 config 一枚经改名,见下)。哨兵一枚(席位风格 plain `errors.New`,boot 期库面,不进 apperr 编码体系):`pkgcore.ErrInvalidBootstrapKeyPath`(空路径/空段,错误文本点名路径);根密钥长度错误回归 `dbkit.ErrInvalidKeySize` 原貌。`config.invalid_bootstrap_key_path` 与 `config.invalid_root_key` 两枚 apperr 码随之出清,错误码索引重生成;`go/config` 侧 `derivation.go`/`derivation_test.go` 与两枚哨兵删除,模块在引导层只保留席位声明 `config.cipher_key`。
- **形态理由**:六枚材料全部在 `Kernel.Bootstrap` 之前被消费(serializer 注册先于 `dbkit.Open`,后者先于 Bootstrap),收 `reg.Bootstrap.Keys()` 的批量签名服务不到真实宿主可用,故取按声明键路径取键的纯函数;"路径属于已声明键"由宿主绑定对账(`config.Verify` + 既有 `verifyBootstrapBinding`)承担。
- **稳定性契约**:目的串内嵌键路径且 HKDF 确定性,故**重命名声明键路径 = 轮换**——写进席位 godoc,并由 `go/pkgcore` 的冻结表测试钉死六枚目的串字面量(自 `go/config` 的派生测试迁移)。
- **参考应用迁移**(语义逐字不变):`internal/app/server.go` 的六枚 `speed.reference-app.*.v1` purpose 常量块删除;`internal/app/bootstrap.go` 的 `resolveKey` 形参 `purpose`→`keyPath`,派生改调两调用组合(`pkgcore.BootstrapKeyPurpose(keyPath)` 得 purpose、`dbkit.DeriveKey(rootKey, purpose)` 得材料),错误文案点名键路径;六个调用点传声明键路径字面量。`hostConfig.RootKey`、宿主键清单(`hostBootstrapKeys`)与六枚可辨认字节段开发默认值**保留**(归宿主)。
- **派生值变化**:六枚材料由 `speed.reference-app.*.v1` 派生改为 `speed.*` 派生,字节全部改变——版本未发布(v0.0.0),无既有状态需要迁移;`.env.example`、`DEPLOY.md` 与 zh 站页的机制表述同步改为平台归属。
- **测试**:`flowtests/server_config_test.go` 的六键期望改平台 API(互异断言保留);新增声明对账测试(boot 后遍历 `reg.Bootstrap.Keys()` 的 hexkey 键,断言每枚派生值==cfg 对应字段,错拼路径字面量必红——已以临时错拼实证);`server_config_equivalence_test.go` 的 oracle 改按平台规则派生(与生产调用点各自拼写,分歧即等价性失败);`internal/app/bootstrap_test.go` 的 `resolveKey` 用例同步。
- **文档面**:zh 站页 `configuration.md` 改写为平台口径(不含宿主变量名);生成产物机制句进 `bootstrapSecretsNote`(md 与站点 EN 页共享,JSON 无叙述字段不受影响):派生规则、六个 purpose 形状、"显式胜过派生"、"重命名=轮换";`go/dbkit/key_derivation.go` 与 `go/dbkit/AGENTS.md` 的 `APP_ROOT_KEY` 指认改指向席位的 purpose 约定与本原语的组合(纯注释);`go/config/AGENTS.md`(文件表/小节/Rules)、`doc.go`、`example_test.go` 的派生条目全部撤下,`pkgcore` 侧(席位 godoc、可编译 Example、AGENTS.md 条目与错误索引行)配齐。
- **改名=轮换(2026-09-10 用户裁定)**:六枚冻结目的串中 config 一枚随用户裁定改名——`speed.config.master_key.v1` → `speed.config.cipher_key.v1`,声明键路径随之 `config.master_key` → `config.cipher_key`。理由:密钥管理词汇里 "master" 指派生/解锁其他密钥者(KEK/根),而此枚是直接加密数据的 DEK、不派生任何东西;平台的根是宿主的 Root Key,旧名制造了并不存在的层级。新名与仓内家族同构(`authn.pii_cipher_key`、`pki.local_key_cipher_key`)。**因目的串内嵌键路径,改名即轮换**:该键的派生材料随之改变;版本未发布(v0.0.0)、零部署,无既有状态需要迁移,故按"改名=轮换"入册,而不是当作一次普通编辑(上列稳定性契约的第一个实例)。涟漪同工落地:`go/config` 声明与 AGENTS、席位 godoc 与冻结表测试、reference-app(字段路径、六个调用点字面量、对账与等价测试面)、示例与生成器机制句、站点页;宿主变量名 `APP_CONFIG_KEY` 属宿主命名(裁定 A),保持不变。
