# 24. 延迟事项路线图(排期记录)

> 本文档是 2026-09-08 全库审查(共 378 项判定)中 ROADMAP 与 BLOCKED 两类共 218 项的**排期记录**:记录类别归属、依赖关系与裁决标记。它不是 deferral 原文汇编——每项事项的完整说明以所属模块的 AGENTS.md、包 README 与 docs/internal 各文档为准,本文档只承载"何时做、被什么挡、归谁家"的调度视图。行文以记录时点 main@1c279637(2026-09-09)为准。

## 1. 来源与构成

- **输入**:2026-09-08 全库审查逐项判定(库外协调记录,文件名 `deferral-triage-2026-09-08.json`,不入仓)。判定分布:ROADMAP 175、BLOCKED 43、IMPLEMENTABLE_NOW 41、TEXT 71、CLOSED_ON_MAIN 32、NEEDS_CLASSIFICATION 16。
- **本文档覆盖**:判定分布的前两类共 218 项,按类别分章;第 10 章附 TEXT 71 项的三档对账。
- **不在本文档**:
  - IMPLEMENTABLE_NOW 41 项:无外部阻塞、可独立成轮,另行排期(其中若干已随 2026-09-08 落地轮闭合,如 notes HTTP delete/restore、总线本地扇出 panic 收容、APP_OTLP_ENDPOINT 接线、三模块指标埋点、脚手架 npm 类别、GO_PINNED 机器核验)。
  - CLOSED_ON_MAIN 32 项:审查时已确认"记录闭于 main",闭时点早于本档案,不在表中(需要时回查库外清单)。
  - NEEDS_CLASSIFICATION 16 项:跨内核架构决策族(Queue 缝注册表化等),归协调侧待裁决队列;裁决后按类别归位本文档。
- **普查号**:各表首列编号即库外清单 ROADMAP/BLOCKED 顺序号,可回溯逐项原判与原文。

## 2. 类别定义与裁决标记

| 类别 | 含义 | 数量 | 标记 |
|---|---|---|---|
| 凭据类 | 阻塞点=外部账户/实时凭据/外部服务:支付网关与真实资金、微信商户证书、支付宝通知腿、短信与社交平台渠道、Renovate App 凭据、issues:write token、GeoIP 许可证(子类) | 20(GeoIP 4 + 其余 16) | ★加排 |
| 真实发布类 | 阻塞点=发布机制/发布基线/发布凭据:锁步发布序列、registry/goreleaser、oasdiff 基线、publint/changesets、post-release 触发、trivy 镜像扫描、SBOM/provenance、upgrade 版本发现、许可证扫描发布扩展 | 19 | ◆M4-forward |
| 真实消费方类 | 待办动作明确,但等真实消费方/宿主/前端包出现(含 "Not planned, unless a real consumer asks" 形态) | 18 | — |
| 设计决策类 | 已记录裁决(要做须先推翻)、反投机触发(等真实需要)、跨切面/跨内核设计未定、产品策略未决 | 39 | — |
| 路线图行类 | 路线图 M1–M4 行或具名轮已点名、计划命令已具名,以及无锚 backlog | 117 | 章内坐标 |
| 闭于 main(记录时点) | 218 项中已完全落地者(普查滞后于落地轮) | 5 | 第 9 章记 sha |

两项用户裁决标记(2026-09-08 裁决,记录于协调侧):

- **★加排**:**GeoIP 类(GeoIP 许可证审查)与其余凭据类**一律记录待排期,不设里程碑;解除依赖外部前提达成(许可证过 license scanner、真实账户/凭据到位或政策变化)。
- **◆M4-forward**:**真实发布类**原随 v1.0(M4)版本冻结与真实发布落地——发布前这些机制没有可作用的对象(零 tag 无基线、无发布事件可挂、无制品可扫)。首个真实发布 v0.0.1(2026-09-10)已提前落地该类的发布执行腿——Go 21 个模块 tag 曾推送成功;npm 十二包发布尝试失败(E403,`@speed` scope 未关联仓库 owner 的 GitHub Packages 安装,十二包零上 registry);该版本随即作废(21 个模块 tag 已从远端与本地删除、版本号不复用,见第 5 章),org 侧关联后由下一个版本号的发布完整收敛。类内其余机制仍随 v1.0(M4)。

## 3. 用户裁决与闭合记录

2026-09-08 对 ROADMAP/BLOCKED 讨论的五项逐一裁决如下;其中第 1、3、4、5 项判实现并已闭于 main(sha 于 main 历史核验),第 2 项与其余凭据类记 ★加排,真实发布类记 ◆M4-forward(标记定义见第 2 章)。裁决之外不产生过程叙述。

| 裁决项 | 裁决 | 落地(闭于 main) |
|---|---|---|
| 1. dbkit 软删模型 Update 清标缺陷 | 实现(不等真实调用方) | 普查行 109 闭。`907e864a`(fix(dbkit): keep a stale model's Update from clearing a row's soft-delete mark),错误码索引随行 `d7c9a8b3` |
| 2. GeoIP 许可证审查(MaxMind GeoLite2 条款) | ★加排 | 未实现;触发行=异常登录检测(新设备/新地区/不可能位移)整族。见第 4.1 节 |
| 3. SMS 厂商适配器 | 实现 | 普查行 143、166 闭。`e10d3d49`(feat(authn): add Aliyun, Tencent Cloud and Twilio SMS provider adapters),行文修复随行 `3e1933f6`。真网关验收残余:三适配器对真实账号的验收以 `ALIYUN_SMS_*`/`TENCENT_SMS_*`/`TWILIO_SMS_*` 环境变量门控的集成 leg 形式存在(缺凭据自跳过),见 go/pkgcore/AGENTS.md "SMS carrier adapters"(适配器已随 SMS seam 升格自 go/authn/sms 移入 pkgcore/sms,`87364ed4`;authn 侧记录见其 "The SMS seam is pkgcore's" 节) |
| 4. X.509 层真实消费方 | 实现 | 普查行 174 闭。`66c81ee9`(CAService.SignCertificate 签发)+ `e0f4e691`(reference-app 对 AI 输出做链验证公证并门控公开分享),残余记录 `65cd4362`、`d2e991ee`。到期驱动续期机制本身仍开,见第 7 章普查行 8、121 |
| 5. 基准套件 | 实现 | 普查行 86、179 的 benchmark 半边闭。`f09dc0db`(jobs)、`c88a26ab`(authn)、`0c8d858f`(notification)、`f89c4e20`(rbac),nightly GATE 文本随行 `78dbe0e7`、doc 20 记录 `cde7c11d`。千级组织树压测仍缺(普查行 74、88,归属 org 侧) |

## 4. 凭据类(★加排)

解除依赖外部前提:真实/沙箱账户与凭据、平台账号、外部服务入驻或政策变化。仓库内不持任何发布或第三方凭据是此类事项的公共背景。GeoIP 子类单独成节。

### 4.1 GeoIP 子类(★加排)

阻塞=本地 GeoIP 库(MaxMind GeoLite2)许可证审查未过 license scanner;`ip_region` 列已建、恒空为现状。

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 10 | BLOCKED | `go/authn/model.go` | IP 区域(GeoIP)解析器未接线 | 加排:MaxMind GeoLite2 许可审查未过,ip_region 列已预留恒空 |
| 12 | BLOCKED | `go/authn/AGENTS.md` | 异常登录检测(新设备/区域)未实现 | 加排:范围表原文 needs GeoIP first |
| 167 | BLOCKED | `docs/internal/05-identity-and-access.md` | ip_region 归属地列与异常登录检测未实现 | 加排:docs/05 唯一未实现格 |
| 204 | BLOCKED | `docs/internal/23-admin.md` | 平台层操作限流与异常行为检测未做 | 加排:风控归属与 GeoIP 许可两个外部前提均缺 |

### 4.2 活支付网关与真实资金流动

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 110 | BLOCKED | `go/billing/AGENTS.md` | 活网关消费者/真实资金移动未实现 | 加排:无真实接线网关资金流动;需活/沙箱支付凭证+购买流设计 |
| 126 | BLOCKED | `CLAUDE.md` | billing 经由真实接线网关的资金流动 | 加排:无真实接线网关资金流动;需活/沙箱支付凭证+购买流设计 |
| 152 | BLOCKED | `go/billing/AGENTS.md` | 演示 credit seed 非购买流 | 加排:demo 以 Grant 种子代替,真实 credit-pack 购买流需 live 通道凭据 |
| 182 | BLOCKED | `internal/app/server.go` | 真实支付网关集成(credit-pack/subscription 付费腿)未接线 | 加排:credit-pack/subscription 付费腿需 live 支付通道凭据 |
| 111 | BLOCKED | `go/billing/gateway/stripe/event.go` | stripe 续期发票 InvoiceID 首周期后陈旧 | 加排:stripe 续期发票陈旧;需 recurring 订阅消费方+真实凭证驱动 |
| 112 | BLOCKED | `go/billing/AGENTS.md` | Invoice/支付网关生命周期/Quota 路径无真实 caller | 加排:Invoice/网关生命周期无真实 caller;Quota/UsageReader 接线半属宿主 backlog |

### 4.3 微信证书轮换与支付宝通知腿

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 19 | BLOCKED | `go/billing/gateway/AGENTS.md` | wechat 平台证书发现/轮换未实现 | 加排:微信商户证书发现/轮换需真实商户环境 |
| 20 | BLOCKED | `go/billing/gateway/alipay` | 沙箱支付后半段(通知)无法驱动 | 加排:支付宝沙箱仅 Android+余额支付,通知边界需沙箱商户 |
| 70 | BLOCKED | `go/billing/AGENTS.md` | 接收活 webhook 的 HTTP 面缺失 | 加排:活 webhook HTTP 面;验证依赖真实渠道投递(alipay 腿 env-gated) |
| 72 | BLOCKED | `go/billing/gateway/AGENTS.md` | 各 provider 真实账户边界未验证 | 加排:微信无沙箱需真实商户;真 payer 会话/Stripe 签名无凭据不可验 |

### 4.4 社交与身份平台渠道(QQ/微博/支付宝/SAML/WebAuthn)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 191 | BLOCKED | `go/authn/AGENTS.md` | SAML/WebAuthn/passkeys/QQ/Weibo/Alipay 等渠道未实现 | 加排:QQ/微博/支付宝需平台账号凭据;SAML/WebAuthn 半=v1.0 后显式延后 |
| 206 | BLOCKED | `docs/internal/05-identity-and-access.md` | QQ/微博/支付宝渠道与 SAML 未实现 | 加排:QQ/微博/支付宝需平台账号凭据;SAML/WebAuthn 半=v1.0 后显式延后 |

### 4.5 其它外部凭据与外部服务

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 86 | BLOCKED | `docs/internal/20-quality-and-security.md` | nightly 停在 gated stub | 加排:nightly gate=issues:write token(仓内无凭据可满足);benchmark 半已闭 |
| 129 | BLOCKED | `.github/workflows/security.yml` | Renovate 自动依赖更新与 gitleaks pre-commit hook 未做 | 加排:Renovate 需 GitHub App 凭据;gitleaks pre-commit 半归 dev 工具轮 |
| 163 | BLOCKED | `docs/internal/20-quality-and-security.md` | Renovate 依赖自动化未落地 | 加排:Renovate 依赖自动化需外部服务/GitHub App 凭据 |
| 179 | BLOCKED | `.github/workflows/nightly.yml` | nightly 管线未实现(gated stub) | 加排:nightly gate=issues:write token(仓内无凭据可满足);benchmark 半已闭 |

