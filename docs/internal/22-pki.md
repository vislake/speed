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

值得注意的是，speed 自己的 `go/authn` 患的是**同一个病的轻症**：签名密钥没有轮转机制——密钥集构造后不可变，"何时换新密钥、谁生成、旧密钥何时退役"全靠宿主在启动时决定，reference-app 传的是一把写死的开发种子。本模块的密钥生命周期层接管了这半个缺口：`authn` 现在经 `KeySource` 从 pki 取密钥（见"authn 的接入"）。

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

### X.509 层已有真实消费者（2026-09-08 关闭破例），残留面精确记录在案

[15 里程碑](15-roadmap.md) 与仓库根 CLAUDE.md 都把「`examples/reference-app` 真实接入」列为模块完成的强制条件——模块 API 没有被真实消费者用起来，不算完成。**本模块的两层现在都满足这条。**

- **密钥生命周期层有消费者**：`authn` 通过 `KeySource` 消费它，而 reference-app 装配 `authn`，因此是它的间接真实消费者。这条链是完整的。
- **X.509 层有消费者（2026-09-08 的消费者轮关闭破例）**：reference-app 新增 `internal/attestation`（AI 输出真实性签章层，包文档有完整产品叙事）。参考应用是牙科 AI SaaS，本身仍然"不签发证书"给诊所——但它真实地签发自产 AI 输出并对其做链验证门控，这正是"平台代持密钥、私钥永不出模块"形态下唯一真正成立的消费形状。逐项消费面：启动时 `EnsureAuthorityChain` 经 `CreateRootCA` + `CreateIntermediateCA` 每库建一次应用 CA 链（固定 subject 名幂等寻回，重启不重复建链）；每个租户观察到的成功仿真输出经 `IssueCertificate`（purpose `simulation.attestation`，365 天）签发租户证书、`SignCertificate` 签名、落应用侧签章行；对外分享经 `VerifyCertificate` 链验证 + 叶公钥验签 + 实时摘要比对门控。`flowtests/attestation_flow_test.go` 把吊销（`pki_revokeCertificate` HTTP 操作，首次真实驱动）与 CRL 拉取（`pki_getAuthorityCrl` HTTP 操作，首次真实驱动）也真实走通。

**首次集成只发现一处真实 API 缺口**：签发出来的证书密钥无法被用于任何签名——"issue -> use -> verify"的 use 在模块层不存在。本轮以**纯增量**方式补上 `CAService.SignCertificate`（sign.go；守卫全部复用模块既有形态：`ErrCertificateRevoked` 走 `walkAuthorityChain` 同一条链遍历、有效窗外拒绝沿用"expiry 不编码"惯例、Signer 失败包装同 `GenerateCRL`），未破坏任何既有签名。

**曾有的三条补偿义务按实际收窄如下：**

1. **godoc `Example` 义务保留、叙述收窄**——编译即运行的 `Example` 仍是本层最便宜的常真消费者证明；`example_test.go` 本轮新增 `ExampleCAService_SignCertificate`（签发→签名→叶公钥验签→吊销后签名拒绝）。
2. **"未经真实消费验证"标注义务关闭**——消费者已落地，模块的 Known limitations 相应改写为"已有真实消费者、残留面精确记录"。
3. **"首个消费者可破坏性调整"豁免按实际收窄**——真实接入只做了一处增量扩展；豁免现在只覆盖下面"精确残留"列出的面，其余 X.509 API 已按真实消费者形状稳定下来（不过仍未达到密钥生命周期层的冻结标准）。

**残留面精确记录**（每条即未来仍可自由调整之处，及不消费的理由，与 `go/pki/AGENTS.md`"X.509 layer: real consumer, precise residuals"一节同源）：

