# 运维 XIRANG

[English](OPERATIONS.md) | [简体中文](OPERATIONS.zh-CN.md)

适用于 XIRANG **1.1.0**。

本手册包含备份、恢复、升级和回滚。可能中断服务的操作应安排在维护窗口，并将
恢复点复制到独立于控制节点的存储中。

## 备份与校验

在线创建、列出和校验恢复点：

```bash
xirangctl backup-create
xirangctl backups
xirangctl backup-verify BACKUP_ID
```

恢复点包含控制状态、遥测、拓扑、流量和告警数据库的一致性快照，以及 SHA-256
清单。创建完成后应将整个目录复制到其他设备，并配置保留策略。

## 离线恢复

恢复前停止所有可能访问同一状态的控制节点：

```bash
sudo systemctl stop xirangd xirangd-standby
sudo xirang-maint verify /var/lib/xirang/backups/BACKUP_ID
sudo xirang-maint restore /var/lib/xirang/backups/BACKUP_ID /var/lib/xirang
sudo systemctl start xirangd
```

维护工具会校验哈希与 bbolt 结构，拒绝覆盖仍被占用的数据库，并把被替换文件保存
到回滚目录。恢复后检查 `/readyz`、`/api/v1/version`、Agent 重连、队列预约和只读
网页会话。

## PostgreSQL 主备部署

两个控制节点应使用相同的 TLS PostgreSQL DSN 和命名空间，被动实例增加
`-standby`。一个命名空间只能属于一个 XIRANG 集群。

XIRANG 恢复点保护应用层数据库镜像；还应执行 PostgreSQL 原生备份，单独保存
校验和，并先在独立数据库中演练恢复。覆盖已有命名空间前必须停止两个控制节点。

维护者安装 PostgreSQL 16 工具后，可在无容器环境中运行隔离的主备与恢复演练：

```bash
make ha-drill
```

演练覆盖领导锁互斥、控制进程故障、数据库中断、状态连续性、损坏拒绝和
dump/restore，但不代表已经验证独立电源、机架或交换机故障域。

## 升级

1. 使用 `SHA256SUMS` 校验安装包并创建恢复点。
2. 先升级被动控制节点。
3. 停止活动控制节点，让新版被动节点接管。
4. 检查就绪状态、版本、Agent 重连、资源预约和网页访问。
5. 升级原活动节点，并将其恢复为被动节点。
6. 分批升级 Agent；节点上有任务时先排空再重启。

不要绕过数据版本错误，也不要让不兼容的控制器访问同一个 PostgreSQL 命名空间。
systemd 主机升级后应检查 `/etc/xirang/shared-write-paths.d`，并重启相关服务，使
精确共享路径权限生效。

构建签名密钥应独立轮换：先把新公钥部署到 Agent，再启用新签名私钥；旧编译
产物不再使用后，才移除旧公钥。

## 回滚

停止所有控制节点，恢复由旧版本创建的恢复点，安装匹配的旧版原生包，再启动一个
控制节点。确认集群状态正确后，才能重新启动备用节点或继续升级 Agent。
