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

## Names (specs.md §30)

To decide before the solver, the scanner and `atoll names check` need them:

- **decision**, before phase 2a: should Atoll's own view refuse, with an error, a rename that a copy's filesystem refuses, instead of following §15.3?
- Pin in `test/loopdev` what mergerfs and ntfs3 do with a refused name or an `ads` stream (`docs/research/name-compatibility.md`), re-reading through a cold cache, so that a release that changes it fails a test. The measurements so far were throwaway scripts in qemu; `make test-qemu` runs `test/loopdev` on both kernels, and its guest now has mergerfs, which `TestBreakGlass` mounts.
- Measure on the maintainer's Windows machine, with their go-ahead, what a report says is not established: which reserved names Windows 11 still refuses, and what Windows does with `Film.mkv` beside `film.mkv` on an NTFS volume (a VHD).
- NTFS 8.3 short names: never decide that a file exists with `stat` alone, and treat an `EEXIST` without a visible exact entry as name-incompatible.
- `disk add`: refuse ext4 `casefold`, XFS `ascii-ci`, ntfs3 `nocase`, ntfs3 `ads` and lowntfs-3g `ignore_case`, which the mount check of §10 already reports. Watch `FS_CASEFOLD_FL` on folders under `<root>/`.
- **decision**: the `ntfs` driver of Linux 7.1 and later (§10, §15.8, §22), the only NTFS driver of Debian 14's kernel: support it like ntfs3, keep it `readonly`, or something in between. Measured in `docs/research/ntfs-driver.md`, whose last section lists the decisions it needs. Until then a disk mounted with it is `readonly`.

## Pure core still without vectors (specs.md §29.4)

- **Solver, phase 1b** (§13.3): moving copies when rebalancing (`atoll balance`), the hysteresis, and the reachable capacity as a dry run of that plan (`atoll capacity`), which needs the moves to mean what it says. Moving an Atoll copy also fixes a file whose copies cover too few domains, which stays `degraded` until then.
- **Solver, phase 2b** (§12.4): `colocate` and the `ordered` and `percent` strategies. Until then the caller gives the plan `most-free` and no `colocate` (§12.2): `solver.Placement` has neither.
- **BLAKE3**: run `conformance/vectors/blake3.json` on real arm64 and AVX-512 hardware, measure throughput there, and move `zeebo/blake3` to a tagged release when one holds the arm64 backends (`docs/research/blake3.md`).

## Contracts not yet defined (specs.md §29.3)

Every item here is a **decision**:

- `api/openapi.yaml` and `api/schemas/` for the `--json` outputs.

## Phase 1a, the alpha, not started (specs.md §28)

- Configuration in the shell (§24.3, §10):
  - in `atoll rules check`, the rules that no file reaches or that an earlier one hides (§11.3);
  - `--help`, the French translation of the CLI, and human output that writes durations as the configuration does: `atoll rules explain` prints `settle_ns = 600000000000`;
  - the fallbacks of §12.2 for the modes, when jobs act on them: `rules.Policy` holds what the configuration asks for, and `atoll scan` already gives the plan only what has an effect;
  - `disk add` and `disk remove` editing the file: every other byte kept, the result re-read before it replaces the file.
- After `catalog rebuild`, the disks' trash entries are not in the new catalog: list them again from `trash/<date>/<op id>/`, with a reason to choose, and their expiry from the date.
- `atoll init` over a catalog that went missing must start `ops.id` past the ids in each disk's trash and `tmp/`, as `catalog rebuild` now does, so that a new operation's folder or `<op id>.part` does not meet an old one (§14.2, §17). Until then such a meeting costs one `io-error`, and the next run takes another id.
- Memory of the daemon on 200,000 files (§26): `atoll scan` and every pass of `daemon --until-idle` measured stay under the limit, the highest at about 187 MiB, a scan after a rename of a folder holding everything. The daemon that runs for good, with its watcher and its grouping, is still to measure over hours of changes.
- What the mount check of §10 leaves to do for disks:
  - `atoll disk show`, which could show `time_precision_ns`, and reading `Atoll/disk.toml` at each mount check;
  - the topology check of R-06 at each start, not only at `disk add`, with the catalog's `disks.device`.