- **两个 JWKS 导出**（`ExportAuthorityChainJWKS` 与 HTTP `pki_getAuthorityJwks`/`pki_getKeyJwks`）仍无调用方：本应用的外部验证者直接拿原始证书与 CRL 文档，不需要 JWKS。
- **CRLDP 扩展嵌入路径**：应用权威不带 `CRLDistributionPoint`（应用没有可声明的公开 origin），`RootCAParams`/`IntermediateCAParams.CRLDistributionPoint` 的真实证书嵌入未被走到。
- **`EnqueueCRLRegenerate` 周期性调度仍不接线**：应用的验证路径是行状态 + 链验证，CRL 流程内按需 Go API 生成 + HTTP 拉取；周期性刷新依旧没有读者在等。
- **`pki_authorities`/`pki_certificates` 的到期驱动生命周期仍未建**：现在可点名等待对象——签章证书有真实的 365 天有效期，过期即分享拒发，续期/告警机制的受益者有名有实。
- **`AuthorityStatusRevoked` 仍无写入方法**：链拒绝路径经由模块自身直接播种的测试驱动，与应用无关。

密钥生命周期层不适用上述历史豁免——它有真实消费者，按正常标准要求；本条收窄同样不触及它。


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
    // 接口收完整消息；AWS KMS 要 ED25519_SHA_512 配 MessageType:RAW
    // （PureEdDSA，JWT 的 EdDSA 即此），配成 MessageType:DIGEST 的
    // ED25519_PH_SHA_512 会签出验不过的签名。
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

**子包而非独立 module**，因为子包已经足够——Go 按包解析依赖，隔离一路穿透到 `go.sum` 与 MVS 版本选择。同一模块内的实测：只 import `pkgcore` 根包的消费者，`go.mod` 与 `go.sum` 里都没有 `koanf` 的任何条目（它只被 `pkgcore/config` 子包使用），import 该子包的消费者 `go.sum` 里才出现相应条目。既然隔离效果相同，就没有理由为它多开一个模块：**模块是发布单元，应当按领域内聚性划分，不该被打包机制的需求扯变形。** lockstep 下每多一个模块就要多一份 `go.work` 条目、CI 矩阵行、`AGENTS.md`、changesets 固定版本组条目与版本标签，而子包这些全都不要。

只有当某套实现需要独立于 `pki` 的发布节奏、或消费者会绕开 `pki` 单独使用它时，才值得升格为模块——在 lockstep 版本策略下，这两种情况都不成立。

这正是 `database/sql` 的驱动模式，也是 `pkgcore` 的 `SeamRegistry` 当初照着它设计的原因。**没 import 的项目，`go.mod` 里不出现这个名字。**

这一点由 CI 兜底：depguard 的 SDK 禁令按实现所在的子包粒度放行，谁把 SDK import 写回根包或另一个实现包都会失败（见 [18 CI/CD](18-cicd.md) 文首注记）。

（澄清一个容易做出的错误归因：`testcontainers-go` 虽然出现在 `pkgcore` 与 `authn` 的 `go.mod` 主 require 块里，但它不会传染给消费者。Go 1.17+ 的模块图裁剪只加载"构建被 import 的包"所需的依赖，上游模块自身测试的依赖不在其中——`pkgcore` 的 testcontainers 只被 `integration_test` 文件 import，`authn` 的则来自 `authn/internal/testutil` → `dbkit/dbtest` 这条测试专用链路，业务项目的 `go.mod` 拿不到它们。）

### 能力声明

除 [03 部署模式](03-deployment-modes.md) 已有的三项能力外，本模块引入一项：

| 能力 | 含义 |
|---|---|
| `KeyNeverLeavesBoundary` | 私钥从不以明文进入应用进程内存 |

`local` 不具备该能力；`vault`/`aws-kms` 在**直签模式**下具备，在信封模式下不具备。

