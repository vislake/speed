---
title: storage
description: "媒体对象存储:元数据在租户表里,字节在你的 ObjectStore,三步上传协议在完成时复验实际到达的内容。"
weight: 1
---

# storage

storage 是 speed 的媒体对象模块:描述一个租户已存对象的元数据住在
数据库里,字节住在内核解析的 `ObjectStore` 里,三步上传协议带服务
端复验把字节送进来——以实际到达的字节为权威,上传者的声明不算。

## 它做什么

模块拥有的是**元数据,不是字节**。对象的字节躺在你的 `ObjectStore`
里(独立部署是本地目录,分布式是 S3——模块从不知道是哪个),键由
模块从租户与对象 id 推导、从不暴露:消费方只按 id 指名对象。两张租
户作用域的表(`objects`、`object_derivatives`)承载生命周期故事。
`NewModule` 构建三个服务:

- `ObjectService`——传输生命周期(`Create`、`Upload`、`Complete`)
  与读面(`Get`、`OpenContent`、`List`);
- `DeriveService`——从完成的图片派生缩略图;
- `LifecycleService`——删除与过期清扫(`Delete`、`Sweep`、
  `EnqueueExpirySweep`)。

上传走三步协议。`Create` 校验你的声明——尺寸、媒体类型、可选
SHA-256 校验和、可选保留期——并开一个上传窗口(行处于
`uploading`)。`Upload` 把请求体流进存储,逐字节受声明尺寸约束。
`Complete` 在行变得可读之前复验实际到达的内容:已存尺寸与校验和同
声明对账,MIME 从内容魔数探测并对照允许清单,头部像素检查,以及结
构性元数据剥离(JPEG 的 EXIF/XMP/IPTC/COM,PNG 的 `eXIf` 与文本
块)——walker 无法结构验证的文件一律拒绝,绝不放行。此后行才推进
到 `completed`,缩略图派生任务入队,`storage.object.completed` 事件
发出。

删除是崩溃收敛协议:标 `deleting`,删对象字节与每个派生的字节,再
在一个事务里删行——任一步被打断都由下一次运行收敛,绝不重复。读
面只服务 `completed` 对象。

它**不是**什么:不是通用 blob 仓库——被准入的媒体类型必须是模块
能查像素**且**能剥离元数据的(注册时的准入门拒绝其它一切);不是
预签名上传服务——上传经 `Upload` 在你的服务器内流式完成;也不是保
留计时器——过期在 create 时校验,由你的宿主调度的按租户清扫
(`EnqueueExpirySweep`,任务 `storage.expiry_sweep`)执行,绝无模块
自有的计时器。

## 何时选用

你的产品接收用户上传的媒体——图片优先——它们必须存在数据库外、
去掉位置与作者元数据后回传、保留期过后被清理。需要缩略图时,派生
流水线已备。任意类型的文档、或不经服务器一跳的浏览器直传存储,暂
不适用。

## 怎么接线

```go
m := storage.NewModule(db,
    storage.WithQueue(queue),           // 一个 jobs.Queue——没有它 Register 直接拒绝
    storage.WithMaxUploadBytes(10<<20), // 可选:单对象上限
)
// 把 m 交给 the assembly 的模块集;ObjectStore 与 EventBus
// 来自已解析的注册表,每次调用现读。

svc := m.ObjectService()
created, err := svc.Create(ctx, storage.CreateParams{
    DeclaredSize:      size,          // 传输观测到的字节长度
    DeclaredType:      "image/jpeg",  // 允许清单内;"" 表示不作声明
    DeclaredChecksum:  hexSHA256,     // 可选:64 个小写十六进制字符
})
// 处理 err——任何字节移动之前的拒绝都发生在这里
err = svc.Upload(ctx, created.ID, &size, body)
finalized, err := svc.Complete(ctx, created.ID)
```

`DeriveService`(`DeriveThumbnail`)与 `LifecycleService`(`Delete`、
`Sweep`、`EnqueueExpirySweep`)是另两个面;过期清扫按租户进行,宿
主按自己的节奏为每个租户调度一个任务。

## 核心概念与 API 面

- **到期在 create 时定死。** 请求的有限保留期落成那个期限(受宿主
  `WithMaxObjectLifetime` 上限约束,默认 90 天);不请求——普通上
  传——落成同一上限处的到期;只有构建时开了
  `WithNoExpiryAllowed()` 的模块上显式 `NoExpiry: true` 才留下无
  到期的行。永久需要两次刻意动作,绝不是一次省略。
- **状态与窗口闸门。** 过了上传窗口或不在 `uploading` 的行拒绝进
  一步写入;窗口在途中关闭的完成会输掉定稿;清扫与并发传输不可能
  留下孤儿或双占的字节。
- **选项**(各有具名默认):`WithMaxUploadBytes`(100 MiB)、
  `WithMaxImagePixels`(40 000 000)、`WithDerivativeMaxEdge`
  (320 px)、`WithUploadTTL`(30 分钟)、`WithMaxObjectLifetime`
  (90 天)、`WithNoExpiryAllowed`(关)、`WithAllowedTypes`
  (image/jpeg、image/png)。
- **HTTP 面。** `/api/v1/storage` 下七个操作——`storage_createObject`、
  `storage_uploadObjectContent`、`storage_completeObject`、
  `storage_listObjects`、`storage_getObject`、
  `storage_getObjectContent`、`storage_deleteObject`;线上没有
  `tenant_id`。模块声明权限 `storage:read`/`storage:write`、审计动
  作 `storage.object.*`(声明但不发出)与完成/删除两个事件。

## 已知限制与链接

- 元数据剥离分类的是**载体,不是载荷字节**:塞进熵编码图像数据的
  元数据会穿过,头部像素探测看不到头部以下。全解码重编码未实现。
- 上传与 `Complete` 只在**单进程内**按对象串行;跨分布式部署副本
  的残余竞态记在模块的 `AGENTS.md` 里,不藏。
- 不调度清扫的宿主会保留一切:模块没有自己的计时器;清扫绝不快速
  失败——一行坏数据饿不死同租户其余行(`storage.sweep_partial_failure`
  点名失败行)。
- 副作用发布(完成事件、派生入队、删除事件)只告警、不使调用失
  败。
- 结构化错误码:见[错误码索引(English)](/docs/user-guide/error-codes/#storage)——
  `storage.type_not_allowed`、`storage.size_mismatch`、
  `storage.object_not_found`、`storage.store_unavailable` 等。

### 出处

- [go/storage/AGENTS.md](https://github.com/vislake/speed/blob/main/go/storage/AGENTS.md)——权威文档(生命周期、复验流水线、键文法、已知限制、未实现清单)
- 相关页面:[平台服务](../)、[notification](../notification/)、域指南[存储、分享与 AI](../../../domains/storage-sharing-and-ai/)
