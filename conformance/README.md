# Conformance suite

The language-neutral contract between the Go and Rust versions of Atoll (`specs.md` §29.4).

| Folder | Content |
|---|---|
| `vectors/` | Input and expected output for the pure functions: rule matching, globs and the regex subset, durations, sizes and time windows, name compatibility per filesystem, NTFS collisions, original selection, capacity formula, solver plans, BLAKE3 file and chunk hashes. Unit tests of both versions load the same files. |
| `scenarios/` | Black-box cases: initial disk trees, a sequence of actions (CLI commands, direct file operations, operations through mergerfs, disconnections, crashes), expected trees, catalog content and `--json` outputs. |
| `corpus/names/` | The name corpus of `specs.md` §15.8, with a verdict per filesystem for each name. |
| `runner/` | Drives any `atoll` binary through its CLI and the filesystem only. It imports nothing from `internal/`. |

Every behavior change first adds or updates a vector or scenario.

Run the scenarios with `make conformance`. Their file format is not defined yet, so the runner refuses any scenario it finds instead of passing over it.
