# 使用 XIRANG

[English](USAGE.md) | [简体中文](USAGE.zh-CN.md)

适用于 XIRANG **1.2.0**。

## 快速开始

1. 打开 `https://控制节点IP:8899`，创建首位管理员。
2. 检查或创建用户组，设置配额、最长运行时间和新用户默认组。
3. 在“节点接入”中接入 Agent，再到“节点状态”分配管理、计算或存储角色；角色可
   重叠。管理角色会在该节点 `8899` 端口启用面板，无管理角色节点不提供网页服务。
4. 在“共享存储”检查默认共享空间、目标节点、文件系统容量和权限。
5. 载入管理员发布的模板或编写 `XIRANG 1` 脚本，验证后在“作业调度”中提交。
6. 在相应工作区查看排队原因、最新日志、告警、节点资源和个人用量核算。

右上角统一管理刷新间隔、语言和深浅色模式。2–60 秒间隔同时控制浏览器刷新和与
Agent 协商的遥测频率；拓扑与告警页显示最新状态，不提供面向用户的回溯历史。

## 首次登录

打开 `https://控制节点IP:8899`。首次进入时创建管理员账户；后续新用户注册
需要管理员审批。普通用户只能查看节点状态，并管理自己提交的任务。

管理员可在“身份管理”中为之后注册的新账户指定默认用户组，也可以不设置。
修改该策略不会重写已有账户的组关系。XIRANG 账户属于控制平面，不会在集群
节点上创建 Linux 用户。

## 集群设置

管理员可从独立的“集群设置”页面修改集群显示名称和 Logo。名称为 1–64 个字符；
Logo 支持最大 512 KiB、2048 × 2048 像素的 PNG/JPEG，可先预览再保存，也可恢复
默认名称和 Logo。保存后当前侧栏、登录页和浏览器标题更新，其他已打开页面在
重新获得焦点或一分钟内更新。更改属于集群公共展示信息，未登录用户也可看到。
名称不会更改节点身份、证书或协议标识。

配置与控制器数据库一起保存、备份和恢复，并随外部 PostgreSQL 状态同步。
同时编辑时，过期版本的保存会被拒绝；重新载入后再修改。普通用户没有设置入口，
后端也禁止其修改。

## 系统健康与 BMC

管理员可在“系统健康”页面选择节点，检查数据源状态、BMC 传感器、系统错误、
硬件已纠正事件、恢复观测和人工维护记录。Agent 每 60 秒独立采集，旧 Agent 未
协商 `host-health-v1` 时不会上报。失败、缺失、过期和范围受限均明确显示。

- Linux journal 默认自动接入：回看 24 小时，每次最多读取 64 条优先级 0–4
  记录及 64 条纠错/恢复关键词匹配。需要 `journalctl` 和系统 journal 读取权限。
  不包含 Windows Event Log、OpenRC syslog、完整 rasdaemon 数据库或无损历史归档。
- 本地有 IPMI 设备且已安装 `ipmitool` 时自动探测，也可通过 Agent 环境文件
  配置 `XIRANG_IPMI_TOOL`。读取最多 256 个传感器，保留单位、状态、临界阈值和
  原始数值精度；不把小数位数等同于硬件测量精度，不合成缺失值。
- SEL 每次请求最近 128 条，并报告日志已满或溢出。列表查询在返回记录前超时
  时，使用有时限的直接查询尝试读取最新一条标准事件，明确标为部分覆盖、较早
  记录缺失，不清空日志。BMC 时钟/时区没有独立校准，原始日期和时间保留在
  消息中，排序采用接收时间。节点断电或 Agent 离线时本通道不能继续采集。
- 每节点保留最多 2000 条事件，最长为首次接收后 30 天。重复来源记录去重；窗口
  外、高流量及较长断线期间的事件可能遗漏。自动分类仅作线索，应核对原始消息。
- “硬件已纠正”和“恢复观测”均不证明根因已修复。管理员可追加带操作者和时间的
  修复记录，并关联原始事件；原始事件不被覆盖。

远程 BMC 需要在 Agent 环境文件配置数值 IP 地址 `XIRANG_BMC_HOST`、用户名
`XIRANG_BMC_USERNAME`、绝对路径 `XIRANG_BMC_PASSWORD_FILE`，以及明确的工具路径。
密码文件须为 Agent 用户拥有的普通文件且权限为 0600。使用 IPMI v2 `lanplus` 和
密码套件 17；密码不进入网页或命令行参数。配置示例见
[Agent 环境文件](../../deploy/config/xirang-agent.env.example)。已有 Redfish 功能仍为
指定 HTTPS Thermal 端点的 CPU 温度后备读取，不代表完整 Redfish 管理。

