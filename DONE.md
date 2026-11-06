# Done

Finished work, newest first. Each item names its commits; unless it says otherwise, they are on `claude/funny-allen-jyq7pa` (pull request #2). Open work is in [TODO.md](TODO.md), lessons in [LESSONS.md](LESSONS.md).

## Effective policy of a file (specs.md §11.3, §12.4, §24.2)

Commit "Compute the effective policy of a file", on `main`.

- `rules.Effective` takes the first rule that matches and fills what it leaves out from the pool-wide values. `Policy.Sources` says where each value comes from, for `atoll rules explain`. The policy holds what the configuration asks for: the fallbacks of §12.2 are for the code that acts on it.
- The maintainer's decisions:
  - `settle` and `cache` never go with a size condition, as they are read before the size is known;
  - `tags_mode = "strict"` covers `tags` and `domains`, and a disk must satisfy both lists;
  - `strict` from `[placement]` asks nothing of a rule with no list, and `strict` written on such a rule is an error.
- While the size is not known, a rule with a size condition does not match, following §19.
- Vectors: `policy.json`, including the example of §24.1.
- Classifying one file takes 1.8 µs on an i7-12700K with the 10 rules of that example, so 200,000 files take about 0.4 s.

## Configuration schema and loader (specs.md §24)

Commit "Define the configuration schema and load it", on `main`.

- Report: [docs/research/toml.md](docs/research/toml.md). go-toml v2.4.3 and Rust's `toml` 1.2.0 agree on all 746 files of the official corpus; BurntSushi v1.6.0 accepts 16 invalid files they refuse. All three accept TOML 1.1 syntax.
- The maintainer's decisions:
  - `github.com/pelletier/go-toml/v2`, and TOML 1.1 in the contract;
  - `integrity.chunk_size`, `trash.keep_overwritten`, `placement.move_originals` and `read.striping` removed: each contradicted the spec or served nothing;
  - a setting of a later phase is accepted with a warning, and `view.engine = "atoll"` is an error until phase 2a;
  - a `[maintenance]` table for the window, the rescans and the catalog backups, which the other windows follow;
  - rule properties keep their sections of §24, in the order rule, section, preset, default;
  - identifiers in lowercase ASCII, and a required, unique `label` on each rule;
  - `0s` and `0%` refused where they would remove a protection or stop a task.
- `schema/config.schema.json` is the one list of keys, types, allowed values and defaults: `internal/config` reads it at load time. Decoding into a struct cannot tell `Pool_ID` from `pool_id` in either Go library, so the loader checks the generic tree first.
- Vectors: `config.json`. `deploy/atoll.toml` is the example of §24.1, byte for byte.
- A port written in Rust from the spec alone, without the Go code, passes the same vectors. It found the sentences that did not settle a case (a second byte order mark, the case of `\uXXXX`, `catalog` equal to a mount, and others), which were then fixed.

Effective policy, and everything that reads the file from disk, is still open: [TODO.md](TODO.md).

## BLAKE3 library and hashes (specs.md §16, §29.7)

Commit "Hash files and chunks with zeebo/blake3", on `main`.

- Report: [docs/research/blake3.md](docs/research/blake3.md). On one core, zeebo is 36% to 42% faster than lukechampine, with 2 allocations against 10,756 per 64 MiB.
- The maintainer's decisions:
  - `github.com/zeebo/blake3`, main branch, pinned to commit `28a397d`, for its NEON and SVE2 backends;
  - an empty file has no chunks.
- Contract: `conformance/vectors/blake3.json` holds 17 cases, whose hashes come from the official `b3sum`, 10 lengths also checked against the official test vectors.
- Code: `integrity.Hasher`, one read feeding both hashes, with no I/O. Nothing calls it outside its tests yet.
- Not measured: AVX-512, and throughput on real arm64.

## Capacity: `min_free`, strict domains, reachable (specs.md §12.4, §13)

Commit "Define the capacity with min_free and strict domains", on `main`.

- The maintainer's decisions:
  - a disk's size in the formula is its capacity minus `min_free`, for the disks the solver can write to;
  - with fewer copies than domains, each copy goes to a different domain, and a disk with no `domain` forms its own;
  - the reachable capacity is a dry run of the solver, plus the formula on the space left. It waits for the solver.
- Derived: with strict domains, the formula applies to the domains' sizes when k < m; otherwise the capacity is the smaller of the formula and the smallest domain.
- Contract: `conformance/vectors/min_free.json` holds 23 cases, and `capacity_strict.json` 28. `capacity.json` gains the three §13.2 cases with `min_free`.
- Code: `value.MinFree` and `solver.StrictCapacity`. Nothing calls them outside their tests yet.
- Tests:
  - a property test places one-byte files by trying every choice on small pools, and compares with the formula;
  - the cases were recomputed outside Go, in Python with exact integers, from what the numbers mean.

## Copies that differ at `disk add` and `catalog rebuild` (specs.md §4, §9, §15.7, §18)

Commit "Define copies that differ when a disk is added or the catalog rebuilt", on `main`.

- The maintainer's decisions:
  - copies with the same content and different modification times are in sync, each keeps its own time, and nothing is written to them;
  - `files.mtime_ns` holds the original's time;
  - at `catalog rebuild`, different content is a `conflict`, whatever the times.
- Only a later change is applied to the other copies, which makes the times equal again.
- No code and no vector: this lives in the scan and the catalog, which do not exist yet. The scenario is listed in §27, and the scenario format is still to be defined.

## Modification and change times (specs.md §9, §14)

Commit "Define what happens to times a disk or the catalog cannot hold", on `main`.

- Measured on Linux 6.12.115 and 7.3.1:
  - ext4 and XFS clamp a time without an error: a copy to ext4 of an NTFS file dated 1601 holds 1901, and `cp` exits 0;
  - Go's `UnixNano` wraps: 1601 becomes a date in 2185.
- The maintainer's decisions:
  - the copy is made with the time the disk keeps, which is recorded per copy; it stays in sync and the difference is never applied to the other copies. Invariant 6 now says so;
  - a time out of range is stored at the nearest limit, and Atoll sets times from a copy's `stat`, never from the catalog.
- Contract: `conformance/vectors/time_ns.json` holds 26 cases.
- Code: `solver.TimeNS`, with `CreationTimeNS`, in `times.go`. Nothing calls them outside their tests yet.

## Choosing the original (specs.md §7)

Commit "Define the choice of the original", on `main`.

- Measured per driver, on Linux 6.12.115 and 7.3.1: report in [docs/research/file-times.md](docs/research/file-times.md). ext4 reports a crtime of 0 as 1970, and ntfs3 a FILETIME of 0 as 1601, which does not fit in `btime_ns`.
- The maintainer's decisions:
  - creation times are compared in exact nanoseconds;
  - a date of exactly 0, or out of range, counts as absent;
  - every copy that is in sync and can be read is eligible, `surplus` and `draining` included;
  - a file in conflict keeps its previous original while that copy is eligible.
- Contract: `conformance/vectors/original.json` holds 37 cases, and `creation_time.json` 23. `absent` joins the vector flags.
- Code: `solver.SelectOriginal` and `solver.CreationTimeNS`. Disks and the catalog do not exist yet, so nothing calls them outside their tests.
- Left open: modification and change times out of range, in `TODO.md`.

## The name report and the state a file shows (specs.md §4, §15.9)

Commit "Define the name report and the state a file shows", on `main`.

- The maintainer's decisions:
  - one finding per name to fix, the shortest path with the problem, in a flat sorted list;
  - `ext4` and `ntfs` are always checked, whatever the pool holds;
  - exit code 0 whenever the check ran, with or without findings;
  - the name states come before `degraded` in the order of states.
- Also stated:
  - placement and job order read the copies, never the state;
  - the report reads the catalog only, so it wakes no disk;
  - collisions are listed between siblings; a difference in a parent is covered by the finding on that parent;
  - a well-formed logical path is defined in §4.
- Contract: `api/schemas/names-check.schema.json`, the first of the `--json` schemas. `conformance/vectors/names_check.json` holds 56 cases, and `file_state.json` 27.
- Code: `names.Check` and `solver.ShownState`. The CLI and the catalog do not exist yet, so nothing calls them outside their tests, and nothing writes this JSON yet.

## Total path length (specs.md §12.4, §15.8)

Commit "Limit the total path length on mergerfs branches", on `main`.

- Measured:
  - with mergerfs 2.40.2, a copy past 4095 bytes on its branch is listed and `stat` gives `ENOENT`, so it protects nothing, and a deletion or a rename through the view leaves it behind;
  - on Windows 11 with `LongPathsEnabled` = 1, six built-in tools work at 259 UTF-16 units and fail at 260, while `cmd /c type` and PowerShell work at 300.
- The maintainer's decisions:
  - a path past the branch limit is a hard constraint of the solver;
  - a path past the Windows limit is only listed by `atoll names check`.
- Contract: `conformance/vectors/branch_path.json` holds 24 cases, and `windows_path.json` 22.
- Code: `names.FitsBranch` and `names.FitsWindows`. The solver and `atoll names check` do not exist yet, so nothing calls them outside their tests.
- Tests:
  - the cases were generated and checked outside Go, in PowerShell with .NET byte and UTF-16 counts;
  - 24 code mutations each make a vector fail, and 14 mutations of the vector shape each make the test fail.

## ntfs3 `ads` and chunks in the trash (specs.md §10, §15.2, §17)

Commit "Refuse ntfs3 ads and fix the chunk layout in the trash", on `main`.

- Measured in qemu on ntfs3, on Linux 6.12.115 and 7.3.1, re-reading after dropping the caches:
  - on 7.3.1, with `ads` on by default and with or without `windows_names`, a rename through mergerfs onto `m:n` succeeds and replaces the content of the file `m`, even when that was its only copy;
  - `ads=0` stops it, and 7.3.1 then behaves as 6.12, which behaves as ntfs-3g;
  - ntfs3 holds names that differ only by case.
- The maintainer's decisions:
  - ntfs3 with `ads` is refused at `disk add`, and a disk found with it later becomes `readonly`, with an alert;
  - overwritten chunks go to `Atoll/trash/<date>/<op-id>.chunks/<logical path>/<index>`, which holds a file name of 255 bytes.
- Also stated in §17: the index format, and a copy whose name the driver refuses, which cannot reach the trash: it stays, is reported, and is never refreshed or repaired.
- No code: the trash and `disk add` are not written yet. The tests to write are listed in §27.

## Renames to a name a disk refuses (specs.md §15.2, §15.3)

Commit "Decide what happens on a rename a disk refuses", on `main`.

- Measured: when one branch refuses a rename and another accepts it, mergerfs returns success and removes the source from the branch that refused. A file loses that copy, with no trash. A folder that is not empty stays at the old name, and the merged view shows both folders. §15.2 said that renames reach every copy; it now states this.
- The maintainer's decisions:
  - the copy left at the old name goes to the trash, with the new reason `renamed`, and the solver looks for another compatible disk;
  - `windows_names` stays recommended, with its cost stated in §15.8.
- Also stated in §15.3 and §6.1:
  - a copy changed meanwhile stays, as a `conflict`;
  - the renamed copy counts as a user copy, and the user's rename counts as the command that removes a user copy;
  - the mass-deletion guard counts these copies;
  - `renamed` is purged as late as `deleted`.
- No code: the check already exists (`names.CheckPath`, `names.TargetNames`), and propagation is not written yet. The scenario is listed in §27, and the scenario format is still to be defined.

## Name collisions on NTFS targets (specs.md §15.7)

Commit "Refuse name collisions on NTFS targets", on `main`.

- The maintainer's decisions:
  - a colliding copy is blocked: a hard constraint of the solver, not only a report;
  - `entries` gets an `lpath_key` column, so folders are looked up the same way as files.
- Also stated in §15.7:
  - names already side by side on an NTFS disk, created outside Atoll, are only reported;
  - a case-only rename does not collide with its own old name;
  - between two new names, the first in the plan's order gets the disk.
- Contract: `conformance/vectors/ntfs_collision.json` holds 39 cases.
- Code: `names.TargetNames`. The solver and the catalog do not exist yet, so nothing calls it outside its tests.
- Tests:
  - the cases were checked outside Go, in PowerShell with .NET normalization and an upcase built from the rule of §15.8;
  - a property test compares it with a pairwise comparison, on names mixing case, NFC and NFD forms and invalid UTF-8;
  - 11 code mutations each make a vector fail, and 11 mutations of the vector shape each make the test fail.
- Measured: ntfs-3g and, in a later measurement, ntfs3 hold both names, so the drivers do not protect against collisions. Not measured: what Windows does with such names.

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
