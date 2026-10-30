# XIRANG Repository Automation Policy

[English](AGENTS.md) | [简体中文](AGENTS.zh-CN.md)

## Engineering working principles

- Complete all work the user has authorized, including related fixes and necessary
  verification. Carry the objective and accepted constraints across turns.
- Decide routine design and implementation details from the current code,
  engineering evidence, and a clear recommendation. Ask only when a material
  decision cannot reasonably be made without the user's input; continue independent
  work while waiting. Do not repeatedly request already-granted permission.
- Begin substantive work with a short visible task list and update it at meaningful
  milestones. Read current project status and validation records before extending
  existing work, and preserve unrelated working-tree changes.
- Define deliverables, invariants, and acceptance checks before implementation.
  Validate parsers and scientific claims against independent references where
  appropriate, rather than only fixtures produced by the implementation itself.
- Treat unavailable or stale observations explicitly. Distinguish error reports,
  hardware-corrected errors, recovery observations, and confirmed repair actions;
  none of them alone proves the underlying fault has been repaired.
- Keep privileged operations structured, authorized, bounded, and auditable.
  Monitoring must not trigger hardware control, fault clearing, or workload
  execution as a side effect.
- Record exact verification results and remaining limitations. Distinguish source
  implementation, configured defaults, observed runtime behavior, and unverified
  physical hardware or deployment assumptions.

## Branch policy

- `main` is the canonical development, release, and automatic-sync branch.
- All routine implementation commits and automated pushes must target `main`.
- Keep the working checkout on `main` after synchronization and verify that the
  local and remote commits match.
- Do not recreate `TEST/TEST_XIRANG` unless the user explicitly requests a
  separate test branch in a future task.
- Never force-push `main` as part of routine synchronization.
