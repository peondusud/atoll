# Conformance vectors

Input and expected output for the pure functions of Atoll (`specs.md` §29.4). The unit tests of the Go and Rust versions load the same files, so both versions accept and reject the same inputs and compute the same results.

## File format

One JSON file per function. Every file has this envelope:

```json
{
  "format_version": 1,
  "spec": "specs.md §29.5",
  "description": "What the function does, in one or two sentences.",
  "cases": [
    { "input": "90s", "nanoseconds": 90000000000 },
    { "input": "1h30m", "error": true, "note": "one unit per duration" }
  ]
}
```

- `format_version`: the version of this vector format, currently `1`. A loader refuses any other value.
- `spec`: the sections of `specs.md` the vectors pin.
- `cases`: the test cases, in the order a reader would follow. The fields of a case depend on the file, below.
- Every case may carry a `note`: a short explanation for reviewers, never read by the tests.
- Unknown fields are errors, so a typo cannot silently disable a check. Keys are lowercase ASCII and match a field exactly, and no key appears twice in one object: Go's decoder would otherwise match `Name` or `NAME` to `name`, and keep the last of two equal keys, where Rust's fails.
- An `error` case has `"error": true` and no expected output. It only says that the input is rejected: error messages and their wording are not part of the contract.
- A flag (`error`, `no_key`, `absent`, `complete`, `no_rule`, `no_entry`, `no_magic`, `other_case`, `first`, `reloaded`, `gone`, `back`, `check`, `hash_pending`, `read_hashes`) is `true` or left out, never `false` or `null`, so a case has one spelling. Go's decoder would read `null` as `false` where Rust's fails. The loader checks it.
- Integers are exact JSON integers within the signed 64-bit range, without a fraction, an exponent or `-0`; they are never written as strings or floats.
- Paths follow `specs.md` §29.3: `path` holds a path that is valid UTF-8; otherwise `path_b64` holds its raw bytes in standard base64 (RFC 4648, with padding), written exactly as the standard encoder writes it: no line break, no other spelling. A case has exactly one of the two.
- A file is valid UTF-8 and never holds an unpaired `\uD800`–`\uDFFF` escape; a high surrogate escape followed by a low one is a single character. Go's decoder would silently turn an unpaired escape, or an invalid byte, into U+FFFD, and Rust's would fail, so the two versions would test different names. A name that is not valid UTF-8 goes in a `*_b64` field. The loader checks both rules.
- Cases are written one per line, so a change shows up as a one-line diff.

## Files

### `duration.json` — durations (§29.5)

| Field | Type | Meaning |
|---|---|---|
| `input` | string | the value as written in the configuration |
| `nanoseconds` | integer | the duration in nanoseconds |
| `error` | boolean | `true` when `input` is rejected |

### `listen.json` — `api.listen` (§25.1, §29.5)

| Field | Type | Meaning |
|---|---|---|
| `input` | string | the value as written in the configuration |
| `address` | string | the address, in its canonical form: dotted decimal, or RFC 5952 for IPv6 |
| `port` | integer | the port |
| `error` | boolean | `true` when `input` is rejected |

### `size.json` — sizes (§29.5)

| Field | Type | Meaning |
|---|---|---|
| `input` | string | the value as written in the configuration |
| `bytes` | integer | the size in bytes |
| `error` | boolean | `true` when `input` is rejected |

### `percent.json` — percentages (§29.5)

A percentage is checked through its use: the share of a total, such as `min_free` of a disk's capacity.

| Field | Type | Meaning |
|---|---|---|
| `input` | string | the value as written in the configuration |
| `of` | integer | the total, in bytes |
| `result` | integer | the share of `of` that `input` gives, in bytes |
| `error` | boolean | `true` when `input` is rejected |

### `window.json` — time windows (§29.5)

| Field | Type | Meaning |
|---|---|---|
| `input` | string | the window as written in the configuration |
| `at` | string | an instant, in RFC 3339 with an offset; the offset gives the wall-clock time in the configured `timezone` |
| `inside` | boolean | whether `at` falls inside the window |
| `next` | string | the first instant inside the window at or after `at`, in `at`'s offset; left out where a case does not ask |
| `opened` | string | for an `at` inside the window, when the stretch of it that holds `at` began: its start that day, or the day before for a window wrapping past midnight, in `at`'s offset; left out where a case does not ask. Outside, there is none |
| `error` | boolean | `true` when `input` is rejected |

### `glob.json` — glob patterns (§29.5)

