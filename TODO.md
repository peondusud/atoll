# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## Decisions for the alpha (the maintainer, 2026-11-12)

All of it is done (ROADMAP.md): it stays here as the record of what was decided.

The work went to a working branch, `develop` then `beta`, with one pull request once the big work is done. An ambiguity these do not settle takes the safest option, and DONE.md lists it as decided without the maintainer, to confirm. New contracts follow the style of the existing ones, vectors or scenarios first, each listed for review.

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

All of it is done but the maintainer's own step, moving Plex's libraries to the merged view: it stays here as the record of what was decided.

- **Order:** repair and quarantine of corrupt copies, then the duplicate report (R-14, R-15); the `immediate` and `scheduled` modes; Gotify and SMTP alerts, and SMART; notifications to Plex, Emby and Jellyfin; `atoll conflicts resolve`; failure domains and tags as hard constraints, with `atoll balance` and `atoll capacity`; the link guard last.
- **Plex** reads each disk's folders today. So its libraries move to the merged view, as §21.4 says, before the first replica, and a scan notification names a folder as that view shows it.
- **Hard links:** none in the maintainer's library for now, but the link guard must handle them, for users whose Sonarr or Radarr link a torrent client's file into `media/`. Through Atoll's own view too, made on the original's disk (DONE.md, 2026-11-19); mirroring them on replicas is not done (what it would cost, in that entry).
- **Alerts:** Gotify and SMTP, both, beside ntfy.
- **The link guard:** files that Sonarr or Radarr already hard-linked to a torrent client's folder get the guard's link too, and Atoll never counts its own link in `nlink`. Turned off after use, the guard's links are removed, journaled, and a link whose file has no other name goes to the trash first.
- **Automatic balancing:** with `balance.schedule = "immediate"`, the trigger is checked at most once an hour, as a check plans the whole pool; with a time window, once a stretch of it; the moves stop at `balance.budget`.
- **The rest of the beta, in order:** the daemon's balancing, then the alert on an NTFS volume marked dirty, then the link guard.

## Names (specs.md §30)

Still open, though the solver, the scanner and `atoll names check` are built:

- Measure on the maintainer's Windows machine, with their go-ahead, what a report says is not established: which reserved names Windows 11 still refuses, and what Windows does with `Film.mkv` beside `film.mkv` on an NTFS volume (a VHD).
- `$LogFile` is read since 2026-11-21 (DONE.md), measured with restart pages written on the device, never with Windows: what Windows writes there at Fast Startup and at a full shutdown, on a data volume (a VHD), with the maintainer's go-ahead, and what ntfs3 then does with a journal it can replay.
- The type `ntfs` without `windows_names` (§22): `windows_names` is recommended since 2026-11-19, as the maintainer chose; whether Atoll leaves a name holding `" * < > ?` alone on a disk mounted without it, as a name refused, is still to decide, and its own trash move on such a name is still to measure there.
- Run `test/loopdev` on the type `ntfs` in qemu (mainline 7.3), and consider booting Debian sid's 7.3.3, the kernel with no ntfs3 (point 7 of the report).. Measured in `docs/research/ntfs-driver.md`, whose last section lists the decisions it needs. Until then a disk mounted with it is `readonly`.

## Pure core still without vectors (specs.md §29.4)

- **Reachable capacity** (§13.3): `atoll capacity` has five scenarios and no vector. It is worked out in `internal/watch/capacity.go`, outside the pure core, from `solver.PlanEach` and `solver.Capacity`: a vector file needs that part moved into `internal/solver` first. Moving copies when rebalancing, the hysteresis of `balance.trigger`, and a file whose copies cover too few domains have their vectors (`plan.json`, `percent.json`, `daemon_wake.json`).

## GitHub Actions and releases (DONE.md, 2026-11-19)

