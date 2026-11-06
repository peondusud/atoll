# Lessons learned

What this work taught, so the next change goes faster and breaks less. Finished work is in [DONE.md](DONE.md), open work in [TODO.md](TODO.md).

## Process

- **Verify independently, every time.** Each review round found real problems the previous round missed:
  - a validator that accepted `[!-&&]`, which the spec rejects;
  - a weight that let anchors, captures and nested `?` cost nothing;
  - a figure that measured the wrong quantity;
  - a key version that ignored the library release.

  Spec-only ports in another language, written without reading the Go code, are the best way to find ambiguous spec text. Judged by the same conformance runner as the Go binary, one passed 21 of 24 scenarios from the documents alone, and all three failures were gaps in the documents: a list of settings that lived only in `phase.go`, and a `--size` form nobody had written down.
- **Vectors first, then code, then a mutation check.** Breaking each rule on purpose and seeing a test fail shows which clauses the vectors really pin. One commit message claimed every weight clause was pinned: a mutation run showed `+`, `?`, `\z` and the lazy `?` were not.
- **A mutation run proves nothing unless the tests pass first.** One run showed every mutation as caught, while a hand-written vector was wrong and the test failed before any mutation. Read the baseline before the mutations.
- **Compare libraries at equal core counts.** At the default `GOMAXPROCS`, a BLAKE3 library looked three times faster than another because it used all 20 cores; on one core the other was 36% to 42% faster. Hundreds of allocations in a hash function were the clue.
- **Measure what the limit actually bounds.** Rust's `size_limit` is checked against the memory used while the automaton is built. The finished automaton is smaller: 2.8 MB against 4.2 MB for the same regex. State the measurement method next to every number in the spec.
- **A bound must cover every construct that costs something.** The first weight rule counted characters and classes only. 989 anchors repeated 1000 times then weighed 1000 and needed three times Rust's limit.
- **Ask about contracts, with a recommendation and its cost.** The maintainer took the recommended option every time when it came with measured numbers and the side effect stated plainly (for example "`^\pL{10}$` becomes refused"). Facts that follow from an earlier decision do not need a new question. Neither do clarifications of what the code already does.
- **Commit messages must be exact.** Say what changed and what was verified, never more.
- **An example is not a contract.** Reading §24 as a schema turned up four keys that contradicted an invariant or served nothing (`chunk_size`, `keep_overwritten`, `move_originals`, `striping`), and work the spec ran "inside the time window" with no key for it. Check every key against the sections that use it.
- **A vector can pass for the wrong reason.** A new rule refused `tags_mode = "strict"` with no list, and an older case that expected `action = "ignore"` to refuse `tags_mode` went on passing through the new rule. Only a mutation showed it. Rerun the older mutations after adding a rule.
- **Ask what is known when a value is read.** `settle` decides when a file's size may be read, so a rule that needs the size cannot set `settle`. The spec said both things in two places and never put them together.
- **Measure memory before calling a function done.** The first placement plan allocated about 270 MB for 200,000 files, past the whole budget of §26, and passed every test. A per-prefix key map, a large struct per file and a result per healthy file were most of it. A tree of folders, a pointer for what is rare and no result for a healthy file brought what it holds to about 100 MB.
- **A surviving mutation may show a check that does nothing.** Two checks in the conformance runner survived being switched off, because the next step failed anyway on what they guarded. They went, rather than gaining a test that pinned them.
- **Read what a port assumed, not only whether it passes.** A port written from the documents listed that a name in `unreadable` is not also `absent`. The reference did say absent, and for a folder it would have said so of everything below it: a question for a user's data, which no scenario could reach. Its other notes showed that nothing passed the plan which disks are past `missing_grace`.
- **A soft memory limit can be free.** A first scan peaked at 306 MiB with nothing set, and at 184 MiB with `GOMEMLIMIT=170MiB`, in the same 12 s: most of that peak was the collector's headroom, not live data. Measure before shrinking anything further, and measure the time too.
- **Run what a vector assumes on the real thing.** A table of reasons passed all its vectors with `ignore-case` before `ascii-ci`. On a real XFS with `ascii-ci` the root answers in the other case too, so the feature could never be reported, and the message advised a remount that cannot help. Vectors pin an order; only the machine says which order makes sense.
- **A rule about a text the kernel writes must meet real tables.** The vectors said a backslash that escapes nothing refuses the whole mount table, which seemed safe. Docker Desktop's 9p writes `path=C:\` in its options, so one mount that is none of the pool's would have made every disk `missing`. Only the test that runs the delivered binary found it, once a check that hid it was removed. A line says nothing about the other lines.
- **Break the thing, not only the test.** The first measurement of a dead disk looked at a mount, not at the device: `statx` and even `getdents` keep answering on an unplugged ext4, from the cache. Unplug it for real (qemu's `device_del`), and only then choose what to ask.
- **Mutate what only a real machine can judge, on that machine.** Thirteen test binaries, each with one change to the system calls or the real observer, ran in one qemu boot. Ten were caught. One survivor was a check that did nothing, one was already pinned by a unit test, and one showed a guard that matters only on btrfs, with no test.
- **Break the schema too.** A loader driven by a schema is only as tested as each constraint in it: drop every `pattern`, bound, `enum` value and default in turn, in process, and read what no vector notices.
- **`ru_maxrss` can be the parent's.** A harness that kept each child's output showed a command that streams at 260 MiB, and the same command in text at 21 MiB. `exec` copies the peak of the old memory into the child's counter, and after a `vfork`, which is how Go starts a process, that memory is the parent's. With the output thrown away, both peak at 21 MiB. Two numbers that should agree and do not are worth a second measurement before any fix.
- **A value that is displayed cannot also be the only record.** `files.state` held the state a file shows, by an order made for display, and it was also all that remembered a conflict. An outage made `unavailable` win, and when the disks returned the conflict was gone. Two reference implementations agreed on this: only a scenario worked out by hand from what the file should be said otherwise. Ask of each stored field what else could tell the truth if it were overwritten.
- **A panic exits with 2, the code of a usage error.** So a scenario cannot tell a refused command line from a crash on it: `--path` with no value passes either way. Keep one test that calls `run` in process for each command that takes a value.

## Unicode and normalization

- Go's x/text inserts U+034F after 30 non-starters (UAX #15 stream-safe format), even in NFKD. Rust's `unicode-normalization` does not.
  - Go counts non-starters in the NFKD of each character: a nonzero combining class, or NFC_Quick_Check = Maybe.
  - To count them, decompose character by character, never with `norm.NFKD` on the whole string, which inserts U+034F itself.
- Two x/text releases with the same Unicode version can still normalize differently (v0.41.0 and v0.43.0). A version tag for cached keys must name the release.
- Go's `encoding/json` silently turns an unpaired `\uD800` escape, or an invalid byte, into U+FFFD, while `serde_json` fails. Shared JSON files must be valid UTF-8 with no unpaired surrogate escape; other names go in a `*_b64` field.
- Go's `encoding/json` also matches keys without case (even `ſ` for `s`), keeps the last of two equal keys, and decodes `-0` into an integer. Go's base64 skips line breaks even in strict mode. A shared-format loader must check keys, numbers and base64 spelling itself, or Go accepts files that Rust rejects.
- NFC can change regex syntax: a combining mark after an escape letter or a range endpoint composes with it (`\n` followed by U+0303 becomes `\ñ`). The grammar must apply to the NFC form, and errors must show the typed text.
- A UTF-16 length is never more than the UTF-8 length of the same valid text. So on NTFS the 255-byte limit always binds before the 255-unit limit.

## SQLite

- A commit that changes one column rewrites the whole row, so a large BLOB in a row taxes every state change: 780 KB of WAL instead of 5 KB. Keep large BLOBs out of rows that change.
- To measure what a commit writes, turn `wal_autocheckpoint` off. Otherwise the WAL is recycled, and its size says far less than what was written: the first run showed 117 KB for what was really 780 KB.
- In a table that is not `STRICT`, a Go string written to a `BLOB` column is stored as `TEXT` with no error, which Rust cannot read back when the bytes are not UTF-8.

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
- `blkid` without `-p` answers from a cache, and gave the UUID of the image that was on a loop device before. `mkntfs` gives two volumes made in the same second the same serial. ext4 logs "Remounting filesystem read-only" while Linux 6.12 still shows `rw` in the mount table.
- A check needs its answers from somewhere that does not hide a failure, and that does not wake a disk: a buffered read of a device costs no device read once cached, and an `O_DIRECT` read always reaches it.
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
