# pkg/ - core 横切设施

此目录存放 core 内部的横切工具和通用组件。

## 目录结构

```
pkg/
├── auth/        # 认证（JWT/API Key）
├── errors/      # 错误处理（错误码定义在 contracts errcode）
├── gate/        # 功能门控
├── logger/      # 日志工具封装
├── metrics/     # 指标
├── middleware/  # HTTP 中间件
├── paircode/    # 配对口令归一（p2pc 口令；直传域 + 联邦域共用单源）
├── registry/    # 联邦注册中心基址归一（直传域 + 联邦域共用单源）
├── resp/        # HTTP 响应工具
├── security/    # 安全设施（SSRF 校验等）
├── transfer/    # 传输相关
└── utils/       # 通用工具函数
```

## 与 kit 的边界

| 位置 | 用途 | 定位 |
|------|------|------|
| [`pigeonbox/kit`](https://github.com/pigeonbox/kit) | 通用工具库（async/retry/ratelimit/… 共 28 包） | 零生态依赖，任何项目可 `go get` 消费 |
| 本 `pkg/` | core 横切设施 | 与 core 业务/配置绑定的专用设施，随 core 版本走 |

## 主要组件

### errors/
- AppError 错误结构
- errcode（定义在 contracts）到错误消息/HTTP 响应的映射

### logger/
- Zap 日志封装
- 日志级别、文件输出配置
- 便捷日志方法

### resp/
- HTTP 响应工具函数
- Success/Error/Page 等标准响应
- 错误码自动映射

### paircode/ 与 registry/
- 跨域共用事实的单源下沉（避免各域各存一份同值副本、改一处即分叉）：
  - `paircode.Normalize`：配对口令（p2pc）归一——去空白/连字符、大写、4-4-4 分组；
  - `registry.Effective`：联邦注册中心（p2pd）基址归一——多主备取首个、留空回填官方公共。

## 添加新工具

1. 在 `pkg/` 下创建新目录
2. 实现工具函数
3. 在需要的地方导入使用

```go
import "github.com/pigeonbox/core/pkg/yourpkg"
```
