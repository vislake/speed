# 密钥与证书生命周期：`go/pki`

> 本文描述 `go/pki` 模块的设计。它管的是**需要生命周期的密钥材料**——签名密钥与 X.509 证书——的签发、轮转、吊销与保护。
>
> 它不管传输层 TLS 证书（那是部署层的事，见"刻意不做的事"）。

## 需求来源：一次真实系统的诊断

本模块的需求不来自 [14 示例应用](14-reference-app.md)，而来自对一套真实生产系统（某企业级 DBaaS 平台的证书子系统）的诊断。该系统自建了三级 CA（根 10 年 / 中间 5 年 / 终端实体 1 年），用终端实体证书的私钥签发 JWT，并把租户证书的私钥下发给数据面集群使用。诊断出的问题按严重度排列如下，它们直接构成本模块的设计约束：

| 问题 | 表现 | 本模块的应对 |
|---|---|---|
| **零轮转能力** | 全代码库无续期/轮转逻辑，唯一的过期感知是签发 JWT 时调用 `checkValidity()` 抛异常——即证书一过期，登录全线不可用，且事前无任何预警 | 生命周期状态机 + `jobs` 到期扫描 + 提前续期 + 重叠期（见"生命周期状态机"） |
| **主密钥管理不成立** | 加密私钥用的主密钥默认由 `java.util.Random`（非密码学安全，48 位种子可预测）生成 32 位纯小写字母，明文写入进程工作目录的一个文件。容器重启文件丢失即所有私钥永久无法解密；多副本各生成各的，互相解不开 | 主密钥必须由宿主注入，无隐式后备路径（见"没有第二条路径"） |
| **私钥加密算法弱** | `Cipher.getInstance("AES")` 即 AES/ECB/PKCS5Padding，无 IV、无认证标签，而 PEM 是高度结构化明文 | 私钥不进业务表，交由 `Signer` seam 保护；`local` 实现走 `dbkit` 字段级加密 |
| **根 CA 私钥常年在线** | 三级私钥同库同表、同一把弱密钥加密，一次库泄漏即整条信任链失守，且架构上无法接入 KMS/HSM | `Signer` seam 的形状使"私钥永不离开边界"成为可实现的实现（见"Signer seam"） |
| **无吊销** | 删除账户只软删证书行，已下发的私钥到期前一直有效；链校验里明确关闭了吊销检查 | 吊销状态 + CRL 生成（见"吊销"） |
| **数据模型缺字段** | 存在 `CertificateType` 枚举却不落库，表中无 type/issuer/status/revoked_at，连"哪些证书 30 天内到期"都查不出分类 | 见"数据模型" |
| **序列号可预测且会冲突** | 序列号取 `System.currentTimeMillis()`，同毫秒并发签发撞唯一约束 | 序列号取 16 字节密码学随机数 |

这套系统与 speed 无代码关系，**不存在迁移需求**，它只作为需求镜子。但它证明了一件事：这些问题不是理论风险，是一套已上线系统的真实状态。

值得注意的是，speed 自己的 `go/authn` 患的是**同一个病的轻症**：`authn.KeySet` 有轮转所需的数据结构（active + retired 多 kid 验证），却没有轮转机制——`KeySet` 构造后不可变，"何时换新密钥、谁生成、旧密钥何时退役"全靠宿主在启动时决定。reference-app 传的是一把写死的开发种子。所以本模块不是"接管 authn 的轮转"，是**补上双方都没有的那一半**。

## 定位：为什么是独立模块

依赖图上位于 `config`/`jobs` 之后、`authn` 之前（完整图见 [01 整体架构](01-architecture.md)）：

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs
        -> storage / notification / pki
        -> authn / rbac / org / metering -> ...
