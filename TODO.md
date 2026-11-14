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

## The review's decisions, made by the maintainer on 2026-11-13, to do

Each point the review left to decide was put to the maintainer as a question, with its options and what each costs. What he chose is below; the reasons and the costs are in the entries of the sections that follow, which stay until each is done. Each is done as any fix: a test that fails first, the spec, DONE.md.

16. **The soak of 72 hours** started on 2026-11-13 on `c294b35` was stopped the same evening, after 3 passes: several of the above change the loop it measures. Fifteen of the sixteen done and the local CI green on `42e9c65`, it runs again there since 2026-11-14 at 00:54 UTC, for 72 hours (container `soak-72h`).

Four more, on what the measure of 10 showed, answered on 2026-11-14:

19. **`MakeLink` keeps the sync of each file it links**, as it is: the prepared change (`decision-10-prepared.patch`) is not applied. Half an hour once, when the guard is turned on; nothing for a pool of NTFS disks.
20. **CLAUDE.md's invariant 4** says that a read that fails on a disk that confirms right after is the chunk's fault: the copy is `corrupt` (§16), as decision 1 has it. Done with this record.

Left as recommended, with no question put: the jobs read whole, the renames by rounds and the bound on a path's depth, the bursts of the watcher, `cannotLeave` and `GonePaths`; `guard list` is to write as it reads when the local API serves large pools.

## What the review of the code found, still open (2026-11-13)

Eight reads of the whole code, by area, for performance, security and conformance to the spec, at the commit of pull request #21. What follows was found by reading and is not fixed: each is to check in the code as it is now, then to fix after a test that fails without the fix. What was fixed is in DONE.md.

### Security

- **A work folder that becomes another account's after `disk add` is told of, and still written to** (§10), as the maintainer decided on 2026-11-13: an alert, and nothing stops unattended. What that leaves: the owner of a disk's top can rename `<workdir>` and make another, at any time, and reads what Atoll copies there until someone acts on the alert. The owners of `tmp/`, `links/` and `trash/` are looked at as `<workdir>`'s is, by the daemon alone, and `links/` and `tmp/` are closed to other accounts when found open.

### Data and conformance


### Performance, each to decide

What the review found costly, less what a test and a fix settled (DONE.md). None is a fault of what Atoll decides: each is time, memory or a lock held. For each, what it costs, what would change it, and what is recommended, by what it gains (performance), what it risks (security), what it changes (functionality) and when it counts (product).

- **The jobs are read whole** (`internal/catalog/replicate.go`, `propagate.go`): by their plans, the folder jobs and the trash jobs of a disk scan the table, `DueReader` walks `jobs_to_do` from its start for each page of 1,000, and `ReplicateJobs` below a path and `JobOfPart` read every job of a file. The table holds what waits: nothing most days, and 200,000 rows while a new pool makes its first replicas, when a pass reads it some ten times, a second or two beside the two minutes its walks take. **Recommended: leave**, until the passes walk only what changed (phase 2b); then an index on `jobs(kind, dst_disk)`, and a `DueReader` that starts each page after its last key.
- **Renames by rounds**: a chain of files each renamed to the next's name costs the square of its length, in the plan (`assignTargets`, `internal/solver/propagate.go`) and in the pass (`internal/jobs/work.go`); `addParents` hashes every ancestor of every path, the cube of the depth for a chain of nested folders, and the walk has no bound on depth. No library has such shapes, and whoever can write one can already fill the disk. **Recommended: leave**, with the bound on a path's depth below.
- **The watcher gathers the events of a burst before it sends any** (`drain`, `internal/watch/watcher.go`): what the kernel's queue holds, 16,384 events and a few MiB at most, unless events come as fast as they are read. And each slice of the hashes starts again from the first file with no hash (`internal/watch/scan.go`), which a look at its name now settles when it is no longer what its row says. **Recommended: leave.**
- **`cannotLeave` asks about every replaced copy once for each folder that waits** (`internal/jobs/pass.go`); `guard list` and `/v1/guards` hold every held path three times, some 60 MB for 200,000 paths of a guard that tripped; `GonePaths` reads every `copy-gone` event the catalog ever recorded (`internal/catalog/linkguard.go`), by its plan, each time a link's file has no row left, a tenth of a second for 100,000 events. **Recommended: leave** the first and the last; `guard list` is to write as it reads, when the local API serves large pools.

### Decisions

- **No bound on a path's depth**, which §15.8 says does not bind Atoll: the walk holds a handle a level, and its memory grows as the square of the depth.

## Phase 1a, the alpha, not started (specs.md §28)

