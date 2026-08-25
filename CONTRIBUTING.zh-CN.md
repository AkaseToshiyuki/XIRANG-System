# 参与 XIRANG 开发

[English](CONTRIBUTING.md) | [简体中文](CONTRIBUTING.zh-CN.md)

欢迎依据 GPLv3 贡献代码。请保持改动聚焦，同时更新英文和简体中文文档，并为
行为变更补充测试。

提交拉取请求前请运行：

```bash
make verify
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run audit:i18n
```

请在拉取请求中说明用户可见行为和兼容性影响。禁止提交凭据、私钥、Token、
密码、私有网络地址、个人数据或实时集群状态。
