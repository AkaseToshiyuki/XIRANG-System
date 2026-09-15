# XIRANG Repository Automation Policy

[English](AGENTS.md) | [简体中文](AGENTS.zh-CN.md)

## Branch policy

- `main` is the canonical development, release, and automatic-sync branch.
- All routine implementation commits and automated pushes must target `main`.
- Keep the working checkout on `main` after synchronization and verify that the
  local and remote commits match.
- Do not recreate `TEST/TEST_XIRANG` unless the user explicitly requests a
  separate test branch in a future task.
- Never force-push `main` as part of routine synchronization.