- Configuration in the shell (§24.3, §10):
  - in `atoll rules check`, the rules that no file reaches or that an earlier one hides (§11.3);
  - the fallbacks of §12.2 for the modes, when jobs act on them: `rules.Policy` holds what the configuration asks for, and `atoll scan` already gives the plan only what has an effect;
  - `disk add` and `disk remove` editing the file: every other byte kept, the result re-read before it replaces the file.
- After `catalog rebuild`, the disks' trash entries are not in the new catalog: list them again from `trash/<date>/<op id>/`, with a reason to choose, and their expiry from the date.
- Memory of the daemon on 200,000 files (§26): `atoll scan` and every pass of `daemon --until-idle` measured stay under the limit, the highest at about 187 MiB, a scan after a rename of a folder holding everything. The daemon that runs for good, with its watcher and its grouping, held four hours of a change every second on 4,000 files, its heap in use between 2.8 and 6.8 MiB with no growth (§26). Over days, and on 200,000 files, it is still to measure: `SOAK=72h SOAK_FILES=200000`. Its first 7 hours there: 218 passes, 3.6 MiB of heap in use, 7 goroutines and 14 open files, and at most 222.8 MiB resident.
- The 200 MB of §26 on 200,000 files a disk: the soft limit of 170 MiB bounds the heap alone, and what SQLite and the binary hold comes on top, about 26 MiB in a pass with nothing to do. Measured on `728fc4c` (`TestScale`): the passes, the scan and `atoll status` with a strict rule are under it now, 175.3 to 188.9 MiB for 190.7 MiB allowed. The link guard is not. A pass with the guard peaks at 198.1 MiB, the Go runtime 164.5 of it. Its two passes that journal 400,000 operations each, the one that makes the links and the one that removes them, peak at 212.0 and 217.1 MiB, 15 and 20 MiB more than when they were last measured, with 42 and 48 MiB outside the Go heap. To attribute first: run `TestScale` on the commits since that measure. The index `ops_by_key` (migration 0012) is the first to look at: each of those operations goes into it, and it is 14 MiB of the catalog after them. If it is the index, look at whether it must hold the operations on links, against what `PlacesOf` asks of it: leaving them out is a migration, to decide. The soak's process was at 222.8 MiB (DONE.md, the soak on 200,000 files), to measure again by a soak. `TestScale` and the soak log, for each peak, what the Go runtime held and what the heap held live: run them again to tell what a lower limit would cost, and what the soak's daemon holds past a pass's, before choosing one. What the link guard still holds for each link: its name; a look of four fields; the plan's own entry; a step of the plan, kept or not; and what the plan's maps hold of each copy.
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
- The link guard (§17): measured at 200,000 files a disk (§26), where its passes hold up to 217.1 MiB, past the 200 MB: see the item on that limit above. A link nothing names only shows as a warning in the log: say so in `atoll status` or an alert. A command that sends a copy to the trash leaves its link to the next pass.
- `balance` (§12.4, §13.3): a plain `atoll balance` does not say yet that `--move-originals` would move a user copy that breaks a strict setting. `atoll status` plans the whole pool to give `bytes_placed` whenever a strict setting exists: at 200,000 files a disk that doubles its time, 1 min 13 s for 35 s in qemu without KVM, and peaks at 192.5 MiB (§26). The local API's `/v1/status` does the same in the daemon's own process, beside its passes.
- The media-server notifications (§21.5) were checked against fake servers built from the documented APIs, never against a real Plex, Jellyfin or Emby: to try on the maintainer's Plex once its libraries read the merged view. A new file a walk finds is told to no server.
- What the alerts leave (§23): nothing of phase 1b; SMART, Gotify, SMTP and a volume marked dirty are done. Without udev, as in a container, no `/dev/disk/by-uuid/` names the device, and a dirty volume is not seen. No reading of SMART sets the state `standby` yet (§4): one an hour old says nothing of now, and `smartctl -n standby` would have to be asked before each pass. The counts read the whole of `files` after each pass, with no index: to measure on 200,000 files, before an index on the states that alert.
- The operation journal (`ops`) covers the trash, the renames of propagation, files and folders, and the copies (`create`) so far (§14.2, §17.1, §17.2). Each new kind of operation needs its own reprise, with kill -9 at each step and fault injection.
- The break-glass test runs in `test/loopdev` (`TestBreakGlass`), so in the CI's qemu job, on two disks and every driver. What it leaves: a pool of more disks, a disk away when Atoll goes, and, once Atoll's own view comes (phase 2a), comparing that view with plain mergerfs.

## Later phases (specs.md §28)

- **Observability, for phase 3 (the Rust rewrite)**, as the maintainer asked on 2026-11-12: OpenTelemetry, Vector, or both. OpenTelemetry would give the daemon's traces, metrics and logs over OTLP. Vector (vector.dev) would collect the logs and metrics and send them on. Phase 2b already brings Prometheus's `/metrics`; what the rewrite exports, and through which, is to decide then.