| Field | Type | Meaning |
|---|---|---|
| `pattern` | string | the glob, without the `re:` prefix that marks a regex |
| `path`, `path_b64` | string | the logical path, or the file name, matched against `pattern` |
| `match` | boolean | whether `pattern` matches the path |
| `error` | boolean | `true` when `pattern` is rejected |

These vectors pin the matcher alone, which compares characters exactly. The NFC form and the case folding of `ignore_case` (§11.3) apply to rule conditions, in `match.json`.

### `nfc.json` — NFC form of a path (§11.3)

| Field | Type | Meaning |
|---|---|---|
| `path`, `path_b64` | string | the input |
| `nfc`, `nfc_b64` | string | its NFC form, run by run: each maximal run of valid UTF-8 is normalized, every other byte is kept |

### `match.json` — rule conditions (§11.1, §11.3, §29.5)

| Field | Type | Meaning |
|---|---|---|
| `condition` | string | `path`, `filename` or `ext` |
| `value` | string | for `path` and `filename`, a glob, or a regex after `re:`, as written in the configuration; for `ext`, a list of one entry |
| `values` | array of strings | for `ext` only, instead of `value`: the whole list |
| `ignore_case` | boolean | the rule's `ignore_case` |
| `path`, `path_b64` | string | the logical path of the file; `filename` and `ext` look at its last component |
| `match` | boolean | whether the condition holds for the file |
| `error` | boolean | `true` when `value` is rejected when the configuration loads |

A case has exactly one of `value` and `values`. An `error` case has no path and no `match`.

### `regex.json` — the regex grammar (§29.5)

| Field | Type | Meaning |
|---|---|---|
| `pattern` | string | the regex, without the `re:` prefix |
| `path`, `path_b64` | string | a logical path, matched by the regex as a `path` condition without `ignore_case` |
| `match` | boolean | whether the regex matches the path |
| `error` | boolean | `true` when the grammar rejects `pattern`, the length or weight is over its limit, or the text holds more than 30 non-starters in a row |

Every accepted pattern comes with one path that shows its meaning, the same in Go `regexp` and Rust `regex`. Every accepted pattern, with and without `(?i)`, compiles in Rust with `size_limit` 10 MiB and `nest_limit` 1000, on a thread with at least 8 MiB of stack (§29.5).

### `name_compat.json` — name compatibility of whole paths (§15.8)

| Field | Type | Meaning |
|---|---|---|
| `path`, `path_b64` | string | the logical path |
| `fs` | string | the target filesystem: `ext4`, `xfs`, `btrfs` or `ntfs` |
| `problems` | array | the failing components, in path order: each is `{ "component": <index from 0>, "reasons": [<reasons>] }`, with the reasons of the name corpus. An empty array means the path can be written there. |
| `error` | boolean | `true` when `path` is not a well-formed logical path: empty, with a leading or trailing `/`, an empty, `.` or `..` component, or a NUL byte |

The rules for a single name are pinned by the name corpus, `conformance/corpus/names/names.json`; these vectors pin how a path is split and that folders are checked too.

### `lpath_key.json` — collision keys (§15.7, §15.8)

| Field | Type | Meaning |
|---|---|---|
| `path`, `path_b64` | string | the logical path |
| `key` | string | its `lpath_key` |
| `no_key` | boolean | `true` when the path has no key (NULL in the catalog), because it is not valid UTF-8 |

A case has exactly one of `key` and `no_key`. Paths that collide have the same key.

### `ntfs_collision.json` — name collisions on an NTFS target (§15.7)

| Field | Type | Meaning |
|---|---|---|
| `existing` | array of strings | the names on the target that are valid UTF-8; each one also stands for its parent folders |
| `existing_b64` | array of strings | optional: the names on the target that are not valid UTF-8, in base64 |
| `path`, `path_b64` | string | the path to write |
| `component` | integer | the first component that collides, from 0; absent when `with` is empty |
| `with` | array of strings | the exact names on the target that the path collides with at that component, in byte order; an empty array means no collision |
| `error` | boolean | `true` when `path` or a name on the target is not a well-formed logical path |

### `short_form.json` — the form of a short name on NTFS (§15.8)

| Field | Type | Meaning |
|---|---|---|
| `name` | string | one component |
| `short` | boolean | whether it has the form Windows gives the short name of a longer one, which a look on an NTFS disk must not take for a name of its own |

### `branch_path.json` — the path limit of a mergerfs branch (§15.8)

| Field | Type | Meaning |
|---|---|---|
| `mount` | string | the disk's mount path |
| `root` | string | the disk's managed root |
| `path`, `path_b64` | string | the logical path |
| `fits` | boolean | whether `<mount>/<root>/<path>` holds at most 4095 bytes |
| `error` | boolean | `true` when `path` is not a well-formed logical path, or `mount` or `root` is empty |

