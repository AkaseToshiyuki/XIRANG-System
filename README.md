<p align="center">
  <img src="web/public/brand/xirang-mark.png" alt="XIRANG logo" width="180">
</p>

<h1 align="center">XIRANG</h1>

<p align="center">
  <strong>eXtensible Infrastructure for Resource-aware Adaptive Networked Grids</strong>
</p>

<p align="center">
  Native orchestration and scheduling for heterogeneous Linux clusters.
</p>

<p align="center">
  <strong>English</strong> · <a href="README.zh-CN.md">简体中文</a>
</p>

XIRANG brings Linux servers with different processors, accelerators, storage,
and network connections into one manageable cluster. It provides a single Web
console for enrolling nodes, observing health and topology, sharing storage,
and submitting native computing jobs.

Current stable version: **1.3.0**. See the [release notes](docs/user/RELEASE_NOTES.md).

**Reference:** Haochen Yang. *XIRANG: Native Build-to-Placement Contracts for Heterogeneous Linux Clusters*. Preprint, 2026.

## Highlights

- **Easy cluster growth** — install an Agent, approve it in the Web console, and
  let XIRANG discover the available paths between cluster nodes.
- **Clear operational view** — inspect node health, utilization, temperatures,
  network links, link speeds, and fallback paths from one place.
- **Resource-aware scheduling** — queue jobs by priority while considering node
  capacity, user-group quotas and runtime limits, requested hardware, and
  administrator-defined preferences; impossible per-node requests fail early
  instead of waiting forever.
- **Usage and cost accounting** — review per-user measured CPU, memory, GPU,
  and workspace activity with effective-dated administrator billing rates.
- **Native execution** — run verified Linux workloads directly, without
  requiring a container runtime between the job and the hardware.
- **Shared workspace management** — assign a storage path to the whole cluster,
  one node group, or an explicit set of nodes.
- **Multi-user control** — first-run administrator setup, registration approval,
  role-based access, personal job ownership, quotas, and administrator-only
  node controls.
- **Heterogeneous by design** — prebuilt XIRANG packages support Linux on AMD64
  and ARM64, while jobs can target compatible node architectures and devices.
- **Build on the target architecture** — approved Agents can compile the same
  checksum-pinned source on AMD64 or ARM64 and publish signed shared build outputs.

## Install

Clone the repository and run the guided installer:

```bash
git clone https://github.com/AkaseToshiyuki/XIRANG-System.git
cd XIRANG-System
./install.sh
```

The installer detects the Linux distribution and CPU architecture, installs
required host tools, downloads and verifies the matching release package, and
then asks whether this machine will be a controller or a managed node. It asks
for administrator privileges through the normal `sudo` prompt only when they
are needed.

You can also install an already downloaded package:

```bash
./install.sh --archive /path/to/xirang-1.3.0-linux-amd64.tar.gz
```

See the [deployment guide](docs/user/DEPLOYMENT.md) for supported distributions,
ports, unattended options, and troubleshooting.

## Use

After controller initialization, open:

```text
https://CONTROLLER_IP:8899
```

Create the first administrator, enroll nodes, configure roles and shared
storage, and submit jobs using a readable `XIRANG 1` job script. The
[user guide](docs/user/USAGE.md) walks through the complete workflow.

The user guide also covers Job templates, GROMACS, multi-node work, native builds,
and accounting. Backup, recovery, and upgrades are in the
[operations guide](docs/user/OPERATIONS.md).

## Build from source

Developers can build and test the project with:

```bash
make verify
make web-install web-build
make package-native VERSION=1.3.0
```

Public bilingual manuals are available under [`docs/user`](docs/user).

## License

XIRANG is licensed under the [GNU General Public License version 3](LICENSE),
`GPL-3.0-only`. Modification and redistribution are permitted, including
commercial redistribution. Distributed derivative works must remain under GPLv3
and provide corresponding source. See
[third-party notices](THIRD_PARTY_NOTICES.md) for dependency licenses.