**尚未落地：这项声明目前不被任何地方校验。** `Kernel.Bootstrap` 对能力的解析与校验（`resolveKernelSeam`/`validateSeamCapability`）只覆盖四个固定的内建 seam（`EventBus`/`KVStore`/`Mailer`/`ObjectStore`），完全不知道 `pki.SignerRegistry` 或 `pki.Signer` 的存在；`go/pki` 一侧也没有等价的校验——`pki.Module.WithSigner` 不接收 `Capability`/需求参数，`SignerRegistry.Build` 只是把注册时声明的 `Capability` 原样返回，不与任何期望值比较。也就是说，宿主即便装配了一个不具备 `KeyNeverLeavesBoundary` 的实现，即便本意是要求它，也不会得到任何错误——高安全部署的"声明即校验"仍是意图，不是实现。这项差距记录在 `go/pki/AGENTS.md` 的 Known limitations 中。

### Ed25519 在三套实现上都能直签

`local` 用标准库；Vault Transit 支持 `ed25519`；**AWS KMS 也支持**——密钥规格 `ECC_NIST_EDWARDS25519`，仅用于签名验签，两个签名算法：

| 签名算法 | `MessageType` | 对应 |
|---|---|---|
| `ED25519_SHA_512` | `RAW`（完整消息） | FIPS 186-5 §7.6，PureEdDSA——正是 RFC 8037 定义的 JWT `EdDSA` |
| `ED25519_PH_SHA_512` | `DIGEST` | FIPS 186-5 §7.8，HashEdDSA（Ed25519ph） |

**`aws-kms` 实现必须用 `ED25519_SHA_512` + `MessageType:RAW`**，因为 JWT 的 `EdDSA` 是 PureEdDSA。AWS 文档明确警告两种 `MessageType` 不可互换；选错的表现是签名能生成但验不过。

结论：Ed25519 密钥在三套实现上都能**直签**，都拿得到 `KeyNeverLeavesBoundary`，不必退回信封模式。

## authn 的签名算法：保持 EdDSA 单一，但让算法由密钥决定

> 曾经有人主张把 `authn` 的 JWT 算法允许列表从 `{EdDSA}` 放松为 `{EdDSA, ES256}`，唯一的必要性论据是"AWS KMS 不支持 Ed25519，所以 AWS 部署签不出 EdDSA"。该前提不成立（见上节）——AWS KMS 有 `ECC_NIST_EDWARDS25519` 密钥规格，三套 Signer 实现都能直签 Ed25519——放松的必要性随之消失，因此允许列表保持单一 EdDSA 不放松。

`go/authn` 把 JWT 签名算法钉死为 EdDSA，`token.go` 的注释说明了理由：防止算法混淆攻击——`alg: none`，以及把非对称公钥当作 HMAC 密钥去签（公钥不是秘密）。**这条保持不变。**

保持单一算法此刻是纯收益：三套 `Signer` 实现都能直签 Ed25519，没有任何部署形态被它挡住；而 Ed25519 相对 ECDSA P-256 还更好——签名与验签更快，且不依赖每次签名的随机数（ECDSA 的 nonce 一旦重复或可预测就直接泄漏私钥，这是它最经典的实现陷阱）。多一种算法就是多一份攻击面和多一条要维护的代码路径，在没有任何部署需要它的情况下不值得。

**但有一件事仍然要做**：`pki_signing_keys` 有 `algorithm` 列，验签时**必须检查 token header 的 `alg` 等于该 `kid` 对应密钥自己声明的算法**，不等即拒。

这道检查在单一算法下是冗余的——parser 的允许列表已经只放行 EdDSA。仍然加它，是因为它把安全性从"依赖允许列表这一处配置"变成"依赖允许列表**和**密钥声明两处一致"：将来若真的需要加第二种算法，那道闸已经在位，不必在改允许列表的同时想起来补它。冗余的防御在这里成本接近零，而遗漏的代价是算法混淆攻击。

将来若确有部署需要第二种算法（例如某个 HSM 只支持 ECDSA），加进允许列表即可，前提是**绝不混入任何 HMAC 家族**——非对称与对称同列才是算法混淆的必要条件。国密 SM2 同样不在当前范围内：它不是 JWT 标准算法（仅有草案），等真实密评需求出现时再议。

## 数据模型

