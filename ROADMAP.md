# Roadmap

Where Atoll stands and what remains before each milestone. The scope of each phase is fixed by [specs.md](specs.md) §28, which this file follows. [TODO.md](TODO.md) details each open item, and [DONE.md](DONE.md) the finished work.

## Milestones

A milestone is reached when its phase and every phase before it are done.

| Milestone | Phase | What it brings | Progress |
|---|---|---|---|
| **alpha** | 1a — Safe core | a pool used through mergerfs, whose copies Atoll makes, verifies and keeps in step, safe to stop or uninstall | about 98 % |
| **beta** | 1b — Complete Linux MVP | every replication mode, repair, SMART, notifications, `balance` and `capacity` | about 98 % |
| **v0.1** | 2a — Own view | Atoll's FUSE view in place of mergerfs, and the contracts frozen at format version 1: the first stable version | about 97 % |
| — | 2b — Disk sleep | SSD cache, NUT, the `ordered` and `colocate` strategies, Prometheus | |
| — | 3 — Rust rewrite | the same contracts and conformance suite, switched to without migrating data | |
| — | 4 — Windows | a Windows service and a WinFsp pool drive, in Rust | |
| — | 5 | web interface, openmediavault plugin, duplicates by metadata | |

Progress counts everything needed to reach the milestone, weighted by an estimate of the effort each part takes. It is an estimate, not a measurement, as of 2026-11-16.

## Alpha: phase 1a, the safe core

### Done

- **Configuration and rules.** The TOML configuration with its schema and presets, ordered rules giving each file its policy, the in-house glob matcher and the regex subset shared with Rust (§11, §24, §29.5).
- **Names.** Name compatibility per filesystem, NFC and NTFS collisions, path lengths, and the name report (§15.7 to §15.9).
- **Solver and catalog.** Placement, the capacity formula and the original of each file (§7, §12, §13), in a pure solver. The SQLite catalog, with its migrations (§9).
- **Disks.** Identity read from the superblock, the mount check, and the confirmation of a disk past the page cache before any write (§10).
- **Commands.** `atoll init`, `scan`, `status`, `rules check` and `explain`, `names check`, and `disk list` (§15.9 to §15.11, §25).
- **Copies.** The copy pipeline (`atoll replicate`), verified before it is renamed into place, and journaled with its reprise after a crash (§14, §17.2).
- **Propagation.** `atoll daemon --until-idle` propagates renames, deletions and modifications, and applies changes of modification time, mode and owner to the other copies. It runs the `deferred` mode (§12.3, §15.12).
- **Mass guards.** The guards against mass deletions and mass modifications, with `atoll guard` (§15.6).
- **Trash.** `atoll prune`, `trash list`, `restore`, to its own disk or `--to` another through the copy pipeline, and `purge`, the purge by retention and `max_size`, also run by the daemon, and Atoll's own empty folders removed (§17).
- **mergerfs.** `atoll mergerfs-config` (§21.1).
- **The daemon that runs for good.** fanotify, or inotify, with the events grouped per path; the catalog's lock taken per pass, with commands waiting for it; rescans and new files' hashes inside the maintenance window; the mount check every 30 s; mergerfs's branches changed at runtime; the remount after a USB reset; reload on `SIGHUP`; a systemd unit (§15.13).
- **The command line.** `--help`, a French translation chosen by the locale, and durations written as the configuration writes them (§25).
- **The local API.** The daemon answers status, disks, trash and guards over a Unix socket, and over TCP with a token on loopback or Tailscale (§25.1).
- **Alerts.** ntfy alerts after each pass, kept in the catalog so that a restart sends again only what is due, and `atoll notify test` (§23).
- **The catalog.** `atoll catalog backup`, every night by the daemon too, `verify` against the disks, and `rebuild` from them (§9).
- **The scrub.** `atoll scrub`, and the daemon inside `integrity.window`, check each chunk of the copies due, quarantine a corrupt copy when a healthy one exists, and resume at the chunk the budget reached (§16).
- **The disk commands.** `atoll disk add`, `show`, `remove` with `--evacuate`, `fail` and `replace`, which edit the configuration keeping every other byte, check the topology and the mount point, and drain a disk through the daemon's passes (§10, §18).
- **Tests.** The break-glass test, and the failed-mount test (§27).
- **Contracts and conformance.** 45 vector files and 543 black-box scenarios shared with the Rust version (§29).
- **Local CI.** Race detector, integration, conformance, lint, and `test/loopdev` in qemu on Linux 6.12 and 7.3 (`test/ci/run.sh`).

### Remaining

The maintainer settled the open questions of the alpha on 2026-11-12 (TODO.md, "Decisions for the alpha").

1. **The memory of a daemon that runs for days**, measured. Four hours are: its heap in use stays between 2.8 and 6.8 MiB, with no growth (§26). On 200,000 files a disk, 12 hours are, on the code of the review's decisions: 293 passes, its heap in use 3.6 to 3.9 MiB, with no growth. Days remain: a soak of 24 hours, after phase 2b unless the maintainer asks sooner (2026-11-16). The most it holds went past the 200 MB of §26 at that size, up to 220.4 MiB in the soak and 229.6 MiB in the link guard's passes, by transparent huge pages the kernel kept resident: with them off and a soft limit of 155 MiB, done on 2026-11-15, every step measured peaks at 181.6 MiB at most, which the soak is to confirm (TODO.md).

## Beta: phase 1b, the complete Linux MVP