```

**不并入 `pkgcore`**：与 [11 横切能力](11-cross-cutting.md) 拒绝把 `ratelimit` 并入 `pkgcore` 同理——`pkgcore` 只收纳每个模块都需要的通用原语，密钥生命周期是部分消费者需要的能力。而且本模块需要建表、需要 `jobs` 调度、需要租户上下文，这三样 `pkgcore` 都不该有。

**不并入 `authn`**：密钥生命周期与"谁是调用者"是两件事。X.509 证书签发与认证毫无关系，而 `authn` 已经是仓库里最大的模块。

**不并入 `dbkit`**：`dbkit` 在依赖图底层，本模块要用 `dbkit` 存表，并入即循环依赖。

**它实现 `pkgcore.Module`**，不同于 `ratelimit` 那样的纯库——它要注册配置项、权限、审计动作、事件与任务处理器。

## 两层结构

模块内部分两层，消费者只看自己那一层：

```
go/pki
├── 密钥生命周期层   状态机 / Signer seam / 到期扫描 / 事件      <- authn 消费
└── X.509 层         CA 链 / 证书签发 / PEM / CRL / JWKS 导出   <- 暂无消费者, 见下节
```

X.509 层建立在密钥生命周期层之上：一张证书就是"一把有生命周期的密钥"外加"一份由 CA 签名的身份声明"。

### X.509 层暂时没有真实消费者，这是一处明确破例

[15 里程碑](15-roadmap.md) 与仓库根 CLAUDE.md 都把「`examples/reference-app` 真实接入」列为模块完成的强制条件——模块 API 没有被真实消费者用起来，不算完成。**本模块的两层里，只有下面一层满足这条。**

- **密钥生命周期层有消费者**：`authn` 通过 `KeySource` 消费它，而 reference-app 装配 `authn`，因此是它的间接真实消费者。这条链是完整的。
- **X.509 层没有**：reference-app 是牙科 SaaS，不签发证书；催生本模块的那套 DBaaS 系统是 Java 的，只作需求镜子，不会成为 speed 的消费者（见"需求来源"）。

**仍然实现它，理由是这些需求已经被真实系统验证过。** 诊断出的问题——零轮转、根 CA 私钥常年在线、无吊销、序列号可预测——是一套已上线系统的真实状态，不是设想出来的。等到 speed 生态里出现第一个需要内部 CA 的项目再从头做，等于让那个项目自己踩一遍同样的坑，而"不让每个项目重造这些轮子"正是 speed 存在的理由。

**破例的代价必须说清楚：没有真实使用验证过的 API 形状很可能是错的。** 单元测试只能证明代码按自己的设想工作，证明不了这个设想对不对；缺的是那种"接进去才发现这个参数根本拿不到"的反馈。因此本层附带三条约束：

1. **必须有 godoc `Example` 函数**，覆盖签发一条完整证书链的主路径。CI 在每个模块的单元套件里编译并运行 `Example`，所以它至少保证这套 API 在**外部调用者的视角下**能编译、能跑通——这是无真实消费者时能拿到的最强保证。
2. **在 `go/pki/AGENTS.md` 的 Known limitations 里如实标注"X.509 层未经真实消费验证"**，不写成已完成。
3. **第一个真实消费者接入时，允许对这一层做破坏性调整**，不受"公开 API 视为冻结"的约束。speed 尚未发布，这条本来就成立；写在这里是为了让将来的人知道这个调整是**预期之内**的，不是设计失误。

密钥生命周期层不适用这三条——它有真实消费者，按正常标准要求。


**`authn` 不得 import X.509 层的任何符号**。JWT 验签只需要公钥和 kid，证书链对它毫无价值，反而引入证书解析与链校验的攻击面。这条由 semgrep 规则钉住，不靠自律。

## Signer seam：签名操作，不是取出私钥

这是本模块最重要的一个接口形状决定。

> 命名提示：本节的 `Signer` 是 pki 的基础设施接缝（"用这把密钥做一次签名"）。`go/authn` 里已有一个同名类型 `authn.Signer`，那是签发 JWT 的业务对象。两者不同包、不冲突，但下文同时出现，一律写作 `pki` 的 `Signer` seam 与 `authn` 的 `Signer`。

一个直觉的设计是让 seam 负责"保护私钥"——`Protect(key)` / `Unprotect(ref)`。**这个形状是错的**：它的语义是"把私钥解密后交给我"，意味着私钥必然以明文出现在应用进程内存里。接上 KMS 也只是换了个存放位置，私钥照样要取出来。诊断中那套系统正是如此。

正确的接缝是签名操作本身：

```go
// 接口形状示意，字段以实现时为准
type Signer interface {
    GenerateKey(ctx context.Context, algorithm string) (keyRef string, public crypto.PublicKey, err error)

    // Sign 的 input 语义由算法决定，不能统一成"传摘要"：
    //   ed25519    -> 完整消息。PureEdDSA 内部自己做哈希，标准库
    //                 crypto.Signer 对它的约定也是传消息、opts 取 crypto.Hash(0)
    //   ecdsa-p256 -> 消息的 SHA-256 摘要
    // 这个差异同时影响 KMS 直签的调用形状：Vault Transit 的 ed25519
    // 接口收完整消息，而 ECDSA 类接口收摘要。
    Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error)

    Public(ctx context.Context, keyRef string) (crypto.PublicKey, error)
    Destroy(ctx context.Context, keyRef string) error
}
```

`keyRef` 是一个不透明句柄，不是密钥材料。**业务表里只存 `keyRef` 与 signer 名字，从不存私钥。** 私钥在哪、以什么形式存在，完全是 `Signer` 实现的内部事务。

接口不复用标准库的 `crypto.Signer`，是因为它的 `Sign` 不接受 `context.Context`。对 `local` 实现无所谓，但 KMS 直签每次签名都是一次网络调用，没有 context 就没有超时、没有取消、没有 trace 传递——这三样在一个每次登录都要走的路径上都不是可选项。

这个形状同时容纳两种模式：

- **信封模式**——真实私钥由外部服务加密后存在本地，用时解密到内存签名。
- **直签模式**——私钥在外部服务内生成、从不导出，每次签名是一次 API 调用。

做成 `Protect/Unprotect` 会把直签模式从架构上永久排除，而直签模式恰恰是保护根 CA 私钥的唯一正确方式。

### 三套实现，各自独立成子包

| 包 | 模式 | 私钥所在 | 说明 |
|---|---|---|---|
| `go/pki`（根包，内置 `local`） | — | 本地库，`dbkit` 字段级加密 | 零外部依赖，`task dev` 用它，兼作测试替身 |
| `go/pki/signer/vault` | 信封 + 直签 | Vault Transit 引擎内 | 私有化部署的主力选择 |
| `go/pki/signer/kmsaws` | 信封 + 直签 | AWS KMS 内 | 公有云部署 |

**每套供应商实现是 `go/pki` 模块内的一个独立子包**，在 `init()` 中向 `SignerRegistry` 注册自己，宿主 import 哪个就有哪个：

```go
import _ "github.com/vislake/speed/go/pki/signer/kmsaws"   // 只有这一行带来 AWS SDK