## 5. 真实发布类(◆M4-forward)

此类事项原以 v1.0(M4)版本冻结与真实发布为共同前置。首个真实发布的机制已于 2026-09-10 落地(Go 模块经 `go/<module>/<version>` tag 由 Go module proxy 服务;`release.yml` 由只读验证扩展为验证+发布,权限 contents: write + packages: write——闭合 sha 见第 9 章行 127、212)。首个真实运行(v0.0.1)半成功:Go 21 个模块 tag 推送成功;npm 十二包发布失败——首个包 @speed/tokens PUT 即 403 permission_denied,`@speed` scope 未关联仓库 owner 的 GitHub Packages 安装(仓库外 org 配置,非代码缺陷),十二包零上 registry。该版本随后作废:21 个模块 tag 已从远端与本地删除、代理缓存不可改写使该版本号不可复用,下一次发布必须换新版本号——npm 半不再重发,org 侧关联 @speed scope(或等效配置)后由下一个版本号完整收敛。半成功态的恢复机制(已存在 tag 一律跳过——模块 tag 与仓库根 tag 同规;npm 步发布前先探测 registry、对已存在的版本跳过)保留为发布流水线的部分态语义,面向后续版本:任何已发布内容都不会被重复发布。类内其余机制仍随 v1.0(M4):npmjs.org registry、changesets 流程与 npm provenance、SBOM 与 GitHub Release 对象、制品腿(goreleaser 二进制、版本化镜像发布、speed.yaml 附件与文档站版本目录)、oasdiff 接线、post-release 触发、trivy、许可证扫描传递依赖扩展。未随动的发布机制行文(docs/02、docs/18、tools/release 运行文本、Taskfile)由第 10 章对应普查行跟踪。以下分组只表达各事项在发布机制中的位置,不表达先后依赖。

补记(2026-09-10):上述跟踪清单之外另有两条同族行文残留,不在普查行内,已随文改述并在此补记——scaffold-verify.yml 触发注释(该文件 :36-39)原称 "release.yml carries no publish credential"、仓内无发布,与 5.4 行 51/52/80 已重述的 "release.yml dispatch 即发布" 前提相悖;web/.changeset/README.md 的版本现状句(该文件 :15-18)原称十二包全在 0.0.0、与 Go 半同处过渡态,与 5.3 行 85/130 记的直接 bump 落地相悖。

**同一版本 tag 的语义(2026-09-10 定稿;权威叙述见 [02 仓库结构与发布](02-repo-and-release.md))**:模块 tag `go/<module>/<version>` 与根 tag `<version>` 并存、互不替代——模块 tag 是 Go module proxy 的解析面,钉在发布提交上(v0.0.1 的 21 个模块 tag 发布时指向 `fbaaaf98`,后随该版本作废从远端与本地删除),永不重打或移动;根 tag 是版本里程碑参照,由发布运行在其 checkout(dispatch 时的 main 尖端)创建,因此对早期版本的补发运行而言,根 tag 可能指向发布之后的修复提交,与模块 tag 不在同一提交上——**同一版本两个 tag 的 sha 不同是设计使然,不是漂移**。发布机制对两者一视同仁:已存在即跳过(模块 tag 与根 tag 同规),重发从任意部分状态收敛。

### 5.1 真实发布序列、发布凭据与发布衍生

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 165 | BLOCKED | `docs/internal/02-repo-and-release.md` | lockstep 真实发布未实现 | 部分落地:00e24732 v0.0.1 发布机制(Go tag 推送 + npm 发布至 GitHub Packages);docs/02 的"只读/未实现"行文已随 2026-09-10 机制与文档更新改述为现状(执行腿落地 + 模块/根 tag 语义,见本章导言) |
| 33 | BLOCKED | `docs/internal/18-cicd.md` | release 流水线第 2/4/6/7 步接线未完成 | 部分落地:00e24732 接第 3 步(Go tag 推送)与第 4 步 npm 半程(直接 bump + npm publish 至 GitHub Packages);changesets 流程、2/5/6/7 步仍随 v1.0(M4);docs/18 注记文本未随动 |
| 162 | BLOCKED | `docs/internal/18-cicd.md` | 真实 registry 发布与 goreleaser 未落地 | 部分落地:v0.0.1 的 Go 21 个模块 tag 曾发布经代理服务,该版本随后作废(tag 已从远端与本地删除、版本号不复用);npm 十二包发布尝试失败(E403,`@speed` scope 未关联仓库 owner 的 GitHub Packages 安装,十二包零上 registry),org 侧关联后由下一个版本号的发布收敛;npmjs.org registry 与 goreleaser 仍随 v1.0(M4) |
| 160 | ROADMAP | `go/saasctl/AGENTS.md` | upgrade 无版本发现 | upgrade 版本发现未随 v0.0.1 首次发布落地,仍待后续发布配套 |
| 201 | ROADMAP | `docs/internal/20-quality-and-security.md` | SBOM 与 npm provenance 未接线 | v0.0.1 发布不含 SBOM 与 provenance(npm 发布无 provenance 属性),仍随 v1.0(M4) |
| 97 | ROADMAP | `tools/license_scan.py` | 传递(非直接)依赖与 reference-app 自身依赖的许可扫描未覆盖 | v0.0.1 发布未扩展传递依赖扫描,仍随发布准备(v1.0) |

### 5.2 oasdiff 破坏性变更基线

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 156 | BLOCKED | `web/packages/api-sdk/AGENTS.md` | oasdiff breaking-change gate | v0.0.1 的 21 个模块 tag 已从远端与本地删除、该版本作废;基线取自仓库自身历史——发布提交 `fbaaaf98` 是 main 的祖先,其树带着完整的合并文档与全部 spec 片段;Go module proxy 只解析 21 个模块中的 17 个(admin、ai-gateway、integration、saasctl 返回 404),单靠代理拼不出完整基线。下一个发布版本成为新基线。oasdiff 接线仍未做,随 v1.0(M4) |
| 164 | BLOCKED | `docs/internal/21-api-contract.md` | oasdiff 破坏性变更闸门未交付 | v0.0.1 的 21 个模块 tag 已从远端与本地删除、该版本作废;基线取自仓库自身历史——发布提交 `fbaaaf98` 是 main 的祖先,其树带着完整的合并文档与全部 spec 片段;Go module proxy 只解析 21 个模块中的 17 个(admin、ai-gateway、integration、saasctl 返回 404),单靠代理拼不出完整基线。下一个发布版本成为新基线。oasdiff 接线仍未做,随 v1.0(M4) |
| 178 | BLOCKED | `.github/workflows/api-contract.yml` | oasdiff 破坏性变更检测未接线 | v0.0.1 的 21 个模块 tag 已从远端与本地删除、该版本作废;基线取自仓库自身历史——发布提交 `fbaaaf98` 是 main 的祖先,其树带着完整的合并文档与全部 spec 片段;Go module proxy 只解析 21 个模块中的 17 个(admin、ai-gateway、integration、saasctl 返回 404),单靠代理拼不出完整基线。下一个发布版本成为新基线。oasdiff 接线仍未做,随 v1.0(M4) |
| 195 | ROADMAP | `web/packages/api-sdk/AGENTS.md` | release-time SDK packaging and browser-page leg | 部分:api-sdk 发布期打包随 v0.0.1 直接 bump 发布落地(不经 changesets/发布期再生成——生成物已提交);oasdiff 基线门与浏览器腿仍随 v1.0(M4) |

### 5.3 发布机制装配(publint 与 changesets)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 85 | BLOCKED | `docs/internal/20-quality-and-security.md` | publint 与 changesets wiring 未接线 | v0.0.1 npm 发布走直接 bump + release.yml 发布腿,不经 changesets 流程;publint/changesets 接线仍随 v1.0(M4) |
| 130 | ROADMAP | `.github/workflows/reusable-npm-package-ci.yml` | publint 发布形态校验与 changesets 发布接线未做 | v0.0.1 npm 发布走直接 bump + release.yml 发布腿,不经 changesets 流程;publint/changesets 接线仍随 v1.0(M4) |

### 5.4 post-release 触发

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 51 | BLOCKED | `.github/workflows/release.yml` | post-release scaffold-verify 触发未接线 | v0.0.1 起发布事件真实存在(release.yml dispatch 即发布);触发接线仍未做,随 v1.0(M4) |
| 52 | BLOCKED | `.github/workflows/scaffold-verify.yml` | post-release 触发未接线 | v0.0.1 起发布事件真实存在(release.yml dispatch 即发布);触发接线仍未做,随 v1.0(M4) |
| 80 | BLOCKED | `docs/internal/16-verification.md` | scaffold-verify 发布后触发未接线 | v0.0.1 起发布事件真实存在(release.yml dispatch 即发布);触发接线仍未做,随 v1.0(M4) |

### 5.5 镜像扫描(trivy)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 32 | BLOCKED | `docs/internal/18-cicd.md` | trivy 镜像扫描未接线 | v0.0.1 发布不产镜像,前提未变;仍随 v1.0(M4) |
| 99 | BLOCKED | `.github/workflows/security.yml` | trivy 容器镜像扫描未接线 | v0.0.1 发布不产镜像,前提未变;仍随 v1.0(M4) |

## 6. 真实消费方类

