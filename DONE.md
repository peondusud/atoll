# Done

Finished work, newest first. Each item names its commits; unless it says otherwise, they are on `claude/funny-allen-jyq7pa` (pull request #2). Open work is in [TODO.md](TODO.md), lessons in [LESSONS.md](LESSONS.md).

## Capacity formula (specs.md §13.1)

Commit "Add the capacity formula and its vectors", on `main`.

- The maintainer's decisions:
  - the result is rounded down to whole bytes;
  - with more copies than disks, the capacity is 0.
- Also stated in §13.1: k is at least 1, no size is negative, and the total fits in a signed 64-bit integer. Any other input is an error.
- Contract: `conformance/vectors/capacity.json` holds 42 cases.
- Code: `solver.Capacity`, with integers only.
- Tests:
  - the values were checked outside Go, in PowerShell with exact integers, against the formula and against the largest x such that k copies of x bytes fit;
  - a property test compares the function with that second definition on 20,000 small pools;
  - of 20 code mutations, 19 make a vector fail. The last, a function that sorts its own input, makes the property test fail.

## One spelling for vector flags (specs.md §29.4)

Commit "Refuse vector flags other than true", on `main`.

- `internal/vectors` refuses `error` and `no_key` with any value but `true`, in every vector file and in the corpus. Only `name_compat.json` refused `"error": false` before.
- `window.json`: an error case can no longer carry `"at": ""`.
- No vector file changed: none used a refused spelling.
- Mutation checks on all nine vector files that have a flag: a flag written as false, an error case with an expected result, and a case with neither. Each test fails on each mutation.

## Name compatibility per filesystem (specs.md §15.8)

Commits `f7b6502`, `10c1c2b`, `b9fbeb3`, and the follow-up after the review.

- Researched:
  - the Windows 10 and 11 rules;
  - ntfs3 on Linux 7.2 and 7.3, measured in qemu;
  - ntfs-3g and lowntfs-3g;
  - ext4, including `casefold`;
  - XFS, including `ascii-ci`, and btrfs.

  Report: [docs/research/name-compatibility.md](docs/research/name-compatibility.md).
- The maintainer's decisions:
  - every NTFS disk follows the Windows rules, as a fixed table in both versions;
  - reserved names as Windows 10 reads them, with `CONIN$` and `CONOUT$`;
  - at most 255 bytes per component on NTFS too;
  - ext4 `casefold`, XFS `ascii-ci` and NTFS `nocase` are refused at `disk add`;
  - every component is checked, folders included;
  - a closed list of six reason codes.
- Contract:
  - `conformance/corpus/names/names.json` holds 101 names with their reasons on ext4, xfs, btrfs and ntfs;
  - `conformance/vectors/name_compat.json` holds 27 whole-path cases;
  - vector files must not hold an unpaired surrogate escape. Keys must be lowercase ASCII and unique, integers plain, and base64 written exactly as the standard encoder writes it, because Go's decoder would otherwise accept what Rust's rejects.
- Code:
  - `internal/names/compat.go`: `ComponentReasons`, `CheckPath`;
  - `internal/vectors` loads the corpus and checks the text rules.
- Tests:
  - an independent Python model agrees on 300,000 random names;
  - mutation checks;
  - fuzzing;
  - `sudo make test-loopdev` creates the corpus on real ext4, XFS and btrfs loop images, and on NTFS with every driver present (ntfs3, ntfs-3g, lowntfs-3g), with and without `windows_names`;
  - an independent review: two spec-only ports agreed with Go on 5.4 million names and paths, and the corpus was replayed on ntfs3 (Linux 7.2 and 7.3), ntfs-3g and lowntfs-3g. The review found lowntfs-3g `ignore_case`, now a refused variant.

## Regex grammar and weight (specs.md §29.5)

Commits `9579074` to `a4e82fa`.

- A positive grammar, the common subset of Go `regexp` and Rust `regex`, which Atoll checks itself in both versions. It is limited to 1000 bytes in NFC form, with Go's nested-repeat product rule.
- A weight limit of 1000:
  - every element that adds to the automaton weighs at least 1 per copy, and a property weighs 100;
  - Rust keeps `size_limit` at 10 MiB and sets `nest_limit` to 1000;
  - the costliest regex found needs 0.41 of the limit.
- A pattern with more than 30 non-starters in a row is rejected. They are counted in the NFKD of each character, exactly where Go's NFC inserts U+034F.
- Class pairs `--`, `&&` and `~~` are rejected anywhere in a class. The grammar applies to the NFC form, and errors quote the typed text.
- Verified by several rounds of independent review:
  - spec-only Rust validators;
  - 4.5 million Go/Rust engine pairs;
  - 5.2 million weight comparisons;
  - adversarial searches for the costliest regexes in Rust.
- Documented and kept as is, by the maintainer's choice:
  - Rust needs an 8 MiB thread to compile the deepest regexes in debug builds;
  - with `ignore_case`, a pathological regex can take about 0.7 s to compile.

## `lpath_key` (specs.md §15.7, §15.8)

Commits `aa7da56`, `5cd9ab7`, `31e7278`, `5fed1ed`, `63e9cf3`.

- The key is NFC of the NTFS upcase table, plus `ς` → `Σ`, applied to NFD.
  - The table is generated from Unicode 5.1 data: 973 entries.
  - It was checked byte for byte against `$UpCase` from Windows 10 images, and from a Windows 11 volume read by the maintainer.
- `meta.lpath_key_version` names the language, the library, its release and its Unicode version: `go golang.org/x/text v0.43.0 17.0.0`. Any difference recomputes every key.

## NFC and case folding in rule matching (specs.md §11.3)

Commits `7db3410` to `34a644d`.

- Research report: [docs/research/nfc-and-case-folding.md](docs/research/nfc-and-case-folding.md).
- NFC comes from `golang.org/x/text/unicode/norm` v0.43.0, applied run by run on invalid UTF-8, only through `internal/names`.
- `ignore_case` uses Unicode simple case folding, exactly as regex `(?i)`.
- Rule conditions are implemented: `path`, `filename` and `ext` (a list), with globs and regexes.
- Vectors: `nfc.json`, `match.json`, `lpath_key.json`.

## Earlier work

Commits `b1cdf1d` to `e384a9c`.

- The specification, CLAUDE.md, the project skeleton, the license, and the `api/`, `schema/` and `deploy/` folders.
- The conformance vector format.
- Value grammars: durations, sizes, percentages, time windows.
- The in-house glob matcher.
