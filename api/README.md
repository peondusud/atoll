# API contracts

Language-neutral contracts shared by the Go and Rust versions of Atoll (`specs.md` §29.3). Any change here is a contract change: ask first (`CLAUDE.md`).

| Path | Content |
|---|---|
| `openapi.yaml` | The local API: HTTP/1.1 and JSON over a Unix socket (named pipe on Windows). |
| `schemas/` | JSON schemas of the `--json` outputs of the CLI (`specs.md` §25). |

Schemas use JSON Schema draft 2020-12, and refuse unknown fields. Defined so far: `schemas/names-check.schema.json`, for `atoll names check` (`specs.md` §15.9), `schemas/rules-check.schema.json` and `schemas/rules-explain.schema.json`, for `atoll rules check` and `atoll rules explain` (`specs.md` §11.3, §25), `schemas/disk-list.schema.json`, for `atoll disk list` (`specs.md` §10, §25), `schemas/init.schema.json` and `schemas/scan.schema.json`, for `atoll init` and `atoll scan` (`specs.md` §15.10, §25), `schemas/status.schema.json`, for `atoll status` (`specs.md` §15.11), and `schemas/prune.schema.json`, `schemas/trash-list.schema.json`, `schemas/trash-restore.schema.json` and `schemas/trash-purge.schema.json`, for `atoll prune` and `atoll trash` (`specs.md` §17.3, §17.4). `openapi.yaml` is not defined yet.
