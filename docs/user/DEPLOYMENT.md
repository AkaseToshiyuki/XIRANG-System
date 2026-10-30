# Deploying XIRANG

[English](DEPLOYMENT.md) | [简体中文](DEPLOYMENT.zh-CN.md)

Applies to XIRANG **1.2.0**.

This guide installs a prebuilt native release. A container runtime is not
required.

## Supported hosts

- Linux on AMD64 or ARM64
- systemd or OpenRC
- apt, dnf, yum, zypper, pacman, apk, xbps, or Portage
- TCP `8899` for the Web console and management API
- TCP `8444` for controller-to-Agent communication
- TCP `8844` temporarily when an Agent waits for Web approval

Limit these ports to trusted cluster networks with the host firewall.

## Guided installation

```bash
git clone https://github.com/AkaseToshiyuki/XIRANG-System.git
cd XIRANG
./install.sh
```

The installer:

1. checks the Linux architecture and service manager;
2. installs missing host tools with the detected package manager;
3. requests system privileges through the standard `sudo` prompt;
4. downloads the matching GitHub Release package;
5. verifies the release `SHA256SUMS` signature, then the package against it;
6. enables an existing NTP client or installs Chrony and confirms synchronization;
7. installs services and starts the controller/node setup wizard.

The script never reads, stores, or uploads a sudo password. Only the local
`sudo` program handles it.

Reliable wall-clock synchronization is required for mTLS certificate validity,
OIDC sessions, telemetry ordering, and accounting. Ordinary NTP is sufficient;
PTP is not required. In an intentionally offline staging environment, use
`--allow-unsynchronized-clock` to continue with a warning, then synchronize the
host before enrollment. `--skip-dependencies` never silently disables this check.

## Select a release or local package

Install a specific release:

```bash
./install.sh --version 1.2.0
```

Install a downloaded package. The installer reads `SHA256SUMS` and
`SHA256SUMS.sig` from the same directory:

```bash
./install.sh --archive ~/Downloads/xirang-1.2.0-linux-amd64.tar.gz
```

If no checksum file is present, supply a trusted SHA-256 value:

```bash
./install.sh \
  --archive ~/Downloads/xirang-1.2.0-linux-amd64.tar.gz \
  --sha256 EXPECTED_SHA256
```

## How a release is trusted

Every release publishes `SHA256SUMS.sig`, an Ed25519 signature over the checksum
manifest. The installer verifies it against a public key embedded in
`install.sh` before it reads a single checksum, and refuses to continue when the
signature is absent or does not match.

The checksum alone cannot establish trust, because the package and the manifest
arrive over the same channel from the same GitHub Release: whoever can replace
one can replace the other. The signature is what makes a substituted release
detectable, because producing it requires the private key, which never leaves
the release workflow.

Verifying requires OpenSSL 1.1.1 or newer, which the installer adds to the host
tools it installs.

Passing `--sha256` establishes the same trust out of band — you assert a digest
you already have — and therefore replaces the signature check. Use it for an
air-gapped install, or to install a release published before signing existed.

## Initialize a controller

Choose `controller` in the wizard and enter an IP address reachable from the
managed nodes. The installer starts the controller, enrolls its local Agent, and
prints the Web address.

Open `https://CONTROLLER_IP:8899` and create the first administrator account.
The emergency bootstrap token is stored at `/var/lib/xirang/admin.token` with
restricted file permissions. Use Web accounts and scoped API tokens for normal
operation.

## Initialize a managed node

Choose `node`, then one enrollment method:

- `direct` — enter the controller IP and a cluster join token.
- `wait` — the Agent listens on `8844`; enter the node IP on the controller's
  Nodes page, check its identity, enter the one-time pairing code printed by the
  installer, and approve it. If the terminal output is no longer available, read
  the code from the `xirang-agent` service log on that node.

After enrollment, the Agent reports hardware and interfaces and probes available
paths to known cluster nodes.

## Accounts and service identities

XIRANG Web accounts are virtual control-plane identities. Registering or approving
a user does not create a Linux login account on any node. The administrator may
select a default user group for future registrations in Identity management, or
leave the default unset.

