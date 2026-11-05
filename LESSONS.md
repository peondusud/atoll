# Lessons learned

What this work taught, so the next change goes faster and breaks less. Finished work is in [DONE.md](DONE.md), open work in [TODO.md](TODO.md).

## Process

- **Verify independently, every time.** Each review round found real problems the previous round missed:
  - a validator that accepted `[!-&&]`, which the spec rejects;
  - a weight that let anchors, captures and nested `?` cost nothing;
  - a figure that measured the wrong quantity;
  - a key version that ignored the library release.

  Spec-only ports in another language, written without reading the Go code, are the best way to find ambiguous spec text.
- **Vectors first, then code, then a mutation check.** Breaking each rule on purpose and seeing a test fail shows which clauses the vectors really pin. One commit message claimed every weight clause was pinned: a mutation run showed `+`, `?`, `\z` and the lazy `?` were not.
- **Measure what the limit actually bounds.** Rust's `size_limit` is checked against the memory used while the automaton is built. The finished automaton is smaller: 2.8 MB against 4.2 MB for the same regex. State the measurement method next to every number in the spec.
- **A bound must cover every construct that costs something.** The first weight rule counted characters and classes only. 989 anchors repeated 1000 times then weighed 1000 and needed three times Rust's limit.
- **Ask about contracts, with a recommendation and its cost.** The maintainer took the recommended option every time when it came with measured numbers and the side effect stated plainly (for example "`^\pL{10}$` becomes refused"). Facts that follow from an earlier decision do not need a new question. Neither do clarifications of what the code already does.
- **Commit messages must be exact.** Say what changed and what was verified, never more.

## Unicode and normalization

- Go's x/text inserts U+034F after 30 non-starters (UAX #15 stream-safe format), even in NFKD. Rust's `unicode-normalization` does not.
  - Go counts non-starters in the NFKD of each character: a nonzero combining class, or NFC_Quick_Check = Maybe.
  - To count them, decompose character by character, never with `norm.NFKD` on the whole string, which inserts U+034F itself.
- Two x/text releases with the same Unicode version can still normalize differently (v0.41.0 and v0.43.0). A version tag for cached keys must name the release.
- Go's `encoding/json` silently turns an unpaired `\uD800` escape, or an invalid byte, into U+FFFD, while `serde_json` fails. Shared JSON files must be valid UTF-8 with no unpaired surrogate escape; other names go in a `*_b64` field.
- NFC can change regex syntax: a combining mark after an escape letter or a range endpoint composes with it (`\n` followed by U+0303 becomes `\ñ`). The grammar must apply to the NFC form, and errors must show the typed text.
- A UTF-16 length is never more than the UTF-8 length of the same valid text. So on NTFS the 255-byte limit always binds before the 255-unit limit.

## Filesystems and Windows

- Linux NTFS drivers accept names Windows cannot open. Even with `windows_names`, they accept `CON .txt`, `COM¹`, `CONIN$` and U+007F. ntfs3 on Linux 7.3 reads `a:b` as a stream of `a`, and a rename onto `m:n` replaces `m`. Never rely on the driver: check names with a fixed table.
- Windows 10 and 11 differ on device names with an extension. Microsoft's list changed in 2022, 2023 and 2025. Follow the strictest version as a table; never call `RtlIsDosDeviceName_U`.
- ntfs-3g gives `EINVAL` for `RENAME_NOREPLACE`, and its `stat` of an 8.3 short name returns another file.
- lowntfs-3g limits names to 255 bytes, and mount information cannot tell it apart from ntfs-3g.

## Tooling in this environment

- This cloud sandbox runs as root, with loop devices, FUSE, `mkfs.*`, `mkntfs` and ntfs-3g, so `sudo make test-loopdev` runs here. ntfs3 and ext4 `casefold` are not in its kernel: test them in qemu with a mainline kernel.
- `learn.microsoft.com` does not resolve here, and GitHub pull-request pages are blocked. Read Microsoft pages from their GitHub source repositories (MicrosoftDocs/win32, dotnet/docs).
- `\u` escapes typed into a shell heredoc can reach the file as raw characters. Write such text through a Python script, check it with `cat -A` or `grep -P '[^\x00-\x7F]'`, and prefer `\u` escapes in Go source for invisible or confusable characters.
- `go test` runs in the package folder: pass absolute paths in environment variables.
- golangci-lint rules here:
  - `noctx`: use `exec.CommandContext` with `t.Context()`, or `context.Background()` for a mount helper that must outlive its command;
  - `gochecknoglobals` in the pure core: use a `switch`, not a global map;
  - `depguard`: no `math/rand` in the pure core, so use a small deterministic xorshift in tests;
  - `staticcheck` QF1002: a tagged `switch`.
- Patch files with scripts that assert how many times each old string occurs, so a silent no-op is impossible.
- Go 1.27 runs through `GOTOOLCHAIN=go1.27.0`. golangci-lint is in the session scratchpad's `bin/`.
