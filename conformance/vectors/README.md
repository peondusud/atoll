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

These vectors pin the matcher alone, which compares characters exactly. The NFC form and the case folding of `ignore_case` (§11.3) apply to rule conditions; their vectors will come with the rule matcher.
