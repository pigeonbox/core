# core · 业务核心库

[![CI](https://github.com/pigeonbox/core/actions/workflows/ci.yml/badge.svg)](https://github.com/pigeonbox/core/actions/workflows/ci.yml)
[![Tag](https://img.shields.io/github/v/tag/pigeonbox/core)](https://github.com/pigeonbox/core/tags)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/github/license/pigeonbox/core)](LICENSE)

PigeonBox 业务核心库:18 个域服务(含 config 系统配置中心) + 数据访问 + 存储抽象 + HTTP 装配(bootstrap)。**不含 main、不含静态资源、不含配置文件**——本模块定位是**被复用的库**,消费方:

- [`pigeonbox/server`](https://github.com/pigeonbox/server) —— 标准独立部署(自托管)
- [`pigeonbox/fnos`](https://github.com/pigeonbox/fnos) —— 飞牛 fnOS 应用(单容器库式调用)
- [`pigeonbox/openwrt`](https://github.com/pigeonbox/openwrt) —— OpenWrt/iStoreOS 原生包(库式调用)
- [`pigeonbox/qnap`](https://github.com/pigeonbox/qnap) —— 威联通 QTS 原生 QPKG(库式调用)

> 🗂️ [PigeonBox 生态](https://github.com/orgs/pigeonbox)成员仓 · 总览见 [装配仓 pigeonbox](https://github.com/pigeonbox/pigeonbox) · [架构图集](https://github.com/pigeonbox/pigeonbox/blob/main/docs/architecture.md)

## 结构

| 目录 | 说明 |
|------|------|
| `bootstrap/` | 库入口:`Bootstrap(configPath)` 拉起全部业务,返回 `*server.Hertz` |
| `app/` | 17 个域服务:share / chunk / anonymous / presign / admin / config / user / notify / qrcode / setup / storage / federation / mcp / moderation / oidc / preview / request(模块间近零耦合;跨域引用仅 admin→config 与 share→moderation 钩子窄接口,原 presign→share 已消解;2026-10-22 Wave B' 删 p2px 设备直传编排域) |
| `repo/` | 数据访问:db(gorm dao/model)、redis、external;dao 构造**必填 `*gorm.DB`** 依赖注入,全局回退已退役 |
| `storage/` | 存储抽象(OpenDAL 多后端) |
| `pkg/` | 12 包横切设施:auth / baseurl / errors / gate / logger / memkv / metrics / middleware / resp / security / transfer / utils |
| `transport/` | 手写 http handler 与中间件(admin 面增强端点已 IDL 化收编 gen 链,手写区收窄为存储注入/多文件/OIDC 等桥接) |
| `preview/` | 文件预览服务 |
| `conf/` | 配置结构定义 |
| `gen/` | thrift 生成的 handler / router(模型类型在 [`pigeonbox/contracts`](https://github.com/pigeonbox/contracts)) |

> **p2p 联邦面**:2026-10-22 两应用分拆(Wave B')后,Box 侧**只保留联邦面** ——
> 设备直传(p2pc/p2pcweb)已拆为独立应用 **PigeonBeam(信鸽直传)**,Box 不再编排
> 直传(直传九端点与 p2px 域整体退役)。`/admin/p2p/*` 由 contracts
> `AdminP2PService`(`idl/p2p.thrift`)定义,经 IDL gen 链接入,现存**四端点**
> (invites 待收列表 / invites/accept 认领 / invite-push 推送 / directory 在线名录);
> 另有公开收件 `POST /api/v1/p2p/invite`(P2PPublicService,限流归桶;
> 口令=bearer,信任级同手工交接)。accept 为**纯信封**动作(出箱 + 留痕 + 返回
> 信封),文件接收由 PigeonBeam 客户端完成。
>
> **mcp**:单端点 18 个工具;P2P 工具为联邦三件套(p2p_peers / p2p_invites /
> p2p_accept,admin 级);直传四工具(p2p_status / p2p_tasks / p2p_send / p2p_recv)
> 随直传编排层一并退役。

## 分层职责(继承自原仓库 internal 设计)

1. `app/` —— 业务逻辑层,不依赖具体传输协议,不直接操作数据库(经 repo 层)
2. `transport/` + `gen/` —— 传输层,协议适配,调用 app 层服务
3. `repo/` —— 数据访问层,封装 DB/Redis/外部服务
4. `pkg/` —— 项目内横切工具(不含对外通用库)

## 库用法

```go
import "github.com/pigeonbox/core/bootstrap"

h, err := bootstrap.Bootstrap("/path/to/config.yaml")
if err != nil { log.Fatal(err) }
defer bootstrap.Cleanup()
go h.Spin()
```

## 依赖规则(强制)

```
core ──► contracts ──► (thrift runtime)
  └─ 不允许 import server / frontend;不引入任何 UI 与部署物
```

- thrift 版本约束由 contracts 以 `require` 传递,**下游无需任何 replace**(这是从原仓库拆出的重要修复:原仓库靠 `replace thrift => v0.13.0` 钉版本,库使用者被迫复述该约束)。
- 显式钉 `bytedance/sonic v1.15.0`:hertz 声明的 v1.12.7 在 go1.27/arm64 有 linkname 链接错误。

## 来源

拆分自 [zy84338719/fileCodeBox](https://github.com/zy84338719/PigeonBox) 的 `backend/api`、`backend/gen/http/{handler,router}`、`backend/cmd/server/bootstrap`,import 路径机械替换,业务逻辑零变更。

## License

[Apache-2.0](LICENSE)