- What the daemon that runs for good (§15.13) leaves:
  - the watcher's limits: a folder that leaves the root under inotify and comes back in another read is watched again from scratch; a path past `PATH_MAX` cannot get a mark of its own on ntfs-3g; the process of an event already gone reads as `user`;
  - the remount after a USB reset of an ntfs-3g disk, through `mount.ntfs-3g`, as the dedicated user, is not tested.
- The modes `immediate`, which needs the watcher to know a file closed and stable, and `scheduled`, which runs inside `[replication].window`: until then both fall back to `deferred`, which `daemon --until-idle` runs, settle counted from when a file was first seen (§12.2, §12.3, §14.1).
- What the copy pipeline leaves for later (§14): one copy at a time, in one pass, is all the daemon runs, so `max_parallel_per_disk` waits for its loop; the creation time set after the rename on Windows (§7).
- A folder renamed on one disk into the old name of one that waits there (`mv Dx N; mv D Dx`, while a copy below Dx on another disk has a clock running, is held by the guard, or cannot be read): what only other disks hold below Dx stays there, in the folder now named Dx (specs.md §15.12, `daemon-folder-rotation-while-waiting`). The fix holds back what lands in that old name, folders and copies moved in from elsewhere, until the one that waits is decided. It needs that wait known before the files of a pass are decided, and must not block a cycle, where each folder lands in the other's old name.
- The grouping of events per path comes with the watcher (§15.1).
- What modifications leave as limits (§15.4, §15.12): an overwriting move (`mv P Q`, Q there) and a swap of two names on one disk, and a copy replaced by anything but a regular file, which are only reported. `deletes.direct = "restore"` (§15.5), which the copy pipeline now allows, still propagates nothing.
- What pull request #3 left of renames and deletions: judge a port written from the documents on the 269 new scenarios, which needs a port of the daemon, and settle three gaps no vector reaches (specs.md §15.12): a file reaching its own path as a later target, a behind copy first seen after the file left, and whether a file both not due and held says wait or held. The decisions taken there without the maintainer, listed in [DONE.md](DONE.md), were confirmed on 2026-11-12.
- A volume that ntfs3 left with names that disagree (§17.2) shows only as a warning in the log and exit 4 on every run on that disk: say what to do, which is a repair of the volume, in an alert, which §23 does not list yet, and in `atoll status`.
- The rest of the trash (§17): the `.chunks` folders with `repair`, `restore --to` with the copy pipeline, the link guard, and the reasons `rebalance` and `repaired`, which come with balancing and repair. A folder Atoll emptied in a run that was stopped before its end stays, as nothing records it (§17.5). A folder's attributes other than its mode and owner wait for the pipeline.
- The scrub: `files.state` holds one state, so a file both `corrupt` and in conflict shows `corrupt`, and `repair` (phase 1b) must look for the conflict again before it clears that.
- The settings of the openmediavault mergerfs plugin from `atoll mergerfs-config` (§21.1), which the maintainer left for later: the fstab line and `--json` are done.
- What the alerts leave (§23): Gotify and SMTP, SMART and a volume marked dirty, with phase 1b. The counts read the whole of `files` after each pass, with no index: to measure on 200,000 files, before an index on the states that alert.
- The operation journal (`ops`) covers the trash, the renames of propagation, files and folders, and the copies (`create`) so far (§14.2, §17.1, §17.2). Each new kind of operation needs its own reprise, with kill -9 at each step and fault injection.
- The break-glass test runs in `test/loopdev` (`TestBreakGlass`), so in the CI's qemu job, on two disks and every driver. What it leaves: a pool of more disks, a disk away when Atoll goes, and, once Atoll's own view comes (phase 2a), comparing that view with plain mergerfs.

## Later phases (specs.md §28)

- **Observability, for phase 3 (the Rust rewrite)**, as the maintainer asked on 2026-11-12: OpenTelemetry, Vector, or both. OpenTelemetry would give the daemon's traces, metrics and logs over OTLP. Vector (vector.dev) would collect the logs and metrics and send them on. Phase 2b already brings Prometheus's `/metrics`; what the rewrite exports, and through which, is to decide then.