定位灯需要主机管理员显式设置 `XIRANG_BMC_ALLOW_IDENTIFY=true`。网页管理员可
请求亮起 120 秒或关闭；命令有过期时间、重放记录和审计，界面区分排队、确认、
失败与回执未知。BMC 确认不代替现场确认灯状态。不开放电源切换、重启、风扇控制、
阈值写入、原始 IPMI 命令或清除 SEL；监测不会自动修复或改变硬件状态。

## 接入节点

管理员可以在节点页面：

1. 输入待接入节点 IP。
2. 扫描该 IP 的 Agent。
3. 核对 Agent 返回的主机名、架构和身份指纹。
4. 输入 Agent 安装程序打印的一次性配对码（也可从 `xirang-agent` 服务日志
   查看），然后批准接入。
5. 按需为节点启用管理、计算和存储角色。这三个角色可以同时启用。

也可以在被控节点安装时使用控制节点 IP 和入群 Token 直接接入。

## 配置共享空间

管理员在“共享存储”页面创建共享空间，选择存储节点上的源路径和计算节点使用的
统一挂载路径，然后把它分配给：

- 全部节点；
- 一个节点分组；
- 指定节点。

“全部节点”和“节点分组”会随着节点或组成员变化自动重新同步。页面会分别显示
共享目录占用量和源文件系统容量；提交任务前，应确认目标节点使用相同挂载路径，
并且任务身份具有所需的读写权限。

共享空间的导出与挂载目前要求每个参与节点配置管理地址和 SSH 用户。应把控制节点
的 SSH 公钥授权给该账户，并把免密 sudo 严格限制为
`/usr/local/bin/xirang-agent mount-helper`，不能开放通用免密 shell。这条管理通道
与使用 Agent mTLS 的网页终端相互独立；如果管理网络并非可信网络，应预先写入并
核对 SSH 主机密钥。

## 提交任务

任务使用 `XIRANG 1` 文本脚本。最小示例：

```text
XIRANG 1

project = demo
queue = default
priority = 10
attempts = 1
artifact = https://controller.example:8899/artifacts/amd64/worker
sha256 = REPLACE_WITH_64_HEX_DIGEST
architecture = amd64
entrypoint = worker

cpu = 2
memory = 2G
timeout = 30m

arg = --input
arg = /shared/demo/input.dat
```

`artifact` 指向可执行文件，`sha256` 用于验证下载内容，资源字段用于选择合适
节点并限制任务占用。`cpu` 必须填写正整数，表示任务需要的逻辑 CPU 核心数；
内存使用 `M`、`G` 或 `T`，例如 `512M`、`4G`。任务脚本不允许声明存储需求；
存储配额由管理员按用户组设置，数值 `0` 表示无限制。不要在脚本中写入密码或
访问 Token。

`timeout` 支持 `12h`、`7d`、`30d` 等写法。未声明时，任务自动继承所属用户组
的最长运行时间。新建和历史用户组默认均为 7 天，管理员可提高组上限，管理员
账户自身不受限制。超过用户组上限的任务会在进入队列前被拒绝。如果任务的单
节点 CPU、内存、架构或设备需求超过全部已纳管计算节点，系统也会立即给出错误；
仅因资源当前被占用而暂时不足的任务仍会正常排队。

任务数组最多展开 1000 个独立任务，并为每个任务提供
`XIRANG_ARRAY_INDEX`、`XIRANG_ARRAY_SIZE` 和 `XIRANG_ARRAY_ID`：

```text
array = 0-31
```

对于从文件读取参数的批量任务，在共享空间清单中每行写一个任务项。XIRANG 会为
每行创建一个可独立观察、取消和核算的子任务，并使用节点实测 `ComputeWeight` 和
最大余数法，将子任务分配到最多 `batch_nodes` 个兼容节点：

```text
batch_space = share_default
batch_file = inputs/gromacs-tpr.txt
batch_arg = -s
batch_nodes = 4
```

每行内容会追加到基础参数之后；声明 `batch_arg` 时会先追加该参数。清单最大 1 MiB、
最多 1000 项，且不能与 `array` 同时使用。程序内部仍可自行循环，但只有拆成任务数组
或清单子任务，XIRANG 才能跨节点调度并分别核算。

依赖可以重复声明；默认条件是 `afterok`，`afterany` 表示等待前置任务以任意
结果结束：

```text
depends = job_abc123
depends = job_cleanup:afterany
```

当实际工作区占用超过管理员配额，或计算节点可用空间低于安全余量时，XIRANG
会发送严重告警并挂起进程，但保留 CPU、内存和设备预约。作业状态会显示具体
原因；条件恢复后 Agent 自动继续执行，管理员也可以直接取消。

可在网页 Jobs 页面打开、粘贴、校验并提交脚本，也可使用命令行：

