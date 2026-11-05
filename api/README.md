# API contracts

Language-neutral contracts shared by the Go and Rust versions of Atoll (`specs.md` §29.3). Any change here is a contract change: ask first (`CLAUDE.md`).

| Path | Content |
|---|---|
| `openapi.yaml` | The local API: HTTP/1.1 and JSON over a Unix socket (named pipe on Windows). |
| `schemas/` | JSON schemas of the `--json` outputs of the CLI (`specs.md` §25). |

Schemas use JSON Schema draft 2020-12, and refuse unknown fields. Defined so far: `schemas/names-check.schema.json`, for `atoll names check` (`specs.md` §15.9). `openapi.yaml` is not defined yet.
