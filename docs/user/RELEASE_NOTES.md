# XIRANG 1.2.0 release notes

[English](RELEASE_NOTES.md) | [简体中文](RELEASE_NOTES.zh-CN.md)

XIRANG 1.2.0 strengthens build isolation and authorization and refreshes the
bilingual console. It supersedes 1.1.0. The management API and Agent protocol
remain `v1`.

## Security and correctness

- API-token permissions now intersect the account's current permissions;
  account demotion takes effect without enlarging existing token grants.
- Flow metadata requires administrator access. Private build artifacts require
  ownership or administrator authorization, including resolved-path checks.
- Global application publication and node mappings require administrator
  approval. Unapproved legacy mappings are excluded from application resolution.
- Build output collection uses pinned descriptors and rejects parent/leaf
  symlinks. Local artifact reads reject FIFOs and other special files without
  blocking. Build logs retain a bounded 64 KiB tail while draining output.
- Builds sharing an OS identity serialize staging, execution, collection and
  cleanup across Controller and Agent processes. Landlock restricts compiler
  filesystem access; incomplete cleanup or a daemon crash quarantines the UID.
- Recursive owned shared-directory deletion uses directory descriptors and
  refuses replaced or newly untracked entries.
- Agent authority is rechecked independently of incoming heartbeats. Revoked,
  replaced or expired certificates cannot retain a selectable build worker or
  continue exchanging commands; silent streams expire.
- Updated gRPC to 1.83.2 and x/text to 0.41.0, including required transitive
  updates, to address dependency security advisories.
- Relative controller state paths work with backup initialization. Browser SBOM
  downloads carry authentication and display download failures.

## Console

The console uses neutral surfaces and one primary accent, with less repeated
copy and decoration. Navigation has accessible names and current-page state;
login exposes language and theme choices. Terminal sessions remain connected
across theme and telemetry-interval changes. Browser-storage failures, node
stream reconnect state, builder controls, narrow layouts and rendered contrast
have regression coverage.

## Upgrade requirements

Back up controller state before upgrading; upgrade passive controllers before
the active instance. Use the current installer so service permissions and the
host build-identity lock directory are provisioned.

**Native builds require enabled Landlock ABI 3 or newer (Linux 6.2+)**, a
dedicated non-root identity and delegated cgroup v2. Toolchains and dependencies
must be installed in the standard system paths listed in [Usage](USAGE.md).
Unsupported native-build hosts fail closed; this requirement is specific to
native builds. Confirm these capabilities before enabling a build worker.

Administrators must re-save legacy application target mappings to approve them
for global resolution. After a build crash or failed cleanup, follow the UID
recovery procedure in [Usage](USAGE.md) before admitting another build.

## Verification and operating boundaries

Go tests, race/static checks, Linux AMD64/ARM64 compilation, API/Proto contracts,
Web audits and all 25 browser regressions passed for the release candidate.
Hardware, MPI, multi-node NFS and HA behavior still require site qualification.
Browser access to `file://` SBOM locations retains its existing deployment limit.

Nested shared-directory metadata lookup and explicit architecture filtering of
mixed-architecture application variants retain their existing limitations.
Use correctly mapped, explicitly selected nodes for the latter case.

Release assets contain the Linux AMD64 and ARM64 archives, `SHA256SUMS` and its
Ed25519 signature `SHA256SUMS.sig`. Keep all verification files with the archive;
the installer verifies the signature and checksum before installation.