- The first release, `v0.1.0` (its `.deb` `0.1.0-1`), once the core is proven and after the soak of 24 hours: he pushes the tag, checks the drafted release and its attestations, publishes it.
- Before it, the maintainer's: the ruleset that keeps tags `v*` to him, a setting of GitHub's no file can make (`README.md`, "Protecting the tags `v*`, on GitHub"; threat model G21). And a first run of the release's three jobs (DONE.md, 2026-11-22), which no run has tried on GitHub: a tag of a prerelease, `v0.1.0-rc.1`, on a commit the CI passed on, then its draft deleted. On 2026-11-22 the API listed no run of `ci.yml` on `main` at all, `b6c1104` (#23's merge) included: a tag there is refused until the CI runs on it, by a dispatch on the tag.
- `CLAUDE.md`, the maintainer's to change, still says that GoReleaser drafts the release, where `release.yml`'s last job now does, and names only `mutation.yml` for the mutation tests, not `mutation-weekly.yml` (DONE.md, 2026-11-22).

## After the two flaws TLC found, fixed on 2026-11-19 (DONE.md)

- A file renamed while a job runs reappears at its old name on that disk once the reprise finishes the copy: the reprise could take `first_seen_ns` from the operation's `started_ns`.
- `atoll scan` and `conflicts resolve` still touch a running job; `restore --to` records the look after its syncs.
- Run in qemu the loop-device tests these two fixes touch: replicate, a device failing during a copy, copies at once, precision, mode, owner, break glass, the failed mount; ntfs-3g first.
- The copy state `unavailable`: say what sets it, or say in §4 that nothing does.
- Next TLA+ models, by risk: the view's journal and its parts written in parallel, renames and folders, the SSD cache's transfer, the link guard, copies at once. tla2tools 1.8.0 once released.

## The threat model after 2026-11-19 (DONE.md)

- The audit by scrutineer of 2026-11-22 (DONE.md): its eight findings are checked and the real ones fixed; run it again on `beta`, whose head it did not see. Two pools on one machine with one account: `tellDaemon` counts the other pool's daemon too, as it does not compare `--config`. The media servers are told in the daemon's own turn: Plex within a budget since 2026-11-22, Jellyfin and Emby a hundred folders a request, each up to 10 s.
- The threat model's low gaps G7 to G17, G19 and G20 (G20 only partly covered by `UMask=0027`: what root's `atoll init` makes still takes root's umask); of G21, done on 2026-11-22, the ruleset is left, and Dependabot does not update the images' digests: pin them again now and then, or a tool that does.
- Measure the remount through systemd with systemd 252 (Debian 12, openmediavault 7): the polkit rule is checked there, the remount itself is not.
- The view (experimental): a file or folder it makes takes the caller's group, not a setgid folder's; a non-owner's write to a setuid file whose presented mode differs from its disk's fails until a pass. Run `TestViewAsAnotherAccount` on Linux 7.3 too.

## The tests after the gaps of 2026-11-19 (DONE.md)

- The race detector fails on `beta`'s `25a5687`, in the CI's `race` job (run 44978285362, push and pull request): a `DATA RACE` in `TestPlexScansWithinABudget` (`internal/notify`), the test F050's fix brought. Found on 2026-11-22 while trying the release's check of the CI, which refuses that commit; not looked at further.