### `windows_path.json` — the path limit of Windows programs (§15.8)

| Field | Type | Meaning |
|---|---|---|
| `root` | string | the disk's managed root |
| `path`, `path_b64` | string | the logical path |
| `fits` | boolean | whether `D:\<root>\<path>` holds at most 259 UTF-16 code units; `true` for a path that is not valid UTF-8, which gets no verdict |
| `error` | boolean | `true` when `path` is not a well-formed logical path, or `root` is empty |

### `names_check.json` — the report of `atoll names check` (§15.9)

| Field | Type | Meaning |
|---|---|---|
| `paths` | array of strings | the logical paths of the pool that are valid UTF-8, in any order; each also stands for its parent folders |
| `paths_b64` | array of strings | optional: the logical paths that are not valid UTF-8, in base64 |
| `disks` | array of objects | the disks of the pool, in any order: `id`, `mount` and `root`, all required |
| `under`, `under_b64` | string | optional: the path given to `--path` |
| `checked` | integer | the names in scope |
| `findings` | array of objects | the findings, in the order of §15.9, as `api/schemas/names-check.schema.json` states them |
| `error` | boolean | `true` when a path, `under` or a disk is rejected |

### `file_state.json` — the state a file shows (§4)

| Field | Type | Meaning |
|---|---|---|
| `applies` | array of strings | the states that apply to the file, in any order |
| `state` | string | the one it shows |
| `error` | boolean | `true` when a state is unknown |

### `trash_path.json` — where a copy goes in the trash (§17)

| Field | Type | Meaning |
|---|---|---|
| `started_ns` | integer | when the operation started |
| `op_id` | integer | its id |
| `path`, `path_b64` | string | the logical path |
| `trash_path`, `trash_path_b64` | string | the location inside `<workdir>/trash/` |
| `error` | boolean | `true` when the id is not positive or the path is not well-formed |

### `prune.json` — what `atoll prune` does with each surplus copy (§17.3)

| Field | Type | Meaning |
|---|---|---|
| `state` | string | the file's state |
| `replicas` | integer | its wanted replicas |
| `hash` | boolean | whether it has a hash |
| `copies` | array of objects | its copies, in any order: `disk`, `position`, `state`, `created_by`, `disk_state`, `verified` and `unchanged`, all required |
| `outcomes` | array of objects | for each `surplus` copy, by position: `disk` and `outcome` |
| `error` | boolean | `true` when a state or a creator is unknown, or a disk or position appears twice |

### `trash_purge.json` — what a purge of the trash reaches, and why (§17, §17.4)

| Field | Type | Meaning |
|---|---|---|
| `now_ns` | integer | the time of the purge |
| `older_than_ns` | integer or null | `--older-than`, in nanoseconds; null for the retention and `max_size` |
| `max_size` | string | `trash.max_size`, a percentage (§29.5) |
| `disks` | array of objects | `id`, and `capacity` as the catalog last recorded it, null when none was |
| `entries` | array of objects | the trash, in any order: `disk`, `op`, `reason`, `size` (null when unknown), `deleted_ns`, `expires_ns`, and `held` for an entry that reached the trash in this run's reprise |
| `purge` | array of objects | what goes, by operation: `op` and `why`, one of `expired`, `older-than` and `max-size` |
| `full` | array of strings | the disks whose trash still holds more than `max_size` of their capacity once those are gone, by id in byte order |
| `error` | boolean | `true` when a reason is unknown, a size, a capacity or the age is negative, an operation id is below 1 or appears twice, or a disk is unlisted or listed twice |

### `op_recovery.json` — what a reprise does with an operation (§17.2)

| Field | Type | Meaning |
|---|---|---|
| `kind` | string | `trash`, `restore`, `rename`, `unlink` for a purge, `mkdir`, or `rmdir` |
| `source`, `target` | string | `absent`, `same` or `other`, compared with `ops.file_key`, and for `mkdir` with being a folder. An `unlink` or an `rmdir` has no target, and a `mkdir` no source |
| `changed` | boolean | optional: for `trash`, whether the file is no longer what its row says |
| `action` | string | `finish` or `undo` |
| `remove_source`, `remove_target` | boolean | which name must go |
| `move_back` | boolean | whether the file at the target goes back to the source |
| `warn` | boolean | whether it gets an `op-undone` event |
| `error` | boolean | `true` on an unknown word or kind, or a name that kind does not have or lacks |

### `copy_verdict.json` — what a look found of one copy (§15.1, §15.12)

