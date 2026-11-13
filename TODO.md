# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## Decisions for the alpha (the maintainer, 2026-11-12)

The work goes to the `develop` branch, with one pull request once the big work is done. An ambiguity these do not settle takes the safest option, and DONE.md lists it as decided without the maintainer, to confirm. New contracts follow the style of the existing ones, vectors or scenarios first, each listed for review.

- **Order:** the daemon that runs for good, then ntfy alerts, `atoll names check`, the disk commands, the scrub. The alpha also takes in `atoll catalog backup | verify | rebuild`, `atoll disk fail` and `disk replace`, the owner applied to the other copies, and the local API.
- **The daemon:**
  - **Passes.** A pass walks every disk, as `daemon --until-idle` does: each pass confirms every writable disk anyway, and sparing the disks' sleep is phase 2b.
  - **The lock.** The catalog's lock is taken for each pass, and a command that finds it taken waits for it, saying so, instead of exiting with 1.
  - **`--foreground`.** `atoll daemon` without it is the same as with it.
  - **The account.** A systemd unit runs it as a dedicated user `atoll`, with the capabilities `CAP_SYS_ADMIN`, `CAP_DAC_READ_SEARCH`, `CAP_DAC_OVERRIDE`, `CAP_CHOWN` and `CAP_FOWNER`, the group `disk`, `NoNewPrivileges`, `ProtectHome=read-only` and `PrivateTmp`. Measured: so the user writes mergerfs's runtime branches, and runs `mount` and `umount`.
  - **After a USB reset.** With `health.remount_after_reset`, the remount goes through the fstab entry of the mount point: `umount -l`, then `mount <mount point>`. Without an entry, an alert only. A dirty NTFS volume is never forced.
- **The disk commands:**
  - **`disk add`.** It takes the disk's id from the mount point's last component, or `--id`; one that is not a valid identifier, or is taken, is refused.
  - **`disk remove --evacuate`** is in the alpha. It marks the disk `draining` and returns, and the daemon's passes re-create elsewhere what only that disk holds. Once nothing depends on it, it is `retired`, and `disk remove` without `--evacuate` takes it out of the configuration.
  - **`disk replace u2 --with <mount>`.** The new disk comes in as `disk add` brings one, with an id of its own, and the domain and tags of `u2`. It receives the copies, and `u2` ends `retired`: the catalog never reads what it knows of one disk against another (§9).
  - **`disk fail u2 --yes`.** It re-creates at once the missing replicas, and leaves `u2` `failed` in the configuration: never a source or a target again, its copies counting for nothing, `failed` even if it comes back. `disk remove u2` takes it out.
  - **Configuration changes.** The daemon reads the configuration again only on `SIGHUP`, which `systemctl reload atoll` sends. `disk add` and `disk remove` tell it so themselves. A change made by hand needs that reload.
  - **`chattr +i`.** The flag of the covered mount point is read through a non-recursive bind mount of its parent, in a private, throwaway mount namespace.
- **The scrub.** A corrupt copy goes to the trash when a healthy copy exists, re-verified first, and replication re-creates it. A file with one copy is marked and alerted only. A scrub resumes at the chunk where its budget stopped, through a catalog migration.
- **Alerts.**
  - **When.** The daemon sends them, not the commands: when a condition appears, then every 24 h while it lasts. The files of one pass go in one alert, and the state of the alerts is kept in the catalog.
  - **ntfy.** A new key, `alerts.ntfy_token_file`, gives ntfy's token. `atoll notify test` sends a test alert.
- **The local API.** A Unix socket, and HTTP with a token (§26), described in `api/openapi.yaml`.
- **Settled at the same time:**
  - **The `ntfs` driver** stays `readonly` in the alpha.
  - **A nested btrfs subvolume** is another mount: the walk leaves it out and lists it in `other_mounts`.
  - **The choices DONE.md listed as "to confirm"** are confirmed.
  - **Catalog backups.** They are daily, in `<catalog folder>/backups/`, 7 kept. With `catalog_on_disks`, the latest also goes in each disk's `Atoll/catalog/`. A rebuild sets the old catalog aside.

