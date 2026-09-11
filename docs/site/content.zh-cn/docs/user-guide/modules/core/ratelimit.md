---
title: ratelimit
weight: 7
description: "共享的 KVStore 承载限流原语——每次 Allow 一个滑窗计数维度,无租户与 HTTP 语义,由调用方组合。"
---

# ratelimit

speed 的共享限流原语:一个 `Limiter`,判定对调用方给出的 `key` 再来
一次命中是否落在调用方给出的 `Limit` 之内,后端完全是
`pkgcore.KVStore`。它是**纯库**——与平台其他模块不同,它不实现
`the module contract`:无可注册之物、无内核可接,消费方直接调
`ratelimit.New(store)`。它存在是因为六个互不相干的业务模块各自都
需要限流——`authn` 的登录暴力破解守卫、`integration` 的三层 API
密钥节流、`notification` 的验证码预算、`org` 的邀请投递限额、
`ai-gateway` 的每租户请求限额、`sharing` 的公开链接滥用守卫——
而对着每种部署形态都提供的同一个接口(`pkgcore.KVStore`)做一个
共享原语,好过把同一个滑窗计数器以六种略不相同的方式各写一遍。

## 何时选用

每当你要按业务逻辑定义的某个 key 约束「多久一次」就用它。限流器
刻意对你的领域一无所知:没有租户、没有 HTTP、没有账号、没有 IP——
那些都只是你选的 `key` 字符串。要按租户限?把租户编进 key
(`"ai-gateway:tenant:" + tenantID`)。要 IP 加账号?每个维度调一次
`Allow`,任一拒绝就整体拒绝——多维组合是调用方的事,设计如此。

## 接线与最少使用

```go
limiter := ratelimit.New(store) // store: pkgcore.KVStore —— 内存、Redis……

byAccount, err := limiter.Allow(ctx,
    "authn:login:account:"+accountBlindIndex, // 标识符的盲索引,绝不落明文
    ratelimit.Limit{Rate: 5, Per: time.Minute})
if err != nil {
    return false, err // KVStore 失败——放行还是关闭由你决定
}
byIP, err := limiter.Allow(ctx, "authn:login:ip:"+ip,
    ratelimit.Limit{Rate: 20, Per: time.Minute})
if err != nil {
    return false, err
}
return byAccount.Allowed && byIP.Allowed, nil
```

`Allow` 就是全部表面:每次调用都是一次命中(没有「只查不计」模式),
`Decision{Allowed, Remaining, ResetAfter}` 是纯数据——把拒绝翻译
成带 `Retry-After` 与配额头的 429 是你 HTTP 层的活,用包提供的唯一
协议辅助 `RetryAfterSeconds(remaining)`(向上取整,亚秒尾巴绝不读
成「立即重试」)。

## 核心概念与 API 要点

- **算法**——滑窗*计数*,不是滑窗日志:时间按 `limit.Per` 切成固定
  窗口,每窗一个 KVStore 键,判定把当前窗计数加上前一窗计数按流逝
  时间衰减后的权重——`current + previous*(1-elapsed)`。这抹平了
  经典固定窗口的洞:卡在窗口边界的突发本可拿到 2 倍配置速率。记录
  命中是一次原子的 `IncrByFloatWithTTL` 调用(ttl 只在创建键的那
  次调用上附着,绝不延长活键)——调用方先自增再条件 `Set` 会重开
  的竞态由该原语封闭,这正是 `KVStore` 添它的原因。
- **限额与校验**——`Limit{Rate, Per}` 要求 `Rate >= 2`、`Per`
  为正:`Rate` 为 1 直接被拒,带码
  `ratelimit.rate_one_unsupported`(包裹 `ErrInvalidLimit`)——加权
  公式下速率 1 会坍缩成「永远只准一次,此后永久拒绝」,所以在碰
  存储之前就拒绝,绝不静默交付一个永久锁死。别把「每天一次」拼成
  `Rate: 1`;要硬性恰好一次上限的调用方自己建最后成功门。
- **存储契约**——`Allow` 原样返回任何 `KVStore` 错误,包括被取消
  上下文的;它从不在你背后替你做放行/关闭决定,所以每个调用点按
  自己的风险自己选。窗口按调用进程的本地墙钟推进——共享一个
  KVStore 的副本只在时钟一致时描述同一条连贯滑窗。

## 边界与注意

- key 原样落进 KVStore:key 里的邮箱或电话就是静态 PII。标识符敏感
  就按盲索引建 key(经 `dbkit.NewBlindIndexer`)。
- 没有渐进或升级语义——authn 的「失败越多延迟越长直至锁死」是叠
  在裸计数上的业务逻辑,不是本包的某种模式。
- `Decision.Remaining` 是加权近似、下限为零:同一调用 `Allowed` 为
  true 时它也可能读 0。别把它当精确倒计时。
- 成文的收敛行为,不是缺陷:每个窗口都把 `Rate` 打满的客户端,从
  第二个窗口起每窗至多被放行 `Rate - 1` 个(每次调用都是命中,
  被拒的也算)。限流器只会比配置少放行,不会多——方向安全——但
  持续打满的消费方(如 API 密钥节流)应当知道这个形状。
- 没有动态配置:`Limit` 值由各调用点写死在代码里。没有 schema 承载、
  可运维调参的设置——那需要本库刻意不要的模块机制。
- 别给 `Limiter` 加第二个方法、HTTP 专用 `Decision` 或多键 `Allow`;
  别把它做成 `the module contract`;别在包内读租户上下文。一次调用一个
  维度、由调用方组合,就是整个设计。

## Source

- [ratelimit AGENTS.md](https://github.com/vislake/speed/blob/main/go/ratelimit/AGENTS.md)