A look is `inode`, `size`, `mtime`, `ctime`, and `btime` when the file has a creation time. An empty `inode` matches no inode, in a row as in a candidate.

| Field | Type | Meaning |
|---|---|---|
| `row` | object | the look its row records |
| `at_name` | object | optional: what its name holds now. An empty look, with an empty `inode`, stands for what is no regular file |
| `candidates` | array of objects | optional: what else this walk found that could be it: `name` and `look` |
| `own` | boolean | optional: whether the same file is in that disk's own `trash/` or `tmp/` |
| `not_reached` | boolean | optional: whether the walk did not get there |
| `verdict` | string | `unknown`, `there`, `edited`, `moved`, `uncertain`, `replaced`, `own` or `gone` |
| `target` | string | only with `moved`: the candidate's name |

### `propagation.json` — what a pass decides about each file (§15.12)

| Field | Type | Meaning |
|---|---|---|
| `direct` | string | `deletes.direct` |
| `files` | array of objects | `path`, `state` (one of §4), `follow` when a folder renamed gives it a new path, and `copies`: `disk`, `position`, `verdict`, `target`, `due`, `gone_ns`, `created_by`, `first_seen`, `verified`, `old_path`, `guard_held` and `size`, 0 when absent |
| `taken` | array of strings | optional: the paths held by files outside this step |
| `decisions` | array of objects | for each file, by path: `path`, `kind` (`none`, `wait`, `held`, `rename`, `path-taken`, `deletion`, `kept` or `gone`), and when they apply `target`, `src`, `drop_hash` and `clear_conflict`, then `copies`, each `disk` and `action` (`follow`, `pending`, `clear`, `own-file`, `other-target`, `stay`, `drop`, `keep-gone` or `trash`), with `target` for `other-target` |
| `error` | boolean | `true` on an unknown word, a path that appears twice, a disk or a position that appears twice in one file, or a `moved` with no target |

### `modification.json` — what a pass decides about a file a copy of which changed (§15.4)

| Field | Type | Meaning |
|---|---|---|
| `file` | object | `state` (one of §4), `size`, `hash` when it has one, and `running` when a job of it is running |
| `copies` | array of objects | in any order, pending copies left out: `disk`, `position`, `verdict`, `other` when a `replaced` copy holds what is no regular file or the file another copy of its disk moved to, `state`, `size` (what its look found for a changed copy, what its row says otherwise), `due` when a changed copy's `settle` has passed, `readable` when its disk can be read, and `hash` when it has been read. Booleans are `false` when absent |
| `decision` | object | `kind` (`none`, `wait`, `read`, `modified`, `metadata` or `conflict`), and when they apply, by position: `read`, the copies to read before deciding; `changed`; `stale`, the copies a modification makes stale; `reference`, the copy read to compare with where the file had no hash; `hash`, what the file's hash becomes |
| `error` | boolean | `true` on an unknown word, a pending copy, a disk or a position that appears twice, or `other` on a copy not `replaced` |

### `metadata.json` — what a metadata-only change applies to the other copies (§15.4)

| Field | Type | Meaning |
|---|---|---|
| `copies` | array of objects | in any order, pending copies left out: `disk`, `position`, `changed` when it is a changed copy, `state`, `row_mtime` what its row says, `mtime` and `ctime` what its look found, for a changed copy only, `reachable` when it is on a disk the pass can write to, where its look reached it, and `precision_ns`, its disk's `time_precision_ns`, absent when unknown, which compares exactly (§9); `row_mode`, the mode its row says, absent when it has none, and for a changed copy `look_mode`, the mode its look found; both integers, and a mode absent is unknown; `row_owner` and `look_owner` likewise, each an object of `uid` and `gid`. Booleans are `false` when absent |
| `plan` | object | `mtime`, the time applied, and `from`, the changed copy that gives it, both absent when no time moved; `apply`, the copies that get it through an operation `utimens`; `mode` and `mode_from`, the same for the mode, and `chmod`, the copies that get it through an operation `chmod`; `owner` and `owner_from`, the same for the owner, an object of `uid` and `gid`, and `chown`, the copies that get it through an operation `chown`; `stale`, those that go stale instead, once whatever they lack; each by position, absent when empty |
| `error` | boolean | `true` on an unknown state, a pending copy, a disk or a position that appears twice, a mode outside 0 to 4095 (`07777`), or an id outside 0 to 4294967295 |

### `guard.json` — whether the guard of a disk trips (§15.6, §29.5)

| Field | Type | Meaning |
|---|---|---|
| `count` | integer | what counts toward `files` |
| `count_percent` | integer | what counts toward `percent` |
| `rows` | integer | the copies on that disk the share is taken of |
| `files`, `percent` | integer, string | `deletes.mass_guard.files` and `percent` |
| `tripped` | boolean | whether it trips |

