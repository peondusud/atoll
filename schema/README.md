# Catalog and configuration schemas

Language-neutral contracts shared by the Go and Rust versions of Atoll (`specs.md` §29.3). Any change here is a contract change: ask first (`CLAUDE.md`).

| Path | Content |
|---|---|
| `migrations/NNNN_name.sql` | The catalog schema: plain SQL, applied in order by both versions. `PRAGMA user_version` holds the schema version; the pragmas are in `specs.md` §9. Not defined yet. |
| `config.schema.json` | JSON Schema (draft 2020-12) of the TOML 1.1 configuration (`specs.md` §24): every key, its type, its allowed values and its default. Unknown keys are errors. Atoll reads it at load time, so it is the one list of keys. |
| `schema.go` | Embeds `config.schema.json` for the Go version. |

A `format` starting with `atoll-` names a grammar of `specs.md` §29.5. A third-party validator ignores `format`, so it can accept a value Atoll refuses, such as `100.5%`, but never the reverse. The relations between keys are not in the schema: `specs.md` §24.3.