## Decisions for the beta (the maintainer, 2026-11-12)

- **Order:** repair and quarantine of corrupt copies, then the duplicate report (R-14, R-15); the `immediate` and `scheduled` modes; Gotify and SMTP alerts, and SMART; notifications to Plex, Emby and Jellyfin; `atoll conflicts resolve`; failure domains and tags as hard constraints, with `atoll balance` and `atoll capacity`; the link guard last.
- **Plex** reads each disk's folders today. So its libraries move to the merged view, as §21.4 says, before the first replica, and a scan notification names a folder as that view shows it.
- **Hard links:** none in the maintainer's library for now, but the link guard must handle them, for users whose Sonarr or Radarr link a torrent client's file into `media/`.
- **Alerts:** Gotify and SMTP, both, beside ntfy.
- **The link guard:** files that Sonarr or Radarr already hard-linked to a torrent client's folder get the guard's link too, and Atoll never counts its own link in `nlink`. Turned off after use, the guard's links are removed, journaled, and a link whose file has no other name goes to the trash first.
- **Automatic balancing:** with `balance.schedule = "immediate"`, the trigger is checked at most once an hour, as a check plans the whole pool; with a time window, once a stretch of it; the moves stop at `balance.budget`.
- **The rest of the beta, in order:** the daemon's balancing, then the alert on an NTFS volume marked dirty, then the link guard.

## Names (specs.md §30)

To decide before the solver, the scanner and `atoll names check` need them:

- **decision**, before phase 2a: should Atoll's own view refuse, with an error, a rename that a copy's filesystem refuses, instead of following §15.3?
- Pin in `test/loopdev` what mergerfs and ntfs3 do with a refused name or an `ads` stream (`docs/research/name-compatibility.md`), re-reading through a cold cache, so that a release that changes it fails a test. The measurements so far were throwaway scripts in qemu; `make test-qemu` runs `test/loopdev` on both kernels, and its guest now has mergerfs, which `TestBreakGlass` mounts.
- Measure on the maintainer's Windows machine, with their go-ahead, what a report says is not established: which reserved names Windows 11 still refuses, and what Windows does with `Film.mkv` beside `film.mkv` on an NTFS volume (a VHD).
- `disk add`: refuse ext4 `casefold`, XFS `ascii-ci`, ntfs3 `nocase`, ntfs3 `ads` and lowntfs-3g `ignore_case`, which the mount check of §10 already reports. Watch `FS_CASEFOLD_FL` on folders under `<root>/`.
- **decision**: the `ntfs` driver of Linux 7.1 and later (§10, §15.8, §22), the only NTFS driver of Debian 14's kernel: support it like ntfs3, keep it `readonly`, or something in between. Measured in `docs/research/ntfs-driver.md`, whose last section lists the decisions it needs. Until then a disk mounted with it is `readonly`.

## Pure core still without vectors (specs.md §29.4)

- **Solver, phase 1b** (§13.3): moving copies when rebalancing (`atoll balance`), the hysteresis, and the reachable capacity as a dry run of that plan (`atoll capacity`), which needs the moves to mean what it says. Moving an Atoll copy also fixes a file whose copies cover too few domains, which stays `degraded` until then.
- **Solver, phase 2b** (§12.4): `colocate` and the `ordered` and `percent` strategies. Until then the caller gives the plan `most-free` and no `colocate` (§12.2): `solver.Placement` has neither.
- **BLAKE3**: run `conformance/vectors/blake3.json` on real arm64 and AVX-512 hardware, measure throughput there, and move `zeebo/blake3` to a tagged release when one holds the arm64 backends (`docs/research/blake3.md`).

## Contracts not yet defined (specs.md §29.3)

Every item here is a **decision**:

- The local API's commands that write (moving, deleting, answering a guard), which the alpha leaves to the CLI (§25.1).

## What the review of the code leaves (2026-11-13)

