# Roadmap

Where Atoll stands and what remains before each milestone. The scope of each phase is fixed by [specs.md](specs.md) §28, which this file follows. [TODO.md](TODO.md) details each open item, and [DONE.md](DONE.md) the finished work.

## Milestones

A milestone is reached when its phase and every phase before it are done.

| Milestone | Phase | What it brings | Progress |
|---|---|---|---|
| **alpha** | 1a — Safe core | a pool used through mergerfs, whose copies Atoll makes, verifies and keeps in step, safe to stop or uninstall | about 92 % |
| **beta** | 1b — Complete Linux MVP | every replication mode, repair, SMART, notifications, `balance` and `capacity` | about 48 % |
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

1. **The memory of a daemon that runs for days**, measured.

## Beta: phase 1b, the complete Linux MVP

- **Modes.** The `immediate` and `scheduled` replication modes; the watcher brings what `immediate` needs. `scheduled` is done: `immediate` remains.
- **Repair and duplicates.** Done: the repair of corrupt copies, by chunk or whole (§16.1), and the report of duplicates, `atoll dupes` (§16).
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
