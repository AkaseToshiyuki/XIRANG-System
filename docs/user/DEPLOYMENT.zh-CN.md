# 部署 XIRANG

[English](DEPLOYMENT.md) | [简体中文](DEPLOYMENT.zh-CN.md)

适用于 XIRANG **1.2.0**。

本指南使用预构建原生包部署控制节点或被控节点，不需要容器运行时。

## 支持范围

- CPU 架构：Linux AMD64、ARM64
- 服务管理：systemd、OpenRC
- 软件源：apt、dnf、yum、zypper、pacman、apk、xbps、Portage
- 默认网页与管理 API：TCP `8899`
- 控制节点的 Agent 通信：TCP `8444`
- 等待网页扫描接入的临时端口：TCP `8844`

请先按实际网络范围配置防火墙。不要将 `8444` 或 `8844` 暴露到不受信任的
网络。

## 一键安装

```bash
git clone https://github.com/AkaseToshiyuki/XIRANG-System.git
cd XIRANG
./install.sh
```

安装器会：

1. 检查 Linux 架构和服务管理器。
2. 仅在缺少系统工具时调用软件包管理器安装依赖。
3. 通过标准 `sudo` 提示请求系统安装权限。
4. 从 GitHub Release 下载与本机架构匹配的包。
5. 先校验 Release 的 `SHA256SUMS` 签名，再用它校验安装包。
6. 启用已有 NTP 客户端，或安装 Chrony，并确认时间已经同步。
7. 安装服务并进入控制节点或被控节点初始化向导。

脚本不会读取、保存或上传 sudo 密码。密码只由本机 `sudo` 程序处理。

mTLS 证书有效期、OIDC 会话、遥测排序和用量核算要求主机具有可靠的系统时间。
普通 NTP 已经足够，不强制要求 PTP。在有意离线的暂存环境中，可使用
`--allow-unsynchronized-clock` 带警告继续，但必须在节点接入前完成时间同步。
`--skip-dependencies` 不会静默绕过时间检查。

## 选择版本或本地包

安装指定 Release：

```bash
./install.sh --version 1.2.0
```

安装已下载的包时，安装器会读取同目录下的 `SHA256SUMS` 和 `SHA256SUMS.sig`：

```bash
./install.sh --archive ~/Downloads/xirang-1.2.0-linux-amd64.tar.gz
```

如果没有校验文件，必须显式提供可信的 SHA-256：

```bash
./install.sh \
  --archive ~/Downloads/xirang-1.2.0-linux-amd64.tar.gz \
  --sha256 EXPECTED_SHA256
```

## Release 的信任来源

每个 Release 都会发布 `SHA256SUMS.sig`，即校验清单的 Ed25519 签名。安装器在读取
任何一条校验和之前，先用内嵌在 `install.sh` 中的公钥验证该签名；签名缺失或不匹配
时安装会中止。

仅有校验和不足以建立信任：安装包和校验清单来自同一个 GitHub Release、经由同一条
通道下发，能替换其中一个的攻击者同样能替换另一个。签名才让被替换的 Release 可被
发现，因为伪造签名需要私钥，而私钥不会离开发布流水线。

验证签名需要 OpenSSL 1.1.1 或更高版本，安装器会将其纳入自动安装的主机工具。

使用 `--sha256` 等同于在带外建立信任——由你自己断言一个已知摘要——因此会取代签名
校验。离线安装，或安装签名机制引入之前发布的版本时使用该参数。

## 初始化控制节点

在向导中选择 `controller`，输入其他节点可访问的控制节点 IP。安装器会启动
控制服务、注册本机 Agent，并显示网页地址。

打开 `https://控制节点IP:8899`，根据页面提示创建首位管理员。初始管理员
Token 位于控制节点 `/var/lib/xirang/admin.token`，文件权限仅允许服务账户读取；
后续使用网页账户和受限 API Token。

## 初始化被控节点

在向导中选择 `node`，然后选择一种方式：

- `direct`：输入控制节点 IP 和管理员生成的入群 Token，立即接入。
- `wait`：Agent 在 `8844` 端口等待；管理员在网页的节点发现界面输入该节点
  IP，核对身份并输入安装程序打印的一次性配对码后批准接入。如果安装终端已经
  关闭，可在该节点的 `xirang-agent` 服务日志中查看配对码。

接入后 Agent 会汇报硬件与网卡信息，并探测与已知节点之间的可用链路。

## 账户与服务身份

XIRANG 网页账户是控制平面中的虚拟身份。注册或批准账户不会在任何节点创建
Linux 登录用户。管理员可在“身份管理”中为之后注册的新账户选择默认用户组，
也可以明确不设置默认组。