Each is a **decision**: the code does what the spec says, and the review found what that costs.

- **A file that cannot be opened stops the hashes of its disk** (§15.10). Any error but `ENOENT`, `ELOOP` and `ENOTDIR` stops the reading of a disk, as an I/O error must. One that says nothing of the disk does too: `EACCES`, `ENOKEY` for a file whose key is not loaded, `EMFILE` when the process is out of descriptors. The file stays without a hash, so each slice of the daemon stops at it again, and the files after it in the order of the paths are never hashed on that disk, with nothing to say so. To decide: report such a file as `unreadable` and go on, an I/O error alone stopping the disk.
- **One transaction for each file hashed** (§15.10), so that an interruption loses little. On 400,000 copies of a few bytes that is 31 minutes in qemu without KVM, 4.7 ms a copy, nearly all of it in commits (§26); on a slow system disk it is one `fsync` a file. To decide: one transaction for the files read within a second, which loses as little of a large file and costs a small one nearly nothing.

## What the review of the code found, still open (2026-11-13)

Eight reads of the whole code, by area, for performance, security and conformance to the spec, at the commit of pull request #21. What follows was found by reading and is not fixed: each is to check in the code as it is now, then to fix after a test that fails without the fix. What was fixed is in DONE.md.

### Security

- **A work folder that becomes another account's after `disk add` is told of, and still written to** (§10), as the maintainer decided on 2026-11-13: an alert, and nothing stops unattended. What that leaves: the owner of a disk's top can rename `<workdir>` and make another, at any time, and reads what Atoll copies there until someone acts on the alert; and nothing looks at the owner of `tmp/`, `links/` and `trash/` themselves, only at `<workdir>`'s. `links/` and `tmp/` are closed to other accounts when found open.

### Data and conformance

- **A copy whose part cannot be made for want of room ends `io-error`** (`internal/jobs/replicate.go`): only a write to the part that answers `ENOSPC` ends `no-room`, and making the part can answer it too, with no block or no inode left. Seen in the test of `balance` below level; what differs between the two outcomes for the job is to check.
- **A guard rejected removes the overwrites it held with its deletions** (`internal/catalog/propagate.go`, `RejectGuard`), where §15.6 says "its queued jobs for a deletion go". The next pass gives each stale copy its job again, so nothing stays stale; the job then names the first copy `ok` by disk position as its source, which may not be the one the first job named. And **a dry run of the pass that decides a rename does not list the overwrite** the real pass then runs for a stale copy of that file: the decision drops the job in its listing, and the job a real pass queues again is not worked out there.

### Performance

- **A purge commits two transactions and syncs one folder for each entry**, under the catalog's lock (`internal/trash/trash.go`); `trash restore` of one file loads every entry.
- **What the link guard holds of a disk at once** (above, with the 200 MB of §26); `MakeLink` syncs every linked file where only NTFS needs it; `GonePaths` filters the events by `json_extract`, which no index holds; `OwnFiles` walks the whole trash and `tmp/` to match a few inodes.
- **Each slice of the scrub pays what grows with the pool** (`internal/jobs/scrub.go`): a plan of the whole pool at its end, the disk's copies sorted with no index for each page, a commit for each copy, and each disk woken before the catalog is asked whether anything is due. A read error on one chunk loses the disk at each slice, at the same copy, as §15.10 asks of the hashes. `SavedChunks` holds every chunk a repair saved at once (`internal/ops/repair.go`).
- **The jobs are read whole**: `ReplicateJobs`, `JobOfPart` and `DueReader` use no index (`internal/catalog/replicate.go`), `FolderJobs` scans every job for each folder (`internal/catalog/propagate.go`), and whole lists of jobs are held where a page would do.
- **The plan loads every name of every candidate NTFS disk** as soon as one file needs a copy (`internal/solver/plan.go`, `internal/names/collision.go`): 20 to 40 MB a disk of 200,000 names, by the sizes of the types.
- **Renames by rounds**: a chain of files each renamed to the next's name costs the square of its length, in the plan (`assignTargets`, `internal/solver/propagate.go`) and in the pass (`internal/jobs/work.go`); `addParents` hashes every ancestor of every path, which is the cube of the depth for a chain of nested folders, and the walk has no bound on depth.
- **The modify step reads every changed file whole, in one pass, with no budget** (`internal/jobs/modify.go`): a `touch` or a `chmod -R` over a disk holds the catalog's lock for as long as its files take to read, against §15.13, "A pass ... reads no file whole".
- **The watcher gathers every event before it sends any** (`drain`, `internal/watch/watcher.go`); `readCopy` compares a copy with its row only after reading it whole, and each slice starts again from the first file with no hash (`internal/watch/scan.go`).
- **`MissingBelow` is planned from the missing disk** (`internal/catalog/notify.go`): every copy of it for each folder held, at each pass; `cannotLeave` asks about every replaced copy once for each folder that waits; `guard list` and `/v1/guards` hold every held path several times.

