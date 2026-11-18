# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## Decisions for the alpha (the maintainer, 2026-11-12)

The work goes to the `develop` branch, with one pull request once the big work is done. An ambiguity these do not settle takes the safest option, and DONE.md lists it as decided without the maintainer, to confirm. New contracts follow the style of the existing ones, vectors or scenarios first, each listed for review.

- **Order:** the daemon that runs for good, then ntfy alerts, `atoll names check`, the disk commands, the scrub. The alpha also takes in `atoll catalog backup | verify | rebuild`, `atoll disk fail` and `disk replace`, the owner applied to the other copies, and the local API.
- **The daemon:**
  - **Passes.** A pass walks every disk, as `daemon --until-idle` does: each pass confirms every writable disk anyway, and sparing the disks' sleep is phase 2b. Done there on 2026-11-16: a pass for events or `next_due` walks only the disks something calls for (§15.13).
  - **The lock.** The catalog's lock is taken for each pass, and a command that finds it taken waits for it, saying so, instead of exiting with 1.
  - **`--foreground`.** `atoll daemon` without it is the same as with it.
  - **The account.** A systemd unit runs it as a dedicated user `atoll`, with the capabilities `CAP_SYS_ADMIN`, `CAP_DAC_READ_SEARCH`, `CAP_DAC_OVERRIDE`, `CAP_CHOWN` and `CAP_FOWNER`, the group `disk` and `NoNewPrivileges`; no setting that gives it a mount namespace of its own, as its mounts must reach the host (`ProtectHome` and `PrivateTmp` went on 2026-11-16). Measured: so the user writes mergerfs's runtime branches, and runs `mount` and `umount`.
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

- Measure on the maintainer's Windows machine, with their go-ahead, what a report says is not established: which reserved names Windows 11 still refuses, and what Windows does with `Film.mkv` beside `film.mkv` on an NTFS volume (a VHD).
- **decision**: the `ntfs` driver of Linux 7.1 and later (§10, §15.8, §22), the only NTFS driver of Debian 14's kernel: support it like ntfs3, keep it `readonly`, or something in between. Measured in `docs/research/ntfs-driver.md`, whose last section lists the decisions it needs. Until then a disk mounted with it is `readonly`.

## Pure core still without vectors (specs.md §29.4)

- **Solver, phase 1b** (§13.3): moving copies when rebalancing (`atoll balance`), the hysteresis, and the reachable capacity as a dry run of that plan (`atoll capacity`), which needs the moves to mean what it says. Moving an Atoll copy also fixes a file whose copies cover too few domains, which stays `degraded` until then.
- **BLAKE3**: run `conformance/vectors/blake3.json` on real arm64 and AVX-512 hardware, measure throughput there, and move `zeebo/blake3` to a tagged release when one holds the arm64 backends (`docs/research/blake3.md`).

## Contracts not yet defined (specs.md §29.3)

Every item here is a **decision**:

- The local API's commands that write (moving, deleting, answering a guard), which the alpha leaves to the CLI (§25.1).

## The review's decisions, made by the maintainer on 2026-11-13, to do

Each point the review left to decide was put to the maintainer as a question, with its options and what each costs. What he chose is below; the reasons and the costs are in the entries of the sections that follow, which stay until each is done. Each is done as any fix: a test that fails first, the spec, DONE.md.

16. **The soak.** The one of 72 hours, started again on `42e9c65` on 2026-11-14, was stopped by the maintainer; one of 12 hours on 200,000 files a disk passed (2026-11-14, `f438a3a`, DONE.md), and again on the passes that look at names (2026-11-18, `2d016b3`), which the maintainer asked for once phase 2b was done. One of 24 hours is still to run, with the try of the notifications on the real Plex and the openmediavault plugin. With them, on the maintainer's own disks, which disk spins (§10): CHECK POWER MODE on a SATA disk asleep and awake, and behind each USB bridge of the pool, as no disk sleeps in qemu; the log says which bridges give no answer, and those disks count as spinning. And the SSD cache (§19) on a real SSD, with Plex reading through Atoll's own view while files leave the cache. And the UPS through NUT (§20) on the maintainer's own UPS: unplugged, then plugged back, Atoll stops its copies and starts them again, as its log says; the low battery is not tried there, as upsmon would shut the machine down.

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

