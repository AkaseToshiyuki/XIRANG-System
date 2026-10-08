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
state continuity, corruption rejection, and dump/restore. It does not simulate
independent power, rack, or network failure domains.

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

## Rollback

Stop all controllers, restore the recovery point created by the old version, install
the matching old native package, and start one controller. Confirm cluster state
before starting a standby or upgrading Agents again.