### Decisions

- **What an answer to the mass-modification guard covers, and for how long** (§15.6, `internal/catalog/modguard.go`, `internal/jobs/modguard.go`). As §15.6 says, a confirm or a reject decides every file whose changed copy on that disk changed by the time the guard tripped, by its change time, and the last answer stands for good: a copy found changed a year later, with a change time before that answer's, is decided under it without a question. A file put at a name after the answer has a change time after it, as a rename moves it on all six drivers (§15.1), so nothing made later passes; what passes is what changed before and was found after: a disk restored from an older image, whose every file then reads as changed at an old time, and under a confirm overwrites the newer copies of the other disks through the trash; a folder no look could read at the time (§15.10); a disk whose clock ran behind. A reject errs the safe way, a conflict. To decide, by axis. Security: an answer should speak only of what the guard held when it was given, so it should end: either with the guard's own window (`modifies.mass_guard.window` after the answer), or by covering only the copies a look had found changed by then, which takes a time of first sight on the copy's row. Functionality: the first needs one comparison more and no new state; the second a column and a migration. Performance: none differs. Product: "an answer is good for the window it was asked in" is easy to say and to document; recommended, the first.
- **A corrupt copy whose times moved is for nobody** (§15.4, §16.1), found while fixing the repair that fails, and measured in a test since removed. A repair, and the scrub's, leave a copy whose size or times are no longer its row's to the next pass, as a change; and a pass leaves a file in `corrupt` alone, "nothing changes until that is resolved". So a corrupt copy that anyone touches, with `chmod`, `touch`, or by writing the good content back over it by hand, stays `corrupt` for good, in `media/`, where mergerfs serves it when its branch comes first: every `atoll repair` answers `changed`, no scrub reads a corrupt copy again, and the edit, if it is one, is never propagated nor called a conflict. Nothing is lost on the disks. The same after a crash in a chunk repair on a disk that dropped the write, where the reprise takes the moved change time for someone else's. To decide: what a repair does with a corrupt copy that changed. Read whole, it holds the file's content, and is then `ok` and verified, or it does not, and is then either still corrupt, with new times, or another version, which nothing tells apart. Until then the way out is to remove that copy by hand.
- **A pending copy kept is taken off a disk's room with no check** (`internal/solver/plan.go`), so a copy queued while there was room is written into the reserve of `min_free`: §13.4 is silent on it, and §12.4 lists `min_free` among what is never violated.
- **`depending_first` gives its paths as plain strings**, in `disk show`, `disk remove`, `disk fail` and `/v1/disks/{id}`, and in their schemas, where §29.3 asks for `path` or `path_b64`.
- **An alert that cannot be sent brings a whole pass every 5 minutes** (§23): 288 passes a day while a server is down.
- **A change of metadata alone makes the other copies stale outside the guard of modifications**, and **the copies a refused rename sends to the trash are in no guard's window**: both follow the letter of §15.6.
- **No bound on a path's depth**, which §15.8 says does not bind Atoll: the walk holds a handle a level, and its memory grows as the square of the depth.