五张表，分属两个数据域（数据域定义见 [04 数据层与多租户](04-data-and-tenancy.md)）。**一张表不得混装两个数据域**——`TenantScoped` 是接口，要么实现要么不实现，这正是诊断对象把平台 CA 与租户证书塞进同一张表所犯的错。（`pki_signing_keys`/`pki_authorities`/`pki_certificates`/`pki_local_keys` 四张表随密钥生命周期层落地；第五张 `pki_certificate_revocations` 随吊销机制新增，见下文。）

### `pki_signing_keys` — 平台数据

密钥生命周期层的核心表。`authn` 的签名密钥住在这里，它与 X.509 无关。

| 列 | 说明 |
|---|---|
| `id` | 即 JWT 的 `kid` |
| `purpose` | 用途标识，如 `authn.access_token`。同一 purpose 同时只能有一把 `active` |
| `algorithm` | 目前只有 `ed25519`；列本身为将来的第二种算法预留，验签时必须与 token header 的 `alg` 比对（见"authn 的签名算法"） |
| `signer_name` / `key_ref` | 私钥归哪套 `Signer` 管、句柄是什么。**没有私钥列** |
| `status` | `pending` / `active` / `retiring` / `retired` / `revoked` |
| `public_key` | 公钥（DER），验签用，不敏感 |
| `not_before` / `not_after` | |
| `activated_at` / `retired_at` / `revoked_at` / `revocation_reason` | |

### `pki_authorities` — 平台数据

CA 链。`type` 为 `root` / `intermediate`，`parent_id` 指向签发者，同样只存 `signer_name` + `key_ref`，无私钥列。其余为 `subject` / `serial` / `certificate_pem` / `status` / 有效期与吊销字段，外加为 CRL 生成而设的五列：`crl_distribution_point`（本机构 CRL 的分发点 URL，CA 创建时确定，签发的每张证书都嵌入这个值）、`crl_number`（RFC 5280 §5.2.3 的 CRL 序号，每次 `CAService.GenerateCRL` 单调递增）、`crl_pem` / `crl_issued_at` / `crl_next_update`（最近一次生成的 CRL 本体与时间戳，使读取是取缓存文档而非每次现算）。

### `pki_certificates` — 租户数据

终端实体证书，`TenantScoped`，必须通过 `tenancytest.AssertIsolated`。除与 `pki_authorities` 相同的字段外：

| 列 | 说明 |
|---|---|
| `authority_id` | 签发它的 CA |
| `purpose` / `subject` / `sans` | |
| `key_delivered` | 私钥是否已交付消费方。为真时平台侧不再持有私钥，吊销是唯一的收回手段 |

`key_delivered` 这一列记录了一个重要事实：某些场景下私钥**必须**离开平台（诊断对象就要把私钥打进 JWKS 下发给数据面集群）。这类密钥的 KMS 保护没有意义，真正的改善手段是**缩短有效期加上能轮转**，而不是加密强度。

### `pki_certificate_revocations` — 平台数据

去规范化的、只追加的吊销台账：`CAService.RevokeCertificate` 每次调用落一行，让 `CAService.GenerateCRL` 能枚举一个 CA 吊销过的全部证书，而不必对租户数据表 `pki_certificates` 发起跨租户读取。列为 `id` / `certificate_id` / `authority_id` / `serial` / `tenant_id`（真实存在但不做隔离强制的信息列，与 `go/notification` 的 `send_records`/`platform_blacklist`、`go/dbkit/audit` 的 `AuditEvent` 同一处理）/ `revoked_at` / `revocation_reason` / `created_at`，`tenancytest.AssertNotTenantScoped` 覆盖。

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

