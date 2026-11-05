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
- A flag (`error`, `no_key`) is `true` or absent, never `false` or `null`, so a case has one spelling. Go's decoder would read `null` as `false` where Rust's fails. The loader checks it.
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

### `capacity.json` — capacity of a pool (§13.1)

| Field | Type | Meaning |
|---|---|---|
| `sizes` | array of integers | the size of each disk in bytes, in any order |
| `k` | integer | the copies per file: `replicas` + 1 |
| `capacity` | integer | `U(k)` in bytes, computed exactly, then rounded down; 0 with more copies than disks |
| `error` | boolean | `true` when the input is rejected: `k` below 1, a negative size, or a total that does not fit in a signed 64-bit integer |

## Unicode versions

The Go and Rust libraries do not use the same Unicode version (`specs.md` §29.5). Vectors that involve Unicode data (`nfc.json`, `match.json`, `regex.json`, `lpath_key.json`, `ntfs_collision.json`) only use characters assigned in Unicode 16.0, with expected results that no later version of the libraries' data changes. No accepted pattern and no path holds a run of non-starters long enough for Go's normalization to insert U+034F (`specs.md` §29.5); the Go tests check it. Rejected patterns may: both versions reject them before they normalize them.
