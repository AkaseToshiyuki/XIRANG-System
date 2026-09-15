# XIRANG 仓库自动化策略

[English](AGENTS.md) | [简体中文](AGENTS.zh-CN.md)

## 分支策略

- `main` 是唯一的开发、发行和自动同步主分支。
- 日常实现提交和自动推送必须以 `main` 为目标。
- 同步后保持工作区位于 `main`，并验证本地与远端提交一致。
- 除非用户今后明确要求独立测试分支，否则不要重新创建 `TEST/TEST_XIRANG`。
- 日常同步禁止强制推送 `main`。
