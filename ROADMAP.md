# Roadmap

Where Atoll stands and what remains before each milestone. The scope of each phase is fixed by [specs.md](specs.md) §28, which this file follows. [TODO.md](TODO.md) details each open item, and [DONE.md](DONE.md) the finished work.

## Milestones

A milestone is reached when its phase and every phase before it are done.

| Milestone | Phase | What it brings | Progress |
|---|---|---|---|
| **alpha** | 1a — Safe core | a pool used through mergerfs, whose copies Atoll makes, verifies and keeps in step, safe to stop or uninstall | about 55 % |
| **beta** | 1b — Complete Linux MVP | every replication mode, repair, SMART, notifications, `balance` and `capacity` | about 35 % |
| **v0.1** | 2a — Own view | Atoll's FUSE view in place of mergerfs, and the contracts frozen at format version 1: the first stable version | about 30 % |
| — | 2b — Disk sleep | SSD cache, NUT, the `ordered` and `colocate` strategies, Prometheus | |
| — | 3 — Rust rewrite | the same contracts and conformance suite, switched to without migrating data | |
| — | 4 — Windows | a Windows service and a WinFsp pool drive, in Rust | |
| — | 5 | web interface, openmediavault plugin, duplicates by metadata | |

Progress counts everything needed to reach the milestone, weighted by an estimate of the effort each part takes. It is an estimate, not a measurement, as of 2026-11-12.

## Alpha: phase 1a, the safe core

### Done

- **Configuration and rules.** The TOML configuration with its schema and presets, ordered rules giving each file its policy, the in-house glob matcher and the regex subset shared with Rust (§11, §24, §29.5).
- **Names.** Name compatibility per filesystem, NFC and NTFS collisions, path lengths, and the name report (§15.7 to §15.9).
- **Solver and catalog.** Placement, the capacity formula and the original of each file (§7, §12, §13), in a pure solver. The SQLite catalog, with its migrations (§9).
- **Disks.** Identity read from the superblock, the mount check, and the confirmation of a disk past the page cache before any write (§10).
- **Commands.** `atoll init`, `scan`, `status`, `rules check` and `explain`, and `disk list` (§15.10, §15.11, §25).
- **Copies.** The copy pipeline (`atoll replicate`), verified before it is renamed into place, and journaled with its reprise after a crash (§14, §17.2).
- **Propagation.** `atoll daemon --until-idle` propagates renames, deletions and modifications, and applies changes of modification time and mode to the other copies. It runs the `deferred` mode (§12.3, §15.12).
- **Mass guards.** The guards against mass deletions and mass modifications, with `atoll guard` (§15.6).
- **Trash.** `atoll prune`, `trash list`, `restore` and `purge` (§17).
- **mergerfs.** `atoll mergerfs-config` (§21.1).
- **Tests.** The break-glass test, and the failed-mount test (§27).
- **Contracts and conformance.** 39 vector files and 505 black-box scenarios shared with the Rust version (§29).
- **Local CI.** Race detector, integration, conformance, lint, and `test/loopdev` in qemu on Linux 6.12 and 7.3 (`test/ci/run.sh`).

### Remaining

1. **The daemon that runs for good**, built on the watcher measured in `docs/research/watcher.md`:
   - fanotify, and the grouping of events per path;
   - the loop, which looks only at the disks that changed;
   - the catalog's lock taken per pass, with commands waiting for it;
   - rescans at startup, after an overflow and on schedule, and hashing inside the maintenance window;
   - the mount check every 30 s, which wakes no disk;
   - mergerfs's branches changed at runtime;
   - the remount after a USB reset;
   - a systemd unit;
   - the memory of a daemon that runs for days.
2. **`atoll disk add` and `disk remove`**, which edit the configuration while keeping every other byte. Also the refusal of the variants that change name rules, and the topology check.
3. **The scrub**, detection only (§16).
4. **ntfy alerts**: a guard tripped, a disk `missing` or `readonly`, a `manual` file left unprotected.
5. **The rest of the trash**: `max_size` and its purge order, `restore --to`, the other reasons, and Atoll's empty folders.
6. **The command line**: `--help`, the French translation, durations written as the configuration writes them, and `atoll names check`.
7. **Smaller limits**:
   - an overwriting move or a swap of two names;
   - a folder renamed into the old name of one that waits;
   - operation ids after `catalog rebuild`;
   - NTFS short names;
   - the owner of a file applied to its other copies.
8. **The maintainer's decisions**:
   - the `ntfs` driver of Linux 7.1 and later (`docs/research/ntfs-driver.md`);
   - the local API and the `--json` schemas as a contract;
   - nested btrfs subvolumes;
   - the choices of pull request #3, to confirm.

## Beta: phase 1b, the complete Linux MVP

- **Modes.** The `immediate` and `scheduled` replication modes; the watcher brings what `immediate` needs.
- **Repair.** Repair and quarantine of corrupt copies, and duplicates (§16).
- **Placement.** Failure domains and tags as hard constraints, and `atoll balance` and `capacity`. The capacity formula and the strict domains are done in the solver.
- **Monitoring and notifications.** SMART, Gotify and SMTP alerts, and notifications to Plex, Emby and Jellyfin.
- **Conflicts.** `atoll conflicts resolve`.
- **Link guard.** The optional link guard against deletions through mergerfs (§17).

## v0.1: phase 2a, Atoll's own view

- **Atoll's FUSE view, through cgofuse.**
  - It presents the original of each file, never a stale, corrupt or unavailable copy.
  - It serves every entry from the catalog, so that browsing wakes no disk.
  - It sends deletions to the trash.
  - It replaces mergerfs at the same mount point.
- **The contracts frozen at format version 1**: catalog schema, on-disk layout, configuration keys, local API, `--json` schemas and exit codes. This is what the Rust rewrite (phase 3) must keep.