pki.NewModule(db, pki.WithSigner("kms.aws", cfg))
```

**子包而非独立 module**，因为子包已经足够——Go 按包解析依赖，隔离一路穿透到 `go.sum` 与 MVS 版本选择。同一模块内实测：只 import `pkgcore` 根包的消费者，`go.mod` 与 `go.sum` 里都没有 `koanf` 的任何条目（它只被 `pkgcore/config` 子包使用），而 import 该子包的消费者 `go.sum` 里有 10 条。既然隔离效果相同，就没有理由为它多开一个模块：**模块是发布单元，应当按领域内聚性划分，不该被打包机制的需求扯变形。** lockstep 下每多一个模块就要多一份 `go.work` 条目、CI 矩阵行、`AGENTS.md`、changesets 固定版本组条目与版本标签，而子包这些全都不要。

只有当某套实现需要独立于 `pki` 的发布节奏、或消费者会绕开 `pki` 单独使用它时，才值得升格为模块——在 lockstep 版本策略下，这两种情况都不成立。

这正是 `database/sql` 的驱动模式，也是 `pkgcore` 的 `SeamRegistry` 当初照着它设计的原因。**没 import 的项目，`go.mod` 里不出现这个名字。**

这一点必须严格执行，因为仓库里已有一个可实测的反例：**`pkgcore` 根包内联了 Redis 与 S3 两套实现**（`redis_kv.go` / `redis_eventbus.go` / `s3_object_store.go` 与 `package pkgcore` 的其余文件同包），因此任何 import 该根包的消费者都无条件继承 `go-redis` 与 `minio-go` 及其传递依赖——**哪怕它只调用 `NewMemoryKVStore()`**。Go 的依赖分析是按包而非按符号做的，同包内的 import 无法按需裁剪。

实测（新建空模块 + `go mod tidy`，`GOWORK=off`）：

| 消费形态 | indirect 依赖数 |
|---|---|
| 只 import `pkgcore/apperr` 子包 | **0** |
| import `pkgcore` 根包，只用内存 KVStore | **23** |
| import `authn`（真实业务项目形态） | **82** |

从 0 到 23 全部来自根包内联的那两套实现。KMS 供应商有 6 家以上，把它们内联进 pki 的根包就是把这个代价再叠加六份——所以每家一个子包，宿主 import 谁才拿到谁。

（顺带澄清一个容易做出的错误归因：`testcontainers-go` 虽然出现在 `pkgcore` 与 `authn` 的 `go.mod` 主 require 块里，但它**不会**传染给消费者。Go 1.17+ 的模块图裁剪只加载"构建被 import 的包"所需的依赖，上游模块自身测试的依赖不在其中；`pkgcore` 的 testcontainers 只被两个 `integration_test` 文件 import，`authn` 的则来自 `authn/internal/testutil` → `dbkit/dbtest` 这条**测试专用**链路。上表第三行 82 个 indirect 里没有任何 testcontainers、docker、moby 或 containerd 条目，而 `authn/go.mod` 自己列了 108 个——差出来的 26 个正是它自己测试用的，业务项目拿不到。`pkgcore` 根包内联实现的问题不在本模块范围内，记录于此供后续处理。）

### 能力声明

除 [03 部署模式](03-deployment-modes.md) 已有的三项能力外，本模块引入一项：

| 能力 | 含义 |
|---|---|
| `KeyNeverLeavesBoundary` | 私钥从不以明文进入应用进程内存 |

`local` 不具备该能力；`vault`/`aws-kms` 在**直签模式**下具备，在信封模式下不具备。高安全部署可在装配时声明要求它，由 `Kernel.Bootstrap` 在启动时校验，而不是等事故后才发现。

### 一个必须记录的算法限制

**AWS KMS 不支持 Ed25519。** 其非对称密钥类型只有 RSA、ECC-NIST（P-256/384/521）、secp256k1 与国区 SM2。Vault Transit 支持 ed25519。

后果：Ed25519 密钥在 AWS KMS 上**只能走信封模式**，拿不到 `KeyNeverLeavesBoundary`。这个限制直接催生了下一节的算法决策。

## authn 的算法放松：由密钥决定，不由 token 决定

`go/authn` 当前把 JWT 签名算法钉死为 EdDSA，`token.go` 的注释说明了理由：防止算法混淆攻击——`alg: none`，以及把非对称公钥当作 HMAC 密钥去签（公钥不是秘密）。

**这个理由成立，但"只允许一种算法"不是它的必要条件。** 防御的本质是**不让 token 自己声称的 `alg` 决定用什么算法验签**。因此放松的正确方式是：

1. 允许列表从 `{EdDSA}` 扩为 `{EdDSA, ES256}`，**绝不含任何 HMAC 家族**——非对称与对称混列才是算法混淆的必要条件；
2. `kid` 必须存在且能查到对应密钥；
3. **新增一道现在没有的检查**：token header 的 `alg` 必须等于该密钥自己声明的算法，不等即拒。

放松后是三道闸而非现在的两道，安全性**高于**现状：现在若 allowlist 被误改，攻击面立刻打开；改后即使 allowlist 被误改，密钥算法不匹配这道闸仍然拦得住。

选 **ES256**（ECDSA P-256 + SHA-256）而非 RS256 的理由：AWS KMS 原生支持；签名 64 字节而 RSA-2048 需 256 字节，token 更小；KMS 调用更快；RSA-2048 的安全边际低于 P-256。

默认仍是 EdDSA（`local`/`vault` 部署）；部署在 AWS KMS 直签模式下时用 ES256。**国密 SM2 不在本轮范围**——它不是 JWT 标准算法（仅有草案），等真实的密评需求出现时再议。

## 数据模型

四张表，分属两个数据域（数据域定义见 [04 数据层与多租户](04-data-and-tenancy.md)）。**一张表不得混装两个数据域**——`TenantScoped` 是接口，要么实现要么不实现，这正是诊断对象把平台 CA 与租户证书塞进同一张表所犯的错。

### `pki_signing_keys` — 平台数据

密钥生命周期层的核心表。`authn` 的签名密钥住在这里，它与 X.509 无关。

| 列 | 说明 |
|---|---|
| `id` | 即 JWT 的 `kid` |
| `purpose` | 用途标识，如 `authn.access_token`。同一 purpose 同时只能有一把 `active` |
| `algorithm` | `ed25519` / `ecdsa-p256`；验签时与 token header 的 `alg` 比对 |
| `signer_name` / `key_ref` | 私钥归哪套 `Signer` 管、句柄是什么。**没有私钥列** |
| `status` | `pending` / `active` / `retiring` / `retired` / `revoked` |
| `public_key` | 公钥（DER），验签用，不敏感 |
| `not_before` / `not_after` | |
| `activated_at` / `retired_at` / `revoked_at` / `revocation_reason` | |

### `pki_authorities` — 平台数据

CA 链。`type` 为 `root` / `intermediate`，`parent_id` 指向签发者，同样只存 `signer_name` + `key_ref`，无私钥列。其余为 `subject` / `serial` / `certificate_pem` / `status` / 有效期与吊销字段。

### `pki_certificates` — 租户数据

终端实体证书，`TenantScoped`，必须通过 `tenancytest.AssertIsolated`。除与 `pki_authorities` 相同的字段外：

| 列 | 说明 |
|---|---|
| `authority_id` | 签发它的 CA |
| `purpose` / `subject` / `sans` | |
| `key_delivered` | 私钥是否已交付消费方。为真时平台侧不再持有私钥，吊销是唯一的收回手段 |

`key_delivered` 这一列记录了一个重要事实：某些场景下私钥**必须**离开平台（诊断对象就要把私钥打进 JWKS 下发给数据面集群）。这类密钥的 KMS 保护没有意义，真正的改善手段是**缩短有效期加上能轮转**，而不是加密强度。

### `pki_local_keys` — 平台数据

`local` Signer 的私钥存放处：`key_ref` / `algorithm` / 加密后的私钥（`dbkit` 字段级加密）。**只有 `local` 实现读写它**，`vault`/`aws-kms` 实现完全不碰。它与上面三张表分开，是为了让"私钥不在业务表里"这句话在表结构上成立，而不只是在文档里成立。

`not_after` 上必须有索引——到期扫描依赖它。

迁移是双方言的版本化 SQL，不使用 `AutoMigrate`，不使用 PostgreSQL 独有特性。

## 生命周期状态机与传播窗口

```
                  +-----------+
 生成 ----------> |  pending  |   已生成, 公钥已可见, 但不签任何东西
                  +-----+-----+
                        |  传播窗口届满
                        v
                  +-----------+
                  |  active   |   当前签名用. 同一 purpose 只能有一把
                  +-----+-----+
                        |  新密钥启用
                        v
                  +-----------+
                  | retiring  |   不再签新的, 仍验旧的 (重叠期)
                  +-----+-----+
                        |  重叠期届满
                        v
                  +-----------+          +----------+
                  |  retired  |          | revoked  |  紧急吊销, 立即拒绝
                  +-----------+          +----------+