```bash
xirangctl \
  -server=https://CONTROLLER_IP:8899 \
  -ca-file=/path/to/ca.crt \
  -token-file=~/.config/xirang/token \
  submit-file job.xirang
```

管理员发布的 Job 模板只是可编辑的脚本起点，载入模板不会自动提交任务。

### Job 模板

新控制节点会自动发布三个版本化起点：并行 CPU 圆周率计算、NVIDIA CUDA 向量
加法测试和 ARM64 节点验收。不同架构的可执行文件分别存放在
`/srv/xirang/XirangShare/.xirang/artifacts/v1`，各节点通过相同共享路径读取。
CUDA 模板申请一个 `nvidia.com/gpu` 设备，只有同时具备设备清单与执行能力的节点
才能运行。

管理员在“作业调度 → 模板管理”中维护模板。每个模板都是完整的 `XIRANG 1`
脚本，发布前必须通过同一个服务端解析器；名称唯一，修订号用于防止并发覆盖，
模板中不得写入凭据。用户点击“载入并编辑”后应检查所有字段，再按普通流程校验
并提交；选择模板本身不会启动任务。

### GROMACS 示例

GROMACS 使用普通构建和任务脚本流程。在原生构建页面分别创建 `gromacs` 的
`amd64-nvidia`、`amd64-amd`、`arm64-nvidia`、`amd64-cpu` 等变体，为每个变体选择
运行节点和共享输出路径。一个配方可以包含多个按顺序执行的直接 argv 步骤，例如
先配置 CMake、再执行 `cmake --build`；XIRANG 不会隐式调用 shell。然后编辑
[`examples/gromacs-md.xirang`](../../examples/gromacs-md.xirang)：

```text
application = gromacs
cpu = 8
memory = 16G
env.OMP_NUM_THREADS = 8
arg = mdrun
arg = -s
arg = /shared/gromacs/project/topol.tpr
arg = -deffnm
arg = /shared/gromacs/project/run
```

提交时，XIRANG 会把每个候选节点映射的已签名构建变体固化到作业快照中，分派时
自动使用该节点对应的可执行文件。管理员可以重新映射已完成变体；变更只影响新提交
作业，不会修改已经排队的作业。应用构建输出和 SBOM 只保留在所选共享输出目录。
参数扫描可使用 [`examples/gromacs-batch.xirang`](../../examples/gromacs-batch.xirang)，
并在清单中每行填写一个 `.tpr` 路径。

### 多节点与 MPI 分配

增加三个可选字段即可申请 Gang 分配：

```text
nodes = 2
tasks_per_node = 2
launcher = native
```

CPU、内存和 GPU 均按每个节点计算。XIRANG 会先一次性预留全部节点，所有
Agent 完成准备后再统一放行；任一成员准备失败都会取消整组分配。
`native` 会为每个 rank 启动同一份已校验编译产物；`openmpi`、`mpich` 和
`intelmpi` 仅在所有候选 Agent 都报告通过自检的 XIRANG 运行时适配能力时
才会被调度，仅安装 `mpirun` 不会被误判为具备该能力。

任务可读取 `XIRANG_RANK`、`XIRANG_LOCAL_RANK`、`XIRANG_WORLD_SIZE`、
`XIRANG_HOSTFILE`、`XIRANG_NETWORK_INTERFACE` 和
`XIRANG_PRIMARY_ADDRESS`。rank 通过已鉴权的 Agent 控制通道分发，不需要
广播全连接 SSH 私钥。

完整脚本见
[`examples/multinode-native.xirang`](../../examples/multinode-native.xirang)。

### 多架构原生构建

