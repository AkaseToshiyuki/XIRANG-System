# XIRANG 1.1.0 release notes

[English](RELEASE_NOTES.md) | [简体中文](RELEASE_NOTES.zh-CN.md)

XIRANG 1.1.0 brings the current native Linux implementation to the public stable
release line. It supersedes version 1.0.1. The management API and Agent protocol remain `v1`.

## Included capabilities

- Native AMD64/ARM64 execution, resource and executor checks, queue/group policy,
  task accounting, and cgroup v2 isolation where the required host capabilities
  are available.
- Native build variants with an artifact-digest signature, editable node mappings,
  and explicit runtime requirements checked by both scheduler and Agent. When
  mappings overlap, the first captured mapping wins; runtime mismatch does not
  select an older mapped variant automatically.
- Reciprocal address-pair selection for observed probe paths, conservative
  capacity/RTT ranking, multi-node Gang allocation, and bounded ranked search
  with an explicit completion/exhaustion result. Link speeds are declared bounds,
  not measured application throughput; the controller does not change routes.
- Weighted partitioning of independent batch items, shared-space management,
  owner-scoped files and jobs, hardware telemetry, alerts, and a bilingual console.
- Backup/restore and optional PostgreSQL active/passive leadership. MPI adapters
  require validation against the site's launcher and network environment.

## Changes since the previous public release

- Release checksum manifests are signed with Ed25519 and the installer verifies
  them against its embedded public key. Native archives include both Linux CPU
  architectures and third-party license material.
- Authentication failures remain closed, API-token privileges are bounded, and
  credential endpoints have bounded throttling state. Agent gRPC streams and
  keepalive settings are bounded; controller certificates can renew in service.
- Job lookup uses owner/liveness indexes and scheduling limits work per pass.
  Node removal is audited; topology omits stale unavailable-node evidence and
  shared-space recovery accounts for replaced nodes.
- The console's light theme, topology labels, file-manager text, and bilingual
  built-in templates have been corrected and have browser/audit coverage.

## Known operating boundaries

Publishing into nested shared directories or reading outputs under a different
task identity can require administrator preparation of file access permissions.
Nested shared-directory listing also has a known metadata lookup limitation.
An explicit architecture filter on an application containing mixed-architecture
variants is rejected; use correctly mapped, explicitly selected nodes until that
admission path is revised.

Hardware-dependent execution and MPI behavior require site qualification.

## Upgrade and verification

Back up the controller state before upgrading. Upgrade a passive controller
before the active instance, and check node capabilities and shared-file ownership
before admitting new workloads. The release does not require a change to the
`v1` protocol identifier; optional capabilities are negotiated by the Agent.

Use the current installer and retain `SHA256SUMS` and `SHA256SUMS.sig` with the
native archive. The installer verifies the signature and checksums before use.

## Reference

Haochen Yang. *XIRANG: Native Build-to-Placement Contracts for Heterogeneous Linux Clusters*. Preprint, 2026. [PDF](https://github.com/AkaseToshiyuki/XIRANG-System/releases/download/v1.1.0/xirang-preprint.pdf).
