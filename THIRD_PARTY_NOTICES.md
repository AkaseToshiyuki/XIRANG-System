# Third-party notices

[English](THIRD_PARTY_NOTICES.md) | [简体中文](THIRD_PARTY_NOTICES.zh-CN.md)

XIRANG is distributed under the GNU General Public License version 3. The
following independently licensed components are used to build or run XIRANG.
Their licenses remain in effect for those components.

## Runtime components

| Component | Version | License |
| --- | --- | --- |
| `github.com/cilium/ebpf` | 0.22.0 | MIT |
| `github.com/creack/pty` | 1.1.24 | MIT |
| `github.com/gorilla/websocket` | 1.5.3 | BSD-2-Clause |
| `github.com/jackc/pgx/v5` and its Jackc support modules | 5.9.2 | MIT |
| `go.etcd.io/bbolt` | 1.3.11 | MIT |
| `golang.org/x/crypto`, `x/net`, `x/sync`, `x/sys`, and `x/text` | versions pinned in `go.mod` | BSD-3-Clause |
| `google.golang.org/grpc`, `genproto`, and `protobuf` | versions pinned in `go.mod` | Apache-2.0 / BSD-3-Clause |
| React and React DOM | 18.3.1 | MIT |
| Noto Sans SC variable font | 5.3.0 | SIL Open Font License 1.1 |

## Development and build components

The locked Web toolchain also contains packages under MIT, Apache-2.0, ISC,
BSD-3-Clause, SIL Open Font License 1.1, and CC-BY-4.0 terms. It is used to
produce the static Web bundle and is not installed as an XIRANG service.

Accessibility tests use `@axe-core/playwright` 4.11.1 and the locked `axe-core`
4.11.4 under MPL-2.0. These unmodified development tools are not imported by
the application or included in native release bundles. Their upstream packages
retain their licenses and third-party notices.

Native release archives include the exact license and notice files collected
from the dependency versions used for that build under `share/licenses`.
Source distributions retain `go.sum` and `web/package-lock.json` as the exact
dependency version records.

No third-party dataset is embedded in XIRANG. Host utilities such as
`nvidia-smi`, compiler toolchains, PostgreSQL, and workload applications are
detected or invoked when installed by an operator; they are not redistributed
as part of XIRANG.
