# Operating XIRANG

[English](OPERATIONS.md) | [简体中文](OPERATIONS.zh-CN.md)

Applies to XIRANG **1.4.1**.

This runbook covers backups, recovery, upgrades, and rollback. Perform disruptive
steps during a maintenance window and keep recovery points on storage independent
from the controller.

## Backup and verification

Create, list, and verify an online recovery point:

```bash
xirangctl backup-create
xirangctl backups
xirangctl backup-verify BACKUP_ID
```

A recovery point contains transaction-consistent snapshots of the control,
telemetry, topology, flow, and alert databases plus a SHA-256 manifest. Copy the
completed directory away from the controller and apply a retention policy.

## Offline restore

Stop every controller that can access the state before restoring:

```bash
sudo systemctl stop xirangd xirangd-standby
sudo xirang-maint verify /var/lib/xirang/backups/BACKUP_ID
sudo xirang-maint restore /var/lib/xirang/backups/BACKUP_ID /var/lib/xirang
sudo systemctl start xirangd
```

The maintenance tool verifies hashes and bbolt structure, refuses to overwrite a
database that is still locked, and preserves replaced files in a rollback directory.
Verify `/readyz`, `/api/v1/version`, Agent reconnection, queue reservations, and a
read-only Web session after recovery.

## PostgreSQL active/passive deployments

Configure exactly one primary and one backup against the same TLS-protected
PostgreSQL DSN and namespace. Install the full controller/Web bundle on both,
assign their management responsibilities under **Node status**, and retain their
node IDs. Add these settings to the respective protected controller environment:

```bash
# Primary, in XIRANGD_ARGS (retain the existing listen/TLS/execution settings):
-controller-role=primary -controller-node-id=PRIMARY_NODE_ID \
-external-state-dsn=POSTGRESQL_URL -external-state-namespace=CLUSTER_NAMESPACE
# Backup:
-controller-role=backup -controller-node-id=BACKUP_NODE_ID \
-external-state-dsn=POSTGRESQL_URL -external-state-namespace=CLUSTER_NAMESPACE
```

Both services stay running. The passive controller waits without opening panel or
Agent ports. PostgreSQL expires an inactive leadership session after eight seconds;
the live process checks it every second and before HTTP/Agent messages. On primary
failure the backup acquires the same exclusive state lock and restores committed
state before listening. A returning primary requests handback; after 15 continuous
seconds (adjustable with `-controller-failback-delay`), the backup shuts down and
releases the lock, then returns to waiting. Never use a node heartbeat alone to
elect another controller.

Provide a stable TLS endpoint for both HTTPS `8899` and Agent gRPC `8444`, routed
only to the currently ready controller. Configure this endpoint at Agent enrollment;
a fixed address of the failed host cannot reach the backup by itself. The database
must remain reachable when either management host fails. XIRANG does not install
PostgreSQL, a VIP, load balancer, or replicate local directories automatically.
Do not use two independent local bbolt databases for cross-host takeover.

A namespace persists its two node assignments and trust digest. Duplicate roles,
a third node, mismatched trust material and unassigned controllers are rejected.
To replace a retired controller identity, stop both controllers, back up PostgreSQL,
and have the database administrator remove only that namespace's retired member
row from `xirang_controller_members`; restart with the new enrolled node ID. Never
edit these assignments while either controller is running. During migration, stop
legacy controllers before activating the pair; keep the original CA and import a
verified control-state image using `xirang-maint import-postgresql` as applicable.

Import an existing verified offline backup before starting either paired controller:

```bash
xirang-maint import-postgresql /path/to/verified-backup /etc/xirang/postgresql.dsn CLUSTER_NAMESPACE
```

The DSN file must be owner-only. Import checks the full backup and refuses an
existing namespace. Restore local observation databases and provision PKI separately.

Provision the same protected `pki/` CA/key material on each controller before
failover, with server certificates covering the stable client endpoint. Keep its
backup separate from application database snapshots. PostgreSQL replication does
not replicate CA keys or host-local credentials; a fresh independent CA would
break the trust established at Agent enrollment. Keep administrator authentication
and any configured signing/Webhook secrets consistent through an explicit secure
provisioning process. Verify failover with the original CA; do not disable TLS
verification to make the check pass.

XIRANG recovery points protect the application-level database image. Also take a
native PostgreSQL backup, store its checksum separately, and test its restoration
into a separate database. Stop both controllers before replacing an existing
namespace.

The management role does not start a relay. Only elected `xirangd` serves the panel;
passive nodes report installation state. Independent local observation databases,
SSH keys, artifact paths and site services still need consistent provisioning.

Run the automatic pair drill after building:

```bash
python3 scripts/controller_pair_drill.py --binary bin/xirangd
```

It creates owner-only disposable PostgreSQL and controller directories on loopback,
retains a JSON report and never touches live services. A suspended process may
retain a kernel listening socket until it resumes; readiness requests are fenced
by the database session, and the old process closes/reloads before serving again.
The report distinguishes listening sockets from ready services.

Maintainers with PostgreSQL 16 tools can run the isolated failover and recovery
drill without containers:

```bash
make ha-drill
```

The drill covers leadership exclusion, controller failure, database outage,
state continuity, corruption rejection, and dump/restore using the original CA.
It records monotonic timings to authenticated reads of 16 acknowledged markers in
`report.json`, with a binary digest and environment metadata. Use
`XIRANG_HA_KEEP=1 make ha-drill` to retain the temporary evidence directory;
`XIRANG_HA_WORK_DIR` must name a new, nonexistent directory. Existing evidence is
never overwritten. A single trial and zero missing markers are not general
RTO/RPO guarantees. The drill does not simulate independent power, rack, network
failure domains, in-flight jobs or consensus replication.

