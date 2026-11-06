# Conformance scenarios

Black-box cases that judge an `atoll` binary through its CLI and the filesystem only (`specs.md` §29.4). One JSON file per scenario, in this folder, where every file but this one counts as a scenario. `conformance/runner` runs them all, each in a new temporary folder, and fails on anything it cannot run rather than pass over it: a skip would read as a success.

```json
{
  "format_version": 1,
  "spec": "specs.md §15.3",
  "description": "What it shows, in a sentence or two.",
  "clock": "2026-10-26T01:00:00Z",
  "disks": [
    { "id": "t1", "uuid": "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", "fs": "ext4", "capacity": 1000000,
      "tree": [{ "path": "media/a.mkv", "text": "x" }] }
  ],
  "config": "pool_id = \"p\"\ncatalog = \"${SANDBOX}/catalog.db\"\n[view]\nmount = \"${SANDBOX}/pool\"\n",
  "steps": [
    { "do": "atoll", "args": ["rules", "check", "--json"],
      "stdout_json": { "valid": true, "problems": [], "warnings": [] } },
    { "do": "rename", "disk": "t1", "path": "media/a.mkv", "to": "media/b.mkv" },
    { "do": "advance", "by": "10m" },
    { "do": "expect_tree", "disk": "t1", "tree": [{ "path": "media/b.mkv", "text": "x" }] }
  ]
}
```

## Fields

| Field | Type | Meaning |
|---|---|---|
| `format_version` | integer | `1`. The runner refuses any other value |
| `spec`, `description` | string | for readers; both are required in this folder |
| `clock` | string | the time of the conformance build, in RFC 3339 with an offset. Left out, it is `2026-01-01T00:00:00Z` |
| `disks` | array of objects | `id`, `uuid`, `fs`, `capacity` in bytes, and `tree`, what the disk holds at the start. A disk is the folder `${SANDBOX}/disks/<id>` |
| `config` | string | the text of the configuration, written to `${SANDBOX}/atoll.toml`, which each `atoll` step gets through `--config`, before its `args`. Left out, no step gets it |
| `steps` | array of objects | what happens, in order: below |

`${SANDBOX}`, in `config`, in `args` and in `stdout_json`, is the scenario's temporary folder. An unknown field, or a field on a step it does not apply to, is an error: a typo must not disable a check.

## Steps

| `do` | Fields | Meaning |
|---|---|---|
| `atoll` | `args`, `exit`, `stdout_json` | runs the binary in `${SANDBOX}`, with `ATOLL_CONFORMANCE` and `TZ=UTC` in its environment and nothing on standard input. `exit` is the exit code, 0 when left out. `stdout_json` is its standard output, read as JSON, when given. An item of `args` is a string, or `{"b64": "…"}` for bytes that are not UTF-8 |
| `write` | `disk`, a path, `text` or `size`, `mtime_ns` | writes a file and any folders above it, replacing the content of what is there in place |
| `mkdir` | `disk`, a path | creates a folder and its parents |
| `rename` | `disk`, a path, `to` or `to_b64` | renames, replacing what is at the target as `rename(2)` does |
| `delete` | `disk`, a path | removes a file or an empty folder |
| `symlink` | `disk`, a path, `target` | creates a symbolic link |
| `touch` | `disk`, a path, `mtime_ns` | sets a modification time |
| `advance` | `by` | moves the clock forward, by a duration of `specs.md` §29.5 |
| `expect_tree` | `disk`, `tree` | compares the whole disk with `tree`: below |

A path is `path`, or `path_b64` for bytes that are not UTF-8, relative to the top of the disk. A step that fails stops the scenario, which fails. `specs.md` §29.4 also names operations through mergerfs, disconnections and crashes: those steps arrive with what they test, and a step the runner does not know fails the scenario.

## Comparing

**`stdout_json`** must equal the output exactly: another key, a missing one, or another order in a list is a failure, and numbers compare by their text, so `3.0` is not `3`. A string `${ANY}` matches any string, for what is not part of the contract, such as the wording of a message. Only `--json` output is compared: human output and standard error are translated, and they are no contract.

**`expect_tree`** covers the whole disk. An entry has a path and a `kind`: `file`, when left out, `dir` or `symlink`.

- A file has its content: `text`, a string, or `size`, which means that many bytes of `i mod 251`, the pattern of the official BLAKE3 test vectors. `mtime_ns` is compared when given.
- A symlink has its `target`.
- The folders on the way to a listed entry need no entry, and an empty folder must have one.
- A component that is exactly `*` matches any one name, for an operation's id in `Atoll/trash/<date>/<op-id>/…`.
- Each entry takes one thing on the disk, and each file, symlink and empty folder on the disk must be taken. So one `*` entry does not cover two operations.

## Always checked

After each `atoll` step, on each disk (`specs.md` §6):

- no name starting with a dot, unless the scenario itself gave that name to something, on any disk: a user's hidden file gets replicas;
- nothing at the top of the disk but `media`, `Atoll` and what the scenario itself put there.

## The control file

`ATOLL_CONFORMANCE` names `${SANDBOX}/conformance.json`, which only a conformance build reads (`specs.md` §29.4). The runner rewrites it on `advance`.

```json
{
  "now": "2026-10-26T01:00:00Z",
  "disks": [
    { "id": "t1", "uuid": "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", "fs": "ext4",
      "path": "/tmp/atoll-scenario-1/disks/t1", "capacity": 1000000 }
  ]
}
```
