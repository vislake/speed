---
title: authn
weight: 1
description: "认证——调用者是谁,绝不是他能做什么:账号、密码与短信码登录、Ed25519 访问令牌与轮换密钥、刷新轮换、会话、MFA 与 step-up、社交与企业登录。"
---

# authn

authn 是 speed 的认证模块:它决定**调用者是谁——绝不是他能做什么**。
账号、登录、会话、令牌与 MFA 都住在这里;一切「能不能」的问题属于
`rbac`;authn 每次要重新核验的成员关系事实经宿主接缝而来,`org` 的
名册是它的规范实现。

## 它做什么

authn 拥有身份域的表——`users`、`sessions`、`refresh_tokens`、
`login_attempts`、`user_identities`、验证码表、MFA 因子表与恢复码表
——外加一张租户域的表 `tenant_sso_configs`。其中没有一张是租户级
的:一个人属于多个租户,把人限定在一个租户里会让多租户情形无法表达。
关系里属于单个租户的那一半是 `memberships` 行,它在 `org` 里。

- **密码登录**——argon2id,成本参数随每次存储的哈希自带(PHC):
  提高成本只是一次配置变更,从来不是迁移。
- **访问令牌**——Ed25519 EdDSA 签名,签发与验证密钥在每次
  `Issue`/`Verify` 时经 `KeySource` 接缝解析,`*pki.Service` 结构化
  满足该接缝,两边都没有 import 边。
- **刷新轮换**——单次使用的刷新令牌,只存哈希;重放一个已消费的令
  牌会撤销整个令牌族及其会话。
- **手机号 + 短信码登录**——号码静态加密、盲索引,投递走 pkgcore
  共享的 SMS 接缝。
- **社交登录**——Google、GitHub、微信、钉钉、飞书,外加按租户的企
  业 OIDC。
- **MFA**——纯标准库 TOTP(钉在 RFC 4226/6238 向量上)、恢复码、
  step-up 再验证。

它不是授权——没有角色、没有权限检查,令牌也刻意不带任何权限,因为
冻结进令牌的权限会活过自己的吊销——也不是组织:没有树、没有成员关
系。SAML、WebAuthn/passkeys、QQ/微博/支付宝渠道与异常登录检测没有
实现,每一条都带理由记在模块的 `AGENTS.md` 里。

## 何时选用

任何有人登录的部署:需要账号、带设备列表的会话、能挺过令牌过期的刷
新故事、敏感操作的 MFA、或任何社交/企业渠道。机器访问是另一回事——
调用者只有 API 密钥的产品用 integration 的密钥签发,不用账号。

## 接线与最少使用

两个选项**必选**——两者都没有安全默认值——而成员关系必须失败关闭,
绝不默认放行:

```go
authn.RegisterPIISerializer(cipher) // 在打开 *gorm.DB 之前调用

m, err := authn.NewModule(db,
    authn.WithKeySource(pkiMod.Service()),        // *pki.Service 满足 KeySource
    authn.WithBlindIndexKey(blindIndexKey),       // 邮件/电话盲索引背后的 32 字节密钥
    authn.WithMembershipReader(membershipReader), // nil 即拒绝一切成员关系问题
)
if err != nil {
    return err // 缺必选选项是启动错误
}
```

`WithSMSSender` 决定短信传输——独立部署默认 console;分布式部署还必
须声明 `WithDeploymentMode`,否则构造以
`ErrMissingDistributedSMSSender` 失败。`WithFeatureGate` 让模块声明的
八个功能开关(密码登录、短信登录、各社交渠道、企业 SSO)在请求时生
效;`*config.Service` 结构化满足该门。TTL、`WithRevocationMode`、
`WithSocialProviders` 等其余选项微调默认值;不走 registry 的宿主直接
用 `NewService(db, bus, kv, opts...)` 构造服务。

中间件顺序是固定的:`authn.Middleware(verifier)` 在前——校验是**可
选**的(坏令牌当场 401,缺席则保持匿名),并且绝不注入租户上下文——
随后 `tenancy.Middleware(authn.NewPrincipalResolver())` 把验证过的
principal 变成租户上下文。authn 自己的子树直接从
`authn.Middleware` 的输出挂载,绝不在 `tenancy.Middleware` 下游:它的
大多数操作(注册、登录、刷新、回调)发生在任何租户可知之前。按路由
的强制是 `RequireAuthenticated` 或更严的 `RequireStepUp`,从来不是全
局包装。

## 核心概念与 API 要点

- **`Principal`**——用户、当前租户、会话、认证方式。没有角色、没有
  权限、没有邮件声明——bearer 令牌会落进日志与代理。
- **成员关系失败关闭**——只有 `MembershipReader` 确认活跃成员关系时
  才为某租户签发令牌,每次刷新都重新核验。
- **登录不回答它拒绝回答的问题**——每次失败的密码登录都返回同一个
  无参数的 `authn.invalid_credentials`,无论原因;原因记在
  `login_attempts` 行上,按盲索引键控,绝不存标识符本身。
- **刷新单次使用,重放即失窃**——同一令牌的两次并发刷新与偷来的令
  牌无法区分,客户端必须串行化自己的刷新;重新核验发生在令牌被消费
  *之前*,成员存储抖动不会赔掉正当客户端的会话。
- **吊销是执行出来的,不是仪式**——立即模式下中间件在每次验签请求
  上咨询会话管理器;跑不成的吊销检查即拒绝。
- **绝不只按邮件合并账号**——自动关联要求提供方*已验证*的邮件*且*
  渠道可信;企业 SSO 再加一条:已是配置该 IdP 的租户的活跃成员。微信
  按 `unionid` 键控,绝不用 `openid`。
- **step-up 以令牌为界,不以会话为界**,光秃秃未提权的令牌抢不走已
  激活的 MFA 因子。
- **限流是两层**——`go/ratelimit` 的滑窗计数外加渐进锁定——存储出错
  时失败关闭。

## 边界与注意

- 别把 authn 路由挂在 `tenancy.Middleware` 后面,也别给整个 handler
  套 `RequireAuthenticated`——两者都会弄坏它的公开操作。
- 任何地方都别指望存在性泄露的回答;唯一的已记录例外:注册把重复标
  识符报成冲突,让*注册*成为登录刻意回避的那种枚举 oracle。
- `SearchUsers` 是唯一跨租户的平台搜索,自身**不做任何授权**——只
  能放在调用方自己的权限门后面。
- MFA 不在登录时强制(只有 step-up 门控的敏感动作要求);无因子的账
  号没有 `RequireStepUp` 的退路;声明的动态配置项经选项注入,回读绑
  定尚未建成。
- 结构化错误码:见[错误码表(English)](/docs/user-guide/error-codes/#authn)。

## Source

- [go/authn/AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md)——权威文档(规则、接缝、已知限制)
- HTTP 片段:[go/authn/api/openapi.yaml](https://github.com/vislake/speed/blob/main/go/authn/api/openapi.yaml)
- 相关:域指南[身份与访问](/zh-cn/docs/user-guide/domains/identity-access/)、中间件的另一半[tenancy](/zh-cn/docs/user-guide/modules/core/tenancy/)、密钥源[pki](/zh-cn/docs/user-guide/modules/services/pki/),以及本组页面[rbac](/zh-cn/docs/user-guide/modules/identity/rbac/)与[org](/zh-cn/docs/user-guide/modules/identity/org/)