## Numerical history with Prometheus

The authenticated `/api/v1/metrics` endpoint exposes node scheduling capacity,
resource use, network counters and available CPU/GPU readings. Since 1.4.1,
generated API tokens allow only one authentication. For repeated scrapes, use a
managed OIDC credential with the **viewer** role, or exchange an unused viewer token
via `POST /api/v1/auth/token` with JSON `{"token":"..."}` and store the returned
session secret in the protected credentials file. The session expires within twelve
hours or at the source token's earlier expiry; arrange replacement before that time.
Do not put a raw one-use token into a recurring scrape configuration. The credentials
file must be readable only by the monitoring identity. Use the controller CA and a
hostname in its certificate.
Adapt [the scrape example](../../examples/prometheus/xirang.yml), then validate it
with `promtool check config /etc/prometheus/prometheus.yml`.

Choose explicit storage limits when starting your Prometheus service, for example
`--storage.tsdb.retention.time=30d --storage.tsdb.retention.size=5GB`, and bind its
query interface to loopback unless separately secured. The first reached limit
controls retention, so 30 days is not guaranteed if the size limit fills earlier.
Plan independent backup and free-disk monitoring; this service is not installed
or configured automatically by XIRANG.

Useful queries include `xirang_node_used_cpu_cores`,
`time() - xirang_node_observation_received_timestamp_seconds`, and
`xirang_node_observation_fresh == 0`. Graph a time range in Prometheus to inspect
retained samples. Usage/sensor series disappear when the underlying observation
or receipt is missing, future-dated or older than 180 seconds. Configured capacity
may remain present. `up{job="xirang"} == 0` indicates a failed scrape, including
authentication or controller storage failure, rather than zero resource use.

Scrapes do not replay missed samples, replicate system logs, or prove sensor
accuracy. The source timestamps distinguish repeated observations from new ones.
Same-host Prometheus is not independent high availability. For syntax and storage
options, see the [Prometheus configuration reference](https://prometheus.io/docs/prometheus/latest/configuration/configuration/)
and [storage guidance](https://prometheus.io/docs/prometheus/latest/storage/).

## Host event queue

The Agent state directory contains `health-outbox.db`, protected with mode 0600.
Budget disk space beyond its 16 MiB payload bound for bbolt pages and indexes.
Watch the node health sources for queue write/read failures, journal cursor gaps
and cumulative eviction counts. A full or inaccessible state filesystem prevents
safe cursor advancement. Restore capacity/access and let the Agent retry; do not
clear the database to hide a warning. Source journal/SEL retention can still lose
records before capture, and the controller's bounded event list is not an archive.

Upgrade both endpoints to enable `host-event-outbox-v1`. An older controller cannot
acknowledge the new queue. Preserve the database during rollback or backup, stopping
the Agent before a file copy. Do not copy a node's database to another node or
reuse it after enrollment changes the node ID. Preserve and explicitly retire the
old database before re-enrollment; automatic deletion would discard evidence.

## Upgrade

1. Verify the release archive against `SHA256SUMS` and create a recovery point.
2. Upgrade the passive controller first.
3. Stop the active controller and let the upgraded passive acquire leadership.
4. Verify readiness, version, Agent reconnection, reservations, and Web access.
5. Upgrade the former active controller and return it as the passive.
6. Upgrade Agents gradually; drain a node before a restart if work is active.

Do not bypass a schema-version error or run incompatible controllers against one
PostgreSQL namespace. On systemd hosts, inspect
`/etc/xirang/shared-write-paths.d` after upgrade; restart affected services so exact
shared-path permissions take effect.

Rotate build-signing keys separately: deploy the new public key to Agents, activate
the new signing key, and remove the old trust key only after old compiled outputs
are no longer scheduled.

## Shared-directory host access

Older export helpers and installers changed the export root to
`xirang-task:xirang-task` with mode `0770`. Upgrading prevents this overwrite,
but cannot reconstruct the original owner, group, or ACL. Restore those from a
known backup or an administrator-approved policy on the storage host, then run
the updated helper to grant cluster access without replacing that policy:

```bash
sudo /usr/local/bin/xirang-agent mount-helper prepare share_default /srv/xirang/XirangShare
getfacl -p /srv/xirang/XirangShare
```

For a site policy that explicitly permits every local user to list and traverse
the shared root, back up its ACL first, then grant only that access:

```bash
umask 077
sudo getfacl -p /srv/xirang/XirangShare > shared-root.before.acl
sudo setfacl -n -m o::r-x /srv/xirang/XirangShare
```

Use the actual source path for a custom share. This command neither grants
world write access nor rewrites descendant permissions. A named-user ACL may
still restrict that user; check it with `getfacl`. Host groups that need write
access should receive an explicit site ACL. ACL masks affect named users and
groups together; review effective permissions when changing a mask. See the
[ACL tool documentation](https://man7.org/linux/man-pages/man1/setfacl.1.html).
Do not recursively chown or chmod a shared tree to repair its root.
Existing per-file and default ACLs still govern shared content, including
files created through the Web interface. Upgrading alone does not republish
already active exports; use `prepare` after restoring their host policy.

To undo the explicit ACL change, run `sudo setfacl --restore=shared-root.before.acl`
before making other metadata changes, then recheck host and cluster access.

## Rollback

Stop all controllers, restore the recovery point created by the old version, install
the matching old native package, and start one controller. Confirm cluster state
before starting a standby or upgrading Agents again.
