---
title: 参考应用演练
weight: 3
description: 启动参考应用,用真实 HTTP 驱动登录、权限与租户隔离——演示账号、访问令牌,以及一个组装好的 speed 产品会应答的错误码。
---

# 参考应用演练

`examples/reference-app` 是 speed 的强制第一消费方:一个 AI 微笑模拟
平台,由它的组合配置选出四十多个组件——平台模块、内置的进程内组件
实现,以及宿主自己的步骤与接线组件——经一次 `app.Assemble` 驱动装
配到一起,并在启动时预置一层演示身份。它也是感受组装好的 speed 产品
在线上实际行为的最快方式。本演练以 standalone 部署形态运行它——单进
程、单个 SQLite 文件、每个基础设施模块都在进程内、零外部依赖——用
真实 HTTP 请求驱动登录、权限与租户隔离。

## 1. 带演示账号启动应用

```sh
cd examples/reference-app
APP_DB_PATH=/tmp/ref.db APP_DEMO_USERS_PASSWORD='a demo passphrase' \
  APP_DEMO_PLATFORM_STAFF_PASSWORD='an admin demo passphrase' \
  go run ./cmd/server
```

第一次启动会跑迁移,并经由真实的注册路由预置演示账号;之后对着同一个
数据库文件再启动会幂等地重申这套预置。先看健康:

```sh
curl -s localhost:8080/healthz
# ok -- 不需要租户,不需要凭据
```

## 2. 租户、账号与令牌

配置了两个演示租户:`tenant-acme` 与 `tenant-globex`。设置
`APP_DEMO_USERS_PASSWORD` 时会预置三个演示账号,三者的密码都是该
变量的值:

| 账号 | 角色 | 租户 |
|---|---|---|
| `demo-owner@example.com` | 内置 owner(任何模块声明的全部权限) | `tenant-acme`、`tenant-globex` |
| `demo-reader@example.com` | 自定义 `note-reader`(只有 `notes:read`) | `tenant-acme`、`tenant-globex` |
| `demo-acme-only@example.com` | 自定义 `note-reader` | 只有 `tenant-acme` |

需要内化的模式:授权是 `(租户, 用户)` 这一对上的事实,从来不是关于
某个用户的孤立事实。平台员工账号在另一个独立变量
`APP_DEMO_PLATFORM_STAFF_PASSWORD` 下单独预置——绝不与演示用户的
口令相同。

租户装在**访问令牌**里,从不放在请求头中。登录时指明想要的租户;应答
的令牌携带调用者的成员关系;此后的每个请求都带
`Authorization: Bearer <token>`,`tenancy.Middleware` 从验证过的
principal 解析租户。没有 `Host` 头路由,也没有 `X-Tenant-Id` 头。
(`Host` 在这套应用里只为一件事起作用:config 的两个免认证展示端点靠
硬编码的 `acme.demo.localhost`/`globex.demo.localhost` 映射挑演示
品牌。)

## 3. 以 reader 身份登录并读取

```sh
TOKEN=$(curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-reader@example.com","password":"a demo passphrase","tenant_id":"tenant-acme"}' \
  | jq -r .access_token)
```

应答携带 `access_token`、`refresh_token` 与 `principal`;没有 `jq`
的话,手动把 `access_token` 的值贴进这个 shell 变量。reader 持有
`notes:read`,所以列表能读:

```sh
curl -s localhost:8080/api/v1/notes -H "Authorization: Bearer $TOKEN"
# {"notes":[]}
```

## 4. 被拒绝的写入:`rbac.permission_denied`

reader 的角色只授 `notes:read`,别的什么都没有——rbac 默认拒绝:

```sh
curl -s -i -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'
# HTTP/1.1 403 Forbidden
# {"code":"rbac.permission_denied", ...}
```

## 5. 以 owner 身份写入

内置 owner 角色也持有 `notes:write`:

```sh
OWNER=$(curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-owner@example.com","password":"a demo passphrase","tenant_id":"tenant-acme"}' \
  | jq -r .access_token)

curl -s -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer $OWNER" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'
# {"id":"<note-id>", "text":"buy milk", ...}

curl -s localhost:8080/api/v1/notes -H "Authorization: Bearer $OWNER"
# {"notes":[{"id":"<note-id>", "text":"buy milk", ...}]}
```

## 6. 租户隔离:授权是 `(租户, 用户)` 上的事实

`demo-acme-only` 的成员关系与 reader 授权只存在于 `tenant-acme`。
为 `tenant-globex` 登录它在到达任何路由之前就会被拒绝:

```sh
curl -s -i -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-acme-only@example.com","password":"a demo passphrase","tenant_id":"tenant-globex"}'
# HTTP/1.1 401 Unauthorized
# {"code":"authn.invalid_credentials", ...}
```

这与错误密码得到的应答完全相同——这是刻意的。账号是真的、密码也是
对的,但登录端点绝不能向匿名调用者证实密码;具体原因(成员关系无法
解析)记在登录历史里,绝不进应答。同一个账号登录 `tenant-acme`
则成功。

## 7. 没有身份时的拒绝形态

notes 路由从验证过的 principal 解析租户,匿名请求没有可解析的东西:

```sh
curl -s -i localhost:8080/api/v1/notes
# HTTP/1.1 403 Forbidden
# {"code":"tenancy.tenant_unresolved", ...}

curl -s -i localhost:8080/api/v1/notes -H 'Authorization: Bearer not-a-real-token'
# HTTP/1.1 401 Unauthorized
# {"code":"authn.token_invalid", ...}
```

提交的凭据验证不过,是一次失败的**身份主张**;根本没有凭据,则不是
主张。两种应答不同,也都什么都不会泄露。

## 8. 你看到了什么

| 请求 | 应答 | 含义 |
|---|---|---|
| `demo-reader` 登录 `tenant-acme` | 200,签发令牌 | 成员关系与角色来自预置 |
| 以 reader `GET /api/v1/notes` | 200 `{"notes":[]}` | 有 `notes:read`,暂无内容 |
| 以 reader `POST /api/v1/notes` | 403 `rbac.permission_denied` | 默认拒绝;未授 `notes:write` |
| 以 owner `POST /api/v1/notes` | 201,返回 note | owner 角色持有写权限 |
| `demo-acme-only` 登录 `tenant-globex` | 401 `authn.invalid_credentials` | 在那里没有成员关系——按错误密码应答 |
| 匿名 `GET /api/v1/notes` | 403 `tenancy.tenant_unresolved` | 没有 principal 可解析租户 |
| 带垃圾 bearer `GET /api/v1/notes` | 401 `authn.token_invalid` | 提交的身份验证不过 |

动手复制之前,先认识两个演示便利设施:`X-Demo-User`/`X-Demo-User-Id`
请求头早于 authn 存在,在某些界面上代替真实登录——它们不是认证,任何
可能有真实用户触达的部署都会设置 `APP_DISABLE_DEMO_USER_HEADER`
把两者一起关掉。另外,你创建的 notes 存在 `/tmp/ref.db` 里:用 Ctrl-C
停掉服务,再对着同一个路径启动,会看到预置与你的 notes 都还在。

## 接下来

- [错误码索引](../error-codes/)——speed 系 API 可能应答的每个错误码,
  及其状态、locale 消息与触发条件。
- [身份与访问](../domains/identity-access/)——本演练走过的中间件顺序,
  以及如何在自己的项目里接出同样的链。
- [生成项目的操作](../operating/)——部署形态与日常 `saasctl` 命令。

## Source

- [authn AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md)——上述应答背后的令牌、会话与中间件契约。
