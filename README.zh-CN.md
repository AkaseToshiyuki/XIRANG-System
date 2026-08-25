<p align="center">
  <img src="web/public/brand/xirang-mark.png" alt="XIRANG 标志" width="180">
</p>

<h1 align="center">XIRANG</h1>

<p align="center">
  <strong>eXtensible Infrastructure for Resource-aware Adaptive Networked Grids</strong>
</p>

<p align="center">
  一个面向异构计算集群的自扩展编排与调度平台。
</p>

<p align="center">
  <a href="README.md">English</a> · <strong>简体中文</strong>
</p>

XIRANG 将处理器、加速卡、存储和网络条件各异的 Linux 服务器组成一个易于
管理的集群。用户可通过统一的网页界面接入节点、查看运行状态与网络拓扑、
管理共享空间，并提交原生计算任务。

当前稳定版本：**1.0.1**。

## 特点与优势

- **轻松扩展集群**：安装 Agent 后在网页中批准接入，XIRANG 会自动发现节点
  之间可用的网络路径。
- **状态一目了然**：集中查看节点健康度、资源占用、温度、链路、协商速率与
  备用路径。
- **智能任务调度**：根据节点能力、用户组配额与运行时长上限、任务优先级、
  硬件需求和管理员偏好安排任务；单节点不可能满足的请求会立即返回错误。
- **用量与费用核算**：按用户查看实测 CPU、内存、GPU 和工作区用量，并由
  管理员通过按生效日期保存的计费标准计算费用。
- **原生硬件性能**：计算任务直接在 Linux 上运行，不要求额外部署容器运行时，
  减少对硬件监测与性能的影响。
- **共享空间管理**：可将存储路径分配给全体节点、一个节点分组或一组指定节点。
- **适合多用户使用**：支持首次管理员创建、注册审批、权限隔离、个人任务、
  用户组配额和仅管理员可用的节点操作。
- **支持异构环境**：XIRANG 提供 Linux AMD64 和 ARM64 预构建包，任务可按
  架构和设备能力选择节点。
- **在目标架构原生构建**：管理员可让已纳管 Agent 在 AMD64 或 ARM64 上编译
  同一份校验和固定源码，并发布带签名的共享编译产物。

## 安装

克隆仓库后运行引导式安装程序：

```bash
git clone https://github.com/AkaseToshiyuki/XIRANG-System.git
cd XIRANG-System
./install.sh
```

安装程序会识别 Linux 发行版和 CPU 架构，安装所需的系统工具，下载并校验
匹配的预构建包，然后询问本机作为控制节点还是被控节点。需要管理员权限时，
程序会通过标准 `sudo` 提示主动请求授权，不读取或保存密码。

也可以安装已经下载到本机的预构建包：

```bash
./install.sh --archive /path/to/xirang-1.0.1-linux-amd64.tar.gz
```

支持范围、端口、自动化参数和故障排查请参阅
[部署指南](docs/user/DEPLOYMENT.zh-CN.md)。

## 使用

控制节点初始化完成后访问：

```text
https://控制节点IP:8899
```

创建首位管理员，然后接入节点、配置节点角色和共享空间，并使用简明的
`XIRANG 1` 脚本提交任务。完整流程参阅[使用指南](docs/user/USAGE.zh-CN.md)。

使用指南同时包含 Job 模板、GROMACS、多节点任务、原生构建和用量核算；备份、
恢复和升级请参阅[运维指南](docs/user/OPERATIONS.zh-CN.md)。

## 从源代码构建

开发者可使用以下命令构建和验证：

```bash
make verify
make web-install web-build
make package-native VERSION=1.0.1
```

公开双语手册位于 [`docs/user`](docs/user)。

## 许可证

XIRANG 采用 [GNU 通用公共许可证第 3 版](LICENSE)，SPDX 标识为
`GPL-3.0-only`。允许修改和二次分发，也允许商业分发；公开分发的衍生版本必须
继续采用 GPLv3 并提供相应源码。依赖许可证见
[第三方声明](THIRD_PARTY_NOTICES.zh-CN.md)。
