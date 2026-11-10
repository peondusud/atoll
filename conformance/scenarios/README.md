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
| `disks` | array of objects | `id`, `uuid`, `fs`, `capacity` in bytes, and `tree`, what the disk holds at the start. A disk is the folder `${SANDBOX}/disks/<id>`. Optional: `renameat2`, `false` for a disk that lacks `RENAME_NOREPLACE`, and `refuses`, the names it refuses as the target of a move or a link |
| `config` | string | the text of the configuration, written to `${SANDBOX}/atoll.toml`, which each `atoll` step gets through `--config`, before its `args`. Left out, no step gets it |
| `steps` | array of objects | what happens, in order: below |

`${SANDBOX}`, in `config`, in `args`, in `stdout_json`, in `rows` and in the `text` of a `write` step, is the scenario's temporary folder. An unknown field, or a field on a step it does not apply to, is an error: a typo must not disable a check.

## Steps

| `do` | Fields | Meaning |
|---|---|---|
| `atoll` | `args`, `exit`, `stdout_json`, `crash_at` | runs the binary in `${SANDBOX}`, with `ATOLL_CONFORMANCE` and `TZ=UTC` in its environment and nothing on standard input. `exit` is the exit code, 0 when left out. `stdout_json` is its standard output, read as JSON, when given. An item of `args` is a string, or `{"b64": "…"}` for bytes that are not UTF-8. `crash_at` names a point of the control file, for this run only |
| `write` | `disk`, a path, `text` or `size`, `mtime_ns` | writes a file and any folders above it, replacing the content of what is there in place |
| `mkdir` | `disk`, a path | creates a folder and its parents |
| `rename` | `disk`, a path, `to` or `to_b64` | renames, replacing what is at the target as `rename(2)` does |
| `link` | `disk`, a path, `to` or `to_b64` | gives a file another name, as `link(2)` does |
| `delete` | `disk`, a path | removes a file or an empty folder |
| `symlink` | `disk`, a path, `target` | creates a symbolic link |
| `touch` | `disk`, a path, `mtime_ns` | sets a modification time |
| `advance` | `by` | moves the clock forward, by a duration of `specs.md` §29.5 |
| `tick` | | waits until the real clock behind a change time has moved, which it may not do within a clock tick (`specs.md` §17.1). `advance` moves another clock |
| `unmount` | `disk` | a failed mount: the disk's folder is left empty and leaves the control file, and its tree is set aside |
| `mount` | `disk` | brings an unmounted disk back, with its tree. A step fails if something was written to the empty folder meanwhile |
| `expect_tree` | `disk`, `tree` | compares the whole disk with `tree`: below |
| `expect_catalog` | `query`, `rows` | runs `query` on `${SANDBOX}/catalog.db`, read-only, and compares what it returns with `rows`: below |

A path is `path`, or `path_b64` for bytes that are not UTF-8, relative to the top of the disk. A step that fails stops the scenario, which fails. `specs.md` §29.4 also names operations through mergerfs, I/O errors and crashes: those steps arrive with what they test, and a step the runner does not know fails the scenario.

## Comparing

**`stdout_json`** must equal the output exactly: another key, a missing one, or another order in a list is a failure, and numbers compare by their text, so `3.0` is not `3`. A string `${ANY}` matches any string, for what is not part of the contract, such as the wording of a message, and `${ANY_OR_NULL}` a string or `null`, for what a filesystem may not have, such as a creation time. Only `--json` output is compared: human output and standard error are translated, and they are no contract.

**`expect_tree`** covers the whole disk. An entry has a path and a `kind`: `file`, when left out, `dir` or `symlink`.

- A file has its content: `text`, a string, or `size`, which means that many bytes of `i mod 251`, the pattern of the official BLAKE3 test vectors. `mtime_ns` is compared when given.
- A symlink has its `target`.
- The folders on the way to a listed entry need no entry, and an empty folder must have one.
- A component that is exactly `*` matches any one name, for an operation's id in `Atoll/trash/<date>/<op-id>/…`.
- Each entry takes one thing on the disk, and each file, symlink and empty folder on the disk must be taken. So one `*` entry does not cover two operations.

