# Conformance suite

The language-neutral contract between the Go and Rust versions of Atoll (`specs.md` §29.4).

| Folder | Content |
|---|---|
| `vectors/` | Input and expected output for the pure functions: rule matching, globs and the regex subset, NFC run by run, durations, sizes, percentages and time windows, name compatibility of whole paths, NTFS collisions, path lengths, the name report, the state a file shows, original selection, creation, modification and change times, capacity formula with `min_free` and strict domains, solver plans, BLAKE3 file and chunk hashes, the configuration, the effective policy of a file. Unit tests of both versions load the same files. |
| `scenarios/` | Black-box cases: initial disk trees, a sequence of actions (CLI commands, direct file operations, operations through mergerfs, disconnections, crashes), expected trees, catalog content and `--json` outputs. |
| `corpus/names/` | The name corpus of `specs.md` §15.8: single names, with the reasons each filesystem refuses them for. Unit tests of both versions load it, and the loop-device tests create its compatible names on real filesystems. See [`corpus/names/README.md`](corpus/names/README.md). |
| `runner/` | Drives any `atoll` binary through its CLI and the filesystem only. It imports nothing from `internal/`. |

Every behavior change first adds or updates a vector or scenario.

The vector format is described in [`vectors/README.md`](vectors/README.md); the Go unit tests load the files through `internal/vectors`.

Run the scenarios with `make conformance`. Their file format is not defined yet, so the runner refuses any scenario it finds instead of passing over it.