构建工作节点就是已纳管的普通 Agent。只有管理员配置专用 `xirang-build` 用户、
委托 cgroup v2 根并显式启用 `-build-enabled` 后才会发布构建能力。控制器严格按
配方声明的架构和工具链选择在线节点，不模拟处理器架构。原生执行还要求启用 Landlock ABI 3 或更高版本（Linux 6.2+），不支持时拒绝执行。工具链及依赖需安装在标准系统目录（`/usr`、`/bin`、`/sbin`、`/lib`、`/lib64`）。构建命令可以读取这些目录、`/etc`、`/proc` 和 CPU 信息，只能写入本任务工作区及基本空设备、随机设备；临时文件也保存在工作区中。即使 NFS 使用 `all_squash`，构建进程也不能读取其他任务的共享暂存区。参见 [Linux Landlock 文档](https://docs.kernel.org/userspace-api/landlock.html)。

同一主机上使用相同 UID 的原生构建会串行执行，互斥覆盖 Controller 与 Agent 的源码暂存、产物收集和工作区清理。升级时需重新运行安装程序，创建 `/var/lib/xirang-build/locks` 并更新服务写权限。清理失败或服务进程崩溃会持久隔离该 UID。管理员必须先停止两个服务、终止该构建 UID 的全部进程并删除残留私有工作区，才能清空对应的 `<uid>.lock` 文件并重启服务。自定义构建 UID 需在相同目录预建 root 所有、`xirang` 组、权限 `0660` 的锁文件。不要向构建身份授予锁目录或锁文件的写权限。

默认目录统一位于 `/srv/xirang/XirangShare/.xirang/builds`。控制器先自动计算并
记录所选源码树的确定性 SHA-256，再把固定后的源码复制到共享工作区；Agent 随后
复制到 `/var/lib/xirang-build` 私有目录并
核对源码 SHA-256，以非特权构建身份执行，最后只原子发布声明的输出。控制器在
签名和生成 SPDX SBOM 前再次核对 Agent 回报的输出 SHA-256。NFS 可以并应继续
使用 `all_squash`，不要为了构建功能开启 `no_root_squash`。

原生构建页面会显示每个在线 worker 上报的工具链版本、可执行文件和目标架构
矩阵。只有矩阵明确声明支持目标架构时，系统才允许交叉编译。管理员可以取消
活动构建、删除已完成记录，并按记录数和保存天数设置自动清理策略；清理构建
管理器自己的文件时，不会删除用户明确发布到共享空间中的输出。
启用“永久保留成功构建”后，自动清理会跳过成功构建的记录、签名信息、SBOM 引用
和应用节点映射；失败与取消记录仍受记录数和保存天数限制。管理员仍可通过明确的
删除操作移除某一成功构建。
应用构建还需声明变体、运行入口和运行节点映射。多个硬件专用变体可以使用同一个
应用名，调度器会为最终选中的节点解析正确的已签名输出。
只有管理员可以发布全局应用映射或修改运行节点。普通构建用户仍可提交不带应用名的
私有构建。从没有映射批准标记的版本升级后，管理员须检查已有应用构建并重新保存
节点映射；未经批准的历史映射不会参与新作业的应用解析。

用户属于多个用户组时，XIRANG 会对 CPU、内存、加速器、存储、并发数和运行
时间采用最严格的非零限额。数值 0 仍表示不限，但不能覆盖另一个组的有限配额。
任一所属组被停用都会阻止新任务提交；管理员还可以选择同时挂起排队任务，并
撤销组成员的登录会话和活动终端。

Open MPI 与 MPICH 使用随软件安装的 `xirang-mpi-adapter-*` 安全 shim。只有
`XIRANG_MPI_OPENMPI_BACKEND` 或 `XIRANG_MPI_MPICH_BACKEND` 指定的绝对路径后端
通过精确的 `xirang-mpi-backend-v1` 自检，Agent 才会发布相应能力；shim 还会在
分派前校验私有 allocation manifest。仅安装 `mpirun` 不会被视为已认证的分布式
MPI 环境。

## 用量核算

“用量核算”工作区向普通用户显示本人实测 CPU 时间、内存 GiB 时、GPU 卡时、
工作区活动、日期热点图、趋势和对应费用。管理员可以选择任意用户，并发布带
生效时间的新计费标准。计费版本一经创建不可修改，因此后续调价不会改写之前
周期。控制器使用 Agent 累计计数和事务数据库中的每日聚合进行核算，不使用
用户申报的资源数冒充实际账单。

命令行也可读取和管理同一套数据：

```bash
xirangctl usage --from 2026-07-01 --to 2026-07-31
xirangctl usage-users --from 2026-07-01 --to 2026-07-31
xirangctl billing-rates
xirangctl billing-rate-create --currency CNY --cpu 0.25 --memory 0.02 --gpu 2.5
```

## 查看任务

普通用户可以查看自己的排队、运行和已完成任务，也可以取消自己的任务。管理员
可以查看整个集群的队列、配额使用和调度情况。

## 管理节点

只有管理员可以更改节点角色、分组、计算优先级、调度约束、证书状态和共享空间
挂载，也只有管理员可以使用网页虚拟终端。普通用户不会看到这些操作入口。

网页虚拟终端是刻意限制为非 root、不可使用 sudo 的运维 shell。主机级变更应
通过带审计的 XIRANG 操作完成；紧急主机管理仍使用站点独立的操作系统登录通道。

## 调整刷新频率

页面右上角可以设置 `2s` 到 `60s` 的统一刷新间隔。该设置同时控制网页更新和
Agent 协商后的汇报频率。更短的间隔会提高状态实时性，也会增加控制节点和网络
开销。

## 备份

管理员应定期创建并验证备份，并将副本保存到独立存储。恢复、升级与回滚步骤见
[运维指南](OPERATIONS.zh-CN.md)。