## Phase 1a, the alpha, not started (specs.md §28)

- Configuration in the shell (§24.3, §10):
  - in `atoll rules check`, the rules that no file reaches or that an earlier one hides (§11.3);
  - the fallbacks of §12.2 for the modes, when jobs act on them: `rules.Policy` holds what the configuration asks for, and `atoll scan` already gives the plan only what has an effect;
  - `disk add` and `disk remove` editing the file: every other byte kept, the result re-read before it replaces the file.
- After `catalog rebuild`, the disks' trash entries are not in the new catalog: list them again from `trash/<date>/<op id>/`, with a reason to choose, and their expiry from the date.
- Memory of the daemon on 200,000 files (§26): `atoll scan` and every pass of `daemon --until-idle` measured stay under the limit, the highest at about 187 MiB, a scan after a rename of a folder holding everything. The daemon that runs for good, with its watcher and its grouping, held four hours of a change every second on 4,000 files, its heap in use between 2.8 and 6.8 MiB with no growth (§26). Over days, and on 200,000 files, it is still to measure: `SOAK=72h SOAK_FILES=200000`. Its first 7 hours there: 218 passes, 3.6 MiB of heap in use, 7 goroutines and 14 open files, and at most 222.8 MiB resident.
- The 200 MB of §26 on 200,000 files a disk: the soft limit of 170 MiB bounds the heap alone, and what SQLite and the binary hold comes on top, about 20 MiB in a pass. The passes peak at 190.0 to 190.6 MiB for 190.7 MiB allowed; `atoll status` with a strict rule at 192.5 MiB, the passes with the link guard at 196.5 to 205.3 MiB, and the soak's process at 222.8 MiB go past it (DONE.md, the soak on 200,000 files). `TestScale` and the soak now log, for each peak, what the Go runtime held and what the heap held live: run them again to tell what a lower limit would cost, and what the soak's daemon holds past a pass's, before choosing one. What the link guard holds of a disk at once is known from its code (`internal/trash/linkguard.go`, `internal/ops/linkguard.go`), and is the first thing to bring down, as no limit lowers what is live: the names of the links folder; a look of 112 bytes for each link, twice, in the list and in a map by name that only gives the size of a link sent to the trash; the plan's own list of links; the disk's copies twice, as the catalog's rows then as the plan's; a map from each file to its inode; and the plan, a step for each link, kept or not. By the sizes of those types that is about 170 MiB for 200,000 links, an estimate. Without the map of looks, with a look of the four fields that are used, the copies read straight into the plan's list and the inode of a file found by search in that list, which is sorted by file, about 77 MiB go.
- What the mount check of §10 leaves to do for disks:
  - `atoll disk show`, which could show `time_precision_ns`, and reading `Atoll/disk.toml` at each mount check;
  - the topology check of R-06 at each start, not only at `disk add`, with the catalog's `disks.device`.
- What the daemon that runs for good (§15.13) leaves:
  - the watcher's limits: a folder that leaves the root under inotify and comes back in another read is watched again from scratch; a path past `PATH_MAX` cannot get a mark of its own on ntfs-3g; the process of an event already gone reads as `user`;
  - the remount after a USB reset of an ntfs-3g disk, through `mount.ntfs-3g`, as the dedicated user, is not tested.
  - passes that walk every copy: under a change every second on 200,000 files they follow one another, about one core's worth on a machine, against the 5 % of §26 (DONE.md, the soak on 200,000 files). A pass that walks only what changed would answer that; the maintainer is to say whether it waits for phase 2b, which changes passes the same way to spare the disks' sleep.