### `original.json` — the original of a file (§7)

| Field | Type | Meaning |
|---|---|---|
| `copies` | array of objects | the copies, in any order: `disk`, `position`, `state`, `disk_state` and `first_seen_ns`, all required, and `creation_ns`, left out when the creation time is absent |
| `previous` | string | optional: the previous original, given for a file in conflict only |
| `original` | string | the disk of the original; empty when no copy is eligible |
| `error` | boolean | `true` when a state is unknown, a disk has no name, or a disk or position appears twice |

### `creation_time.json` — a creation time into `btime_ns` (§7)

| Field | Type | Meaning |
|---|---|---|
| `seconds` | integer | seconds since the Unix epoch |
| `nanoseconds` | integer | nanoseconds counted forward from those seconds, also before the epoch |
| `ns` | integer | the value of `btime_ns` |
| `absent` | boolean | `true` when the time is absent: exactly 0, or out of range |
| `error` | boolean | `true` when `nanoseconds` is outside 0 to 999999999 |

### `time_ns.json` — a modification or change time into the catalog (§9)

| Field | Type | Meaning |
|---|---|---|
| `seconds` | integer | seconds since the Unix epoch |
| `nanoseconds` | integer | nanoseconds counted forward from those seconds, also before the epoch |
| `ns` | integer | the value stored; the nearest limit when the time is out of range |
| `error` | boolean | `true` when `nanoseconds` is outside 0 to 999999999 |

### `same_time.json` — two times at a precision (§9)

| Field | Type | Meaning |
|---|---|---|
| `a`, `b` | integer | two times, in nanoseconds |
| `precision_ns` | integer | the precision; 0, unknown, and 1 compare exactly |
| `same` | boolean | whether each, divided by the precision and rounded down, toward the past, gives the same number |

### `min_free.json` — what `min_free` leaves to fill (§13.1)

| Field | Type | Meaning |
|---|---|---|
| `min_free` | array of strings | the setting: sizes and percentages |
| `capacity` | integer, or `null` | the disk's capacity in bytes; `null` when it is not known, as for a disk no scan has found (§21.1) |
| `reserved` | integer, or `null` | what stays free: the smallest of the values; with a capacity of `null`, the smallest size, or `null` with percentages only |
| `usable` | integer | what the solver may fill: `capacity` minus `reserved`, or 0; left out with a capacity of `null` |
| `error` | boolean | `true` when a value is refused, the list is empty, or `capacity` is negative |

### `daemon_wake.json` — when the daemon checks, passes and sleeps (§15.13)

| Field | Type | Meaning |
|---|---|---|
| `now_ns` | integer | the time asked about |
| `check_due_ns` | integer | when the mount check is next due |
| `first`, `gone`, `back` | boolean | no pass since the daemon started; a disk went `missing`; a disk came back |
| `pass_due_ns` | integer | the last pass's `next_due`; left out when none |
| `group_due_ns` | integer | when a waiting group of the watcher ripens; left out when none |
| `ripe_origins` | array of strings | who made the groups ripe now; left out when none is |
| `last_pass_end_ns` | integer | when the last pass ended; left out before the first |
| `rescan_every_ns` | integer | `maintenance.rescan_every` |
| `window`, `timezone` | string | `maintenance.window`, and the IANA zone its wall clock is in |
| `reloaded` | boolean | the configuration was read again since the last pass |
| `hash_pending` | boolean | files may be waiting for their hash |
| `scrub_pending` | boolean | copies may be due for the scrub |
| `scrub_window` | string | `integrity.window` |
| `scrub_budget_ns` | integer | `integrity.scrub_budget`, which must be positive when `scrub_pending` |
| `scrub_spent_ns`, `scrub_spent_opened_ns` | integer | what the scrub spent, and when the stretch of its window it spent it in began |
| `backup_pending` | boolean | the stretch of the window that holds now, or the next one, has no catalog backup yet |
| `check` | boolean | `true` when the mount check is due |
| `because` | array of strings | why a pass is due, in the order `start`, `reload`, `gone`, `back`, `events`, `due`, `rescan`; empty when none is |
| `read_hashes` | boolean | `true` when the hashes of new files are to be read now: `hash_pending`, inside the window |
| `backup` | boolean | `true` when the catalog is to be backed up now: `backup_pending`, inside the window, and no hash to read |
| `scrub` | boolean | `true` when the scrub is to run now: `scrub_pending`, inside `scrub_window`, its budget not spent in this stretch, no hash to read and no backup due |
| `scrub_opened_ns` | integer | with `scrub_pending`, inside `scrub_window`, when this stretch of it began; left out otherwise |
| `wake_ns` | integer | when to look again: `now_ns` when something is due |
| `error` | boolean | `true` for a window the grammar refuses, an origin outside the list, a `rescan_every_ns` that is not positive, or a scrub budget that is not, with `scrub_pending` |

