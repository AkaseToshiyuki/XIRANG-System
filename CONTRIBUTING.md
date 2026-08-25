# Contributing to XIRANG

[English](CONTRIBUTING.md) | [简体中文](CONTRIBUTING.zh-CN.md)

Contributions are welcome under GPLv3. Please keep changes focused, update the
English and Simplified Chinese documentation together, and add tests for changed
behavior.

Before opening a pull request, run:

```bash
make verify
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run audit:i18n
```

Describe user-visible behavior and compatibility impact in the pull request.
Never commit credentials, private keys, tokens, passwords, private network
addresses, personal data, or live cluster state.
