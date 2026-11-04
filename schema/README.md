# Catalog and configuration schemas

Language-neutral contracts shared by the Go and Rust versions of Atoll (`specs.md` §29.3). Any change here is a contract change: ask first (`CLAUDE.md`).

| Path | Content |
|---|---|
| `migrations/NNNN_name.sql` | The catalog schema: plain SQL, applied in order by both versions. `PRAGMA user_version` holds the schema version; the pragmas are in `specs.md` §9. |
| `config.schema.json` | JSON Schema of the TOML 1.0 configuration (`specs.md` §24). Unknown keys are errors. |

None of these is defined yet.