待办动作明确(建端、接线、建公共读回 API、开注册),等真实消费方/宿主/前端包出现;按反投机纪律不做无消费方的泛化。片段并入合并文档不属此类:凡带 HTTP 片段的平台模块一律进合并文档与 SDK(模块驱动策略,docs/internal/21-api-contract.md 的"模块驱动的合并策略"),无工作区消费者的业务面照常进合并,本类只承载消费证明层面的等待。

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 2 | ROADMAP | `go/ai-gateway/AGENTS.md` | 逻辑键路由与接线发现表面未实现 | 等:运营控制台(admin-shell 类)web 消费方;现无逻辑键路由/接线发现面 |
| 9 | ROADMAP | `go/pki/signer/vault/doc.go` | vault Docker-backed 集成腿未实现 | 等:signer/vault 真实装配宿主;kmsaws 腿按设计排除(LocalStack 分歧)已记录 |
| 14 | ROADMAP | `go/storage/AGENTS.md` | 审计行发射未接线 | 等:需要审计行的宿主接线(三动作已声明,服务只记结构化日志) |
| 17 | ROADMAP | `go/metering/overage.go` | EventOverageThresholdCrossed 无订阅者 | 等:宿主产品决策(overage 事件无订阅方,全仓无订阅代码) |
| 22 | ROADMAP | `go/org/tree.go` | org 无查询列出 mark-deleted 后代 | 等:恢复运维/控制台面(admin-shell 类);Restore 逐节点不级联为设计 |
| 46 | ROADMAP | `CLAUDE.md` | pki Vault/AWS-KMS 签名器集成腿 | 等:signer/vault 真实装配宿主;kmsaws 腿按设计排除(LocalStack 分歧)已记录 |
| 59 | ROADMAP | `go/sharing/AGENTS.md` | PathShares 无 owner-facing 前端面 | 等:sharing 前端包(PathShares owner-facing 面);现由应用直接 HTTP 驱动 |
| 60 | ROADMAP | `go/integration/AGENTS.md` | 非 demo 生产级入站 API 网关未做 | 等:宿主/产品拉动(生产级入站网关,"if" 条件行) |
| 69 | ROADMAP | `go/metering/AGENTS.md` | HTTP 表面与 OpenAPI 片段未实现 | 等:拉动 HTTP 面的消费者(metering 片段;admin 进程内读已闭环) |
| 71 | ROADMAP | `go/billing/AGENTS.md` | 账本/发票窗口 keyset 分页未实现 | 等:页面拉动(keyset 全史读;截断窗口契约为刻意产品形状) |
| 132 | ROADMAP | `go/ai-gateway/api/openapi.yaml` | credential 生命周期(轮换/过期/写历史)与前端消费者未实现 | 等:运营台消费方(凭据轮换/过期/写历史面;现 GET+两 set 已交付) |
| 136 | ROADMAP | `go/sharing/password.go` | 共享口令 writer 成本参数无 host 可配旋钮 | 等:宿主旋钮轮(password writer 成本参数;阅读端按存储参数还原) |
| 142 | ROADMAP | `go/pki/AGENTS.md` | pki:rotate 无 HTTP 门控操作 | 等:控制台触发面(pki rotate/issue 门控操作) |
| 188 | BLOCKED | `go/compliance/AGENTS.md` | 公共 ParseAuditReportCSV/JSON 读回 API 未实现 | 等:真实消费方提出(报表读回 API;解析器仅测试内私有往返) |
| 189 | BLOCKED | `go/sharing/AGENTS.md` | 无浏览器端密码输入页 | 等:sharing web 消费方(浏览器密码收集页属宿主呈现层) |
| 203 | BLOCKED | `docs/internal/22-pki.md` | 签名器能力声明的装配校验未实现 | 等:真实装配 vault/kmsaws 的宿主(签名器能力声明校验;X.509 消费方已存在但签名器装配仍无) |

## 7. 设计决策类

含已记录裁决(做须先推翻)、反投机触发、跨切面/跨内核设计未定、产品策略未决。多数条目在所属模块文档中有自含 why 的现状说明,此处只记决策门与触发条件。

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 1 | ROADMAP | `go/ai-gateway/route.go` | runtime-configurable (config-driven) model routing 层未实现 | 需先推翻已记录决策(防请求中途静默变道)方可做动态路由 |
| 3 | ROADMAP | `go/compliance/config_audit.go` | config 事件缺 ActorType/DisplayName 字段,系统调用方归因不完整 | 触发=未来真正 system-actor 的 config.Set 调用方;需 go/config 事件 schema 先行 |
| 6 | ROADMAP | `go/notification/contact.go` | per-contact locale negotiation 未实现 | 无消费需求;per-contact locale 协商路径与文案对账为 later-round 变更(已刻意延后) |
| 8 | BLOCKED | `go/pki/AGENTS.md` | authority/certificate 无 expiry 驱动生命周期 | 无真实消费者前提已闭(66c81ee9/e0f4e691);到期驱动续期/轮转仍未建,宿主自管到期为现状,机制待真实到期需求 |
| 11 | ROADMAP | `go/authn/AGENTS.md` | 动态配置项运行时不读回(读回绑定未实现) | 读回绑定等有活值消费端才接(反投机;flag 可见性已由宿主 FeatureGate 生效) |
| 15 | ROADMAP | `go/tenancy/system_context.go` | dbkit.Repository[T] 无 system-context 跨租户读逃逸通道 | Repository 级逃逸通道为未定设计选项;跨租户检索已走逐租户循环+WithSystemContext |
| 18 | ROADMAP | `go/billing/AGENTS.md` | 信用过期清扫(scheduler+策略)未实现 | 产品策略(过期策略未定)+账本无 vintage 列锚点;宿主 jobs 接线 |
| 21 | ROADMAP | `go/rbac/model.go` | 权限通配符语法不存在 | 通配符语法为安全面,需专门设计决策;精确匹配为既定语义 |
| 23 | ROADMAP | `web/packages/api-client/AGENTS.md` | backend traceId emission for correlation | 跨栈 trace 来源设计;api-client README 以 M1 diagnostics 轮为参照(未入路线图),解析侧已作预留 |
| 31 | ROADMAP | `docs/internal/17-risks.md` | observability 路由捕获机制未实现 | per-template 捕获按裁决不建(risk 表跟踪);URL.Path+基数上限+RegisterMountedRoutes 为交付的有界设计 |
| 37 | BLOCKED | `docs/internal/22-pki.md` | 自动销毁机制未实现 | 模块级自动销毁已裁决有意不做;触发=真实直签部署消费者;宿主 ReclaimRetired 半已落地(0e1353ff) |
| 40 | ROADMAP | `docs/internal/07-platform-services.md` | storage 病毒扫描钩子与 WebP/水印派生未实现 | 病毒扫描需外部杀毒引擎;WebP/水印属新派生种类+产品语义 |
| 54 | ROADMAP | `examples/reference-app/web/src/useHashRoute.ts` | 正式路由库决策从未做出 | 正式路由库决策延后记录;导航现行为 hash 片段,随壳/平台面增长再定 |
| 55 | ROADMAP | `internal/app/server.go` | authn social per-provider credential 的动态 config 读渠道未实现 | 读回绑定等有活值消费端才接(反投机;flag 可见性已由宿主 FeatureGate 生效) |
| 61 | 实现 | `go/notification/sms.go` | SMS 未成为 pkgcore seam | 闭:`3f06333f`(feat(pkgcore): add the shared SMS seam)。SMS 现为 pkgcore 共享 seam(根包 `SMS`/`SMSSender` 契约 + console/http-gateway 发送器);carrier 适配器移入 `pkgcore/sms/`,authn 与 notification 消费方切换随 `87364ed4`/`7be42b46`。刻意无内核座位(无 registry/preset/capability 条目):两消费方均经各自 WithSMSSender 注入并自带接线期约束,`pkgcore.SMSSender` doc 记录该形状 |
| 63 | ROADMAP | `go/authn/mfa.go` | RequireStepUp 无 MFA 账户无密码重输回退 | RequireStepUp 无密码回退需安全设计决策(证明有效期/与 MFA 组合) |
| 82 | ROADMAP | `docs/internal/17-risks.md` | 数据分域表第五类取舍未定 | 数据分域表第五类取舍 wait-for-trigger 开放裁决 |
| 87 | ROADMAP | `docs/internal/03-deployment-modes.md` | preset 参数通道与配置文件逐项覆盖层未落地 | 闭:`c7aeafd9`(feat(pkgcore)!: carry configuration through the preset channel)。`Preset` 现为 `map[string]SeamPreset`(实现名+`Config`),`resolveKernelSeam` 把条目 `Config` 交给 `Registration.New`;`Preset.With` 提供逐项覆盖(返回副本);03 的状态段与四个实现条目的模板句同步改写,三条路径分工写入正文 |
| 96 | ROADMAP | `CLAUDE.md` | preset 层逐实现参数通道与配置文件覆盖层 | 闭:`c7aeafd9`(同 87)。机制随同批落地;`CLAUDE.md` 正文不描述 preset 形状,无需改动 |
| 104 | ROADMAP | `go/ai-gateway/AGENTS.md` | Known limitations 全节(无动态路由、凭据面无轮换/过期、SQLITE_BUSY WARN 消除需 derive-gate 事务形变、崩溃窗口等) | 聚合项各含自载裁决/触发(SQLITE_BUSY 消除需 go/storage derive-gate 事务形变) |
| 121 | BLOCKED | `docs/internal/22-pki.md` | X.509 到期驱动续期/轮转与证书事件未实现 | 无真实消费者前提已闭(66c81ee9/e0f4e691);到期驱动续期/轮转仍未建,宿主自管到期为现状,机制待真实到期需求 |
| 123 | BLOCKED | `docs/internal/05-identity-and-access.md` | Repository[T].WithinSubtree 未落地 | Repository[T].WithinSubtree 反投机:等真实查询形状消费者(现消费方自拼谓词) |
| 133 | ROADMAP | `go/compliance/doc.go` | subject-scoped export("this is your data")未实现 | subject-scoped export 产品未决定(条件性 future work) |
| 137 | ROADMAP | `go/integration/AGENTS.md` | dead_letter 三种成因无法经 Status 区分 | dead_letter 成因程序化区分反投机:等真实需要(现 LastError 自由文本已记录) |
| 140 | ROADMAP | `go/notification/delivery.go` | double-send 窗口未收窄 | double-send 窗口收窄需 transport 回执类跨 seam 设计(ProviderReceiptID 已预留) |
| 147 | ROADMAP | `go/jobs/job.go` | Cancel 不抢占运行中 Handle | StandaloneQueue 不抢占为文档化限制;补抢占需协作取消语义设计(asynq 半已改进) |
| 148 | ROADMAP | `go/storage/AGENTS.md` | key 空间回收 sweep 未做 | key 空间回收需扩展 ObjectStore seam(本地+S3 两实现)跨切面设计 |
| 149 | ROADMAP | `go/dbkit/AGENTS.md` | 盲索引轮换的重算 jobs 任务未实现 | 盲索引轮换重算任务:双列迁移/容忍错窗决策+dbkit 不能 import jobs,需所属模块承载 |
| 150 | ROADMAP | `go/metering/AGENTS.md` | 分布式聚合后端未实现 | 分布式聚合后端需基础设施设计(进程内聚合+跨副本重建已记录) |
| 159 | ROADMAP | `go/admin/AGENTS.md` | 冒充请求期间下游模块审计捕获行只带 id 无 display name | 下游捕获行实名需 user-lookup seam 或 grant-row 快照设计;admin 自身行已实名 |
| 161 | ROADMAP | `docs/internal/16-verification.md` | WithSystemContext 符号级静态检查缺口 | WithSystemContext 符号级检查需 pkgcore 子包化等 API 结构决策(depguard 仅 import 粒度) |
| 168 | ROADMAP | `docs/internal/06-billing-and-metering.md` | metering.overage_threshold.crossed 无通知订阅方 | overage 通知订阅:通知类型/模板归属为未定设计 |
| 172 | ROADMAP | `CLAUDE.md` | notification: 逐联系人语言协商 | 无消费需求;per-contact locale 协商路径与文案对账为 later-round 变更(已刻意延后) |
| 181 | ROADMAP | `examples/reference-app/web/src/views/session-lifecycle.spec.ts` | 主动注销落点产品决策未决 | 主动注销落点产品问题未决;reachedApp 记忆不重置为现状行为 |
| 183 | ROADMAP | `internal/notes/module.go` | 平台级 config keys(brand/support/AI toggles)由 notes 暂管 | brand/support/AI 平台 config keys 归属裁决未定(现由 notes 暂管,演示装配事实) |
| 187 | ROADMAP | `go/dbkit/AGENTS.md` | 盲索引批量 recompute 不存在 + WithTenantSession commit-time 失败缺口无测试 | 盲索引 recompute 同 149;WithTenantSession commit-time 失败缺口有复现配方与修法,可单列修复轮 |

## 8. 路线图行类