- What the copy pipeline leaves for later (§14): one copy at a time, in one pass, is all the daemon runs, so `max_parallel_per_disk` waits for its loop; the creation time set after the rename on Windows (§7).
- The grouping of events per path comes with the watcher (§15.1).
- What modifications leave as limits (§15.4, §15.12): a swap of two names whose other copies are on NTFS, whose drivers cannot exchange two names, and a copy replaced by anything but a regular file, which are only reported. `deletes.direct = "restore"` (§15.5), which the copy pipeline now allows, still propagates nothing.
- What pull request #3 left of renames and deletions: judge a port written from the documents on the 269 new scenarios, which needs a port of the daemon, and settle three gaps no vector reaches (specs.md §15.12): a file reaching its own path as a later target, a behind copy first seen after the file left, and whether a file both not due and held says wait or held. The decisions taken there without the maintainer, listed in [DONE.md](DONE.md), were confirmed on 2026-11-12.
- A volume that ntfs3 left with names that disagree (§17.2) shows only as a warning in the log and exit 4 on every run on that disk: say what to do, which is a repair of the volume, in an alert, which §23 does not list yet, and in `atoll status`.
- The rest of the trash (§17): a folder Atoll emptied in a run that was stopped before its end stays, as nothing records it (§17.5). A folder's attributes other than its mode and owner wait for the pipeline.
- The scrub: `files.state` holds one state, so a file both `corrupt` and in conflict shows `corrupt`; once no copy is corrupt, the next pass finds the conflict again from the rows of its copies (§16).
- The repair (§16.1): `TestRepairOnEachDriver` leaves out btrfs, which answers a read of a block that rotted with `EIO`, which Atoll takes for a disk that fails (§10): what a scrub of btrfs should make of a checksum error is still to decide: SMART, read now, tells of a disk, not of one block.
- The settings of the openmediavault mergerfs plugin from `atoll mergerfs-config` (§21.1), which the maintainer left for later: the fstab line and `--json` are done.
- The link guard (§17): measured at 200,000 files a disk (§26), where its passes hold up to 205.3 MiB, past the 200 MB: see the item on that limit above. A link nothing names only shows as a warning in the log: say so in `atoll status` or an alert. A command that sends a copy to the trash leaves its link to the next pass.
- `balance` (§12.4, §13.3): a plain `atoll balance` does not say yet that `--move-originals` would move a user copy that breaks a strict setting. `atoll status` plans the whole pool to give `bytes_placed` whenever a strict setting exists: at 200,000 files a disk that doubles its time, 1 min 13 s for 35 s in qemu without KVM, and peaks at 192.5 MiB (§26). The local API's `/v1/status` does the same in the daemon's own process, beside its passes.
- The media-server notifications (§21.5) were checked against fake servers built from the documented APIs, never against a real Plex, Jellyfin or Emby: to try on the maintainer's Plex once its libraries read the merged view. A new file a walk finds is told to no server.
- What the alerts leave (§23): nothing of phase 1b; SMART, Gotify, SMTP and a volume marked dirty are done. Without udev, as in a container, no `/dev/disk/by-uuid/` names the device, and a dirty volume is not seen. No reading of SMART sets the state `standby` yet (§4): one an hour old says nothing of now, and `smartctl -n standby` would have to be asked before each pass. The counts read the whole of `files` after each pass, with no index: to measure on 200,000 files, before an index on the states that alert.
- The operation journal (`ops`) covers the trash, the renames of propagation, files and folders, and the copies (`create`) so far (§14.2, §17.1, §17.2). Each new kind of operation needs its own reprise, with kill -9 at each step and fault injection.
- The break-glass test runs in `test/loopdev` (`TestBreakGlass`), so in the CI's qemu job, on two disks and every driver. What it leaves: a pool of more disks, a disk away when Atoll goes, and, once Atoll's own view comes (phase 2a), comparing that view with plain mergerfs.

## Later phases (specs.md §28)

- **Observability, for phase 3 (the Rust rewrite)**, as the maintainer asked on 2026-11-12: OpenTelemetry, Vector, or both. OpenTelemetry would give the daemon's traces, metrics and logs over OTLP. Vector (vector.dev) would collect the logs and metrics and send them on. Phase 2b already brings Prometheus's `/metrics`; what the rewrite exports, and through which, is to decide then.