**`expect_catalog`** compares `rows`, an array of arrays, with the rows of `query`, in order and exactly: integers, strings and `null`. A column that holds a BLOB is an error, so a query says how to read it, as in `CAST(lpath AS TEXT)` or `hex(blake3)`. `${SANDBOX}`, `${ANY}` and `${ANY_OR_NULL}` work as in `stdout_json`. So a scenario can name a catalog anywhere, but only this path can be compared.

## Always checked

After each `atoll` step, on each disk (`specs.md` §6):

- no name starting with a dot, unless the scenario itself gave that name to something, on any disk: a user's hidden file gets replicas;
- nothing at the top of the disk but `media`, `Atoll` and what the scenario itself put there.

## The control file

`ATOLL_CONFORMANCE` names `${SANDBOX}/conformance.json`, which only a conformance build reads (`specs.md` §29.4). The runner rewrites it on `advance`, `unmount` and `mount`, and around an `atoll` step with `crash_at`.

```json
{
  "now": "2026-10-26T01:00:00Z",
  "disks": [
    { "id": "t1", "uuid": "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", "fs": "ext4",
      "path": "/tmp/atoll-scenario-1/disks/t1", "capacity": 1000000 }
  ],
  "crash_at": "moved"
}
```

`crash_at`, left out when no step asks for it, makes the binary stop at once with exit code 137 the first time it reaches that point of an operation (`specs.md` §17.1), with no cleanup, as `kill -9` would:

| Point | Reached |
|---|---|
| `decided` | once a pass has committed what it decided about a page of files, before anything runs on a disk (`specs.md` §15.12) |
| `modified` | once a pass has committed a modification of one file, before anything runs on a disk (`specs.md` §15.4) |
| `journaled` | once a `trash`, `restore`, `rename`, `create` or purge operation is committed `planned`, before anything is written to a disk |
| `copied` | once a `create` has copied the whole source into its `.part` and recorded every chunk hash, before the source is looked at again and the times are restored |
| `verified` | once a `create` has re-read its `.part` against the chunk hashes and recorded the whole-file hash, before the target is resolved |
| `trash-folders` | once the folders of a move to the trash are made and synced, before the copy is looked at again and moved. A restore and a purge have none |
| `folder-made` | once a restore has made a folder below the root, before it is synced or recorded |
| `linked` | between `link` and the `fsync` that follows it, so only on a disk with `renameat2` at `false` |
| `moved` | once the name has moved, a file's or a folder's, or the file of a purge is gone, before any later `fsync` of what holds it. With `renameat2` at `false`, `unlink` has run too, so the source's name is gone, which is not yet synced, and the target folder was synced once already, between `link` and `unlink` |
| `target-synced` | after `fsync` of the target folder. A purge has none |
| `source-synced` | after every `fsync`, before the operation is committed `done`, and so before the folders left empty go. For a `create`, the source is the folder of its `.part` |

A disk's entry also holds what the scenario says of it, when it says so: `renameat2`, at `false`, makes `RENAME_NOREPLACE` give `EINVAL` on that disk; and with a name in `refuses`, that, a plain `rename`, `link` and `mkdir` give `EINVAL` for a target of that name, which is compared with the last component only.

A disk of the configuration is found here by its `mount`, equal to a `path`, and never by its `id`. To the mount check of `specs.md` §10, a `path` listed here has an entry that names it, with the root `/`, the type `fs` (`ntfs3` for `ntfs`), the mount options `rw` and the superblock options `rw`, plus `iocharset=utf8` on `ntfs3`. Its device answers, and its head holds the magic of `fs` and the identifier `uuid`. Any other folder has no such entry, and fails question 2. Question 10 and `ignore-case` look at the real folder, and the others of the second table find nothing to report.

Its clock is `now`. A disk's capacity is `capacity`, and its free space is that minus the size of every file below `path`, or 0. A conformance build reads this file only for a command that looks at disks, such as `atoll disk list`, even with no disk in the configuration, after the configuration has loaded. With `ATOLL_CONFORMANCE` unset or the file refused, such a command exits with 1.
