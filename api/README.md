# API contracts

[English](README.md) | [简体中文](README.zh-CN.md)

- `openapi/xirang-v1.yaml` describes the management API consumed by the Web
  console and `xirangctl`.
- `proto/xirang/v1/agent.proto` describes the outbound Agent control stream.

The server implements management, enrollment, node lifecycle, and mTLS
heartbeat endpoints. The Agent generates its private key and CSR locally; the
control-plane CA returns a certificate with a node URI SAN. The management contract
also exposes two-phase node certificate rotation, administrative revocation, and
newest-first lifecycle audit events. Phase 1C Go bindings under `gen/xirang/v1` are
checked in so release builds do not require code-generation tools. `make generate`
regenerates them, while `make contract-check` validates the source contract.

Terminal `AssignmentUpdate` messages include independently bounded 64 KiB stdout
and stderr tails. During execution, negotiated `assignment-log-snapshot-v1`
messages replace the controller copy at most twice per second; the authenticated
job SSE endpoint exposes those full snapshots without creating an unbounded chunk
history.

Authenticated Web clients can consume `/api/v1/events/nodes` as a server-sent event
stream. Each `node` event contains the complete latest Node representation; clients
replace state by node ID and may safely reconnect after interruption.

The `native-executor-v1` capability gates `RunAssignment`, `CancelAssignment`, and
`AssignmentUpdate`. The Agent durably registers an assignment before acknowledging
its command and replays the latest persisted outcome after reconnect. The Phase 1C
transport deliberately has no public assignment-submission API; the Phase 3 job and
scheduler service will own operator submission, resource reservation, and retries.
The authenticated `GET /api/v1/assignments` endpoint exposes the latest
controller-persisted report for each assignment attempt for diagnostics.

The administrator-only certificate rotation command endpoint durably records a
rotation request before enqueuing it. `certificate-rotation-v1` Agents generate the
new key and CSR locally, use their current mTLS identity for issuance, atomically
switch identity files, acknowledge the command, and reconnect with the new
certificate. Controller-provided certificate bytes in the gRPC command are
rejected.

The `management-panel-relay-v1` capability gates role-driven panel activation.
`SetManagementPanel` contains only an enabled flag: the Agent derives the HTTPS
upstream from its enrolled controller state, persists accepted state for restart,
and reports observed panel state in each heartbeat. An Agent without the
management role does not bind the panel port.

The `inventory-snapshot-v1` and `resource-observation-v1` capabilities gate Phase
2A reports on the same mTLS stream. Inventory is replaced atomically per node;
resource observations are retained newest-first in a bounded, separate telemetry
store. Administrator clients query them through `GET /nodes/{nodeId}/inventory`
and `GET /nodes/{nodeId}/observations`. Network values are cumulative host counters;
clients calculate rates from two observations and their timestamps.

`topology-observation-v1` carries sorted route, kernel-neighbor, and optional LLDP
observations. The controller validates IP prefixes, addresses, MACs, field lengths,
cardinality, and timestamps before generating a new snapshot. Identical content
does not advance the version. Administrators query the latest snapshot at
`GET /topology` or a retained version at `GET /topology?version=N`.

The latest response overlays inventory line rates and deterministic
`single-best-healthy` selections for parallel endpoint links. Capacity is the
selected link's capacity, never the sum; remaining non-critical links are ordered
fallbacks. This is control-plane policy metadata and does not rewrite host routes.

Contract compatibility rules:

1. Existing field numbers are never reused.
2. Fields are added as optional within `v1`; breaking changes require `v2`.
3. Every command and report has an immutable message or assignment identifier.
4. Capability negotiation gates optional behavior.
