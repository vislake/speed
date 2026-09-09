---
title: pki
description: "有生命周期的密钥材料:Ed25519 签名密钥与内部 X.509 CA——生成、轮换、撤销与 CRL,背后是一个永不暴露私钥的 Signer 接缝。"
weight: 3
---

# pki

pki 是 speed 的密钥材料模块:需要生命周期——生成、轮换、撤销——
的签名密钥与 X.509 证书,由平台拥有,背后是一个永不暴露私钥的
`Signer` 接缝。

## 它做什么

两层。**密钥生命周期层**按 *purpose*(用途)管理签名密钥,状态机由
模块驱动:`pending → active → retiring → retired`(外加 `revoked`),
每个 purpose 至多一个 active 键由数据库强制,转换之间遵守传播窗口
与续期提前量,事件失效的密钥集缓存让一个副本上的轮换或撤销经总线
收敛到其它副本。`Service.EnsurePurpose` 是引导路径(「此 purpose 需
要一把在有效期内有键,没有就造」);`ActiveSigner` 与
`VerificationKeys` 服务签名热路径;过期扫描(`Service.EnqueueExpiryScan`,
一个 jobs 任务)按你宿主调度的节奏驱动转换——模块绝不自己调度。
`Signer` 暴露操作(`GenerateKey`、`Sign`、`Public`、`Destroy`),绝无
把私钥读回来的途径;`LocalSigner` 是零依赖实现,
`go/pki/signer/vault` 与 `go/pki/signer/kmsaws` 对 Vault Transit 与
AWS KMS 实现同一接缝,各注册两个名字——信封模式(密钥外部加密、
本地解密后签名)与直签模式(密钥永不离开边界,经
`pkgcore.KeyNeverLeavesBoundary` 声明)——宿主换提供商只改注册名。

**X.509 层**是内部 CA:`CAService.CreateRootCA`、
`CreateIntermediateCA` 与 `IssueCertificate`(16 字节 `crypto/rand`
序列号)、`VerifyCertificate`(真实链验证,拒绝已撤销的证书或链上任
何位置的已撤销权威)、证书撤销在行与追加式撤销账本之间仲裁、带
RFC 5280 编号的 CRL 生成,以及两个只含公钥的 JWKS 导出。
`/api/v1/pki` 下的小 HTTP 面挂载属于线上的五个操作:
`pki_revokeSigningKey`、`pki_revokeCertificate`、两个 JWKS 读与
`pki_getAuthorityCrl`。参考应用的 AI 输出证明层是 X.509 层的真实消
费者——它按数据库铸一条链,签发按租户的证书,给每个模拟输出签名,
并拒绝分享证书、签名或摘要通不过验证的已证明对象。

它**不是**什么:不是传输 TLS 证书——无 ACME、无 OCSP 应答器(只
有 CRL)、无 Certificate Transparency、无跨部署 CA 联邦、无通用
「导出私钥」API。

## 何时选用

你要用必须能轮换、可撤销的密钥签名——authn 经其结构化满足的
`KeySource` 接缝消费密钥生命周期层,任何其它签名需求(内容证明、
文档签名)同形。你需要内部 CA 签发出可对验证方证明其撤销的证书。
你的威胁模型要求密钥材料永不进入你的进程(`vault`/`kmsaws` 直签模
式)。需要*传输*安全或面向公众的证书时,这不是本模块。

## 怎么接线

```go
pkiModule := pki.NewModule(db, pki.WithQueue(queue)) // 队列启用过期扫描 handler
keySource := pkiModule.Service()  // 结构化满足 authn 的 KeySource
authnModule := authn.NewModule(db, authn.WithKeySource(keySource), /* ... */)

// 要签发证书时,X.509 层:
ca := pkiModule.CA()
err := ca.CreateRootCA(ctx, pki.RootCAParams{ /* subject、有效期 */ })
```

生命周期管道:在 `Bootstrap` 之后为你产品签名的每个 purpose 调
`EnsurePurpose`,并在周期任务循环里调度 `EnqueueExpiryScan`;扫描负
责分阶段准备并提升继任者。宿主选项:`WithSigner(name, signer)`、
`WithPropagationWindow`、`WithRenewalLeadTime`、`WithCacheTTL`、
`WithExpiryScanWindow`。按注册名带能力要求(比如直签)解析签名器,
走 `SignerRegistry` 的 `BuildSignerRequiring`(先空 import provider
子包)。

## 核心概念与 API 面

- **密钥是 `keyRef`,不是材料。** 行携带 `signer_name` + `key_ref`
  ——指向拥有该键的某个 `Signer` 的不透明指针;信封模式的 key ref
  *就是*密文(Vault 或 KMS 封装)。
- **撤销即时且幂等。** `Service.RevokeSigningKey` 立刻把键排除出
  缓存背书的签名路径;`CAService.RevokeCertificate` 由账本仲裁(并
  发调用恰有一个赢家,账本与行永不互相矛盾);`VerifyCertificate`
  与 CRL 拒绝已撤销材料。
- **租户切分。** `pki_signing_keys` 与 `pki_authorities` 是平台数
  据;`pki_certificates` 是租户数据。只有 `pki_revokeCertificate`
  读请求租户;撤销签名键的权限即使在 HTTP 上也在平台域求值。
- **配置**以宿主选项与声明的配置项到达(有效期、传播窗口、续期
  提前量)。`Service.ReclaimRetired`(宿主调用)销毁已退休键的底层
  材料;`PromoteNow` 是配合撤销的手动、尊重传播窗口的提升。
- **结构化错误码**——`pki.key_not_found`、`pki.certificate_revoked`、
  `pki.authority_revoked` 等——索引在[错误码索引(English)](/docs/user-guide/error-codes/#pki)。

## 已知限制与链接

- X.509 层没有过期驱动的生命周期:没有东西自动续权威或证书——
  宿主自己盯有效期,再调 `IssueCertificate`。残余未消费面(JWKS 导
  出、CRLDP 嵌入、周期 CRL 再生成、权威撤销写入方)在 `AGENTS.md`
  里精确列出。
- 无 Vault 或 AWS-KMS 集成层:两个提供商包都只对桩客户端证明
  (LocalStack 的 KMS 与真实服务有偏差,按设计排除 AWS 集成层)。
- `EnsurePurpose` 以「撤销+同调用补造」自愈过期键;对重复调用的算
  法不匹配不做检测(今日安全,已记录)。
- `LocalSigner` 每次 `Sign` 都把密钥解密进进程内存——零依赖实现
  的记录在案的代价。

### 出处

- [go/pki/AGENTS.md](https://github.com/vislake/speed/blob/main/go/pki/AGENTS.md)——权威文档(Signer 接缝、生命周期、X.509 层、残余面、限制)
- 相关页面:[平台服务](../)、[storage](../storage/)