- **Modes.** Done: the `immediate` and `scheduled` replication modes (§12.3).
- **Repair and duplicates.** Done: the repair of corrupt copies, by chunk or whole (§16.1), and the report of duplicates, `atoll dupes` (§16).
- **Placement.** Failure domains and tags as hard constraints, and `atoll balance` and `capacity`. The constraints are done: a rule's tags and domains, `tags_mode` and `spread_mode` in strict mode, and `overflow = "deny"` reach the plan, as does the capacity formula. `atoll capacity` is done, and so is `atoll balance`, by hand and by the daemon inside `balance.schedule` once `balance.trigger` holds, which moves the copies Atoll made that a change of rule leaves misplaced, and with `--move-originals` the user's copies too. `atoll status` gives the bytes correctly placed.
- **Monitoring and notifications.** Done: SMART, Gotify and SMTP alerts, the alert on an NTFS volume marked dirty, and notifications to Plex, Emby and Jellyfin.
- **Conflicts.** Done: `atoll conflicts list` and `atoll conflicts resolve` (§15.7).
- **Link guard.** Done: the optional link guard against deletions and replacements through mergerfs (§17).

Every item of phase 1b is done. What the beta still needs is the alpha's: the memory of a daemon that runs for days, at 200,000 files. The link guard and `bytes_placed` are measured there (§26): with transparent huge pages off and a soft limit of 155 MiB, every step measured, the guard's passes and a strict `atoll status` included, peaks under the 200 MB, at 181.6 MiB at most (TODO.md).

## v0.1: phase 2a, Atoll's own view

- **Atoll's FUSE view.**
  - Done, the read path, read only (DONE.md, 2026-11-16): it presents the original of each file, never a stale, corrupt or unavailable copy, and serves every entry and `statfs` from the catalog, so that browsing wakes no disk. Mounted beside mergerfs over the same disks in qemu, on every driver and both kernels, it lists and reads what mergerfs does, but the original where a replica differs.
  - Chosen on 2026-11-16: go-fuse, pure Go, as cgofuse does not mount where libfuse is 3.17 or later, Debian 13 included; and first the view read only, beside mergerfs, for the media servers, with mergerfs as the engine that writes.
  - Done, that view beside mergerfs (DONE.md, 2026-11-16): `atoll mount` serves it at `view.readonly_mount`, as a process of its own, `deploy/atoll-mount.service`, and checks the disks' mounts every 30 s. Measured in qemu on both kernels, as root and as the unit's user with its two capabilities alone.
  - Done, the writes (DONE.md, 2026-11-16): with `view.engine = "atoll"`, `atoll mount` serves the view at `view.mount` in place of mergerfs. Each operation reaches one disk, a folder's every disk that holds it, and the passes take it to the other copies; a deletion goes to the trash. Measured in qemu with Sonarr's own kinds of operations, then break glass with plain mergerfs.
  - To do, beyond phase 2a's list: a write that meets `ENOSPC` moved to another disk, hard links, extended attributes, copies written in parallel in `immediate` mode.
- **The contracts frozen at format version 1**: catalog schema, on-disk layout, configuration keys, local API, `--json` schemas and exit codes. This is what the Rust rewrite (phase 3) must keep.
  - Done on 2026-11-16, as the maintainer chose: specs.md §29.3 says what a later version keeps, and `conformance/frozen/v1.json` pins it, which a test checks (DONE.md). Every choice DONE.md listed as to confirm was confirmed the same day.
- Phase 2a is done, `read.policy = "prefer-spinning"` and `read.classes` with phase 2b's knowledge of which disk spins (DONE.md, 2026-11-16). What v0.1 needs besides is the alpha's: the memory of a daemon that runs for days, the soak of 24 hours, after phase 2b unless the maintainer asks sooner.

## Phase 2b, disk sleep

- Done, the `colocate` and `new_files` strategies (R-28, DONE.md, 2026-11-16): a replica, and what is new through the view, goes where the other files of its folder or show are, then as `new_files` says, `ordered` filling the disks in order to `fill_to`, `percent` to the same share; the mergerfs line creates with `ff` or `pfrd` for them.
- Done, passes that spare the disks' sleep (§15.13, DONE.md, 2026-11-16): a pass for events or `next_due` walks only the disks something calls for a look at, and neither walks nor confirms the others, which it opens only for work that waits there.
- Done, which disk spins, and the view's reads by it (§7, §10, DONE.md, 2026-11-16): the daemon's check tells `standby` from `online` by the kernel's counters, then by ATA CHECK POWER MODE, which wakes none, and records it; `read.policy = "prefer-spinning"` reads a copy on a disk that spins, then by `read.classes`, then the original.
- Done, the SSD cache (§19, R-26, DONE.md, 2026-11-17), with Atoll's own view: a new file lands on the cache, stays its residency, and leaves it in `replication.window`, or at once above `cache.max_fill`; its copies are made on the data disks, then its copy on the cache goes to that disk's trash and is purged at once. Left apart, as the maintainer chose: a write that runs out of space moved to another disk as it goes.
- Done, the UPS through NUT (§20, R-27, DONE.md, 2026-11-17): the daemon reads `ups.status` from `upsd` every 5 s; on battery, nothing that moves data starts; with the battery low, it stops its work, writes out the disks, and waits for line power.
- Done, `balance.equalize` (§12.4, DONE.md, 2026-11-17): with `percent`, Atoll's own copies go by the most free share of capacity, whatever `new_files` says; nothing is moved.
- To do: Prometheus `/metrics`; walks of only the folders that changed, for the CPU of a disk under constant changes (TODO.md).
