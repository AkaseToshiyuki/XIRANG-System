# 第三方声明

[English](THIRD_PARTY_NOTICES.md) | [简体中文](THIRD_PARTY_NOTICES.zh-CN.md)

XIRANG 依据 GNU 通用公共许可证第 3 版发布。以下独立授权的组件用于构建或运行
XIRANG；这些组件继续适用各自的许可证。

## 运行时组件

| 组件 | 版本 | 许可证 |
| --- | --- | --- |
| `github.com/cilium/ebpf` | 0.22.0 | MIT |
| `github.com/creack/pty` | 1.1.24 | MIT |
| `github.com/gorilla/websocket` | 1.5.3 | BSD-2-Clause |
| `github.com/jackc/pgx/v5` 及其 Jackc 支持模块 | 5.9.2 | MIT |
| `go.etcd.io/bbolt` | 1.3.11 | MIT |
| `golang.org/x/crypto`、`x/net`、`x/sync`、`x/sys` 和 `x/text` | 以 `go.mod` 锁定版本为准 | BSD-3-Clause |
| `google.golang.org/grpc`、`genproto` 和 `protobuf` | 以 `go.mod` 锁定版本为准 | Apache-2.0 / BSD-3-Clause |
| React 和 React DOM | 18.3.1 | MIT |
| Noto Sans SC 可变字体 | 5.3.0 | SIL Open Font License 1.1 |

## 开发和构建组件

锁定版本的 Web 工具链还包含采用 MIT、Apache-2.0、ISC、BSD-3-Clause、SIL
Open Font License 1.1 和 CC-BY-4.0 的软件包。它们用于生成静态 Web 资源，
不会作为 XIRANG 服务安装。

可访问性测试使用 MPL-2.0 的 `@axe-core/playwright` 4.11.1 与锁定的
`axe-core` 4.11.4。这些未修改的开发工具不会被应用导入，也不进入原生发行包；
其上游软件包保留许可证和第三方声明。

原生发行包会把本次构建实际使用的依赖许可证和声明文件收集到
`share/licenses`。源码分发保留 `go.sum` 和 `web/package-lock.json`，作为依赖
精确版本记录。

XIRANG 不内嵌任何第三方数据集。`nvidia-smi`、编译工具链、PostgreSQL 和任务
应用等宿主机工具只会在管理员安装后被探测或调用，不随 XIRANG 再分发。
