# API contracts

Language-neutral contracts shared by the Go and Rust versions of Atoll (`specs.md` §29.3). Any change here is a contract change: ask first (`CLAUDE.md`).

| Path | Content |
|---|---|
| `openapi.yaml` | The local API: HTTP/1.1 and JSON over a Unix socket (named pipe on Windows). |
| `schemas/` | JSON schemas of the `--json` outputs of the CLI (`specs.md` §25). |

None of these is defined yet.
