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
- Unknown fields are errors, so a typo cannot silently disable a check.
- An `error` case has `"error": true` and no expected output. It only says that the input is rejected: error messages and their wording are not part of the contract.
- Integers are exact JSON integers within the signed 64-bit range; they are never written as strings or floats.
- Paths follow `specs.md` §29.3: `path` holds a path that is valid UTF-8; otherwise `path_b64` holds its raw bytes in standard base64 (RFC 4648, with padding). A case has exactly one of the two.
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
| `error` | boolean | `true` when the grammar rejects `pattern` |

Every accepted pattern comes with one path that shows its meaning, the same in Go `regexp` and Rust `regex`.

### `lpath_key.json` — collision keys (§15.7, §15.8)

| Field | Type | Meaning |
|---|---|---|
| `path`, `path_b64` | string | the logical path |
| `key` | string | its `lpath_key` |
| `no_key` | boolean | `true` when the path has no key (NULL in the catalog), because it is not valid UTF-8 |

A case has exactly one of `key` and `no_key`. Paths that collide have the same key.

## Unicode versions

The Go and Rust libraries do not use the same Unicode version (`specs.md` §29.5). Vectors that involve Unicode data (`nfc.json`, `match.json`, `regex.json`, `lpath_key.json`) only use characters assigned in Unicode 16.0, with expected results that no later version of the libraries' data changes. No vector holds a run of non-starters long enough for Go's normalization to insert U+034F (`specs.md` §29.5); the Go tests check it.
