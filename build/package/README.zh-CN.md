# 原生发行包制作

[English](README.md) | [简体中文](README.zh-CN.md)

XIRANG 发布为原生 Linux 压缩包，不发布容器镜像。运行：

```bash
make package-native VERSION=1.0.1
```

生成的 `dist/xirang-<版本>-linux-<架构>.tar.gz` 包含静态控制平面、Agent、CLI、
MPI 适配器二进制文件、网页资源、服务定义、示例和公开双语手册。内部工程档案不会
进入发行包。
