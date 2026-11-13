# XIRANG 1.4.1 release notes

[English](RELEASE_NOTES.md) | [简体中文](RELEASE_NOTES.zh-CN.md)

XIRANG 1.4.1 makes generated API tokens single use, keeps account sign-in separate
from token sign-in, and adds operator-controlled alert ignoring with node status
recalculation. It also includes the controller-pair, adaptive network and shared
storage improvements developed since 1.3.0. The management API and Agent protocol
remain `v1`.

## Sign-in and one-use tokens

- A generated API token becomes unusable immediately after its first successful
  authentication. Consumption is durable and atomic with its audit record;
  concurrent requests have at most one winner. Successful authentication consumes
  the token even when the requested operation is subsequently forbidden.
- Account sessions report `local_password`. Creating a token does not replace the
  current session. Sign-out returns to account login. API-token lists exclude
  interactive sessions and distinguish unused, used, expired and revoked tokens.
- Console token sign-in exchanges an unused token through `POST /api/v1/auth/token`
  for a separate session. The original token cannot be reused. The session lasts at
  most twelve hours, never beyond the original expiry, and retains its permission
  ceiling even if the owner is promoted. Disabled owners lose access. Sessions
  cannot renew themselves through the exchange endpoint.
- The generated secret is shown only on creation. Password sessions, configured
  bootstrap administrator credentials and OIDC credentials retain their existing
  authentication lifecycle; they are not generated one-use API tokens.

## Alerts and node status

The alert center adds **Ignore**, with confirmation, and an **Ignored** view. The
operator, time and incident state are retained across restarts. Ignoring does not
mark a condition repaired or delete its observations. Stale revisions are refused.

Ignored incidents stop contributing to node warning/critical lamps. The node view
recalculates from the remaining alerts, connection state, maintenance state and
current hardware sensors. A node can remain red when another fault remains.
Transitions arrive through the event stream; older poll responses cannot overwrite
a newer decision. A severity escalation reopens the incident, and a condition that
recovers and later recurs creates a new alert. Acknowledgements also record the
actual operator instead of a fixed administrator label.

## Shared storage and network paths

- NAS shares without POSIX ACL support can use preconfigured Unix user/group
  permissions. Existing directory permissions are not broadened automatically,
  and NFS directories are not probed with ACL writes. NFS re-exports use a stable
  filesystem identifier and a mountpoint check.
- `-default-shared-space=false` permits an explicit default-share replacement and
  prevents its recreation after restart. Artifact/build paths and saved job
  templates must move with the share; removing a share configuration preserves data.
- Shared project quotas are negotiated with Agents and enforced only on supported,
  configured filesystems. Unconfirmed shared mounts block new work. Offline nodes
  retain pending cleanup; removed nodes disappear from shared-space results.
- Inter-node transports use verified path discovery and selection, with usable
  capacity and latency preference, periodic reassessment and endpoint identity
  checks. Reciprocal-address stability and cross-node mount deadline handling are
  corrected. Negotiated line rate is not reported as measured throughput.

## Controller startup and availability

One primary and one backup controller can coordinate through a provisioned
PostgreSQL namespace, with primary preference, takeover and stabilized handback.
State-lock loss stops serving stale state. See [Operations](OPERATIONS.md) for
shared identity, stable endpoints and migration requirements.

Cached server certificates now check both their start and expiry dates and can be
reissued after a clock correction without replacing the CA. Service installation
waits for the configured time-synchronization target and enables the timesyncd wait
service when applicable. The pinned build toolchain is Go 1.26.11.

## Upgrade and verification

Back up state, PKI and configuration and retain the previous installation before
upgrading. Previously used non-interactive API tokens are already consumed under
1.4.1; unused tokens allow one authentication. Replace reusable-token integrations
with bounded sessions or managed OIDC credentials, and renew before expiry. This
includes authenticated monitoring scrapes; see [Operations](OPERATIONS.md).
Upgrade both members of a controller pair before using the new authentication and
ignore operations. Older controllers permit API-token reuse; revoke generated
credentials before any downgrade rather than mixing old and new authentication.

Backend tests cover durable one-use consumption, concurrent requests, session
permission ceilings, account-role changes, ignored incident lifecycle and API
permissions. Browser regressions cover login defaults, token exchange, confirmation
and node lamp recalculation while preserving other faults. These checks do not
establish hardware repair, physical failover performance or scientific correctness.

Some NAS/NFS implementations do not support the atomic no-overwrite rename used by
the file API: rename fails and preserves the source. NFS shares do not provide
XIRANG ext4/XFS project quotas. These limitations remain visible; this release does
not enable unsupported operations or claim a successful host reboot or workload.