- The gates of `ops`, `trash` and `watch`, set on 2026-11-22 at their measures rounded down and run each week (DONE.md), to raise as their mutants that live are killed or named, as the pure core's were: `ops` 123 lived and 669 not covered, most of the first an `err != nil` after a step made `err == nil`, which its own tests do not fail (`disk.go`, `linkguard.go`, `view.go`, `repair.go`), most of the second the reprise (`recover.go`, 254), which the tests of `jobs` and `trash` run: its own tests of the reprise and of each step's error first; `trash` 100 lived, `trash.go` 47 and `restoreto.go` 23, among them `conflicts.go:159:13`, `s.apply && summary.Stayed == 0` made `||`, which would resolve a conflict in the catalog on a dry run; `watch` 199 lived, mostly in the comparison of a walk to the catalog (`scan.go` 53, `verdict.go` 47), among them each term of the check that the mount and the root did not change under the walk (`scan.go:357`), which only the loop-device tests trip. `ops` measures 84.15 % now, past its gate of 83. `trash`'s gate leaves about four mutants of margin, `watch`'s about six: a first weekly run on GitHub is to read. `jobs` has no gate: gremlins runs every test of the package for each mutant, half an hour a run with one CPU; a tool that runs only the tests that reach a mutant would measure it.
- What still knows a file by its inode alone before it moves or removes it, left so on 2026-11-21 without the maintainer, to confirm (DONE.md, "Inode + taille + mtime"): an entry of the trash, in a restore, a purge and their reprises, as the `trash` table records its size and no modification time (a column would be format version 2); a link of the guard and a part in `tmp/`, Atoll's own names; the view's moves for a user, which its caller looked at right before; and the copy a move of the view left on a full disk, when its reprise finds it in the trash already, as no row records it.
- An SSD-cache copy a transfer sent to the trash stays for the retention when a fault comes between its move and its purge (§19), kept rather than purged at once; and a disk lost earlier in a pass keeps its own set of the view's attributes (§21.2).
- A guard link a purge cannot remove for an error that is not the disk's (`EACCES` on its unlink) only gets a warning, and the purge goes on: the purged data stays behind that link until the next pass (DONE.md, the four flaws).
- Raise the coverage of `cmd/atoll`'s unit tests (50.8 %, 86.5 % with the scenarios) and of the view (`fusefs` 0 %, `readonly` 13 %, `writer` 76 %), run only in qemu: count the loop-device tests too, with `GOCOVERDIR` in the guest.
- A pass's rename (§15.12, step 3) counts a `failed` disk, which no pass walks, as a disk not looked at that may hold the model of a new parent: with one in the configuration, a rename whose parent no disk present holds waits for good, the copy left behind. A copy waits for no failed disk (DONE.md, 2026-11-21, a copy's folder made like the one on another disk). Wait for a failed disk in neither, as a copy does, or in both: for the maintainer.
- A copy recorded unverified after a reprise (flaw 1's limit, DONE.md) waits for `--full` or a scrub to be read; and which two drivers' renames left the change time as it was on Linux 6.12 is to measure.

## Atoll's own view, after 2026-11-19 (DONE.md)

- Run in qemu `TestViewWrites`, `TestViewXattrs`, `TestViewWritesInParallel`, and again the replicate, cache, link guard and break-glass tests, as the copy pipeline changed (a resume compares, `restore` gives the attributes).
- Serve `user.` attributes from the catalog, so that Samba with `store dos attributes = yes` browses without waking disks: a table, and a list of attributes for each file in the walks.
- Give an attribute change to a copy whose disk was away once it is back: today it keeps its old set.
- Let a part written in parallel follow a rename made after its close (qBittorrent's `.!qB`), rather than drop it.
- Skip the read of a whole file that a link or an attribute through the view only touched in its change time.
- The two names of a hard link have their own `st_ino` through the view.
- `make test-integration` runs nothing: `test/integration` holds only `doc.go`.
- The table of crash points in `conformance/scenarios/README.md` does not list `xattrs-set`.

## The review's decisions, made by the maintainer on 2026-11-13, to do

Each point the review left to decide was put to the maintainer as a question, with its options and what each costs. What he chose is below; the reasons and the costs are in the entries of the sections that follow, which stay until each is done. Each is done as any fix: a test that fails first, the spec, DONE.md.

16. **The soak.** The one of 72 hours, started again on `42e9c65` on 2026-11-14, was stopped by the maintainer; one of 12 hours on 200,000 files a disk passed (2026-11-14, `f438a3a`, DONE.md), and again on the passes that look at names (2026-11-18, `2d016b3`), which the maintainer asked for once phase 2b was done; a third, on the first pass held apart (`ab9e21f`), was stopped after 9 hours on 2026-11-19 at his request, its memory flat and under the 200 MB at every tenth (DONE.md), to run a soak again once the features he decided that day are merged. One of 24 hours passed on 2026-11-21, on `5a3fc74`: 1720 passes, its memory flat and under the 200 MB at every tenth (DONE.md). Still to do: the try of the notifications on the real Plex and the openmediavault plugin. With them, on the maintainer's own disks, which disk spins (§10): CHECK POWER MODE on a SATA disk asleep and awake, and behind each USB bridge of the pool, as no disk sleeps in qemu; the log says which bridges give no answer, and those disks count as spinning. Tried on 2026-11-19 on his machine under Windows, `smartctl -n standby` every 5 min (`bin/power-check/check-power-mode.ps1`): a SATA disk (WD200EDGZ) read in standby three times in a row without waking; both USB bridges (WD Elements 25A3, Seagate Expansion HDD) pass the command and answer, but neither of their disks was seen in standby in an hour. Whether they never slept, or each question keeps them awake, which matters as the daemon asks an idle disk every 30 s (§10), is to tell by a run every 30 min, then under Linux. And the SSD cache (§19) on a real SSD, with Plex reading through Atoll's own view while files leave the cache. And the UPS through NUT (§20) on the maintainer's own UPS: unplugged, then plugged back, Atoll stops its copies and starts them again, as its log says; the low battery is not tried there, as upsmon would shut the machine down.

## What the review of the code found, still open (2026-11-13)

Eight reads of the whole code, by area, for performance, security and conformance to the spec, at the commit of pull request #21. What follows was found by reading and is not fixed: each is to check in the code as it is now, then to fix after a test that fails without the fix. What was fixed is in DONE.md.

### Security

- **A work folder that becomes another account's after `disk add`** gets the alert `workdir-owner:<id>`, and Atoll still writes there, as the maintainer decided on 2026-11-13 (§10): an accepted limit. Until someone acts on the alert, the owner of a disk's top can rename `<workdir>`, make another, and read what Atoll copies there.

### Performance, left as recommended (2026-11-14)

Left as recommended on 2026-11-14, with no question put: the jobs read whole, the renames by rounds, the bursts of the watcher, `cannotLeave` and `GonePaths`; `guard list` is to write as it reads when the local API serves large pools. The first is now due (below).

What the review found costly, less what a test and a fix settled (DONE.md). None is a fault of what Atoll decides: each is time, memory or a lock held. For each, what it costs, what would change it, and what is recommended, by what it gains (performance), what it risks (security), what it changes (functionality) and when it counts (product).

- **The jobs are read whole** (`internal/catalog/replicate.go`, `propagate.go`): by their plans, the folder jobs and the trash jobs of a disk scan the table, `DueReader` walks `jobs_to_do` from its start for each page of 1,000, and `ReplicateJobs` below a path and `JobOfPart` read every job of a file. The table holds what waits: nothing most days, and 200,000 rows while a new pool makes its first replicas, when a pass reads it some ten times, a second or two beside the two minutes its walks take. Recommended then: leave, until the passes walk only what changed. They do since 2026-11-17 (DONE.md), so an index on `jobs(kind, dst_disk)` and a `DueReader` that seeks past its last key are now due: its query carries the key, but the `OR` of its condition makes SQLite scan `jobs_to_do` from its start for each page.
- **Renames by rounds**: a chain of files each renamed to the next's name costs the square of its length, in the plan (`assignTargets`, `internal/solver/propagate.go`) and in the pass (`internal/jobs/work.go`); `addParents` stops at the first ancestor it holds, the square of the depth, and the walk is bounded by the limit of a mergerfs branch (DONE.md, 2026-11-19). No library has such shapes, and whoever can write one can already fill the disk. **Recommended: leave.**
- **The watcher gathers the events of a burst before it sends any** (`drain`, `internal/watch/watcher.go`): what the kernel's queue holds, 16,384 events and a few MiB at most, unless events come as fast as they are read. And each slice of the hashes starts again from the first file with no hash (`internal/watch/scan.go`), which a look at its name now settles when it is no longer what its row says. **Recommended: leave.**
- **`cannotLeave` asks about every replaced copy once for each folder that waits** (`internal/jobs/pass.go`); `guard list` and `/v1/guards` hold every held path three times, some 60 MB for 200,000 paths of a guard that tripped; `GonePaths` reads every `copy-gone` event the catalog ever recorded (`internal/catalog/linkguard.go`), by its plan, each time a link's file has no row left, a tenth of a second for 100,000 events. **Recommended: leave** the first and the last; `guard list` is to write as it reads, when the local API serves large pools.

## Phase 1a, the alpha: what is left, and its known limits (specs.md §28)

- Memory of the daemon on 200,000 files (§26): `atoll scan` and every pass of `daemon --until-idle` measured stay under the limit, the highest at about 187 MiB, a scan after a rename of a folder holding everything. The daemon that runs for good, with its watcher and its grouping, held four hours of a change every second on 4,000 files, its heap in use between 2.8 and 6.8 MiB with no growth (§26). On 200,000 files, 12 hours are measured (2026-11-14, `f438a3a`, DONE.md): 293 passes, its heap in use 3.6 to 3.9 MiB, 8 goroutines and 14 open files at every tenth, with no growth, and at most 220.4 MiB resident. Again on 2026-11-18 (`2d016b3`, DONE.md), once passes looked at names: 911 passes, 3.3 to 3.6 MiB of heap in use, 9 goroutines and 14 open files at every tenth, with no growth, and at most 180.7 MiB resident at each tenth. Over days, it is still to measure: `SOAK=24h SOAK_FILES=200000`.
- The 200 MB of §26 on 200,000 files a disk: every step of `TestScale` is under it since 2026-11-15, at 183.8 MiB at most on `47ab5f6`, the link guard's passes included. The soak's passes are under it since 2026-11-18, at 180.7 MiB at most, and its first pass at 181.7 MiB since the plan holds apart what few files need (DONE.md). The soak of 24 hours confirms it over a day (2026-11-21, DONE.md): 173.0 MiB at most at every tenth, its first pass 180.4 MiB.
- What the daemon that runs for good (§15.13) leaves:
  - the watcher's limits: under a filesystem mark, an event past `PATH_MAX` comes at `""`, and its disk is looked at whole, as `/proc` names no path that long (§15.1); an Atoll command that ended before the watcher read its events reads as `user`, and wakes a pass;
- What the copy pipeline leaves for later (§14): the creation time set after the rename on Windows (§7); balancing moves one file at a time. Copies at once (DONE.md, 2026-11-19) are to measure on the maintainer's SSD cache and HDDs, and their loop-device tests to run in qemu.
- What modifications leave as limits (§15.4, §15.12): a swap of two names whose other copies are on NTFS, whose drivers cannot exchange two names, and a copy replaced by anything but a regular file, which are only reported. Two folders that swap their names stay a limit too: no walk reads them as renamed (DONE.md).
- What pull request #3 left of renames and deletions: judge a port written from the documents on the 269 new scenarios, which needs a port of the daemon, and settle three gaps no vector reaches (specs.md §15.12): a file reaching its own path as a later target, a behind copy first seen after the file left, and whether a file both not due and held says wait or held. The decisions taken there without the maintainer, listed in [DONE.md](DONE.md), were confirmed on 2026-11-12.
- The rest of the trash (§17): a folder Atoll makes takes the mode and owner of the one it stands for, not its modification time.
- The openmediavault plugin's form (`atoll mergerfs-config --omv`, §21.1, DONE.md, 2026-11-19): still to try in the form itself, in a browser, on real disks, and on openmediavault 7, whose plugin differs (`docs/research/omv-mergerfs.md`, "Not measured").
- The link guard (§17): a command that sends a copy to the trash leaves its link to the next pass.
- `balance` (§12.4, §13.3): `atoll status` plans the whole pool to give `bytes_placed` whenever a strict setting exists: at 200,000 files a disk that doubles its time, 1 min 9 s for 31 s in qemu without KVM, under the 200 MB (§26). The local API's `/v1/status` does the same in the daemon's own process, beside its passes.
- The media-server notifications (§21.5), tried on real servers in containers on 2026-11-19 (`make test-mediaservers`, DONE.md): Jellyfin 12.4.0 and Emby 4.10.2 list a movie written, renamed and deleted, each told as the daemon tells it, through `path_map`, in two runs in a row. Plex 1.43.5 is tried too since 2026-11-20, unclaimed, the test in its own network namespace (DONE.md); a claimed Plex, and the test's `allowed-network` case, are the maintainer's to run, with his own claim token. Plex ignores a partial scan in a library none of whose scans found a file yet, until a scan of the whole library finds one, and Jellyfin did the same in one run (§21.5): Atoll asks for none, as the maintainer decided on 2026-11-20, and a library is scanned once from the server itself (§21.5, `deploy/README.md`). The real Plex is still to try once its libraries read the merged view. `atoll scan`, a command, tells no server of the new files it records.
- What the alerts leave (§23): nothing of phase 1b; SMART, Gotify, SMTP and a volume marked dirty are done. Without udev, as in a container, no `/dev/disk/by-uuid/` names the device, and a dirty volume is not seen. No reading of SMART sets the state `standby` (§4), and none is to, as the maintainer decided on 2026-11-14: one an hour old says nothing of now, and `smartctl -n standby` would have to be asked before each pass. The counts of the alerts and of `/metrics` read the catalog after each pass, with no index on the states. Passes of about 7 s on 200,000 files a disk include them (2026-11-18); measure them alone before an index on the states.
- The operation journal (`ops`, and `view_ops` for Atoll's own view) covers copies, renames, the trash, restores, repairs, folders made and removed, times, modes and owners, and the link guard's links (§14.2, §17.1, §17.2). Each new kind of operation needs its own reprise, with kill -9 at each step and fault injection.
- The break-glass test runs in `test/loopdev` (`TestBreakGlass`, `TestBreakGlassWithADiskAway`), so in the CI's qemu job, on two disks and every driver: a pool of more disks is left. Atoll's own view is compared with plain mergerfs after reads (`TestViewMatchesMergerfs`) and after writes (`TestViewWrites`, `TestTheCacheOnRealDisks`). The read-only view, `atoll mount`, is to try on the maintainer's Plex with the rest of §21.5, its libraries at `view.readonly_mount`.

## Later phases (specs.md §28)

- **Passes that walk only what changed, in phase 2b**, as the maintainer decided on 2026-11-14: under a change every second on 200,000 files, passes that walk every copy follow one another, about one core's worth on a machine, against the 5 % of §26 (DONE.md, the soak on 200,000 files). Done by disk on 2026-11-16, to spare the disks' sleep (§15.13): a disk that nothing changed on is not walked. Done by name on 2026-11-17 (DONE.md): a pass for events looks at the names the watcher heard alone, and plans only what may have changed, 12 ms for a pass given one name on 50,000 files a disk, against 1 s. Measured over 12 hours on 2026-11-18 (DONE.md): about 7 s a pass for 105 s, 2 hours of passes in the 12 for 9. Measured over 24 hours on 2026-11-21 (DONE.md): 8.9 s a pass at the median, 4.5 hours of passes in the 24. The operations of Atoll's own view are taken in by their names since 2026-11-19 (DONE.md).
- **Observability**: done in Go, as the maintainer chose on 2026-11-19 (DONE.md): the log in OpenTelemetry's data model, and an optional OTLP/HTTP export of the metrics and the log (§23.2); traces left out. Still to do: measure `TestScale` again in qemu with the new binary, OTLP off and on, as about 5 MiB more of the binary's pages come near the 190.7 MiB; try the export against a real Vector, Elastic Agent and OpenTelemetry Collector in containers, as `test/mediaservers` does; confirm the choices made alone.