- Memory of the daemon on 200,000 files (§26): `atoll scan` and every pass of `daemon --until-idle` measured stay under the limit, the highest at about 187 MiB, a scan after a rename of a folder holding everything. The daemon that runs for good, with its watcher and its grouping, held four hours of a change every second on 4,000 files, its heap in use between 2.8 and 6.8 MiB with no growth (§26). On 200,000 files, 12 hours are measured (2026-11-14, `f438a3a`, DONE.md): 293 passes, its heap in use 3.6 to 3.9 MiB, 8 goroutines and 14 open files at every tenth, with no growth, and at most 220.4 MiB resident. Again on 2026-11-18 (`2d016b3`, DONE.md), once passes looked at names: 911 passes, 3.3 to 3.6 MiB of heap in use, 9 goroutines and 14 open files at every tenth, with no growth, and at most 180.7 MiB resident at each tenth. Over days, it is still to measure: `SOAK=24h SOAK_FILES=200000`.
- The 200 MB of §26 on 200,000 files a disk: brought under it on 2026-11-15 by no transparent huge pages and a soft limit of 155 MiB (DONE.md): every step of `TestScale` peaks at 183.8 MiB at most on `47ab5f6`, by the kernel's own count. The soak of 12 hours on `2d016b3` (2026-11-18, DONE.md) confirms it for its passes, 172.8 to 180.7 MiB at every tenth, but not for its first pass, the one that finds the 400,000 copies of a catalog that knows none: 195.2 MiB, the Go runtime holding at most 173.9 MiB, 19 past its soft limit, and the heap 147.5 MiB live. To look at: what that pass holds past `TestScale`'s first pass, measured under the limit. The soak of 24 hours is to confirm the rest over days. What follows is the history of it. The soft limit of 170 MiB bounded the heap alone, and what SQLite and the binary hold comes on top, about 26 MiB in a pass with nothing to do. Measured on `728fc4c` (`TestScale`): the passes, the scan and `atoll status` with a strict rule are under it now, 175.3 to 188.9 MiB for 190.7 MiB allowed. The link guard is not. A pass with the guard peaks at 198.1 MiB, the Go runtime 164.5 of it. Its two passes that journal 400,000 operations each, the one that makes the links and the one that removes them, peak at 212.0 and 217.1 MiB, 15 and 20 MiB more than when they were last measured, with 42 and 48 MiB outside the Go heap. Attributed on 2026-11-14 by `TestScale` on `3bb07e0` and `b48f3cd`, the commits just before and with the index `ops_by_key`, side by side: not the index, whose run peaks lower in two of the three passes; one run says a step's peak to within some 30 MiB, as what is held outside the Go heap moves from 20 to 55 MiB between runs (§26). So no measure so far shows a rise, and none of them can show one under that spread. What the process holds outside the Go heap, measured on 2026-11-15 (§26): SQLite's allocator 10.2 MiB at every step, the binary's pages 14.5 MiB, and up to 43 MiB that the kernel keeps resident in transparent huge pages after the runtime gave it back, which is what moved between runs. Without huge pages for the process and with a soft limit of 155 MiB, every step of `TestScale` peaks at 181.6 MiB at most, with no time lost that two runs side by side tell apart. Decided and done on 2026-11-15. The soak's process peaks at 212.1 to 220.4 MiB at every tenth of 12 hours on `f438a3a`, the Go runtime at most 170.0 to 183.0 MiB of it, with no growth: what its passes over 400,000 copies hold (DONE.md, the soak on 200,000 files). `TestScale` and the soak log, for each peak, what the Go runtime held and what the heap held live: run them again to tell what a lower limit would cost, and what the soak's daemon holds past a pass's, before choosing one. What the link guard still holds for each link: its name; a look of four fields; the plan's own entry; a step of the plan, kept or not; and what the plan's maps hold of each copy.
- What the daemon that runs for good (§15.13) leaves:
  - the watcher's limits: under a filesystem mark, an event past `PATH_MAX` comes at `""`, and its disk is looked at whole, as `/proc` names no path that long (§15.1); an Atoll command that ended before the watcher read its events reads as `user`, and wakes a pass;
