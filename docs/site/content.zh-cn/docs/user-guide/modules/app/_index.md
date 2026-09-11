---
title: 装配层
weight: 5
description: "在其余各组之上的唯一模块——go/app:装载器、七阶段组件驱动、固定中间件链与无 import 桥接,每个宿主的启动都经其组装。"
bookCollapseSection: true
---

# 装配层

本组只有一个模块:`go/app`——**应用装配层**,依赖图里位于其余各
组之上的唯一模块,也是唯一没有业务域的模块。它拥有每个宿主启动共
享的结构(配置装载、composition 计划、七阶段组件驱动、关停时序与
HTTP 帮手),策略留在宿主(组装哪些组件、用什么取值、自己的路由与
listener)。它也是每个宿主的 `cmd/server` 都会 import 的模块——接
线方式、中间件链、桥接与已知限制见 [app 页](./app/)。