### `event_grouping.json` — the watcher's events grouped per path (§15.1)

| Field | Type | Meaning |
|---|---|---|
| `debounce_ns` | integer | `deletes.debounce`, in nanoseconds |
| `events` | array of objects | the events, in the order they came: `disk`, `path` or `path_b64` (logical, `""` for the root), `kind` (`created`, `written`, `attrib`, `deleted`, `moved-from`, `moved-to` or `overflow`), `folder` (`true` or left out), `origin` (`view`, `atoll`, `user` or `unknown`) and `at_ns` |
| `now_ns` | integer | the time the ripe groups are asked for |
| `ripe` | array of objects | the groups ripe at `now_ns`, in their order: `disk`, `path` or `path_b64`, `folder` (`true` when one of its events names a folder, or left out), `kinds` and `origins` (each a set in byte order), `first_ns` and `last_ns` |
| `waiting` | integer | how many groups are not ripe yet |
| `next_due_ns` | integer | the earliest time one of them will be ripe; left out when none waits |
| `error` | boolean | `true` when an event has a kind or an origin outside the lists, an overflow is not at the root, or the debounce is negative |

### `capacity_strict.json` — capacity with strict domains (§13.1)

| Field | Type | Meaning |
|---|---|---|
| `domains` | array of arrays of integers | the sizes of each domain's disks, in any order |
| `k` | integer | the copies per file: `replicas` + 1 |
| `capacity` | integer | the capacity in bytes, rounded down |
| `error` | boolean | `true` when `capacity.json` would refuse the input, or a domain has no disk |

### `blake3.json` — file and chunk hashes (§16, §29.3)

| Field | Type | Meaning |
|---|---|---|
| `length` | integer | the file's size: that many bytes cycling through 0 to 250, as in the official BLAKE3 test vectors |
| `chunk_size` | integer | the size of a chunk in bytes |
| `file` | string | the whole-file hash, in lowercase hexadecimal |
| `chunks` | array of strings | the hash of each chunk, in order; empty for an empty file |
| `error` | boolean | `true` when `chunk_size` is not positive |

The hashes come from the official `b3sum`, never from an implementation under test.

### `config.json` — loading a configuration (§24)

A case holds the text of a file, and says what loading it gives. Keys are written as §24.3 says, such as `disk[0].uuid`. Warnings about a later phase (§24.4) are never compared, as they change with each phase, and neither is `view.engine = "atoll"`.

| Field | Type | Meaning |
|---|---|---|
| `toml` | string | the text of the file, when it is valid UTF-8 |
| `toml_b64` | string | otherwise, its bytes in base64, with the rules of `path_b64` |
| `error` | boolean | `true` on a syntax error (step 1), which has no key |
| `errors` | array of strings | the keys with a problem (step 2, or step 3 when step 2 found none): each once, in byte order, and no others |
| `effective` | array of objects | values the file must load to, each `{"key": …, "value": …}` with the value as in the file: a string, an integer, a boolean, a list of these, or an empty table. A value in a non-empty `path_map` has its own key. A list of tables is listed item by item (`rule[0].label`), and is the value `[]` when empty |
| `complete` | boolean | `true` when `effective` lists every key the configuration has a value for |
| `not_set` | array of strings | keys that must have no value |
| `warnings` | array of strings | the keys of the warnings about names no disk carries (§24.4), in byte order |

A case has `error`, or `errors`, or at least one of `effective`, `not_set` and `warnings`, and loads in the last case. `path`, `filename` and `ext` are shown as written, not compiled. A rule's keys hold what the file sets, plus `action` and `ignore_case`: its pool-wide values (§24.2) apply when it gives a file its policy, which these vectors do not cover.

### `config_edit.json` — the disk commands' edits of the configuration (§10, §18)

| Field | Type | Meaning |
|---|---|---|
| `toml` | string | the text of the file |
| `op` | string | `add`, `remove` or `state` |
| `disk` | object | with `add`: the `[[disk]]` to append, its keys `id`, `uuid`, `mount`, `fs`, `role`, `domain`, `tags`, `root`, `workdir` and `state`, each written in this order and left out when absent |
| `id` | string | with `remove` and `state`: the disk |
| `state` | string | with `state`: the new state, or `""` to take the key out |
| `result` | string | the whole text after the edit, byte for byte |
| `error` | boolean | `true` when the edit is refused: no such disk, disks written as an array of inline tables, or a table below a disk |