锚点坐标对应 docs/internal/15-roadmap.md 的 M1–M4 行、具名轮(org-web、create-saas-app、admin-shell、notification-ui、scaffold-verify 门、M4 e2e 行、M4 Storybook 行、M4 文档站完整化行等)与 docs/internal/19 计划命令;"待排期"坐标=无锚 backlog,排轮时优先于新需求提出。行内注记与其它类别行的关联。

### 8.1 锚:M1(租户与身份行:org-web、create-saas-app 轮)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 28 | ROADMAP | `go/saasctl/internal/upgrade/upgrade.go` | upgrade 不重写 web/package.json | 锚:create-saas-app 轮(现骨架不含 web 前端);upgrade 不重写 web/package.json |
| 79 | ROADMAP | `go/saasctl/AGENTS.md` | create-saas-app 与 web 侧模板未建;openapi generate 未实现 | 锚:create-saas-app v0.1(M1 行)+M4 出口/web 侧 acceptance;saasctl 模板与三组合矩阵注记 |
| 118 | ROADMAP | `docs/internal/16-verification.md` | create-saas-app 与前端脚手架未建 | 锚:create-saas-app v0.1(M1 行)+M4 出口/web 侧 acceptance;saasctl 模板与三组合矩阵注记 |
| 200 | ROADMAP | `docs/internal/17-risks.md` | saasctl upgrade npm 侧改写与自检未落地 | 锚:create-saas-app;upgrade 的 web/package.json 改写随 web 模板 |
| 205 | ROADMAP | `docs/internal/02-repo-and-release.md` | create-saas-app 与前端模板未实现 | 锚:create-saas-app v0.1(M1 行)+M4 出口/web 侧 acceptance;saasctl 模板与三组合矩阵注记 |
| 213 | ROADMAP | `.github/workflows/scaffold-verify.yml` | create-saas-app 与 web 侧模板未建 | 锚:create-saas-app v0.1(M1 行)+M4 出口/web 侧 acceptance;saasctl 模板与三组合矩阵注记 |
| 215 | ROADMAP | `internal/app/demo_subject.go` | X-Demo-User/X-Demo-User-Id demo 身份头的移除未做 | 锚:org-web 轮;X-Demo-User 演示身份头移除(kill switch 已交付) |
| 218 | ROADMAP | `.github/workflows/scaffold-verify.yml` | 注释 "a web-side scaffold-verify leg waits for create-saas-app"(:31)与 "the other four selections ... do NOT get a per-PR (or per-schedule) dual-mode BOOT proof of their own" 缺口说明(:23-31) | 锚:create-saas-app v0.1(M1 行)+M4 出口/web 侧 acceptance;saasctl 模板与三组合矩阵注记 |

### 8.2 锚:M2(媒体与变现行:storage、notification-ui、billing 前端包、ui-kit 组件族)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 7 | ROADMAP | `go/notification/AGENTS.md` | admin template editing 未实现 | 锚:notification-ui(M2 行点名);admin 模板编辑/预览面随其落地 |
| 38 | ROADMAP | `docs/internal/02-repo-and-release.md` | billing-core/billing-ui/notification-core/notification-ui/admin-shell 前端包未实现 | 锚:M2(billing/notification 行)+M3(admin-shell);billing-ui 已落地,余 billing-core/notification-core/notification-ui/admin-shell |
| 43 | ROADMAP | `CLAUDE.md` | storage 预签名直传与短时读 URL | 锚:storage 行(M2);预签名直传+短时效读 URL(storage 模块提前落地但该半未随行) |
| 62 | ROADMAP | `go/notification/AGENTS.md` | @speed/notification-ui 前端未实现 | 锚:notification-ui(M2 行点名);操作已进 api-sdk,UI 未建 |
| 66 | ROADMAP | `go/storage/AGENTS.md` | 无 presigned 直传与短读 URL 机制 | 锚:storage 行(M2);预签名直传+短时效读 URL(storage 模块提前落地但该半未随行) |
| 124 | ROADMAP | `docs/internal/07-platform-services.md` | notification 多项未实现 | 锚:notification-ui(主);捆包余子项锚见 94/139(RMX)、61(DEC)等各自条目 |
| 173 | ROADMAP | `CLAUDE.md` | notification: 管理端模板编辑 | 锚:notification-ui(M2 行点名);admin 模板编辑/预览面随其落地 |
| 207 | ROADMAP | `docs/internal/07-platform-services.md` | storage 预签名直传与短时效预签名 URL 未实现 | 锚:storage 行(M2);预签名直传+短时效读 URL(storage 模块提前落地但该半未随行) |
| 208 | ROADMAP | `docs/internal/12-frontend.md` | ui-kit 第二组组件未实现 | 锚:M2 用量展示族;ui-kit StatCard/Sparkline/StatusBadge 组未建(FileUploader 已交付) |
| 210 | ROADMAP | `CLAUDE.md` | 前端 @speed/notification-ui | 锚:notification-ui(M2 行点名);操作已进 api-sdk,UI 未建 |

### 8.3 锚:M3(AI 与集成行:admin-shell 控制台家族)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 13 | ROADMAP | `go/config/AGENTS.md` | 无 admin-facing config 编辑 UI | 锚:admin-shell(M3 行)控制台家族;config 编辑/管理 UI |
| 27 | ROADMAP | `go/admin/role.go` | RestoreRole 与 EnsureBuiltinRoles 无 HTTP 路由 | 锚:admin-shell;RestoreRole/EnsureBuiltinRoles 无 HTTP 路由(Service 层可达) |
| 44 | ROADMAP | `CLAUDE.md` | notification: 平台员工推送消费端 | 锚:admin-shell;notification 平台员工推送消费端(订阅即不做自身路由的 unscoped 消费者) |
| 65 | ROADMAP | `go/config/AGENTS.md` | ConfigItem Description 单语言未本地化 | 锚:admin-shell 渲染 schema 轮;ConfigItem Description 单语言未本地化 |
| 73 | ROADMAP | `go/rbac/assign.go` | 角色编辑(修改自定义角色)无 API;DefineRole create-only | 锚:admin-shell 角色管理(D1/D11);DefineRole create-only,无 UpdateRole |
| 107 | ROADMAP | `go/notification/hub.go` | platform-staff push consumer 未实现 | 锚:admin-shell;platform-staff push consumer(hub 与 SSE 流已交付) |
| 113 | ROADMAP | `go/rbac/model.go` | rbac.Role 与 RolePermission 无删除路径 | 锚:admin-shell 角色管理;Role/RolePermission 无删除路径 |
| 114 | ROADMAP | `web/packages/layout-kit/AGENTS.md` | admin-shell and browser automation over the served page | 锚:admin-shell(+M4 浏览器半);layout-kit 页腿记录为 M4 属过期散文 |
| 116 | ROADMAP | `web/packages/product-shell/README.md` | 平台员工面 shell(admin-shell)未构建(原指 a later round) | 锚:admin-shell(M3 行点名包);仅租户面 product-shell 已落地 |
| 122 | ROADMAP | `docs/internal/23-admin.md` | admin-shell 前端未实现 | 锚:admin-shell(M3 行点名包);仅租户面 product-shell 已落地 |
| 125 | ROADMAP | `CLAUDE.md` | 平台员工 admin-shell | 锚:admin-shell(M3 行点名包);仅租户面 product-shell 已落地 |
| 170 | ROADMAP | `docs/internal/11-cross-cutting.md` | 前端配置管理 UI 未交付 | 锚:admin-shell(M3 行)控制台家族;config 编辑/管理 UI |
| 192 | ROADMAP | `go/rbac/catalog.go` | 权限目录只存裸字符串,无 PermissionDecl 元数据 | 锚:admin-shell 控制台;权限目录无 PermissionDecl 元数据(扩 Add 为破坏性变更) |
| 193 | ROADMAP | `go/org/tree.go` | TreeService/MemberService Restore 无 HTTP 表面 | 锚:admin-shell;org/rbac Restore 为 Service-level only(运维/控制台面) |
| 209 | ROADMAP | `CLAUDE.md` | org Restore 的 HTTP 表面 | 锚:admin-shell;org/rbac Restore 为 Service-level only(运维/控制台面) |

