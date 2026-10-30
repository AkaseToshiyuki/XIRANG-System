# XIRANG 1.3.0 release notes

[English](RELEASE_NOTES.md) | [简体中文](RELEASE_NOTES.zh-CN.md)

XIRANG 1.3.0 adds persistent cluster settings, host-health visibility, configurable
alerts and numerical-history export. It supersedes 1.2.0; the management API and
Agent protocol remain `v1`.

## Administration and monitoring

- Administrators can set the cluster name and PNG/JPEG logo. Changes persist and
  appear in navigation, login and page titles.
- Node health combines available journal, IPMI sensor, SEL and chassis readings.
  The console distinguishes reported faults, hardware correction, recovery and
  recorded maintenance. Unavailable or partial collection remains visible.
- Alert settings persist trigger/recovery durations, severity/type routing and
  silence deadlines. Silenced events remain recorded; delivery history explains
  suppression and reports bounded per-destination status.
- Authenticated Prometheus metrics expose numerical observations with freshness
  timestamps. Configure a viewer token, trusted CA and external retention limits
  as described in [Operations](OPERATIONS.md).
- Agents retain a bounded host-event outbox across restarts and reconnects.
  Journal rotation gaps, queue errors and eviction counts are surfaced.

## Shared storage and compatibility

Shared mount/export control now uses bounded commands over the Agent mTLS stream
with acknowledgement and audit. Upgrade both endpoints and explicitly enable
`-shared-mount-control` on participating root/systemd Agents. The option defaults
to off. File access continues to use trusted management SSH; the stream does not
provide an arbitrary shell. See [Deployment](DEPLOYMENT.md).

Shared-root preparation preserves existing owner, mode and ACL semantics instead
of replacing them. Old permissions are not automatically restored; use the
migration instructions and verify access with the affected host identities.
Nested shared-directory metadata lookup, mixed-architecture application filters,
and published-artifact group access are corrected.

## Console and engineering updates

Keyboard focus is retained after dialogs, menus and toasts. Language changes
preserve drafts and terminal state. English/Chinese, 320-pixel layouts and
automated accessibility checks are covered by browser regression tests.

The toolchain is Go 1.26.10, with x/net 0.60.0 and required dependency updates.
Native builds retain the Landlock ABI 3+, dedicated non-root identity and
delegated cgroup v2 requirements introduced in 1.2.0. Previously unapproved
application mappings still require administrator approval.

## Upgrade and operating boundaries

Create and verify a controller backup first. Drain work before restarting an
Agent and retain its `health-outbox.db`; do not copy that database to another
node. For PostgreSQL deployments, pre-provision the same CA, credentials and
stable TLS endpoint on both controllers, then upgrade passive instances first.
Keep the previous programs and verified state backup for rollback.

Hardware locator control requires a separate host opt-in and administrator
authorization. Monitoring does not clear hardware logs or change power, fans or
thresholds. Historical events do not prove a present fault or its repair.
Prometheus is optional and is not installed automatically. Same-host monitoring
or service restart does not establish independent high availability. Sensor
accuracy, multi-node NFS, physical fabrics and actual MPI workloads require site
qualification. Browser access to `file://` SBOM locations retains its deployment
limit.

Release verification covers Go tests, race/static checks, API/Proto contracts,
Web audits, 37 browser regressions and Linux AMD64/ARM64 builds. Native packages,
`SHA256SUMS` and `SHA256SUMS.sig` are the four release assets. Keep verification
files with the package; the installer checks the Ed25519 signature before the
archive checksum.