### `alerts_plan.json` — what the daemon sends after a pass (§23)

| Field | Type | Meaning |
|---|---|---|
| `holding` | array of objects | the conditions that hold, each `key` and `count` |
| `stored` | array of objects | the rows of the table `alerts`: `key`, `first_ns`, `sent_ns` and `count` |
| `now_ns`, `remind_ns` | integer | the time, and how often a condition that lasts is sent again |
| `send` | array of objects | what to send, in key order: `key`, and `why`, `onset`, `more` or `reminder` |
| `store` | array of objects | the rows to write once those are sent, as `stored` |
| `keep` | array of objects | the rows of the conditions that hold and are not sent now, with their counts now |
| `drop` | array of strings | the keys of the conditions that ended, in byte order |
| `error` | boolean | `true` for a key held twice, a negative count, or a `remind_ns` that is not positive |

### `policy.json` — the effective policy of a file (§11.3, §12.2, §24.2)

A case loads a configuration, then asks for the policy of one file. The values are the ones the configuration asks for: the fallbacks of a phase not yet implemented (§12.2) are not applied.

| Field | Type | Meaning |
|---|---|---|
| `toml` | string | the text of the configuration |
| `toml_path` | string | or the path of a file of the repository that holds it, from its root, with `/` |
| `path`, `path_b64` | string | the logical path of the file |
| `size` | integer | its size in bytes; left out when it is not known |
| `rule` | string | the `label` of the rule that matches |
| `no_rule` | boolean | `true` when no rule matches |
| `policy` | object | every value of the policy: `action`, `replicas`, `mode`, `settle_ns`, `priority`, `tags`, `tags_mode`, `domains`, `spread`, `spread_mode`, `colocate`, `overflow`, `cache`, `cache_min_age_ns`. With `action = "ignore"`, only `action` |
| `sources` | object | where some of these come from, by the same names (without `_ns`): `rule`, `file`, `preset` or `default` |

### `plan.json` — the placement plan (§12.4, §13.4)

A case gives a pool and its files, and the whole plan: `jobs`, then `results`, which must match exactly and in order.

| Field | Type | Meaning |
|---|---|---|
| `disks` | array of objects | `id`, `position`, `capacity` and `free`, in bytes; `state` (`online` when left out), `role` (`data`), `fs` (`ext4`), `domain` (none), `tags` (none), `mount` (`/mnt/<id>`), `root` (`media`), `past_grace` (`false`) |
| `files` | array of objects | `path` or `path_b64`, `size`, `copies`; `manual` (`false`), for a file that waits for `atoll replicate`; `policy` (below) |
| `copies` | array of objects | `disk`; `state` (`ok`), `created_by` (`user`), `first_seen_ns` (0), `old_path` (none), `away` (`false`): `true` for a copy whose clock runs (`specs.md` §15.12), which counts and is no source |
| `policy` | object | `replicas` (1), `priority` (0), `tags` (none), `tags_mode` (`prefer`), `domains` (none), `spread` (`domain`), `spread_mode` (`prefer`), `overflow` (`allow`) |
| `entries` | array of objects | `disk`, `path`, `kind`: `dir`, `symlink` or `ignored` |
| `min_free` | array of strings | as in the configuration; `["0%"]` when left out |
| `jobs` | array of objects | `kind` (`refresh` or `replicate`), `path` or `path_b64`, `src`, `dst` |
| `results` | array of objects | for each file with something to say: `path` or `path_b64`; `states`, `surplus`, `back`, `cancel` (disks by position), `overflow`. What is left out is empty or false |
| `error` | boolean | `true` when the input is refused |

### `catalog.json` — what the catalog schema accepts and refuses (§9, §29.3)

Each case runs on a catalog with every migration applied and `catalog_fixture.sql` loaded, in a transaction that is rolled back, with the pragmas of §9. Any SQLite 3.38 or later must agree, so these need no Atoll code to run.

| Field | Type | Meaning |
|---|---|---|
| `sql` | array of strings | statements, run in order |
| `error` | boolean | `true` when the last statement must fail, and only it |
| `query` | string | a query to run after the statements, which must all succeed |
| `rows` | array of arrays | what `query` must return, in order: integers, strings or `null` |

### `mountinfo.json` — the mount table (§10)

