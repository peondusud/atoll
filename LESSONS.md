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
- **A mutation run proves nothing unless the tests pass first.** One run showed every mutation as caught, while a hand-written vector was wrong and the test failed before any mutation. Read the baseline before the mutations.
- **Compare libraries at equal core counts.** At the default `GOMAXPROCS`, a BLAKE3 library looked three times faster than another because it used all 20 cores; on one core the other was 36% to 42% faster. Hundreds of allocations in a hash function were the clue.
- **Measure what the limit actually bounds.** Rust's `size_limit` is checked against the memory used while the automaton is built. The finished automaton is smaller: 2.8 MB against 4.2 MB for the same regex. State the measurement method next to every number in the spec.
- **A bound must cover every construct that costs something.** The first weight rule counted characters and classes only. 989 anchors repeated 1000 times then weighed 1000 and needed three times Rust's limit.
- **Ask about contracts, with a recommendation and its cost.** The maintainer took the recommended option every time when it came with measured numbers and the side effect stated plainly (for example "`^\pL{10}$` becomes refused"). Facts that follow from an earlier decision do not need a new question. Neither do clarifications of what the code already does.
- **Commit messages must be exact.** Say what changed and what was verified, never more.
- **An example is not a contract.** Reading §24 as a schema turned up four keys that contradicted an invariant or served nothing (`chunk_size`, `keep_overwritten`, `move_originals`, `striping`), and work the spec ran "inside the time window" with no key for it. Check every key against the sections that use it.
- **Break the schema too.** A loader driven by a schema is only as tested as each constraint in it: drop every `pattern`, bound, `enum` value and default in turn, in process, and read what no vector notices.

## Unicode and normalization

- Go's x/text inserts U+034F after 30 non-starters (UAX #15 stream-safe format), even in NFKD. Rust's `unicode-normalization` does not.
  - Go counts non-starters in the NFKD of each character: a nonzero combining class, or NFC_Quick_Check = Maybe.
  - To count them, decompose character by character, never with `norm.NFKD` on the whole string, which inserts U+034F itself.
- Two x/text releases with the same Unicode version can still normalize differently (v0.41.0 and v0.43.0). A version tag for cached keys must name the release.
- Go's `encoding/json` silently turns an unpaired `\uD800` escape, or an invalid byte, into U+FFFD, while `serde_json` fails. Shared JSON files must be valid UTF-8 with no unpaired surrogate escape; other names go in a `*_b64` field.
- Go's `encoding/json` also matches keys without case (even `ſ` for `s`), keeps the last of two equal keys, and decodes `-0` into an integer. Go's base64 skips line breaks even in strict mode. A shared-format loader must check keys, numbers and base64 spelling itself, or Go accepts files that Rust rejects.
- NFC can change regex syntax: a combining mark after an escape letter or a range endpoint composes with it (`\n` followed by U+0303 becomes `\ñ`). The grammar must apply to the NFC form, and errors must show the typed text.
- A UTF-16 length is never more than the UTF-8 length of the same valid text. So on NTFS the 255-byte limit always binds before the 255-unit limit.

## TOML

- Both Go TOML libraries match keys without case when decoding into a struct: `Pool_ID` is taken for `pool_id`, nothing reports it, and with both spellings one wins in silence. go-toml also reads `[disk]` as `[[disk]]`. Rust's `toml` refuses all of it. Decode into a generic tree and check the keys first.
- "TOML 1.0" was false of every current library: go-toml, BurntSushi and Rust's `toml` all accept 1.1 syntax. Run a contract's stated version against a corpus rather than trusting it.

## Filesystems and Windows

- Linux NTFS drivers accept names Windows cannot open. Even with `windows_names`, they accept `CON .txt`, `COM¹`, `CONIN$` and U+007F. ntfs3 on Linux 7.3 reads `a:b` as a stream of `a`, and a rename onto `m:n` replaces `m`. Never rely on the driver: check names with a fixed table.
- Windows 10 and 11 differ on device names with an extension. Microsoft's list changed in 2022, 2023 and 2025. Follow the strictest version as a table; never call `RtlIsDosDeviceName_U`.
- ntfs-3g gives `EINVAL` for `RENAME_NOREPLACE`, and its `stat` of an 8.3 short name returns another file.
- lowntfs-3g limits names to 255 bytes, and mount information cannot tell it apart from ntfs-3g. Its `-o ignore_case` lists every name in lowercase, and is invisible too.
- mergerfs treats a rename as a success when one branch accepts it, and removes the source from the branches that refused. So `windows_names`, which makes ntfs-3g refuse a name, turns a rename through mergerfs into the silent loss of that replica. Measure what the layer on top does with a refusal, not only the refusal itself.
- Re-read through a cold cache before concluding that a file survived. After a rename onto `m:n` under ntfs3 `ads`, `cat m` still shows the old content from the page cache; `drop_caches` or a remount shows it is gone. A first run read the cache and wrongly contradicted an earlier, correct measurement.
- A filesystem can change a time without an error: ext4 and XFS clamp a modification time to what they can hold, and `utimensat` still returns 0. Read a time back after setting it. Go's `UnixNano` wraps outside its range, too.
- A real-filesystem test must give each name a fresh folder and must test every driver variant: one driver hid the 255-byte rule, and a leftover name made a later one fail.

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
- Patch files with scripts that assert how many times each old string occurs, so a silent no-op is impossible. Write them with the Write tool: a long Bash heredoc holding quotes can fail before it runs.
- Go 1.27 runs through `GOTOOLCHAIN=go1.27.0`. golangci-lint is in the session scratchpad's `bin/`.

## Tooling on the maintainer's Windows machine

- There is no Go or make on the host. Everything compiles in the `golangci/golangci-lint:v2.15.0` image (Go 1.27.2, make, gcc), with the project mounted read-only and copied inside the container.
- Docker Desktop can fail to start with `remove …: The file cannot be accessed by the system`, on `%LOCALAPPDATA%\Docker\run\dockerInference`, then on `%LOCALAPPDATA%\docker-secrets-engine\engine.sock`.
  - `docker desktop stop` sometimes leaves these empty Unix sockets behind, and each failed start recreates the ones before it, so removing one socket at a time never ends.
  - From a tool, Windows refuses to open them, even through the reparse point: `Remove-Item`, `del` and the recycle bin all give error 1920. Git Bash `rm -f` returns 0 and removes nothing, so check with `dir /a`.
  - With Docker stopped, renaming both parent folders and creating them again empty works, since a rename never opens the children. The cause in Windows or Docker is not established.
- A container uses Docker Desktop's kernel (Linux 6.6, WSL2), which has no ntfs3, and no package can add it. That is no reason to stop: a Debian container boots any kernel in qemu in under a minute, even without KVM, from an initramfs built from its own files with `rdinit=/init`. Extract a mainline `.deb` into a separate folder: over `/`, tar could replace the `/lib` symlink.
- Kernel boolean mount options take `0`, `1`, `no`, `yes`, `false` or `true`: `ads=off` is a bad value.
- This machine runs Windows 11, so a Windows question can often be measured here rather than called unmeasured. Check the setting first: with `LongPathsEnabled` = 1, the first programs tried passed at 300 units and showed nothing about the limit; other programs stop at exactly 259.
- The Write tool, too, can turn `\u` escapes into raw characters. In a vector file, check the bytes: NFC and NFD forms look the same once raw.
- In PowerShell, `@{}` ignores case and `-cne` compares by culture, so `é` equals `e` followed by U+0301. Use a `Dictionary` with `StringComparer.Ordinal` and `[string]::CompareOrdinal` to check names. A control that injects a known error shows whether such a check can fail at all.