> **实现状态注记**：到期驱动的生命周期由真实落地的 `lifecycle.go`（`Service.ScanExpiry`，由 `PromoteDuePending`/`RetireDueRetiring`/`StageDueRotations` 三步组成）推进，**只操作 `pki_signing_keys` 一张表**——签名密钥这一层。证书与 CA（X.509 层）的生命周期推进只有**吊销**这一条路径（见下"吊销"一节），没有到期驱动的续期/轮转扫描，也没有 `pki.certificate.renewed`/`pki.certificate.expiring`/`pki.authority.expiring` 这几个事件——`go/pki/events.go` 真实声明的事件只有 `pki.signing_key.staged`/`.activated`/`.retired`/`.revoked` 与 `pki.certificate.revoked` 五个；`pki.certificate.renewed` 是设计阶段的占位名，从未落地。X.509 层的到期驱动路径仍未排期。

`jobs` 上的周期任务扫描签名密钥 `not_after` 将至的记录，按 purpose 声明的策略提前续期，推进密钥状态机，并在每个转换点发布事件。

**边界必须明确：本模块不做证书下发。** speed 是库，不知道消费者的下发目标——诊断对象的目标是 K8s Secret 加数据面集群重载，另一个消费者可能完全不同。因此：

```
jobs 扫描到期(仅签名密钥)
  -> 生成新密钥 (进入 pending 状态)
  -> 发事件 pki.signing_key.staged
  -> [宿主订阅事件] 自行完成下发
  -> 传播窗口届满 -> active -> 旧的转 retiring
```

模块**不做**：推送到任何外部系统、重启任何进程、验证下发是否成功。宿主如需"下发成功才切换"，订阅事件后自行调用状态推进接口。

这个划分意味着诊断对象最痛的那部分（下发回路）仍需自己实现。这是对的：通用的是状态机、重叠期、扫描、密钥保护与审计，下发本就属于宿主。

## 退役密钥材料的回收：手段在模块，策略在宿主

> **实现状态注记**：回收问题的观察是：`Signer.Destroy` 在仓库里没有任何生产调用者——到期扫描止步于 `retired`，之后密钥的私钥材料永不回收（`LocalSigner` 的密文 `pki_local_keys` 行无限累积；vault/kmsaws 直签模式下云端密钥一直存活）。落地的方案是**宿主显式调用的回收方法**，不是扫描内的自动销毁——销毁可能触碰真实的外部删除，属于宿主策略而非模块自有配置（代码级细节见 `go/pki/AGENTS.md`）。

密钥状态机停在 `retired` 是有意的：`retired` 的语义是"重叠期已过、设计上不再有任何凭证需要这把密钥验签"，而**销毁私钥材料是另一个轴**——它可能触碰真实的外部删除（直签模式的 `Destroy` 会让 Vault 删 Transit 密钥、让 AWS KMS 排入最短 7 天的删除窗口），所以"何时销毁、要不要销毁"是每部署自己的生命周期策略，与"何时轮转"这种模块自有配置不是一类问题。本节的划分与"轮转：模块管状态机，宿主管下发"完全同构：

- **模块提供手段**：`Service.ReclaimRetired(ctx)` 把每个 `retired` 且 `SignerName` 属于本 Service 的密钥交给自己的 `Signer.Destroy`，返回 `ReclaimReport`（`Destroyed` / `NotOwned` / `Failed` 三个 kid 列表，nil 即无）。它**永远不进入** `ScanExpiry`/到期扫描任务——扫描的边界"推送到任何外部系统不是本模块的事"对销毁同样成立。
- **宿主执掌策略**：部署通过"是否调用、以什么节奏调用"表达策略——本地签名器部署可以在每次扫描 drain 后调用（回收的就是累积的密文行）；直签云部署把它当作低频、刻意、有操作记录的销毁动作；envelope 部署调用了也不会回收任何东西（见下）。调用节奏即保留期声明，模块不为此新增任何配置项——与 `DefaultCacheTTL` 保持常量的理由相同（读配置会让 pki 依赖 config）。
- **consultable 的边界已存在**：设计时担心的"词汇表里没有自动销毁可咨询的 retired 之后的状态"——宿主显式调用使这个担心不成立：`retired` 本身就是咨询点，宿主调用即决策，模块不需要一个新状态替宿主做决定。