The text of `/proc/self/mountinfo`, one line per mount: `id parent major:minor root mount-point mount-options [optional fields] - type source superblock-options`. Fields are separated by exactly one space, so an empty source is an empty field. The separator is the first `-` from the seventh field on, so a root named `-` is a root. It is followed by the type, the source, and the superblock options, which are the rest of the line, as they come from each driver. `\ooo`, three octal digits up to `\377`, stands for one byte in every field but the numbers, and any other backslash stays as it is: 9p writes `path=C:\` (measured). Numbers are decimal digits only; `major` and `minor` are unsigned 32-bit, `id` and `parent` unsigned 64-bit. Options are split at each comma before their escapes are read, and an empty field is an empty list. The last newline is optional. A line that cannot be read, an empty one included, is counted, and the others stand.

| Field | Type | Meaning |
|---|---|---|
| `text`, `text_b64` | string | the table |
| `entries` | array of objects | the entries it could read, in order: `id`, `major`, `minor`, `root`, `mount_point` or `mount_point_b64`, `mount_options`, `fstype`, `source`, `super_options` |
| `unreadable` | integer | the lines it could not read; 0 when left out |
| `find` | integer | optional: a mount id to look for |
| `finds` | string | with `find`: the id, when an entry has it; `nothing`, when none has and every line could be read; `unknown`, when none has and a line could not be read |

### `superblock.json` — the identifier of a filesystem (§10)

| Field | Type | Meaning |
|---|---|---|
| `fs` | string | the disk's `fs` |
| `length` | integer | the size of the head: that many bytes, all zero but for `bytes` |
| `bytes` | array of objects | what the head holds: `at`, an offset, and `hex`, the bytes |
| `id` | string | the identifier |
| `variant` | string | `casefold` or `ascii-ci`, when the superblock says so; left out otherwise |
| `error` | boolean | `true` when the head holds no such filesystem, or `fs` is unknown |

### `time_precision.json` — how finely a filesystem holds a time (§9, §10)

| Field | Type | Meaning |
|---|---|---|
| `fs`, `length`, `bytes` | | the head, as in `superblock.json` |
| `time_precision_ns` | integer | the precision, in nanoseconds |
| `error` | boolean | `true` when the head holds no such filesystem |

### `disk_check.json` — the mount check (§10)

A case gives the answers a machine would give to each question, and the verdict. What is left out is a healthy answer, for a disk with `fs` `ext4` (when left out), `uuid` `3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b` (`01D9F3A2B4C5E6F7` on `ntfs`) and `mount` `/mnt/t1`.

| Field | Type | Meaning |
|---|---|---|
| `fs`, `uuid`, `mount` | string | the disk in the configuration |
| `path` | string | `ok` (when left out), `absent` or `io-error`: question 1 |
| `no_entry` | boolean | the table has no entry for the mount id of the path |
| `entry` | object | the entry, where it is not the healthy one: `mount_point` (`mount`), `root` (`/`), `fstype` (`ext4`, `xfs`, `btrfs`, or `ntfs3` on `ntfs`), `mount_options` (`["rw"]`), `super_options` (`["rw"]`, and `iocharset=utf8` on `ntfs3`) |
| `device` | string | `ok` (when left out), `gone` or `unreadable`: questions 5 and 6 |
| `no_magic` | boolean | the head does not hold the magic of `fs` |
| `id` | string | the identifier in the head (`uuid` when left out) |
| `variant` | string | `casefold` or `ascii-ci`, when the head says so |
| `root_folder` | string | `ok` (when left out), `absent`, `not-a-folder`, `other-mount` or `io-error`: question 10 |
| `other_case` | boolean | `<root>` also answers in the other case, with the same inode |
| `state` | string | `online`, `readonly` or `missing` |
| `reason` | string | the reason; left out when `online` |

### `capacity.json` — capacity of a pool (§13.1)

| Field | Type | Meaning |
|---|---|---|
| `sizes` | array of integers | the size of each disk in bytes, in any order |
| `k` | integer | the copies per file: `replicas` + 1 |
| `capacity` | integer | `U(k)` in bytes, computed exactly, then rounded down; 0 with more copies than disks |
| `error` | boolean | `true` when the input is rejected: `k` below 1, a negative size, or a total that does not fit in a signed 64-bit integer |

## Unicode versions

The Go and Rust libraries do not use the same Unicode version (`specs.md` §29.5). Vectors that involve Unicode data (`nfc.json`, `match.json`, `regex.json`, `lpath_key.json`, `ntfs_collision.json`) only use characters assigned in Unicode 16.0, with expected results that no later version of the libraries' data changes. No accepted pattern and no path holds a run of non-starters long enough for Go's normalization to insert U+034F (`specs.md` §29.5); the Go tests check it. Rejected patterns may: both versions reject them before they normalize them.
