# XIRANG 1.0.1 release notes

[English](RELEASE_NOTES.md) | [简体中文](RELEASE_NOTES.zh-CN.md)

XIRANG 1.0.1 is a security and correctness maintenance release for the stable
`v1` management and Agent protocol. It is wire-compatible with 1.0.0 and does
not require a data migration.

## Fixed in 1.0.1

- separated the user-facing management relay's server-authentication TLS from
  the Agent mTLS identity, preventing browser traffic from inheriting node-only
  artifact permissions;
- upgraded `github.com/cilium/ebpf` to 0.22.0 to clear a reachable BTF parsing
  advisory and updated the hand-written eBPF instruction stream for the current
  assembler validation rules;
- excluded critical network edges from placement and treated missing latency as
  unknown instead of zero during topology ranking;
- rejected non-finite CPU/GPU placement weights and normalized hardware
  topology identifiers used by inventory and scheduling;
- tightened the public-source snapshot so internal tests, engineering records,
  research material, and original brand sources stay in the private repository.

## 1.0.0 foundation

XIRANG 1.0.0 was the first stable native Linux release. It established the `v1`
management and Agent protocol while keeping workload execution free of a required
container runtime.

### Included

- guided controller or managed-node installation on AMD64 and ARM64 Linux;
- verified release downloads, SHA-256 checks, sudo isolation, and NTP readiness;
- token or administrator-approved enrollment with full-interface path probing;
- role-driven management panels, node inventory, GPU and temperature telemetry;
- priority queues, quotas, early infeasibility diagnosis, arrays, weighted batch
  manifests, multi-node gang execution, and native MPI adapters;
- shared-space reconciliation, owner-isolated file management, native builds,
  application variants, accounting, alerts, backups, and PostgreSQL standby.

## Compatibility

The control-plane and Agent protocol version remains `v1`. Patch and minor releases may
add optional fields and negotiated capabilities but do not reuse Protobuf field
numbers or make existing OpenAPI fields newly mandatory. Back up the controller
before upgrading and upgrade a passive controller before the active instance.

## Documentation

Native archives contain the bilingual installation, operation, and usage manuals
under `docs/user`.
