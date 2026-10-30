# Operating XIRANG

[English](OPERATIONS.md) | [简体中文](OPERATIONS.zh-CN.md)

Applies to XIRANG **1.2.0**.

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

Configure both controllers with the same TLS-protected PostgreSQL DSN and namespace;
start the passive instance with `-standby`. A namespace belongs to exactly one
XIRANG cluster.

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

The node management role currently controls local access to the Web panel; it
does not by itself create another active controller. The panel relay still
depends on the active controller selected at enrollment. PostgreSQL standby
promotion remains the supported controller failover path. A later multi-controller
gateway will separate the always-available panel listener from the single-writer
controller backend so every management node can remain directly accessible during
leadership changes.

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
resource use, network counters and available CPU/GPU readings. Create a revocable
**viewer** API token in identity management; store it in a protected file readable
by the Prometheus service. Use the controller CA and a hostname in its certificate.
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