### 8.4 锚:M4(审计合规与硬化行:compliance 治理、e2e、Storybook、文档站完整化、scaffold-verify 门、出口条件)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 24 | ROADMAP | `web/packages/ui-kit/AGENTS.md` | Storybook / browser-side visual verification | 锚:Storybook 组件文档站+可视化回归(M4 行) |
| 25 | ROADMAP | `web/packages/tenancy-ui/README.md` | 浏览器驱动真实服务器的 e2e 腿未接线(原指 M4 html-runner/e2e) | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 26 | ROADMAP | `web/packages/account-ui/README.md` | 无 Storybook/预览 harness,真实浏览器对比度验证缺失 | 锚:Storybook 组件文档站+可视化回归(M4 行) |
| 29 | ROADMAP | `docs/internal/16-verification.md` | 浏览器端到端(Playwright)未实现 | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 41 | ROADMAP | `docs/internal/10-compliance-and-audit.md` | compliance 哈希链、按分区归档、自身 HTTP 面未实现 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 47 | ROADMAP | `CLAUDE.md` | compliance 分时归档 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 48 | ROADMAP | `CLAUDE.md` | 对已伺服页面的浏览器自动化 | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 57 | ROADMAP | `go/compliance/AGENTS.md` | audit_events 可选 hash chain 未实现 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 58 | ROADMAP | `go/compliance/AGENTS.md` | tenant-scoped 模型所有者未注册 retention/erasure participant | 锚:M4 权务;tenant-scoped 模型 owner 注册 retention/erasure participant(边界只由注册闭合) |
| 67 | ROADMAP | `go/dbkit/repository.go` | Restore 无保留期窗口强制 | 锚:M4 compliance 保留策略;dbkit Restore 不强制保留窗口 |
| 68 | ROADMAP | `go/dbkit/audit/AGENTS.md` | 审计表无 hash 链、无查询/报表 API、无保留/归档 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 78 | ROADMAP | `web/packages/account-ui/README.md` | 消费者壳无真实 callback 路由,binding 完成腿从不被行使 | 锚:M4 浏览器 leg;社交绑定回调路径→binding 片段桥(壳不服务 callback 路由) |
| 90 | ROADMAP | `docs/internal/07-platform-services.md` | storage 按租户保留策略与 compliance 联动未实现 | 锚:M4 保留联动(compliance);片段入合并文档的半随模块驱动合并策略落地(1f3257ba),storage 路径已在 merged 文档 |
| 92 | ROADMAP | `docs/internal/12-frontend.md` | 浏览器自动化(M4 e2e)未落地 | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 98 | ROADMAP | `.github/workflows/docs-check.yml` | docs/site/ 按版本分目录发布未做 | 锚:M4 文档站完整化(自动生成配置清单/按版本分目录发布);生成入口需真实宿主 schema |
| 100 | ROADMAP | `.github/workflows/reusable-npm-package-ci.yml` | Storybook 组件预览 harness 不存在 | 锚:Storybook 组件文档站+可视化回归(M4 行) |
| 102 | ROADMAP | `examples/reference-app/web/src/views/account-view.tsx` | 社交绑定回调路径到 binding 片段桥缺失 | 锚:M4 浏览器 leg;社交绑定回调路径→binding 片段桥(壳不服务 callback 路由) |
| 103 | ROADMAP | `go/sharing/AGENTS.md` | Known limitations 段(126 行起):share.accessed 事件无消费者、constant-time 由 review 保证、窗口语义测试说明等保留的 deferral | 锚:M4 compliance;sharing.share.accessed 无实时订阅者(模块只发布) |
| 105 | ROADMAP | `go/compliance/AGENTS.md` | time-partitioned archival / cold storage 未实现 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 106 | ROADMAP | `go/compliance/AGENTS.md` | export 送达通知机制未实现 | 锚:M4 compliance 后续轮;export 送达通知(现只铸 share 交还 token) |
| 115 | ROADMAP | `web/packages/tenancy-ui/README.md` | 无 Storybook/预览 harness,真实浏览器对比度验证缺失 | 锚:Storybook 组件文档站+可视化回归(M4 行) |
| 117 | ROADMAP | `go/saasctl/AGENTS.md` | --with 宇宙限于 {authn,rbac,org};其余选择无 CI 双模式 boot 证明 | 锚:M4 scaffold-verify 门;saasctl 选集/三组合双模式 boot 矩阵 |
| 119 | ROADMAP | `docs/internal/16-verification.md` | 三种开关组合构建矩阵未接线 | 锚:M4 scaffold-verify 门;saasctl 选集/三组合双模式 boot 矩阵 |
| 120 | ROADMAP | `docs/internal/20-quality-and-security.md` | 可视化回归基线缺失 | 锚:Storybook 组件文档站+可视化回归(M4 行) |
| 128 | ROADMAP | `.github/workflows/e2e.yml` | e2e 管线未实现(gated stub) | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 131 | ROADMAP | `examples/reference-app/web/e2e/account-surface.spec.ts` | 浏览器级账户变更旅程缺失 | 锚:M4 e2e;浏览器级账户变更旅程(撤销会话/解绑/MFA) |
| 134 | ROADMAP | `go/compliance/AGENTS.md` | compliance HTTP surface / OpenAPI fragment 未实现 | 锚:M4/later 表行;compliance 自有 HTTP 面/OpenAPI fragment |
| 135 | ROADMAP | `go/sharing/AGENTS.md` | sharing.share.accessed 事件无实时订阅者 | 锚:M4 compliance;sharing.share.accessed 无实时订阅者(模块只发布) |
| 158 | ROADMAP | `web/packages/account-ui/README.md` | 浏览器驱动真实服务器的 e2e 腿未接线 | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 175 | ROADMAP | `CLAUDE.md` | compliance 自有 HTTP 表面 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 176 | ROADMAP | `CLAUDE.md` | e2e 与 nightly 流水线 | 锚:M4 e2e(+nightly 半见 CRE);e2e/nightly 流水线 gated stub |
| 184 | ROADMAP | `internal/cases/model.go` | case 创建后加照片(add-photo-after-create)无 API | 锚:M4 页面/需求;cases add-photo-after-create 无端点(photos schema 已预留) |
| 185 | ROADMAP | `go/compliance/doc.go` | What is not shipped: subject-scoped export、HTTP 面、哈希链、时间分区归档(边界形态) | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 186 | ROADMAP | `go/dbkit/audit/doc.go` | audit 表无 hash chain、无保留/归档、无搜索查询面 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 196 | ROADMAP | `web/packages/auth-ui/README.md` | 无 Storybook/预览 harness,真实浏览器对比度验证缺失 | 锚:Storybook 组件文档站+可视化回归(M4 行) |
| 199 | ROADMAP | `docs/internal/14-reference-app.md` | reference-app 端到端链路未串通 | 锚:M4 出口条件行;reference-app 完整业务闭环全链路 |
| 211 | ROADMAP | `CLAUDE.md` | compliance 哈希链 | 锚:M4 compliance 行;哈希链/时间分区归档/HTTP 面 |
| 214 | ROADMAP | `examples/reference-app/web/src/main.tsx` | 浏览器自动化(html-runner/e2e)未落地 | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |
| 216 | ROADMAP | `go/dbkit/hard_delete.go` | HardDelete 外部编排 — 保留窗口配置/清理调度/right-to-erasure 入口均不存在(原 M4 compliance-module 承诺改写为 does-not-exist-yet) | 锚:M4 权务行(保留/被遗忘权);宿主级清理调度与 right-to-erasure 入口;服务层编排大半已落地 |
| 217 | ROADMAP | `examples/reference-app/web/src/main.tsx` | 行 78 'What does not ship is browser automation driving that server-served page' — 代理已改写为现状局限表述并保留(若判定为需报备的 deferral 记录则在此) | 锚:M4 e2e 行;浏览器自动化/html-runner/e2e.yml gated stub(serving 半已落地;本地可跑套件已存在) |

### 8.5 锚:v1.0(M4)之后

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 39 | ROADMAP | `docs/internal/05-identity-and-access.md` | 登录时刻强制 MFA 与 WebAuthn/passkey 未实现 | 锚:v1.0(M4)之后;登录时刻强制 MFA/docs/05 显式注记(SecondFactor 接口位已留) |
| 64 | ROADMAP | `go/authn/AGENTS.md` | 无邮箱 JIT/SSO 账户无法自行添加地址 | 锚:v1.0 之后 profile/账户面;email-less JIT 为刻意设计,users.email 恒空至有加址流 |
| 190 | ROADMAP | `go/authn/AGENTS.md` | MFA 不在登录时强制 | 锚:v1.0(M4)之后;登录时刻强制 MFA/docs/05 显式注记(SecondFactor 接口位已留) |

### 8.6 待排期(无锚 backlog)

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 16 | ROADMAP | `go/metering/AGENTS.md` | 退避曲线未实现 | 待排期:metering re-claim 退避曲线(固定延迟+按失败时刻排程为已交付公平性设计;曲线为模块自声明加固) |
| 34 | ROADMAP | `docs/internal/19-dev-workflow.md` | task new:npm-package 未实现 | 待排期:dev 工具轮(new:npm-package 脚手架/lefthook pre-commit;docs/19 计划未来项) |
| 35 | ROADMAP | `docs/internal/20-quality-and-security.md` | gitleaks pre-commit 钩子未落地 | 待排期:dev 工具轮(new:npm-package 脚手架/lefthook pre-commit;docs/19 计划未来项) |
| 42 | ROADMAP | `docs/internal/12-frontend.md` | DataTable 优先级隐藏布局与 sign-in-view 外层边距遗留 | 待排期:前端小遗留(DataTable 优先级隐藏+sign-in-view 外层边距;已记录于 README/docs) |
| 45 | ROADMAP | `CLAUDE.md` | api-client 上传与 SSE 支持 | 待排期:api-client uploads/SSE 传输(SSE 消费随 notification-ui、upload 随浏览器上传面) |
| 49 | ROADMAP | `README.md` | 本地分布式模式分层 docker-compose 开发文件 | 待排期:dev-infra(本地分布式分层 docker-compose;README 记 planned) |
| 53 | ROADMAP | `Taskfile.yml` | lefthook pre-commit hooks 未做 | 待排期:dev 工具轮(new:npm-package 脚手架/lefthook pre-commit;docs/19 计划未来项) |
| 74 | ROADMAP | `go/rbac/AGENTS.md` | 千节点 prefix-match 基准未做 | 待排期:千级节点组织树前缀匹配压测(owner=org;rbac 自身基准已落地 f89c4e20) |
| 75 | ROADMAP | `web/packages/api-client/AGENTS.md` | uploads and SSE transports in api-client | 待排期:api-client uploads/SSE 传输(SSE 消费随 notification-ui、upload 随浏览器上传面) |
| 77 | ROADMAP | `web/packages/auth-core/README.md` | memory-only 会话无持久化/restore 层 | 待排期:auth-core 会话持久化/restore 层(README 记 later round) |
| 81 | ROADMAP | `docs/internal/16-verification.md` | API 文档覆盖率与 AGENTS.md 存在性检查未接线 | 待排期:docs/16 API 覆盖率/AGENTS 存在性检查未接线(需清单生成脚本) |
| 83 | ROADMAP | `docs/internal/18-cicd.md` | 路径过滤(dorny/paths-filter)未接入 | 待排期:路径过滤(dorny/paths-filter;等模块集合变大) |
| 84 | ROADMAP | `docs/internal/19-dev-workflow.md` | task dev 热重载 runner 未实现 | 待排期:task dev 热重载 runner(计划命令 19;runner 选型未定) |
| 88 | ROADMAP | `docs/internal/05-identity-and-access.md` | 千级节点前缀匹配压测未执行 | 待排期:千级节点组织树前缀匹配压测(owner=org;rbac 自身基准已落地 f89c4e20) |
| 89 | ROADMAP | `docs/internal/05-identity-and-access.md` | change-password operation 未实现 | 待排期:change-password 操作+账户字段编辑(需 spec 新 op+UI 轮;profile 面) |
| 91 | ROADMAP | `docs/internal/11-cross-cutting.md` | config 动态面值打印/编辑与 schema 驱动脱敏未实现 | 待排期:saasctl config print 动态配置面(schema 驱动 redaction;与 13/170 同族) |
| 93 | ROADMAP | `CLAUDE.md` | notification: platform_blacklist 写入方与恢复路径 | 待排期:platform_blacklist 写入器(complaint webhook+delivery hard-failure leg)与地址再证明 |
| 94 | ROADMAP | `CLAUDE.md` | notification: 租户强制偏好层级 | 待排期:租户强制偏好层级(形状已描述:tenant-scoped override 表+读时合并+写面) |
| 95 | ROADMAP | `CLAUDE.md` | api-client Reporter 更丰富的诊断落点 | 待排期:api-client Reporter 诊断落点(console sink 为 stopgap) |
| 108 | ROADMAP | `go/notification/handler.go` | 收件箱流无心跳机制 | 待排期:收件箱 SSE 流心跳(随 admin-shell(M3)之后通知轮) |
| 138 | ROADMAP | `go/notification/AGENTS.md` | platform-blacklist 写入器与地址再证明未实现 | 待排期:platform_blacklist 写入器(complaint webhook+delivery hard-failure leg)与地址再证明 |
| 139 | ROADMAP | `go/notification/AGENTS.md` | tenant-enforced preference tiers 未实现 | 待排期:租户强制偏好层级(形状已描述:tenant-scoped override 表+读时合并+写面) |
| 141 | ROADMAP | `go/pki/internal/testutil/db.go` | pki PostgreSQL 集成腿仅覆盖单回归且不在 CI | 待排期:扩容 pki PostgreSQL 集成腿覆盖(矩阵接线已闭,见 a93af455;覆盖边界记录 2f760d14;仅 migration-dedupe 单回归) |
| 144 | ROADMAP | `go/authn/AGENTS.md` | 企业 SSO 登录无审计行且 SSO 配置无 HTTP 面 | 待排期:SSO 控制面(配置写操作+Callback 漏斗;spec 20 操作无之) |
| 146 | ROADMAP | `go/observability/redact.go` | PII/完整提示的声明机制未实现 | 待排期:PII/完整提示声明机制(apperr 半边已落地;日志侧消费面未建) |
| 151 | ROADMAP | `go/billing/AGENTS.md` | audit.Emit 丢失写无补偿恢复 | 待排期:audit.Emit 补偿投递("log, never return" 契约已记录;异步审计耐久性) |
| 155 | ROADMAP | `web/packages/i18n/src/create.ts` | platform user-profile locale feature | 待排期:profile-language 槽位(host 解析;profile 步骤未落地,create.ts 扩展点已留) |
| 157 | ROADMAP | `web/packages/auth-ui/README.md` | 登录渠道发现端点不存在 | 待排期:登录渠道发现端点+家族采纳(现 channels 由 host 以 props 声明) |
| 169 | ROADMAP | `docs/internal/07-platform-services.md` | integration 到期提醒轮换与手动重投未实现 | 待排期:integration 到期提醒+手动重投(字段齐备无需新迁移;手动重投半另见可立即实现清单) |
| 177 | ROADMAP | `CLAUDE.md` | 双部署模式 x 双方言的组合 CI 运行 | 待排期:M0 出口条件残余:双部署模式×双方言组合矩阵(app 硬编码 SQLite 方言,需参数化) |
| 180 | ROADMAP | `Taskfile.yml` | dev 任务(热重载开发回路)未实现 | 待排期:task dev 热重载 runner(计划命令 19;runner 选型未定) |
| 194 | ROADMAP | `web/packages/ui-kit/src/internal/validation-error.ts` | validation from generated types + code-to-text resolver | 待排期:统一 code-to-text resolver+generated-types 校验(现各面自建白名单为权宜) |
| 197 | ROADMAP | `web/packages/account-ui/README.md` | 家族无改密面与 profile 字段(原属 profile round) | 待排期:change-password 操作+账户字段编辑(需 spec 新 op+UI 轮;profile 面) |
| 198 | ROADMAP | `go/admin/export.go` | 审计导出的一次性下载令牌无同步中继通道 | 待排期:审计导出一次性令牌的同步中继通道+送达通知(运维面设计,未命名归属轮) |
| 202 | ROADMAP | `docs/internal/21-api-contract.md` | saasctl openapi generate 未实现 | 待排期:saasctl openapi generate(不在 v0.1 范围);参考实现已在库内以 reference-app 应用自有生成流落地为种子(task api:gen:app:应用自有合并文档+SDK+全检 porcelain 门禁,21 记录)——产品化时按生成项目形状复刻并重定接缝决策 |
| — | 待排期(轮内设计讨论) | `go/pkgcore/AGENTS.md` | EventBus 发布方载荷形状未规范化 | 待排期:事件契约规范化——发布方统一载荷形状(例如统一经 pkgcore 的编码路径发布),使订阅侧的结构化探测(`EventPayloadString`/`EventPayloadFields` 一族的拼写探测)最终可退役为类型化解码。现状:同进程投递发布方结构体、代理总线投递 JSON 解码后的 map,订阅方因此按字段拼写探测;探测助手已收拢于 pkgcore(见其 AGENTS.md 的载荷解码条目),规范化是后续独立项 |