The native package creates only non-login service identities. `xirang-operator`
runs administrator Web terminals without root or sudo access. Native assignments
receive temporary numeric UIDs from `200000-265535` by default; these identities
exist only while work is active and are not written to `/etc/passwd`. The range can
be changed with `--executor-task-uid-min` and `--executor-task-uid-max`, but it must
not overlap local, LDAP, or NFS identities.

The Agent itself remains a privileged, structured broker because it must manage
cgroups, devices, mounts, and hardware telemetry. It does not pass that privilege
to Web terminals or workload executables.

## Shared-path service permissions

Exporting an existing directory preserves its host owner, owning group, and
local users' effective access. XIRANG adds a POSIX access ACL for the task group
instead of taking ownership or forcing mode `0770`. Existing masked ACL entries
keep their effective permissions; default ACLs and descendant files are unchanged.
New export roots are host-readable/traversable (`0755` plus the cluster ACL).
The filesystem must support POSIX ACLs; failure is reported without a chmod
fallback. NFS continues to use `all_squash` with the task identity.

Installation uses the same permission-preserving preparation for the default
share. Existing mount-parent permissions are also preserved. Administrators must
provide traversal through any private parent directories. Directory access does
not override an individual file's permissions or grant every local user write
access. See [recovery of older shared directories](OPERATIONS.md#shared-directory-host-access)
for directories whose original metadata was already overwritten by an older version.

On systemd hosts, XIRANG grants services write access only to exact shared paths,
not all of `/srv`, `/shared`, or `/mnt`. The privileged mount helper maintains
the allowlist in `/etc/xirang/shared-write-paths.d` and generates service
drop-ins when a shared space is mounted or removed. Export-source operations
remain confined to the privileged helper; tagged `/etc/fstab` mounts are
imported during an upgrade.

The default `/srv/xirang/XirangShare` path is authorized before first startup.
After changing another shared path, restart the affected service during a
maintenance window so its systemd mount namespace receives the new policy.
Drain running jobs before restarting an Agent.

Shared mount/export control requires matching upgraded endpoints and the explicit
Agent flag `-shared-mount-control` (root/systemd only, disabled by default). It uses
the mTLS stream and a fixed, time-bounded host service, outside the Agent's private
mount namespace. Keep existing environment flags when enabling it. No automatic
SSH fallback is available. Existing mounts remain when the capability is disabled.

File data and capacity reads still use the controller's SSH key and configured
management account. New interactive installations enumerate only the `stats`,
`file-list`, `file-read`, `file-write`, `file-mkdir`, `file-rename` and `file-delete`
helper actions in sudoers. Migrate old `mount-helper *` rules explicitly; validate
with `visudo -cf` and preload verified host keys. This account remains privileged
and trusted; API ownership checks are not a direct NFS/SSH tenant boundary.
See [shared storage](USAGE.md#configure-shared-storage) for rollout and failure
semantics. Removing the new flag is necessary before an Agent binary rollback;
file-only sudo rules intentionally do not restore legacy SSH mount mutation.

## Check services

systemd:

```bash
sudo systemctl status xirangd
sudo systemctl status xirang-agent
sudo journalctl -u xirangd -u xirang-agent --since today
```

OpenRC:

```bash
sudo rc-service xirangd status
sudo rc-service xirang-agent status
```

The initialized controller runs `xirangd` and `xirang-agent`. Managed nodes start
only `xirang-agent`, so they expose no Web service by default. Assigning the
management role later enables an Agent-owned panel relay on that node's port
`8899`; removing the role stops it.

## Troubleshooting

- **Release download fails** — verify access to GitHub Releases and retry.
- **No matching asset** — the Release must contain
  `xirang-<version>-linux-<amd64|arm64>.tar.gz` and `SHA256SUMS`.
- **Web console is unreachable** — check `xirangd`, the firewall, and the
  controller IP entered during setup.
- **Enrollment fails** — check ports `8444`/`8844`, host time, and the displayed
  certificate fingerprint.
- **Clock synchronization fails** — verify DNS and UDP `123` access to the NTP
  source, then inspect `timedatectl status` or `chronyc tracking`. Use the bypass
  only for offline staging, never as a permanent cluster configuration.
- **GPU data is absent** — install the vendor driver and management utility,
  such as `nvidia-smi`. XIRANG does not install or replace GPU kernel drivers.