**`Destroy` 逐实现的语义**（`reclaim.go` 的 doc 与各 signer 包自己的 `Destroy` doc 是权威）：

| 实现 | `ReclaimRetired` 调 `Destroy` 后实际发生什么 |
|---|---|
| `LocalSigner` | 物理删除 `pki_local_keys` 密文行——"无限累积"的对象被真正回收 |
| vault/kmsaws 直签 | 真实云端删除（Vault 立即删；KMS `ScheduleKeyDeletion`，最短 7 天的强制删除窗口——"调用返回后密钥并不会立刻消失"，`kmsaws/signer.go` 的 `Destroy` doc 对此如实记录） |
| vault/kmsaws envelope | 校验 keyRef（即行内 key_ref 列的密文本身）仍可解密后 no-op——材料就是这行密文，丢弃它是行的历史策略，属宿主决定，不在本模块能替它做的范围内 |

**回收的保证**：只动 `retired` 行（pending/active/retiring/revoked 一概不碰——retiring 密钥仍在可验证窗口内，"重叠期届满才可销毁"正是"retiring 密钥必须保持有效直到真正被替换"保证的另一半）；只动 `SignerName` 等于本 Service 自己签名器名的行（混合实例共享一张表时，一个 Service 的 `Destroy` 绝不能落在另一个 Service 的密钥上——`ErrKeyNotFound` 会被当作"已回收"计入 `Destroyed`，没有这个守卫，一次跨属主的误删会被静默吞掉）；`ErrKeyNotFound` = 材料已不在，按"已回收"计而不是失败（重复调用安静收敛，本地实现如此作答；对已删密钥用提供商原生错误作答的直签实现会落在 `Failed`，日志带 kid 供对账）；单键失败不中断整轮（记入 `Failed` 并记日志，下一轮重试仍在 `retired` 带材料的键）。

**刻意排除 `revoked`**：吊销是应急路径，事件响应可能还需要刚被停用的密钥材料，应急动作不应静默捆绑删除任何东西。宿主若确需让某把已吊销密钥的材料消失，直接调它自己的 `Signer.Destroy`。

**尚未落地、记录在案的后续**（有意不做，不是漏做）：自动销毁需要的新机制——每实现"可自动销毁"声明、行级 destroyed 标记或新状态、保留期配置——只有出现真实消费者（尤其需要静默幂等重跑的 vault/kmsaws 直签部署）才值得设计实现；`examples/reference-app` 的宿主周期调度器（已真实驱动到期扫描）是回收调用的自然接线点，属宿主侧改动。回收不改任何行、不发任何事件：retired 密钥本就在每副本的可验证集与 active 指针之外，没有副本缓存需要被告知。

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

`WithSigningKeys` 删除。没有在用的发布版本（v0.0.1 发布后作废：21 个模块 tag 已从远端与本地删除，代理缓存不可改写；npm 半从未发布），不考虑兼容性，因此**不保留静态注入的第二条路径**（理由见下节）。受影响的位置：

| 位置 | 改动 |
|---|---|
| `go/authn/token.go` | `KeySet`/`TokenKey`/`GenerateTokenKey`/`NewKeySet` 删除；`Signer`/`Verifier` 改为每次从 `KeySource` 取密钥；算法允许列表扩为两种并新增 alg 一致性检查 |
| `go/authn/module.go` | `WithSigningKeys` → `WithKeySource` |
| `go/authn/service.go` | 构造 `Signer`/`Verifier` 的方式 |
| `go/authn/sms.go` | 注释中对 `WithSigningKeys` 的引用 |
| `go/saasctl` 的 4 套模板 | 开发种子密钥的构造方式；**golden 文件逐字节比对需同步更新** |
| `go/saasctl/internal/db/migrate.go` | 当前为跑迁移硬造了一把名为 `"saasctl db migrate"` 的假签名密钥——`NewModule` 强制要求密钥，连纯迁移都得编一个。接入后这处可以变干净 |
| `examples/reference-app/internal/app/server.go` | 装配方式 |
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

