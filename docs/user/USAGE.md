# Using XIRANG

[English](USAGE.md) | [简体中文](USAGE.zh-CN.md)

Applies to XIRANG **1.2.0**.

## Quick start

1. Open `https://CONTROLLER_IP:8899` and create the first administrator.
2. Create or review user groups, quotas, maximum runtime, and the default group.
3. Enroll Agents from **Node access**, then assign management, compute, or storage
   roles under **Node status**. Roles may overlap. A management role enables that
   node's panel on port `8899`; a node without it exposes no Web service.
4. Confirm the default shared space, its target nodes, filesystem capacity, and
   permissions under **Shared storage**.
5. Load an administrator-published template or write a `XIRANG 1` script, validate
   it, and submit it under **Job scheduling**.
6. Follow queue reasons, current logs, alerts, per-node resources, and personal
   accounting from the corresponding workspaces.

The refresh interval, language, and light/dark theme are global controls in the
upper-right corner. The 2–60 second interval controls both browser refresh and the
negotiated Agent telemetry cadence; topology and alert pages show the latest state,
not a user-facing replay history.

## First sign-in

Open `https://CONTROLLER_IP:8899`. The first visit creates the administrator
account. Later user registrations require administrator approval. Regular users
can inspect node status and manage only their own jobs.

In Identity management, an administrator can choose a default user group for
future registrations or leave it unset. Changing the setting never rewrites
existing memberships. XIRANG accounts are control-plane identities and do not
create Linux users on cluster nodes.

## Keyboard and language controls

Use Tab and Shift+Tab to move between controls. The first link after sign-in
skips navigation and focuses the main content. In the topology graph, Enter or
Space selects the focused node or link. The folder picker keeps focus inside the
modal; Escape, Cancel or the close button returns to its opener. Entering a
subfolder focuses Parent, and returning to the root focuses the storage selector.
Changing language preserves the current page, form draft and terminal connection.

## Cluster settings

Administrators can edit the cluster display name and logo in the separate **Cluster
settings** page. Names contain 1–64 characters. Upload a PNG/JPEG up to 512 KiB and
2048 × 2048 pixels, preview it, save, or restore the defaults. The current sidebar,
sign-in page and browser title update after saving; other open pages update on
focus or within one minute. This presentation data is visible before sign-in. It
does not rename node identities, certificates or protocol identifiers.

Settings are saved and backed up with the controller database and synchronized
through external PostgreSQL state. Stale concurrent updates are rejected; reload
before editing again. Regular users have neither the settings entry nor write
permission on its API.

## Alert and notification policy

Administrators can save trigger/recovery durations, notification filters, a
silence end time, and a delivery objective in **Cluster settings**. Durations
apply to interface alerts; job and storage protection remains immediate. Saved
policy survives restart and takes effect on subsequent evaluations/deliveries.
If another administrator changed it, reload before saving again.

Routing uses the webhook configured by the host administrator. Enabling the
route does not create a destination. Silence (up to 30 days), disabled routing,
severity and type filters affect notifications only. Suppressed records retain
their reason, cannot be manually retried and are not replayed when silence ends.
Clear the silence end time to resume notifications for new transitions.

The alert center reports delivery objectives over its latest **up to 200**
records, from event creation through successful HTTP completion. It separately
shows excluded suppressed, not-yet-due and older unmeasured records. This bounded
view is not a complete long-term SLO. A successful HTTP response does not prove
that a person received or read the message. Use arrow keys, Home and End to
switch alert tabs; alert title buttons open details using the keyboard.

## System health and BMC

Administrators can select a node in **System health** to inspect source status,
BMC sensors, system errors, hardware-corrected events, recovery observations and
operator maintenance records. The Agent collects independently every 60 seconds.
Older Agents without negotiated `host-health-v1` do not report this data. Missing,
failed, stale and partial observations are explicit.

- Linux journal collection starts within the last 24 hours and then resumes from
  persistent cursors, reading up to 64 oldest new priority 0–4 entries and 64
  correction/recovery keyword matches per collection. Missing cursors are reported
  as coverage gaps; source retention and event rate still limit coverage.
  It requires `journalctl` and access to the system journal. Windows Event Log,
  OpenRC syslog, the full rasdaemon database and lossless history are not included.
