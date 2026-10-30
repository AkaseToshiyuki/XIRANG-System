# 运维 XIRANG

[English](OPERATIONS.md) | [简体中文](OPERATIONS.zh-CN.md)

适用于 XIRANG **1.2.0**。

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

切换前须为控制器预置相同且受保护的 `pki/` CA/密钥，服务端证书应覆盖稳定的
客户端访问地址。单独备份这套信任材料；PostgreSQL 不复制 CA 密钥和主机凭据，
新生成的独立 CA 会破坏 Agent 注册时建立的信任。管理员认证及已配置的签名/
Webhook 秘密也需明确、安全地预置一致。验收必须使用原 CA，不能关闭 TLS 校验。

XIRANG 恢复点保护应用层数据库镜像；还应执行 PostgreSQL 原生备份，单独保存
校验和，并先在独立数据库中演练恢复。覆盖已有命名空间前必须停止两个控制节点。

维护者安装 PostgreSQL 16 工具后，可在无容器环境中运行隔离的主备与恢复演练：

```bash
make ha-drill
```

演练覆盖领导锁互斥、控制进程故障、数据库中断、状态连续性、损坏拒绝和
dump/restore，全程校验原 CA。`report.json` 记录恢复到认证读回 16 个已确认标记的
单调时钟耗时、二进制摘要和环境。使用 `XIRANG_HA_KEEP=1 make ha-drill` 保留临时
证据；`XIRANG_HA_WORK_DIR` 必须是尚不存在的新目录，不覆盖旧证据。一次测量和
标记零丢失不构成通用 RTO/RPO 保证；未验证独立电源、机架、网络故障域、执行中
任务或共识复制。

## 使用 Prometheus 留存数值历史

认证接口 `/api/v1/metrics` 导出节点调度容量、资源用量、网络计数和可用的 CPU/GPU
读数。先在身份管理创建可撤销的 **viewer** API 令牌，存入仅 Prometheus 服务身份
可读的保护文件；配置控制器 CA 与证书包含的主机名。修改[抓取示例](../../examples/prometheus/xirang.yml)，
再执行 `promtool check config /etc/prometheus/prometheus.yml` 验证。

启动 Prometheus 时明确保留限制，例如
`--storage.tsdb.retention.time=30d --storage.tsdb.retention.size=5GB`，查询接口默认
只监听回环，需对外开放时单独设置认证。时间和容量先到者决定保留期，5GB 先满时
不能保证 30 天。另行安排备份和剩余空间监控；XIRANG 不自动安装或配置该服务。

可查询 `xirang_node_used_cpu_cores`、
`time() - xirang_node_observation_received_timestamp_seconds` 和
`xirang_node_observation_fresh == 0`，并在 Prometheus 选择时间范围查看历史。采样或
接收时间缺失、在未来、或超过 180 秒时不再导出用量与传感器序列；配置容量可能仍
存在。`up{job="xirang"} == 0` 表示抓取失败，包括认证或控制器存储故障，不表示用量为零。

抓取不补写错过的样本，不复制系统日志，也不证明传感器精度；来源时间戳可区分
重复读数和新采样。同机 Prometheus 不构成独立高可用。语法与保留选项见
[配置参考](https://prometheus.io/docs/prometheus/latest/configuration/configuration/)及
[存储说明](https://prometheus.io/docs/prometheus/latest/storage/)。

## 主机事件队列

Agent 状态目录中的 `health-outbox.db` 使用 0600 权限。除 16 MiB 事件内容上限，
还需为 bbolt 页和索引预留空间。节点健康来源会显示队列读写失败、日志游标缺口和
累计淘汰数。状态盘写满或无权限时不能安全前进游标；恢复容量/权限后允许 Agent
重试，不要通过清空数据库隐藏警告。源 journal/SEL 在采集前仍可能丢失，控制器
有界事件列表也不是完整归档。

两端升级后才能协商 `host-event-outbox-v1`；旧控制器不能确认新队列。回滚或备份
时保留数据库，复制文件前先停止 Agent。不得复制给另一节点或在节点 ID 变化后
复用。重新登记前应保留并明确退役旧数据库，自动删除会丢弃证据。

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

## 共享目录的宿主机访问

旧版导出辅助程序和安装器会将共享根目录改成 `xirang-task:xirang-task`、`0770`。
升级后不会再覆盖这些权限，但无法自动还原原属主、属组和 ACL。应在存储宿主机上
根据已有备份或管理员确定的策略恢复，再使用新版辅助程序补充集群权限：

```bash
sudo /usr/local/bin/xirang-agent mount-helper prepare share_default /srv/xirang/XirangShare
getfacl -p /srv/xirang/XirangShare
```

若站点明确允许所有本地用户列出并进入共享根目录，可先备份该目录的 ACL，再只
授予这两项权限：

```bash
umask 077
sudo getfacl -p /srv/xirang/XirangShare > shared-root.before.acl
sudo setfacl -n -m o::r-x /srv/xirang/XirangShare
```

自定义共享请换成实际源路径。该命令不开放所有用户写入，也不递归修改内部文件。
单独的用户 ACL 仍可能限制对应用户，应通过 `getfacl` 检查；需要写入的宿主机用户组
应按站点策略单独授权。ACL 掩码会共同影响具名用户和用户组，调整时应核对有效权限，
参见 [ACL 工具说明](https://man7.org/linux/man-pages/man1/setfacl.1.html)。不要通过
递归 chown/chmod 修复共享根目录。内部文件及 Web 创建的内容仍遵循各自权限和默认
ACL。已处于 active 状态的共享不会仅因升级而重新导出，恢复本地策略后应执行 `prepare`。

如需撤回这次 ACL 调整，可在其他元数据变更前执行
`sudo setfacl --restore=shared-root.before.acl`，再核对宿主机和集群访问。

## 回滚

停止所有控制节点，恢复由旧版本创建的恢复点，安装匹配的旧版原生包，再启动一个
控制节点。确认集群状态正确后，才能重新启动备用节点或继续升级 Agent。