- What the copy pipeline leaves for later (§14): one copy at a time, in one pass, is all the daemon runs, so `max_parallel_per_disk` waits for its loop; the creation time set after the rename on Windows (§7).
- What modifications leave as limits (§15.4, §15.12): a swap of two names whose other copies are on NTFS, whose drivers cannot exchange two names, and a copy replaced by anything but a regular file, which are only reported.
- What pull request #3 left of renames and deletions: judge a port written from the documents on the 269 new scenarios, which needs a port of the daemon, and settle three gaps no vector reaches (specs.md §15.12): a file reaching its own path as a later target, a behind copy first seen after the file left, and whether a file both not due and held says wait or held. The decisions taken there without the maintainer, listed in [DONE.md](DONE.md), were confirmed on 2026-11-12.
- The rest of the trash (§17): a folder's attributes other than its mode and owner wait for the pipeline.
- The settings of the openmediavault mergerfs plugin from `atoll mergerfs-config` (§21.1), which the maintainer left for later: the fstab line and `--json` are done.
- The link guard (§17): measured at 200,000 files a disk (§26), where its passes hold up to 217.1 MiB, past the 200 MB: see the item on that limit above. A command that sends a copy to the trash leaves its link to the next pass.
- `balance` (§12.4, §13.3): `atoll status` plans the whole pool to give `bytes_placed` whenever a strict setting exists: at 200,000 files a disk that doubles its time, 1 min 13 s for 35 s in qemu without KVM, and peaks at 192.5 MiB (§26). The local API's `/v1/status` does the same in the daemon's own process, beside its passes.
- The media-server notifications (§21.5) were checked against fake servers built from the documented APIs, never against a real Plex, Jellyfin or Emby: to try on the maintainer's Plex once its libraries read the merged view. `atoll scan`, a command, tells no server of the new files it records.
- What the alerts leave (§23): nothing of phase 1b; SMART, Gotify, SMTP and a volume marked dirty are done. Without udev, as in a container, no `/dev/disk/by-uuid/` names the device, and a dirty volume is not seen. No reading of SMART sets the state `standby` (§4), and none is to, as the maintainer decided on 2026-11-14: one an hour old says nothing of now, and `smartctl -n standby` would have to be asked before each pass. The counts read the whole of `files` after each pass, with no index: to measure on 200,000 files, before an index on the states that alert.
- The operation journal (`ops`) covers the trash, the renames of propagation, files and folders, and the copies (`create`) so far (§14.2, §17.1, §17.2). Each new kind of operation needs its own reprise, with kill -9 at each step and fault injection.
- The break-glass test runs in `test/loopdev` (`TestBreakGlass`), so in the CI's qemu job, on two disks and every driver. What it leaves: a pool of more disks; a disk away when Atoll goes is `TestBreakGlassWithADiskAway`. Atoll's own view is compared with plain mergerfs in `TestViewMatchesMergerfs`, the read path for now (phase 2a). The read-only view beside mergerfs, `atoll mount`, is to try on the maintainer's Plex with the rest of §21.5, its libraries at `view.readonly_mount`.

## Later phases (specs.md §28)

- **Passes that walk only what changed, in phase 2b**, as the maintainer decided on 2026-11-14: under a change every second on 200,000 files, passes that walk every copy follow one another, about one core's worth on a machine, against the 5 % of §26 (DONE.md, the soak on 200,000 files). Done by disk on 2026-11-16, to spare the disks' sleep (§15.13): a disk that nothing changed on is not walked. Done by name on 2026-11-17 (DONE.md): a pass for events looks at the names the watcher heard alone, and plans only what may have changed, 12 ms for a pass given one name on 50,000 files a disk, against 1 s. Measured over 12 hours on 2026-11-18 (DONE.md): about 7 s a pass for 105 s, 2 hours of passes in the 12 for 9. Still to do: the soak of 24 hours, to measure it over days; and the operations of Atoll's own view, which still walk their disks whole, by their names too.
- **A write that runs out of space, moved to another disk as it goes** (§19, §21.2), on every disk, the SSD cache's included: done apart from the cache, as the maintainer chose on 2026-11-17. Until then a write through Atoll's own view that meets `ENOSPC` fails with it, and a file lands on the cache only while it has room past `min_free`.
- **Observability, for phase 3 (the Rust rewrite)**, as the maintainer asked on 2026-11-12: OpenTelemetry, Vector, or both. OpenTelemetry would give the daemon's traces, metrics and logs over OTLP. Vector (vector.dev) would collect the logs and metrics and send them on. Phase 2b brought Prometheus's `/metrics` (specs.md §23.1), whose names `api/metrics.json` pins; what the rewrite exports besides, and through which, is to decide then.