原生安装包只创建不可登录的服务身份。管理员网页终端以 `xirang-operator`
运行，不具备 root 或 sudo 权限。原生任务默认从 `200000-265535` 分配只在任务
活动期间使用的临时数字 UID，不写入 `/etc/passwd`。可使用
`--executor-task-uid-min` 与 `--executor-task-uid-max` 调整范围，但必须避开本机、
LDAP 和 NFS 已使用的身份范围。

Agent 本身仍是特权的结构化代理，因为 cgroup、设备、挂载和硬件遥测需要主机
权限；这些权限不会传递给网页终端或任务可执行文件。

## 共享路径服务权限

导出已有目录时会保留宿主机的原属主、属组及本地用户的有效访问权限。XIRANG
通过 POSIX 访问 ACL 为任务组增加权限，不再接管目录属主或强制改成 `0770`。
原有 ACL 被掩码限制的权限不会被意外放开；默认 ACL 和目录内的文件保持原样。
新建导出根目录允许宿主机用户读取和进入（`0755` 加集群 ACL）。文件系统必须
支持 POSIX ACL；不支持时会明确报错，不会回退到修改模式位。NFS 仍使用
`all_squash` 映射到任务身份。

安装器也使用同一准备流程处理默认共享目录，并保留已有挂载父目录的权限。
私有父目录需要管理员配置遍历权限。能访问目录不等于能访问所有文件，也不等于
所有本地用户都获得写权限。旧版本已经覆盖原始元数据的目录，见
[共享目录宿主机访问恢复](OPERATIONS.zh-CN.md#共享目录的宿主机访问)。

在 systemd 主机上，XIRANG 只允许服务写入明确登记的共享路径，不再把整个
`/srv`、`/shared` 或 `/mnt` 设为可写。特权挂载辅助程序在
`/etc/xirang/shared-write-paths.d` 保存白名单，并在共享空间挂载或解除时更新服务
drop-in。导出源目录操作始终限制在特权辅助程序内；升级时会自动导入带标记的
`/etc/fstab` 挂载条目。

默认 `/srv/xirang/XirangShare` 会在首次启动前得到授权。增加或解除其他共享
路径后，应在维护窗口重启相关服务，使新的 systemd 挂载命名空间策略生效；
重启 Agent 前应先排空正在运行的任务。

共享挂载/导出控制要求升级两端并显式启用 Agent 的 `-shared-mount-control`
参数，默认关闭且仅支持 root/systemd。它使用 mTLS 通道及固定、限时的宿主机
服务，在 Agent 私有挂载命名空间之外运行；启用时保留已有环境参数。不自动回退
SSH，关闭能力不移除已有挂载。

文件传输和容量查询仍使用控制器 SSH 密钥及管理账户。新交互安装仅在 sudoers
列举 `stats`、`file-list`、`file-read`、`file-write`、`file-mkdir`、
`file-rename`、`file-delete`。旧 `mount-helper *` 规则须显式迁移，使用
`visudo -cf` 验证并预先核对主机密钥。该账户仍是可信特权身份，API 所有权不构成
直接 NFS/SSH 租户边界。迁移与故障语义见[共享存储](USAGE.zh-CN.md#配置共享空间)。
回滚旧 Agent 前必须移除新参数；文件专用 sudo 规则不会恢复旧 SSH 挂载变更。

## 服务检查

systemd：

```bash
sudo systemctl status xirangd
sudo systemctl status xirang-agent
sudo journalctl -u xirangd -u xirang-agent --since today
```

OpenRC：

```bash
sudo rc-service xirangd status
sudo rc-service xirang-agent status
```

只有控制节点需要运行 `xirangd`；被控节点只需要运行 `xirang-agent`。

## 常见问题

- **无法下载 Release**：检查到 GitHub Releases 的网络访问后重试。
- **没有匹配的资产**：确认 Release 同时包含
  `xirang-<版本>-linux-<amd64|arm64>.tar.gz` 和 `SHA256SUMS`。
- **网页无法访问**：检查 `xirangd` 状态、防火墙和输入的控制节点 IP。
- **节点无法接入**：检查 `8444`/`8844` 端口、系统时间和证书指纹。
- **时间同步失败**：检查 DNS、到 NTP 时间源的 UDP `123`，然后查看
  `timedatectl status` 或 `chronyc tracking`。绕过选项只用于离线暂存，不能作为
  集群的长期配置。
- **GPU 信息缺失**：先安装厂商驱动与管理工具，例如 NVIDIA 驱动提供的
  `nvidia-smi`；XIRANG 不会安装或替换 GPU 内核驱动。
