# Native release packaging

[English](README.md) | [简体中文](README.zh-CN.md)

XIRANG releases are native Linux archives, not images. Run:

```bash
make package-native VERSION=1.0.1
```

The resulting `dist/xirang-<version>-linux-<arch>.tar.gz` archives contain static
control-plane, Agent, CLI, and MPI-adapter binaries, Web assets, service definitions,
examples, and public bilingual manuals. Internal engineering archives are excluded.