## 9. 闭于 main 的普查行(记录时点 2026-09-09)

以下行在普查(2026-09-08)之后由落地轮闭合,普查判定已过期,此处记闭态(不留在上表充当开放行):前 5 行闭于记录时点之前,后 6 行随模块驱动合并策略落地;2026-09-10 另闭 2 行(127、212,真实发布类)随 v0.0.1 发布机制落地,另闭 1 行(4,周期调度席位)随 R2 落地。

| 普查号 | 判定 | 文件 | 主题 | 标记/锚点/落地 |
|---|---|---|---|---|
| 5 | ROADMAP | `go/sharing/AGENTS.md` | HTTP fragment 未并入 build/openapi/speed.yaml 合并文档 | 闭:1f3257ba 模块驱动合并策略落地——sharing 片段并入合并文档与 @speed/api-sdk(并入不再等前端消费方) |
| 36 | ROADMAP | `docs/internal/21-api-contract.md` | org 片段进入合并文档待前端消费者 | 闭:1f3257ba org 片段已并入合并文档与 SDK(模块驱动策略;org-web 轮锚作废) |
| 56 | ROADMAP | `go/ai-gateway/api/openapi.yaml` | ai-gateway fragment 未并入 merged build/openapi/speed.yaml | 闭:1f3257ba ai-gateway 片段已并入合并文档与 @speed/api-sdk |
| 76 | ROADMAP | `web/packages/ui-kit/AGENTS.md` | storage frontend leg (generated storage operations in api-sdk) | 闭:1f3257ba storage 操作已由 orval 生成进 @speed/api-sdk(合并文档现覆盖 storage 片段) |
| 101 | ROADMAP | `Taskfile.yml` | org/storage/sharing/pki/integration/ai-gateway 前端 merge/orval 腿未做 | 闭:1f3257ba Taskfile api:merge/api:gen 与 api-contract.yml 的 merge+orval 腿现覆盖全部十三片段(含 org、storage、sharing、pki、integration、ai-gateway) |
| 109 | BLOCKED | `go/dbkit/soft_delete.go` | Update 不尊重 deleted_at 的未修复缺陷 | 闭:907e864a 修软删模型 Update 清标;d7c9a8b3 错误码索引再生成 |
| 143 | BLOCKED | `go/authn/sms.go` | SMS 厂商适配器(Aliyun/Tencent/Twilio)未实现 | 闭:e10d3d49 阿里云/腾讯云/Twilio 短信适配器;3e1933f6 行文随行;SMS seam 升格后适配器随 87364ed4 移入 pkgcore/sms;真网关验收残余:三适配器真实账号验收为 env 门控集成 leg(缺凭据自跳过) |
| 153 | ROADMAP | `go/org/AGENTS.md` | org fragment 未并入 merged openapi/speed.yaml | 闭:1f3257ba org 片段已并入 merged speed.yaml(AGENTS.md 同步改写) |
| 166 | BLOCKED | `docs/internal/03-deployment-modes.md` | 运营商短信适配器(阿里云/腾讯云/Twilio)未接入 | 闭:e10d3d49 适配器落地(docs/03 已同步现文);3e1933f6 行文随行;适配器随 87364ed4 移入 pkgcore/sms;真网关验收残余:三适配器真实账号验收为 env 门控集成 leg(缺凭据自跳过) |
| 171 | ROADMAP | `CLAUDE.md` | notes 模块删除/恢复 HTTP 端点 | 闭:2b2cd5da notes HTTP delete/restore 端点(spec 先行);8419861b 合并文档再生成 |
| 174 | BLOCKED | `CLAUDE.md` | pki X.509 层的真实消费方 | 闭:66c81ee9 CAService.SignCertificate;e0f4e691 reference-app 公证 AI 输出+分享门控;65cd4362/d2e991ee 记录残余 |
| 30 | ROADMAP | `docs/internal/16-verification.md` | 配置清单生成一致性检查未接线 | 闭:02831bf3 生成核心落地——`examples/reference-app/cmd/configrefgen` 以真实宿主组合(声明配置项的五个平台模块 authn/metering/compliance/sharing/pki+config,内存 SQLite)Attach 冻结 schema 经 `Service.Describe` 导出,产出 `docs/config-reference.md`/`.json` 与根 `.env.example`;`config.example.yaml` 过真实 loader 的加载验证随命令单测;生成器后归位为独立 Go 工具模块 `tools/configrefgen`(go.work use 条目在 go/ 之外);残余:文档站用户引导页版式与按版本分目录发布随 M4 文档站完整化 |
| 50 | ROADMAP | `.github/workflows/docs-check.yml` | config-reference 生成漂移未进 CI | 闭:02831bf3 漂移门接线——docs-check.yml 的 Config reference drift check 步(Go 装好后 `go run ./cmd/configrefgen --check`),声明侧路径(authn/metering/compliance/sharing/pki 的 module.go、go/config/**、internal/app/**、生成器自身)入 PATH SET 按 api-contract 模式自触发;生成器后归位 `tools/configrefgen`,漂移步改为工作目录 `tools/configrefgen` 下 `go run . --check`,PATH SET 改 `tools/configrefgen/**`,单测随 fast-check/full-check 的 `tools/configrefgen` 矩阵行;残余:站点版式与按版本发布随 M4 文档站完整化 |
| 127 | ROADMAP | `CLAUDE.md` | release 真实发布 | 闭:00e24732(2026-09-10)v0.0.1 真实发布执行腿落地——release.yml 验证+发布两 job;首轮真实运行半成功:Go 21 个模块 tag 曾推送发布(`go/<module>/v0.0.1`,指向 fbaaaf98;后随该版本作废从远端与本地删除),npm 十二包发布失败——首个包 @speed/tokens PUT 即 403 permission_denied,`@speed` scope 未关联仓库 owner 的 GitHub Packages 安装(仓库外 org 配置,非代码缺陷),十二包零上 registry;半成功态恢复机制已落地(2026-09-10:跳过已存在 tag、npm 逐包已存在跳过,含根 tag,语义见第 5 章导言)并保留为流水线的部分态语义、面向后续版本;该版本号已不复用,org 侧关联 @speed scope(或等效配置)后由下一个版本号的发布完整收敛;CLAUDE.md 经重构已不载"发布未接线"判定,无现文可改述 |
| 212 | ROADMAP | `.github/workflows/release.yml` | 真实发布序列未接线 | 闭:00e24732(2026-09-10)接线落地——release.yml 由只读验证扩展为验证+发布(Go tag 推送 + npm 发布至 GitHub Packages),权限 contents: write + packages: write,写权限只挂 publish job;web 十二包 0.0.1 bump 随 bd0399b8 先落;首轮真实运行半成功——Go 21 个模块 tag 推送成功,npm 发布半程失败(E403,`@speed` scope 未关联仓库 owner 的 GitHub Packages 安装,属仓库外 org 配置,十二包零上 registry);半成功态恢复机制已落地(2026-09-10:tag 步跳过已存在 tag、npm 逐包探测跳过已存在版本,模块 tag 与根 tag 同规,语义见第 5 章导言)并保留为流水线的部分态语义、面向后续版本;该版本随后作废(21 个模块 tag 已从远端与本地删除、版本号不复用),org 侧关联 @speed scope(或等效配置)后由下一个版本号的发布完整收敛 |
| 145 | ROADMAP | `go/config/module.go` | 两个 pre-auth 端点无 OpenAPI fragment | 闭:5cb0ce68 pre-auth 端点先 secured/versioned——移入 `/api/v1/config/...` 版本化前缀、两路径共享每地址限流预算、成功答案一分钟公开缓存 + `Vary: Host`(行内记录的前置条件达成);片段随 ba3ddb76 落地——`go/config/api/openapi.yaml` 声明 config_getPublicConfig/config_getSystemFeatures 两个操作,`*Module` 以编译期断言实现生成 `api.ServerInterface` 并经生成 wrapper 挂载 |
| 154 | ROADMAP | `web/packages/api-client/src/config-fetcher.ts` | OpenAPI fragment for go/config pre-auth endpoints | 闭:ba3ddb76 go/config 片段落地并进合并文档与 `@speed/api-sdk`;前端手写面与之同步——config-fetcher 保持 per-key 映射层、路径常量与片段一致,片段生成的 hook(useConfigGetPublicConfig/useConfigGetSystemFeatures)为前端的主调用面;前置 secured/versioned 由 5cb0ce68 达成 |
| 4 | ROADMAP | `go/compliance/AGENTS.md` | retention sweep 无模块内置调度点 | 闭:周期调度席位 R2 落地——`pkgcore.Registry.Schedules` 座席(声明即排产)随 `2566a03d`,`jobs.Scheduler` + `TenantLister` seam + 统一窗口键派生随 `73fae4f0`;七处声明随 `0b511ccb`/`5631f94e`/`b4acb18e`/`a40dd80e`/`46fd58c9`/`3a111862`(storage/compliance/pki 两处/billing/sharing/integration;站点前缀与调度器派生的逐字节同键由各模块窗口测试钉住),参考应用宿主 ticker 删除与 `periodicTenantUniverse` 转 `TenantLister` 实现随 `21c4a23c`。compliance AGENTS 该 bullet 已改写为现文(声明制:模块拥有自己的默认排产,宿主的总开关是启不启动调度器) |

**部分闭**:普查行 86、179(基准半闭,issues:write token 半边仍开,见第 4.5 节)、74、88(rbac 自身基准已落地,千级树压测仍缺,见 8.6)、141(pki PostgreSQL 腿已入 full-check 集成矩阵(`a93af455`),覆盖边界记录于 `2f760d14`,加宽覆盖仍开,见 8.6)。

**维护规则**:任何行落地(实现、裁决、文本改述)后,把该行移入本章并记 sha 与闭因;本章行只增不减,直至调度记录使命结束。新 deferral 由审查产出直接补入对应类别表。

## 10. TEXT 71 项对账

TEXT 判定=注释/文档措辞改述候选(流程词、未来承诺句、里程碑代号、finding 代号、悬空 deferral 指针等)。2026-09-08 注释规范清理轮已整体合入 main(记录时点 tip 即其收尾提交),多数条目随之中闭或已由各轮改写达标。三档对账:

- **A 已闭于 main(44 项)**:记录已准确(记录即现状),或文本已由清理轮/落地轮达标;不需动作,若再被普查点名按新发现处理。
- **L 台账缓办(9 项)**:族属协调侧台账 COMMENT-NORMS-REMAINING.md 的缓办组(该台账于 2026-09-08 收尾工作流建立,EXCLUDED/DEFERRED/FIXED 三档分类,DEFERRED 共 9 组,每组有主台账文件位:CLAUDE.md census、spec 散文、工具注释、测试 finding 代号簇、整文件清理等)。台账在协调侧、不入仓;处理时按族核销,核销后移出。
- **R 真剩余(18 项)**:记录时点仍在 main@1c279637 的文本(直接核读条目括注"抽查已核"),即未来文本修复轮的候选清单。

档位判定以普查逐条记录为底;直接核读 31 条(A 13、L 6、R 12,见各行"抽查已核"括注),未核条目以普查为准,处理轮落地时再核。`spec 散文`与`测试 finding 代号`两族(档 L)的修改各受 api-contract 一致性门与测试可读性约束,核销时需一并处理。

### 10.1 已闭于 main(A,47 项)

| 普查号 | 文件 | 主题 | 档位依据 |
|---|---|---|---|
| 1 | `go/dbkit/audit/AGENTS.md` | REVOKE 受限角色层未提供,触发器可被连接角色禁用/删除 | AGENTS 已含 "why a trigger, not a REVOKE-based restricted role" 完整裁决段(自含 why);残余为部署侧注记核对 |
| 2 | `docs/internal/23-admin.md` | 管理员操作审批流未做 | docs/23 边界声明=设计性范围外,记录即现状 |
| 3 | `docs/internal/03-deployment-modes.md` | seed 任务未实现 | docs/03 seed 现文已重述(演示账号经 APP_DEMO_USERS_PASSWORD 真实注册种入);抽查已核 |
| 5 | `go/compliance/AGENTS.md` | Known limitations:SweepAllTenants 无内置 TenantLister、retention sweep 模块内无调度点 | compliance AGENTS 两 bullet 已含完整裁决+宿主 cadence 契约;抽查已核 |
| 6 | `go/authn/standalone_build_test.go` | 注释中 "replace lines get deleted entirely once every module has its first tag" 为对未来锁步发布流程的承诺断言 | authn standalone_build_test "first tag" 承诺断言句已清;抽查已核 |
| 7 | `go/dbkit/repository.go` | Restore 不强制保留窗口 — 配置属 compliance 侧且尚不存在(原 deferred-scope 改写) | dbkit repository.go "(deferred scope)" 标签已清;抽查已核 |
| 16 | `go/billing/AGENTS.md` | 第58行 'a caller (business code today; a live webhook endpoint eventually)' 的 'eventually' 属 C 类未来承诺,未清理(同文件其余未来承诺均已转'not implemented/deliberately not shipped'措辞) | billing AGENTS 指名 "a later round's ... endpoint eventually" 句已清(现文为现状段落);抽查已核 |
| 19 | `.github/workflows/nightly.yml` | stub guard echo "Wire the full matrix + benchmark + flaky-detection legs once full-check is live and the first benchmarks exist" | nightly.yml guard echo 已与头注同步更新(full-check 半解锁、benchmark 落地);抽查已核 |
| 20 | `go/pki/module.go` | CA/证书有效期 config 键未接入发放决策 | pki module.go 配置键注释已为 "declare-but-do-not-read discipline" 现状纪律表述;抽查已核 |
| 22 | `go/sharing/AGENTS.md` | 35 行 if a policy-driven, host-configurable share password writer ever matters, that is this module's own future work(...) | sharing AGENTS 条件式句已按规范改写+就地指针;仅余润色空间 |
| 23 | `go/ai-gateway/image_job_store.go` | 代码注释内 Accepted residual risk:pending 卡死需 operator 手删行、崩溃窗口 | image_job_store "Accepted residual risk" 小节=现状接受式风险陈述;如需收紧属润色 |
| 25 | `go/tenancy/system_context.go` | Repository[T] 无跨租户逃生口(逃生口仍未实现的 deferral) | tenancy system_context.go 前瞻句("Until Repository[T] deliberately implements...")已由 72c619dc(norms pass)改写为现在时现状陈述(现文 ~:102-109 "Repository[T] implements no cross-tenant escape hatch");本档锚点处无该句,记录即现状(逃生口功能侧现状见第 7 章普查行 15) |
| 33 | `go/billing/AGENTS.md` | Alipay/WeChat 原生周期扣款(代扣)不实现 | Alipay/WeChat 周期扣款不实现=设计性排除结论已记录 |
| 34 | `go/rbac/module.go` | rbac 不挂 HTTP 路由;RestoreRole 无 HTTP 表面 | rbac 不挂路由=固定设计姿态;角色管理由 go/admin 控制面承接 |
| 35 | `go/rbac/assign.go` | RestoreRole 不重新校验 AssignRole 前置条件 | RestoreRole 不重验前置条件=已文档化设计选择(需重验者走 Revoke+重新 Assign) |
| 36 | `go/admin/AGENTS.md` | 冒充 grant TTL 固定常量且 impersonation 无限流/异常检测 | admin AGENTS:21 impersonation 限流/异常检测=deliberate, documented exclusion |
| 37 | `docs/internal/16-verification.md` | 其余四个 saasctl 选集无 CI 双模式 boot 证明 | docs/16 其余四选集=刻意记录选择(五倍成本+有代表性子集先例) |
| 38 | `docs/internal/17-risks.md` | 固定端口与 freePort 碰撞面加固留待真实碰撞 | docs/17 固定端口碰撞=决定已记录(硬失败+下次真撞再加固) |
| 39 | `docs/internal/22-pki.md` | key_delivered 私钥交付场景未实现 | docs/22 key_delivered 裁决完整(刻意永久不声明);抽查已核 |
| 41 | `go/sharing/password.go` | writer 成本参数为包常量的已知局限(重复记录) | sharing password.go 成本参数延期已就地记录;重复记录条目 |
| 44 | `web/packages/layout-kit/README.md` | 258 行 deferral 条目 "a product-shell-level concern once real pages exist" | layout-kit README 258 "product-shell-level concern" 条目已清;抽查已核 |
| 45 | `examples/reference-app/web/src/test-utils/render.tsx` | 行 29 '(extracting a shared harness package is recorded DEFERRED)' | render.tsx "recorded DEFERRED" 已清;抽查已核 |
| 47 | `CLAUDE.md` | 行 8×6、行 11×2 共 8 处 "the roadmap's M1/M2 … cell"-族里程碑坐标标签(config/storage/notifications/authn/org/CLI 六格 + tenancy-ui/product-shell 的 "M1 web-package list"/"M1 row names";原记"行 12"锚已校为行 11) | 8 处实测(抽查已核):storage/notifications 两处的史性子句 "landed ahead of its planned window as this round's module" 已删,两模块条目改以现状陈述;其余 6 处纯坐标锚保留并记因——锚定已落模块/包至 roadmap 行格,具导航值,非史性定位 |
| 48 | `go/ai-gateway/AGENTS.md` | 平台声明式 vendor base-URL 白名单(更强收敛)未实现 | ai-gateway base-URL 白名单=产品决定已记录,非实现缺口 |
| 49 | `go/compliance/audit_query.go` | SQL 级审计过滤未实现 | audit SQL pushdown=explicit documented choice(应用层过滤);需改 dbkit 公共表面,无消费者要求 |
| 50 | `go/authn/service.go` | 注册重复标识符的枚举oracle未闭合 | authn 注册枚举 oracle=诚实立场已记录;闭合需 check-your-inbox 产品 UX 决策+未实现投递流 |
| 51 | `go/observability/redact.go` | 红线决策追踪未实现(有意保持静默) | redact.go 红线决策追踪=by deliberate choice 已记录 |
| 52 | `go/storage/AGENTS.md` | storage fragment 无前端 SDK 表面 | storage fragment 无前端 SDK 面=recorded design,记录准确 |
| 53 | `go/pkgcore/preset.go` | Preset 层无 per-implementation 参数通道 | 闭:`c7aeafd9`:`Preset` 为 `map[string]SeamPreset`(实现名+`Config`),参数随条目到达 `Registration.New`;`Preset.With` 逐项覆盖;`With*` 注入保留为共享资源/测试 double/typed 配置路径 |
| 54 | `go/billing/AGENTS.md` | 多并发订阅模型未实现 | billing 多并发订阅=范围声明准确(A later round, if ever needed) |
| 55 | `web/packages/api-client/src/react.ts` | config-hook auto-polling / window-focus revalidation | react.ts 无轮询=记录即结论(refresh() 为显式再验证杠杆) |
| 56 | `go/admin/AGENTS.md` | audit 查询与 send-record 搜索分页为应用层切片/逐租户 limit | admin AGENTS 分页=记录准确(继承 compliance.AuditQuery 无分页/D10 逐租户 limit) |
| 57 | `docs/internal/16-verification.md` | config print 动态配置面未实现 | docs/16 config print=现文已改写(孪生关系测试钉死) |
| 58 | `docs/internal/17-risks.md` | SystemContextEnteredEvent 缺影响记录数字段 | docs/17 SystemContextEnteredEvent 行=文档声明已重述 |
| 59 | `docs/internal/19-dev-workflow.md` | 数据库初始化与 lefthook 预提交钩子未实现 | docs/19 db 初始化+lefthook=记录即现状(启动 Apply 承担迁移;CI 全量门已覆盖) |
| 60 | `docs/internal/03-deployment-modes.md` | saasctl 生成项目模板未 blank-import exporter/prometheus | docs/03 exporter 已按现状重述(生成模板 blank-import 缺口记录于 observability AGENTS);抽查已核 |
| 61 | `CLAUDE.md` | notification: pkgcore 级 SMS 缝 | 原前提已随普查行 61 闭合而变:SMS seam 已升格 pkgcore(3f06333f/87364ed4/7be42b46),CLAUDE.md 相应措辞已改写为共享 seam 现文(pkgcore/authn/notification 三条目+notification 延付清单去项) |
| 62 | `CLAUDE.md` | Trivy 每 PR 容器镜像扫描 | CLAUDE.md Trivy DEFERRED 条目已随 docker-image 轮更新为具体缺前置的现文 |
| 63 | `.github/workflows/docs-check.yml` | docs/ 整树 markdown 链接检查未做 | docs-check 整树链接检查=记于 workflow 的刻意决定(DELIBERATELY NOT WIRED) |
| 64 | `go/integration/AGENTS.md` | Deliberately not in scope 表 + Known limitations 段(8 条)在轮次史改写中保留的 deferral 记录(无手动重投、无 API-key expiry sweep、Rotate 非原子、Payload 冻结、无每租户量限等) | integration 范围表+Known limitations 保持最新(两条已标 DISCHARGED) |
| 65 | `go/compliance/AGENTS.md` | 「Not here \| Why」表:哈希链/归档/HTTP 面/subject-scoped export/报告 reader 未实现(时间分区归档附注 'simply not shipped') | compliance Not-here 表每条带明确现状标注 |
| 66 | `go/compliance/export.go` | 24h 窗口旁 '(see ExportDelivery)':export 送达通知未实现,已从'later round's job'改写为现状 | export.go 已为现状表述(24h 窗口+送达通知缺失一致);无 later round 措辞 |
| 67 | `go/pkgcore/AGENTS.md` | broker 后端(eventbus/redis\|nats\|postgres)同实例本地 fan-out 的 panic 防护不延伸(原 future-work 改写为 deliberately-not-extended) | pkgcore broker 本地 fan-out panic 防护已实际落地(31246d14)+AGENTS 记录闭合(19a4456b/33b70db5) |
| 71 | `CLAUDE.md` | census 自指措辞 4 处(行 8×2 "this census's `go/dbkit` entry records" 与 "this census's `go/dbkit` entry has the detail";行 11 "the auth-ui census entry below";行 13 "the auth-ui census defers to this shell";原记行 12/14 锚已校为行 11/13) | 4 处实测均在(抽查已核);按"可改可留"全部保留并记因——四句均为 census 条目间互指(指向 go/dbkit、auth-ui 条目),属导航措辞而非流程措辞;原记引语 "The census closes with…" 经 CLAUDE.md 全史检索不存在,已从记录删除 |
| 24 | `go/storage/api/openapi.yaml` | 22 行 "Merging this fragment into an application-wide build/openapi/speed.yaml stays future work until the merge tooling lands"(已被现状取代) | 闭:1f3257ba 随片段并入合并文档改写为现状(future-work 句由 merge 成员段落取代;另见第 9 章普查行 153 同批闭) |
| 30 | `.github/workflows/release.yml` | Report 步 echo "Real publishing is scheduled for the v1.0 release at M4, by design of this M0 round"(运行时文本保留 M0/M4 承诺;job name 的 "(M0)" 反而删了,不一致) | 闭:00e24732(2026-09-10)随 v0.0.1 发布机制改写——Report 步与头注现述验证+发布现状(M0/M4 承诺句已删);由 10.3 移档于此 |
| 70 | `web/packages/api-client/README.md` | 286 行表格 "(no spec fragment exists yet)" — "yet" 残余(低信号;config-fetcher.ts 同义句已改),未改未声明(已被现状取代) | 闭:ba3ddb76 README 表格行改述为现状——两个路径常量与 go/config 的 OpenAPI 片段同步(片段生成 `@speed/api-sdk` 的 `useConfigGetPublicConfig`/`useConfigGetSystemFeatures`);api-client 包内该句无残留;由 10.3 移档于此 |

### 10.2 台账缓办(L,9 项)

| 普查号 | 文件 | 主题 | 档位依据 |
|---|---|---|---|
| 8 | `go/billing/api/openapi.yaml` | description 文本含未来承诺型 deferral 语句 | spec 散文组:billing openapi 未来承诺句仍在(44-46/353 行,抽查已核);需改源文+api:gen 再生成 |
| 10 | `web/packages/ui-kit/src/components/FileUploader.test.tsx` | 标题字符串含 (P2-7)/(P2-8)/(P1-1)x2 代号,未改未声明 | 测试 finding 代号簇:FileUploader.test.tsx 4 处 (P2-7/P2-8/P1-1) 仍在;抽查已核 |
| 14 | `examples/reference-app/internal/notes/api/notes-server.gen.go:22` | 生成文件 doc 注释引 docs/internal/11-cross-cutting.md | spec 散文组:notes-server.gen.go:22 生成 doc 引 docs/internal/11(引用正确,是否合规属文字判断;需源文+再生成) |
| 17 | `web/packages/ui-kit/src/theme/AppThemeProvider.test.tsx` | 标题字符串含 (P1-4)x2 代号(同文件注释内的 P1-4/PRE-FIX 叙述已清理),未改未声明 | 测试 finding 代号簇:AppThemeProvider.test.tsx (P1-4) 2 处仍在;抽查已核 |
| 26 | `web/packages/product-shell/AGENTS.md` | 整文件未清理:Deferrals (recorded, do not re-open silently) 节保留 reviewer P1-1/P2-2/P2-3、a11y finding、own round/later round/M4 等违禁形态 | 整文件清理组:product-shell AGENTS 仍含 P1-1/P2-2/P2-3 编号引用 4 处;抽查已核 |
| 32 | `examples/reference-app/internal/smilesim/api/smilesim-server.gen.go:118` | 生成 doc 注释引 docs/internal/11-cross-cutting.md(smilesim openapi.yaml description 源头) | spec 散文组:smilesim gen:118 引 "backend coding standard §6.2; docs/internal/11";需源文+再生成 |
| 43 | `web/packages/ui-kit/src/components/ConfirmDialog.test.tsx` | it()/describe() 标题与注释仍含 (P1)/(P2-9) 流程代号 | 测试 finding 代号簇:ConfirmDialog.test.tsx (P1)/(P2-9) 2 处仍在;抽查已核 |
| 46 | `tools/release/lockstep-release.py` | 运行时输出字符串保留 M0/M4/v1.0 时间承诺 | v1.0 发布轮联动:lockstep-release.py M0/M4 字符串仍含(处数为普查台账原记录 16,直核 8)+测试 assertIn;文字在 v1.0 前仍准确,随首次发布更新 |
| 69 | `web/packages/ui-kit/src/components/DataTable.test.tsx` | 标题字符串含 (P2-6)/(D5)/(P2-2)x3/(P2-3) 代号,未改未声明 | 测试 finding 代号簇:DataTable.test.tsx (P2-6)x1/(D5)x1/(P2-2)x3/(P2-3)x1 共 6 处仍在(P2-6@184、D5@223、P2-2@279/335/373、P2-3@946);抽查已核 |

### 10.3 真剩余(R,15 项;未来文本修复候选)

| 普查号 | 文件 | 主题 | 档位依据 |
|---|---|---|---|
| 4 | `go/integration/AGENTS.md` | 207 行 the follow-up this leaves 措辞 | integration AGENTS:215 "See Known limitations above for the follow-up this leaves" 尾句仍在;低信号导航句 |
| 9 | `web/packages/account-ui/AGENTS.md` | 整文件未清理:'in the same round' 轮次语句与 docs/internal/21 出处引用 | account-ui AGENTS 2 处残留("in the same round" 同步规则句+docs/internal/21 引用);低信号 |
| 11 | `docs/internal/12-frontend.md` | reference-app 浏览器自动化(Playwright/e2e)腿的 deferral 记录形态过时 | docs/12 157 行 html-runner M4 表述把伺服半一并划入;伺服已落地,归属句待精化 |
| 12 | `.github/workflows/e2e.yml` | stub guard 文本与头部 PURPOSE 以计划时态描述未实现套件 | e2e.yml guard 运行时文本仍为计划时态(抽查已核);随 M4 e2e 门编写一并改写 |
| 13 | `Taskfile.yml` | release:plan desc "offline (M0 round)" 与 "Real publishing is not wired..." 文本 | Taskfile 502 行 "offline (M0 round)" 仍在;抽查已核 |
| 15 | `go/storage/repository.go` | 302 行注释保留 "Paging a state listing is future work if a tenant's backlog ever grows past one task's worth" —— C 类未来承诺句式,未改写为纯边界陈述亦未记录 | storage repository.go:302 "Paging a state listing is future work" 仍在;抽查已核 |
| 18 | `examples/reference-app/web/src/test-utils/real-client.ts` | 行 22 'extracting a shared rig package is recorded DEFERRED' — deferral 语句,文件未被代理改动/报备 | real-client.ts:22 "recorded DEFERRED" 悬空指针仍在;抽查已核 |
| 21 | `docs/internal/01-architecture.md` | admin 对 billing/metering/cfg 的用量看板依赖尚未建设 | docs/01 长注仍称 admin 对 billing/metering/cfg 依赖"尚未建设",而 usage.go 已 import 二者;抽查已核 |
| 27 | `go/admin/export_test.go` | 测试失败消息字符串保留过程措辞("on the unfixed code…" 等) | 测试失败消息字符串判为边界合法;残余=把字符串/注释边界判断写进规范文档(13 章) |
| 28 | `web/packages/i18n/src/create.test.ts` | it() 标题含 "(M1 extension point)" 里程碑代号,未改未声明 | create.test.ts(非 .tsx)96 行 "(M1 extension point)" 标题仍在;抽查已核 |
| 29 | `examples/reference-app/web/src/test-utils/matchMedia.ts` | 行 18 'recorded DEFERRED, as in real-client.ts' — 同族 deferral 语句 | matchMedia.ts:18 "recorded DEFERRED, as in real-client.ts" 仍在;抽查已核(需与 18 两处同步) |
| 31 | `.github/workflows/reusable-docker-build.yml` | header deferral 指针 "not wired (release.yml's own header)" 与 "trivy image scanning (security.yml's own DEFERRED note)" | reusable-docker-build 头注 "those three stay gated stubs" 句失真(release.yml 非 stub、e2e 套件已存在) |
| 40 | `.github/workflows/fast-check.yml` | react-hooks ESLint 插件不存在 | fast-check 头注 react-hooks "无阻塞声明"前提已过时,声明在配置头内 |
| 42 | `web/packages/auth-core/AGENTS.md` | 整文件未清理:两条 C 类 deferral 语句残留 | auth-core AGENTS:171 "Planned for a later round." 仍在;抽查已核 |
| 68 | `web/packages/tenancy-ui/AGENTS.md` | 整文件未清理:'needs a round of its own' 轮次语句与 docs/internal/12-frontend.md 出处引用 | tenancy-ui AGENTS:18 "needs a round of its own" 仍在;抽查已核 |