`dbkit.NewCipher(active, retired...)` 确立了仓库里"轮转"的既有形态——active 加若干仍可验证的旧密钥。`pki` 沿用这个形态，只是把"何时轮转"从宿主手里接管过来。

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

**审计动作**：真实声明并落地的只有四个——`pki.authority.create` / `pki.certificate.issue` / `pki.key.revoke` / `pki.certificate.revoke`。`pki.key.rotate` 与 `pki.private_key.deliver` 是**刻意永久不声明**，不是尚未补齐的待办：`go/pki/module.go` 自己的注释说明轮转是系统驱动的后台过程，没有审计模型 Actor/Resource 要回答的那种单一"谁做的"人类操作者（`Service.PromoteNow` 这个手动触发接口也只是操作者覆盖一个既有的系统过程，不是新增了一种需要审计的动作类型）；密钥/私钥下发则完全在当前的交付场景之外——`key_delivered` 这个交付场景本身还没有实现，谈不上要不要审计它。

**事件**：真实声明的是 `pki.signing_key.staged` / `.activated` / `.retired` / `.revoked`（密钥生命周期层）与 `pki.certificate.revoked`（X.509 层）。`pki.certificate.issued` / `.renewed` / `.expiring` 与 `pki.authority.expiring` **不存在于代码中**——证书/CA 层目前只有吊销这一条生命周期路径（见"轮转"一节的实现状态注记），到期驱动的续期/告警从未落地，这几个事件名停留在设计占位状态。事件名一律用过去式，与仓库既有惯例一致（`authn.session.revoked` 是事件，`authn.session.revoke` 是审计动作）——这也是没有 `.pending` / `.retiring` 这两个事件的原因：它们是状态名而非已发生的事，进入 `retiring` 由 `.activated` 同时表达（新密钥启用即旧密钥转入重叠期）。

**任务处理器**（`jobs`）：到期扫描、状态推进、CRL 重新生成。

**错误码**（`apperr`）：`pki.authority_not_found` / `pki.key_not_found` / `pki.no_active_key` / `pki.algorithm_unsupported_by_signer`（即 AWS KMS + Ed25519 的情形）/ `pki.certificate_revoked` / `pki.signer_unavailable` / `pki.propagation_window_not_elapsed`。

**i18n**：`zh-CN` 与 `en-US` 双语消息。

## 测试策略

- **单元测试**：`local` Signer + SQLite，不需要 Docker。状态机的每个转换、传播窗口、重叠期边界都有用例。
- **PostgreSQL 集成腿**：双方言迁移从零应用，`pki_certificates` 跑 `tenancytest.AssertIsolated`，其余三张表跑 `AssertNotTenantScoped`。
- **Vault 集成腿**：testcontainers 起真实 Vault（dev 模式），验证信封与直签两种模式。
- **AWS KMS**：**无集成腿**。LocalStack 是重依赖且其 KMS 实现与真实服务有偏差。用 SDK 接口打桩做单元测试，真实验证靠手工，并在 `AGENTS.md` 的 Testing 一节如实记录这个缺口——不假装它被覆盖了。
- **算法一致性检查**：必须有一条用例构造"header 的 alg 与密钥声明不符"的 token 并断言拒绝，这是放松算法后新增的那道闸，不能只存在于文档里。

## 交付与现状

本模块不在 [15 里程碑](15-roadmap.md) 原有排期内，是计划外模块；`authn` 已交付，接入它属于回头改造。模块的交付内容——数据模型与双方言迁移、内部 CA 签发、`Signer` seam 与 `local` 实现、生命周期状态机与 `jobs` 到期扫描、传播窗口与重叠期、`authn` 切换到 `KeySource`（含 saasctl 模板与 golden 文件）、吊销与 CRL、JWKS 导出、HTTP 面与 OpenAPI 片段、`go/pki/signer/vault` 与 `go/pki/signer/kmsaws` 两个子包——均已落地，各部分当前实现状态见 `go/pki/AGENTS.md` 的 Status 行。