```

### `pending` 状态存在的理由：分布式竞态

多副本各自缓存密钥集。若新密钥直接置为 `active`，副本 A 立刻用它签发 token，而副本 B 尚未刷新缓存——B 拿到 A 签的 token 时找不到该 `kid`，验签失败。用户看到的是随机的、只在轮转后几十秒内出现的登录失败。

因此新密钥必须先进入 `pending`：公钥立即对所有副本可见（通过事件推送与缓存刷新），**等待一个传播窗口**之后才允许启用。窗口长度是配置项，默认取缓存刷新周期的数倍。

诊断对象与 `authn` 现状都没有这个状态，因为它们根本没有运行时轮转。这是"补上从来没有的那一半"的具体内容之一。

### `retiring` 重叠期的长度由消费者声明

重叠期必须覆盖"用这把密钥签发的凭证的最长存活时间"——对 `authn` 就是 access token 的 TTL（默认 15 分钟，`authn.DefaultAccessTokenTTL`）。**pki 不知道这个数字**，持有它的是消费者，所以由消费者通过 `KeySource.EnsurePurpose` 声明：

```go
// 在 authn 内部，装配时调用一次
err := keySource.EnsurePurpose(ctx, "authn.access_token", "ed25519", cfg.ttl)
```

注意它**不是** `pki` 包上的一个函数——`authn` 不 import `pki`，调不到那样的函数。它是 `KeySource` 接口的一个方法，这也是该接口存在的原因之一（详见"`authn` 的接入"一节对该设计的论证与被否决的替代方案）。

轮转周期（多久换一次密钥）与重叠期不同，它不是消费者的知识，属于 pki 自己的配置项。

重叠期设短了会让未过期的凭证突然失效；设长了会延长一把已退役密钥的可用窗口。让持有该数字的模块自己声明，是唯一能算对的方式。

### 缓存

签名与验签是高频路径，不能每次查库。进程内缓存密钥集，由本模块自己的事件失效——与 `rbac` 的决策缓存同一模式：一个副本上的轮转通过事件总线让其他副本收敛，而不是靠 TTL 到期。缓存之后仍保留一个兜底轮询，防止事件丢失。

## 轮转：模块管状态机，宿主管下发

`jobs` 上的周期任务扫描 `not_after` 将至的密钥与证书，按 purpose 声明的策略提前续期，推进状态机，并在每个转换点发布事件。

**边界必须明确：本模块不做证书下发。** speed 是库，不知道消费者的下发目标——诊断对象的目标是 K8s Secret 加数据面集群重载，另一个消费者可能完全不同。因此：

```
jobs 扫描到期
  -> 生成新密钥/证书 (进入 pending 状态)
  -> 发事件 pki.signing_key.staged / pki.certificate.renewed
  -> [宿主订阅事件] 自行完成下发
  -> 传播窗口届满 -> active -> 旧的转 retiring