- An installed `ipmitool` and local IPMI device enable automatic detection;
  `XIRANG_IPMI_TOOL` can explicitly configure the Agent tool. Up to 256 sensor
  readings retain their units, states, critical thresholds and reported numerical
  precision. Decimal places do not establish measurement accuracy; unavailable
  readings are not fabricated.
- SEL queries request the latest 128 entries and report full/overflowed logs. If
  listing times out before returning records, a bounded direct query attempts to
  read the latest standard event; coverage is then marked partial, with older
  records missing. No log is cleared. BMC clock/timezone is not independently
  calibrated: original date/time remains in the message and receipt time is used
  for ordering. This channel cannot collect while the host or Agent is down.
- Each node retains up to 2,000 events for 30 days from first receipt. Source records
  are deduplicated. Older events, bursts and long outages can leave gaps. Automatic
  classification is a clue; inspect the original message.
- Neither a hardware correction nor a recovery observation proves root-cause
  repair. Administrators can append maintenance records with actor/time and an
  optional link to an original event. Original events remain intact.

Remote BMC access requires a numeric `XIRANG_BMC_HOST` address, `XIRANG_BMC_USERNAME`,
an absolute `XIRANG_BMC_PASSWORD_FILE`, and an explicit tool path in the Agent
environment. The password file must be a regular file owned by the Agent user
with mode 0600. Connections use IPMI v2 `lanplus` and cipher suite 17; the password
does not enter the Web console or command-line arguments. See the
[Agent environment example](../../deploy/config/xirang-agent.env.example).
Existing Redfish support remains a CPU-temperature fallback from a configured
HTTPS Thermal endpoint, rather than full Redfish management.

Locator control requires host opt-in with `XIRANG_BMC_ALLOW_IDENTIFY=true`. Console
administrators may request a 120-second light or turn it off. Commands have expiry,
replay records and auditing; the UI distinguishes queued, acknowledged, failed and
unknown outcomes. BMC acknowledgement does not replace physical LED verification.
Power changes, reboot, fan control, threshold writes, raw IPMI and SEL clearing are
not exposed. Monitoring does not repair or alter hardware automatically.