```

模块**不做**：推送到任何外部系统、重启任何进程、验证下发是否成功。宿主如需"下发成功才切换"，订阅事件后自行调用状态推进接口。

这个划分意味着诊断对象最痛的那部分（下发回路）仍需自己实现。这是对的：通用的是状态机、重叠期、扫描、密钥保护与审计，下发本就属于宿主。

到期未能续期时发布 `pki.certificate.expiring` 事件供 `notification` 订阅告警。**本模块不依赖 `notification`**——业务模块发事件、`notification` 订阅，这是既有范式。

## 吊销

吊销状态为一等公民：`revoked` 的密钥立即拒绝签名，`revoked` 的证书在链校验中拒绝。

**生成 CRL，不做 OCSP responder。** OCSP 需要一个常驻、高可用的响应服务，而本模块面向的是内部信任链——短有效期加 CRL 已经足够。CRL 分发点 URL 是配置项，签发时写入证书扩展；URL 为空时不写该扩展（而不是写一个不可达的地址）。

**CRL 的适用边界要说清楚，否则它会是个摆设。** CRL 只对真正做 X.509 链校验的消费者有意义。诊断对象的数据面集群拿到的是 `jwks.json`，走的是 JWT 验签而非链校验，它不会去拉 CRL——对这类消费者，吊销的**实际**生效手段是从下发的 JWKS 里移除该密钥并重新下发，而那属于宿主的下发回路（见"轮转"一节的边界）。所以吊销分两层：本模块内的状态与 CRL 是权威记录，让已下发的凭证真正失效则依赖宿主。把这两层混为一谈，会得到一个"我已经吊销了"但对方仍在正常工作的错觉。

对 `authn` 而言，吊销一把签名密钥等于让所有用它签发的 token 立即失效。这与 `authn` 已有的会话撤销（`KVStore` 上的撤销列表）是两套独立机制，用途不同：前者是密钥层面的紧急手段，后者是单个会话的正常下线。

## JWKS 导出

X.509 层提供把证书链导出为 JWKS 的能力（诊断对象需要它，把 `jwks.json` 下发给数据面集群），密钥生命周期层提供把 `active` + `retiring` 的公钥导出为 JWKS 的能力（供外部验证方拉取）。

导出**永远只含公钥**。诊断对象把私钥打进下发的 JWKS 是其业务需要（数据面要用它签），那属于宿主自己的组装，本模块的导出接口不提供这个能力。

**这不意味着要给 `authn` 加一个 JWKS 端点。** `authn` 现在没有 JWKS 端点，接入 pki 之后也不需要——speed 的 access token 由同一进程内的 `Verifier` 验签，公钥直接从 `KeySource` 取，不经过 HTTP。（`authn` 代码里出现的 JWKS 只在 OIDC RP 一侧，那是去消费**别人的** JWKS。）JWKS 导出的对象是**进程外的验证方**：诊断对象的数据面集群是一例，未来任何需要独立验证 speed 签发的 token 的系统是另一例。没有这类消费者的部署，这个端点可以不挂。

## `authn` 的接入

**`authn` 不 import `pki`。** 它在自己这边声明所需的接口，由 `pki` 的服务结构化满足——与 `org`/`rbac` 之间那套无 import 接缝一致。

这套手法有一个**必须遵守的前提**：接口的结构化满足要求方法签名逐字一致，包括参数与返回值的类型。两个包各自定义的具名结构体永远不是同一个类型，因此**签名里只能出现标准库类型**（这正是 `org.Scope` 的签名全部由标准库类型构成的原因）。`KeySource` 因此长成这样：

```go
// 在 go/authn 中声明，authn 的 go.mod 里没有 pki
type KeySource interface {
    // EnsurePurpose 声明本模块对签名密钥的需求。重叠期必须覆盖
    // maxCredentialLifetime；由 authn 自己传入，理由见下。
    EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error

    // ActiveSigner 返回当前签名密钥的 kid、算法，以及一个带 context 的签名函数。
    // 返回签名函数而非 crypto.Signer：后者的 Sign 不接受 context，而 KMS
    // 直签是一次网络调用，需要超时、取消与 trace 传递。
    ActiveSigner(ctx context.Context, purpose string) (
        kid string, algorithm string,
        sign func(context.Context, []byte) ([]byte, error), err error)

    // VerificationKeys 返回该 purpose 下所有仍可验签的密钥。
    // 匿名结构体是为满足"只用标准库类型"付出的代价——具名类型会让
    // 结构化满足失效，两个 map 并列则可能不一致。
    VerificationKeys(ctx context.Context, purpose string) ([]struct {
        KID       string
        Algorithm string
        Public    crypto.PublicKey
    }, error)
}
```

`pki` 是**装配层面**的必需依赖（不注入 `KeySource` 则 `NewModule` 失败），不是编译层面的依赖。`authn` 的单元测试用一个假的 `KeySource` 即可运行，不需要拉起整个 pki。

### purpose 由消费者声明，不由宿主声明

`EnsurePurpose` 放在这个接口里，而不是让宿主在装配 `pki` 时写一份 purpose 清单，是一个刻意的选择。被否决的两个替代方案：

- **宿主声明**：宿主得知道 `authn` 的 access token TTL 才能算对重叠期。一旦宿主用 `WithTokenTTL` 改了 TTL 而忘了同步改 purpose，重叠期就短于凭证寿命——表现为轮转后一批未过期的 token 突然失效，且没有任何报错。让持有该数字的模块自己声明，这类静默错误在结构上不成立。
- **给 `pkgcore.Registry` 加第 9 个注册表**：`Registry` 现有 8 个注册表（Routes / Config / Features / Permissions / Jobs / Notifications / Events / AuditActions），加一个确实符合它"新增横切机制不改 `Module` 接口"的设计意图。但那会让 `pkgcore`——所有模块的依赖底座——凭空多出"签名密钥用途"这个概念，而它只有一个消费者。`KeySource` 已经是消费者与 `pki` 之间的通道，不需要第二条。

### 改造范围

`WithSigningKeys` 删除。speed 尚未发布，不考虑兼容性，因此**不保留静态注入的第二条路径**（理由见下节）。受影响的位置：

| 位置 | 改动 |
|---|---|
| `go/authn/token.go` | `KeySet`/`TokenKey`/`GenerateTokenKey`/`NewKeySet` 删除；`Signer`/`Verifier` 改为每次从 `KeySource` 取密钥；算法允许列表扩为两种并新增 alg 一致性检查 |
| `go/authn/module.go` | `WithSigningKeys` → `WithKeySource` |
| `go/authn/service.go` | 构造 `Signer`/`Verifier` 的方式 |
| `go/authn/sms.go` | 注释中对 `WithSigningKeys` 的引用 |
| `go/saasctl` 的 4 套模板 | 开发种子密钥的构造方式；**golden 文件逐字节比对需同步更新** |
| `go/saasctl/internal/db/migrate.go` | 当前为跑迁移硬造了一把名为 `"saasctl db migrate"` 的假签名密钥——`NewModule` 强制要求密钥，连纯迁移都得编一个。接入后这处可以变干净 |
| `examples/reference-app/cmd/server/server.go` | 装配方式 |
| 各模块测试与集成测试 | `go/authn` 5 个测试文件、reference-app 1 个集成测试 |

### 一个必须承认的代价

`authn` 的签名私钥从此**落库**了。现状是纯内存——宿主从环境变量或密钥管理服务读出后注入，私钥不进数据库。

| | 收益 | 代价 |
|---|---|---|
| 现状 | 私钥不落库 | 无法轮转、多副本靠人工保持一致、重启依赖宿主重新注入 |
| 接入后 | 能轮转、多副本天然一致、有到期扫描与审计 | 库泄漏叠加主密钥泄漏即可签发任意 token |

代价由 `Signer` seam 抵消：用 `vault`/`aws-kms` 直签时私钥根本不在库里，库里只有句柄。**两者是配套的，不能只做一半**——只把私钥挪进数据库而不提供 KMS 路径，是净损失。

## 没有第二条路径

`authn` 只保留一条密钥来源。不提供"没有配置 pki 时退回静态注入"的后备路径。

理由不是简洁，是**两条路径就是两种行为**。诊断对象的主密钥管理之所以崩坏，病根正是存在一条隐式后备路径（配置里没有就自己生成一个落盘），导致生产上究竟走的哪条无人说得清。留一个静态注入的口子，最终一定会有项目在生产上用它，并在第二个副本上线时出事。

零外部依赖的纪律不因此破坏：`local` Signer 与自签 CA 全程不需要任何外部服务，`task dev` 仍是单进程零依赖启动。

### 在 `saasctl` 模块选择集中的位置

`saasctl new` 的可切换模块集是 `{authn, rbac, org}`，带下闭包校验。`pki` **不进入这个选择集**，而是跟随 `authn`：选了 `authn` 就带上 `pki`，`--with=""` 生成的纯 config 应用不需要它（没有签名密钥，也就没有生命周期可管）。因此 `pki` 不增加合法选择组合的数量，五种选择保持不变；受影响的是四套含 `authn` 的模板各自的装配代码与 golden 文件。

## 与 `dbkit` 字段加密密钥的边界

仓库里会存在两处密钥管理，**它们不合并**：

| | `dbkit.NewCipher` | `go/pki` |
|---|---|---|
| 保护对象 | 静态数据（加密列） | 签名操作 |
| 轮转含义 | 需要配合全量数据重新加密 | 只需重叠期，旧密钥自然退役 |
| 依赖位置 | 依赖图底层 | 依赖 `dbkit` |

`dbkit` 在依赖图底层，收编即循环依赖——这是技术约束。但即使没有这个约束也不该合并：两者的轮转是两件不同难度的事，把"改一个字段就要重写全表"和"等 15 分钟旧 token 过期"塞进同一套状态机，只会让两边都别扭。

`authn.KeySet` 的注释说明它的形状是刻意对齐 `dbkit.NewCipher(active, retired...)` 的，目的是让仓库里"轮转"只有一种形态。`pki` 沿用这个形态（active + 若干仍可验证的旧密钥），只是把"何时轮转"从宿主手里接管过来。

## 刻意不做的事

- **不做 ACME，不管传输层 TLS 证书。** 站点 HTTPS 与数据库连接 SSL 属于部署层，cert-manager 与云厂商做得更好，且它们的信任锚是公共 CA，与本模块的内部信任链是两个体系。
- **不做证书下发。** 见"轮转"一节。
- **不做 OCSP responder。** 见"吊销"一节。
- **不做证书透明度（CT）日志。** 内部 CA 不进公共信任库，CT 无意义。
- **不收编 `dbkit` 的字段加密密钥。** 见上节。
- **不做跨部署的 CA 联邦 / 交叉签名。** 没有需求，且会把信任模型复杂度提高一个量级。
- **不提供"导出私钥"的通用接口。** 需要把私钥交付给消费方的场景（`key_delivered`）走专门的、单独审计的路径，不是一个随手可调的方法。
- **不做国密 SM2。** 等真实密评需求出现再议，届时 `Signer` seam 与 PKCS#11 实现是自然的落点。

## 模块契约

按 [01 整体架构](01-architecture.md) 的模块接入契约，`Register(reg *Registry)` 注册：

**配置项**（`config` 模块）：CA 与证书的默认/最长有效期、提前续期天数、传播窗口长度、CRL 分发点 URL。**没有 Sensitive 项**——私钥不经过配置系统。

**权限**（`rbac`）：`pki:read` / `pki:issue` / `pki:revoke` / `pki:rotate`。

**审计动作**：`pki.authority.create` / `pki.key.rotate` / `pki.key.revoke` / `pki.certificate.issue` / `pki.certificate.revoke` / `pki.private_key.deliver`。最后一项对应 `key_delivered` 的场景，是必须留痕的高危操作。

**事件**：`pki.signing_key.staged` / `.activated` / `.retired` / `.revoked`，`pki.certificate.issued` / `.renewed` / `.revoked` / `.expiring`，`pki.authority.expiring`。事件名一律用过去式，与仓库既有惯例一致（`authn.session.revoked` 是事件，`authn.session.revoke` 是审计动作）——这也是没有 `.pending` / `.retiring` 这两个事件的原因：它们是状态名而非已发生的事，进入 `retiring` 由 `.activated` 同时表达（新密钥启用即旧密钥转入重叠期）。

**任务处理器**（`jobs`）：到期扫描、状态推进、CRL 重新生成。

**错误码**（`apperr`）：`pki.authority_not_found` / `pki.key_not_found` / `pki.no_active_key` / `pki.algorithm_unsupported_by_signer`（即 AWS KMS + Ed25519 的情形）/ `pki.certificate_revoked` / `pki.signer_unavailable` / `pki.propagation_window_not_elapsed`。

**i18n**：`zh-CN` 与 `en-US` 双语消息。

## 测试策略

- **单元测试**：`local` Signer + SQLite，不需要 Docker。状态机的每个转换、传播窗口、重叠期边界都有用例。
- **PostgreSQL 集成腿**：双方言迁移从零应用，`pki_certificates` 跑 `tenancytest.AssertIsolated`，其余三张表跑 `AssertNotTenantScoped`。
- **Vault 集成腿**：testcontainers 起真实 Vault（dev 模式），验证信封与直签两种模式。
- **AWS KMS**：**无集成腿**。LocalStack 是重依赖且其 KMS 实现与真实服务有偏差。用 SDK 接口打桩做单元测试，真实验证靠手工，并在 `AGENTS.md` 的 Testing 一节如实记录这个缺口——不假装它被覆盖了。
- **算法一致性检查**：必须有一条用例构造"header 的 alg 与密钥声明不符"的 token 并断言拒绝，这是放松算法后新增的那道闸，不能只存在于文档里。

## 交付分轮

本模块不在 [15 里程碑](15-roadmap.md) 原有排期内，是计划外模块；`authn` 已交付，接入它属于回头改造。

| 轮次 | 内容 |
|---|---|
| 轮 1 | 四张表与双方言迁移、内部 CA 签发、`Signer` seam 与 `local` 实现、租户隔离套件、密钥生命周期层接口定义 |
| 轮 2 | 生命周期状态机、`jobs` 到期扫描、传播窗口与重叠期、事件；**`authn` 切换到 `KeySource`**（含 saasctl 四套模板与 golden 文件） |
| 轮 3 | 吊销与 CRL、JWKS 导出、HTTP 面与 OpenAPI 片段 |
| 轮 4 | `go/pki/signer/vault` 与 `go/pki/signer/kmsaws` 两个子包 |

轮 1 必须把表结构砌对，否则轮 2 的状态机要推倒重来——诊断对象就是活例子：`CertificateType` 枚举存在却没有落库，导致连"哪些证书快到期了"都查不出分类。