The Agent continues collection while its controller connection is unavailable.
Selected events are stored locally and, with negotiated `host-event-outbox-v1`,
removed only after the controller confirms its durable commit. Retries are
idempotent and do not replace current sensor snapshots. The queue is bounded by
4,096 events, 16 MiB of encoded event content and seven days since capture,
whichever is reached first; database overhead adds disk use. `event-outbox` reports
cumulative unacknowledged evictions and journal cursor/skipped-record gaps. Such
warnings persist across restart and do not prove an exact global loss count.
Controller retention remains 2,000 events per node and 30 days. A running Agent
is required; this is not lossless archival or independent backup. Older controllers
receive legacy snapshots while the pending queue stays bounded and delivery is
marked partial. See [operations](OPERATIONS.md#host-event-queue) for recovery.

## Enroll a node

An administrator can enroll a waiting Agent from the Nodes page:

1. Enter the node IP address.
2. Scan that IP for the Agent.
3. Check the reported hostname, architecture, and identity fingerprint.
4. Enter the one-time pairing code printed by the Agent installer (or shown in
   the `xirang-agent` service log), then approve enrollment.
5. Assign management, compute, and storage roles as needed. Roles can overlap.

A node can also join directly during installation with the controller IP and a
cluster join token.

## Configure shared storage

On the Storage page, choose a source path on a storage node and the common mount
path used by compute nodes. Assign it to all nodes, one node group, or an explicit
set of nodes. An all-node or group assignment is reconciled when membership changes.
Verify permissions and the displayed directory usage and source-filesystem capacity
before scheduling jobs that use the path.

Shared-space export and mount mutations use the Agent mTLS channel. Upgrade both
endpoints and explicitly append `-shared-mount-control` to the participating
systemd Agents' `XIRANG_AGENT_ARGS`; it is disabled by default and requires a
root-owned installed Agent running as root. Preserve existing flags. Restart only
after draining jobs. The source still needs a management address for NFS. A node
without the capability fails explicitly; mutations never fall back to SSH. OpenRC
hosts do not support this capability yet.

File browsing, transfers and capacity reads still require the configured SSH user
and controller key. Limit its sudo rule to `mount-helper stats`, `file-list`,
`file-read`, `file-write`, `file-mkdir`, `file-rename` and `file-delete`; prepopulate
verified host keys. Existing broad helper rules must be migrated separately.
This SSH account remains a trusted administrator. Web ownership checks isolate API
users, but do not establish per-user authentication for direct all-squash NFS or
privileged SSH access.

A command acceptance is not completion. A lost result or timeout reports an unknown
outcome; inspect node results and the audit log before retrying. Existing mounts
must match the requested NFS source. Unmount refuses missing or ambiguous XIRANG
fstab ownership. Interrupted operations can leave partial effects; no automatic
rollback removes data or unrelated mounts.

## Submit a job

Jobs use a readable `XIRANG 1` text file:

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

The artifact is downloaded and checked against `sha256`. Resource requests help
XIRANG select a compatible node and limit the workload. `cpu` is a positive
integer number of logical CPU cores. Memory uses `M`, `G`, or `T` (for example
`512M` or `4G`). Jobs cannot declare storage: administrators set user-group
storage quotas, and zero means unlimited. Never place passwords or access
tokens in a job file.

`timeout` accepts durations such as `12h`, `7d`, or `30d`. If it is omitted,
the job inherits the maximum runtime configured for the user's group. New and
legacy groups default to seven days; administrators can raise that limit, while
administrator accounts themselves are unlimited. A submission above its group
limit is rejected before it enters the queue. XIRANG also rejects a request
when its per-node CPU, memory, architecture, or device requirement exceeds
every enrolled compute node; temporary resource pressure still waits in queue.

An array creates up to 1,000 independent jobs and exposes
`XIRANG_ARRAY_INDEX`, `XIRANG_ARRAY_SIZE`, and `XIRANG_ARRAY_ID` to each task:

```text
array = 0-31
```

For file-driven parameter sweeps, keep one work item per non-empty line in a
shared-space manifest. XIRANG creates one observable child job per line and
apportions the children across up to `batch_nodes` compatible nodes using their
measured `ComputeWeight` (largest-remainder allocation):

```text
batch_space = share_default
batch_file = inputs/gromacs-tpr.txt
batch_arg = -s
batch_nodes = 4
```

Each manifest line is appended to the base arguments, after `batch_arg` when it
is present. The manifest is limited to 1 MiB and 1,000 items. Batch manifests
and `array` are mutually exclusive. Shell loops still work inside a program,
but XIRANG can distribute and account for work only when it is expressed as an
array or manifest children.

Dependencies can be repeated. `afterok` is the default; `afterany` waits for
any terminal result:

```text
depends = job_abc123
depends = job_cleanup:afterany
```

If actual workspace use exceeds the administrator quota, or a compute node
falls below its filesystem safety reserve, XIRANG sends a critical alert and
suspends the process without releasing its CPU, memory, or device reservation.
The status includes the exact cause. The Agent resumes it when the condition
recovers; administrators can still cancel it.

Open or paste the file on the Jobs page, validate it, and add it to the queue.
The same file can be submitted with:

```bash
xirangctl \
  -server=https://CONTROLLER_IP:8899 \
  -ca-file=/path/to/ca.crt \
  -token-file=~/.config/xirang/token \
  submit-file job.xirang
```

Administrator-published Job templates are editable starting points; loading one
does not submit it automatically.

### Job templates

New controllers publish three versioned starting points: a parallel CPU pi
calculation, an NVIDIA CUDA vector-add test, and an ARM64 node acceptance test.
Their architecture-specific executables are installed below
`/srv/xirang/XirangShare/.xirang/artifacts/v1` and are reached through the same
shared path on every node. The CUDA template requests one `nvidia.com/gpu` device
and is eligible only on a node that advertises both inventory and enforcement.

Administrators manage templates from **Jobs → Template management**. A template is
a complete `XIRANG 1` script and must pass the same server parser as a direct
submission. Names are unique, revisions prevent lost updates, and templates must
not contain credentials. Users select **Load and edit**, review every field, and
submit through the ordinary validation path; selecting a template never starts a
job automatically.

### GROMACS example

GROMACS uses the ordinary build and script paths. On the Builds page, create
separate `gromacs` variants such as `amd64-nvidia`, `amd64-amd`, `arm64-nvidia`,
and `amd64-cpu`. Select their runtime nodes and a shared output path. A recipe
may contain ordered direct-argv steps, for example CMake configuration followed
by `cmake --build`; XIRANG does not insert a shell. Then edit
[`examples/gromacs-md.xirang`](../../examples/gromacs-md.xirang):

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

At submission, XIRANG snapshots the signed variant mapped to each eligible node;
dispatch then uses that node's exact executable. An administrator may remap a
completed variant, which affects new submissions without mutating queued jobs.
All application outputs and SBOMs remain in the selected shared output directory.
For a sweep, use [`examples/gromacs-batch.xirang`](../../examples/gromacs-batch.xirang)
and a manifest containing one `.tpr` path per line.

### Multi-node and MPI allocations

Add three optional fields to request a gang allocation:

```text
nodes = 2
tasks_per_node = 2
launcher = native
```

Resources are per node. XIRANG reserves every node before starting any rank and
cancels the whole allocation if one member cannot prepare. `native` launches the
same verified artifact for every rank. `openmpi`, `mpich`, and `intelmpi` are
accepted only when every selected Agent reports a matching, self-tested XIRANG
runtime adapter. Installing `mpirun` alone does not advertise this capability.

The task receives `XIRANG_RANK`, `XIRANG_LOCAL_RANK`, `XIRANG_WORLD_SIZE`,
`XIRANG_HOSTFILE`, `XIRANG_NETWORK_INTERFACE`, and
`XIRANG_PRIMARY_ADDRESS`. Rank distribution uses the authenticated Agent
control channel; no cluster-wide SSH private-key distribution is required.

See [`examples/multinode-native.xirang`](../../examples/multinode-native.xirang) for
a complete editable script.

### Multi-architecture native builds

Build workers are ordinary enrolled Agents and are disabled unless an
administrator enables `-build-enabled` with a dedicated `xirang-build` user
and delegated cgroup v2 root. Native execution also requires enabled Landlock ABI 3 or newer (Linux 6.2+); it fails closed when unavailable. Toolchains and their dependencies must be installed in standard system paths (`/usr`, `/bin`, `/sbin`, `/lib`, `/lib64`). Build commands may read those paths, `/etc`, `/proc` and CPU information; they may write only their own workspace and basic null/random devices. Temporary files stay inside the workspace. Shared staging remains inaccessible even when NFS uses `all_squash`. See the [kernel Landlock documentation](https://docs.kernel.org/userspace-api/landlock.html). The controller chooses an online worker by the
recipe's exact architecture and toolchain; architecture is never emulated.

Native builds sharing a host UID run one at a time, including staging, artifact collection and workspace cleanup, across both Controller and Agent processes. Rerun the installer when upgrading to provision `/var/lib/xirang-build/locks` and the service write permissions. Cleanup failures or daemon crashes quarantine that UID persistently. Before clearing its `<uid>.lock` file, an administrator must stop both services, terminate all processes under that build UID and remove its residual private workspaces; then restart the services. Custom build UIDs require a root-owned, `xirang`-group lock file with mode `0660` in the same directory. Never give a build identity write access to the lock directory or its files.

The default roots live under
`/srv/xirang/XirangShare/.xirang/builds`. The controller computes and records a
deterministic SHA-256 for the selected source tree, then copies the pinned tree
into the shared work area. The selected Agent
copies it again into `/var/lib/xirang-build`, verifies the source SHA-256,
builds as the unprivileged build identity, and atomically publishes only the
declared output. The controller compares the Agent's output SHA-256 before it
signs the artifact and writes the SPDX SBOM. NFS may remain `all_squash`; do
not enable `no_root_squash` for this feature.

The Builds page shows the toolchain version, executable and supported target
architecture matrix reported by every ready worker. Cross-compilation is
eligible only when that matrix explicitly advertises the selected target.
Application builds also declare a variant, runtime entrypoint and runtime-node
mapping. Multiple hardware-specific variants may share one application name;
the scheduler resolves the correct signed output for the selected node.
Only administrators may publish global application mappings or change their
runtime nodes. Ordinary builders can still create private builds without an
application name. After upgrading from a version without routing approvals,
an administrator must review each existing application build and save its node
mapping again; unapproved legacy mappings are excluded from new job resolution.
Administrators can cancel active builds, delete completed records, and set a
record-count and age retention policy; cleanup removes build-manager artifacts
but not outputs deliberately published into a user shared space.
Enabling **Preserve successful builds permanently** excludes successful records,
signed artifact metadata, SBOM references, and application node routing from
automatic age/count cleanup. Failed and canceled records remain bounded by the
normal policy. An explicit administrator delete remains available.

When a user belongs to multiple groups, XIRANG applies the most restrictive
non-zero CPU, memory, accelerator, storage, concurrency and runtime limit.
Zero remains unlimited and cannot weaken a finite limit from another group.
Disabling any assigned group blocks new submissions. Administrators can also
choose to suspend queued jobs and terminate member login/terminal sessions as
part of the disable operation.

Open MPI and MPICH use the installed `xirang-mpi-adapter-*` safety shims. A
shim advertises capability only when the corresponding absolute backend from
`XIRANG_MPI_OPENMPI_BACKEND` or `XIRANG_MPI_MPICH_BACKEND` answers the exact
`xirang-mpi-backend-v1` probe. The shim validates the private allocation
manifest before dispatch. XIRANG does not treat the presence of `mpirun` as a
certified distributed MPI environment.

## Usage accounting

The Usage accounting workspace shows the signed-in user's measured CPU time,
sampled memory GiB-hours, allocated GPU device-hours, workspace activity, daily
heatmap, trend, and cost. Administrators can select any user and publish a new
effective-dated billing standard. Rate versions are immutable so later changes
do not rewrite an earlier period. The controller stores cumulative Agent
counters and daily aggregates in its transactional state database; it does not
derive invoices from user-declared resource values.

The same data is available to scripts and administrators:

```bash
xirangctl usage --from 2026-07-01 --to 2026-07-31
xirangctl usage-users --from 2026-07-01 --to 2026-07-31
xirangctl billing-rates
xirangctl billing-rate-create --currency CNY --cpu 0.25 --memory 0.02 --gpu 2.5
```

## View jobs

Regular users can view, follow, and cancel their own queued, running, and completed
jobs. Administrators can inspect cluster-wide queues, quota use, and scheduling
state.

## Manage nodes

Only administrators can change node roles, groups, compute priority, scheduling
constraints, certificates, shared mounts, or use the browser terminal.

Enabling the **management** role starts an Agent-owned HTTPS panel relay on port
`8899` of that node. The relay can reach only the controller recorded during
enrollment and uses the node's mTLS identity upstream. Removing the role stops
that relay. A node without the management role does not expose a Web service.
The original controller's `8899` listener remains owned by `xirangd`, so its
management role cannot be removed while that service is active.

The browser terminal is deliberately a non-root operational shell and cannot use
sudo. Host-level changes remain available through audited XIRANG actions; emergency
host administration must use the site's separate operating-system access path.

## Set the refresh interval

The top-right control accepts `2s` to `60s`. It controls both Web updates and the
negotiated Agent report interval. Shorter intervals increase control-plane and
network activity.

## Backups

Create and verify backups regularly, then keep copies on independent storage. See
the [operations guide](OPERATIONS.md) for recovery, upgrade, and rollback.
