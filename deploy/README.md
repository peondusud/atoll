# Deployment files

| Path | Content |
|---|---|
| `atoll.toml` | A sample configuration: the example of `specs.md` §24.1, byte for byte. A test compares the two, and loads it. |
| `atoll.service` | The systemd unit of the daemon that runs for good (`specs.md` §15.13): `Type=notify`, as the dedicated user `atoll`. |
| `atoll-mount.service` | The systemd unit of `atoll mount` (`specs.md` §21.2): Atoll's own view, at `view.mount` where it writes with `view.engine = "atoll"`, and read only at `view.readonly_mount`, as the same user. |
| `deb/postinst`, `deb/postrm` | The scripts of the `.deb` a release holds (`specs.md` §26, `.goreleaser.yaml`): the account, and nothing else. |

## The .deb

A release's `.deb` (README.md, "Installing a release") installs the binary at `/usr/bin/atoll`, and these two units in `/usr/lib/systemd/system/`, with that path in `ExecStart` in place of `/usr/local/bin/atoll`. Its `postinst` makes the group and the account `atoll` when they are not there, as `useradd --system` does with no home and no shell, and has systemd read its units again. It enables and starts no unit, and writes no configuration: `atoll init` writes one, and refuses to where one is, so the sample is `/usr/share/doc/atoll/examples/atoll.toml`. An upgrade restarts nothing: `systemctl restart atoll atoll-mount` takes the new binary. Removed or purged, the package leaves the account, which may own each disk's work folder, `/etc/atoll`, `/var/lib/atoll` and a unit that runs as they are. It recommends mergerfs and smartmontools, and suggests fuse3, for `fusermount3` where `mount(2)` is not allowed, and ntfs-3g.

## The account

The maintainer chose a dedicated user over root. The units give it no supplementary group, and these capabilities, and no others:

| Capability | What needs it |
|---|---|
| `CAP_SYS_ADMIN` | fanotify's filesystem marks (§15.1), `umount` and `mount` after a USB reset, mergerfs's runtime branches, and the view's `mount(2)` |
| `CAP_DAC_READ_SEARCH`, `CAP_DAC_OVERRIDE` | reading every file of the library, and writing copies and trash where the library's owners allow nothing; reading a device's head when a disk is confirmed (`specs.md` §10) |
| `CAP_CHOWN`, `CAP_FOWNER` | giving a copy its source's owner, mode and times |
| `CAP_FSETID` | keeping setgid on a copy, or on a folder made for one or through the view, whose group is not `atoll`'s: without it the kernel takes the bit off without a word. With the units of before 2026-11-19, a shared folder made 2775, as openmediavault makes them, was copied 0775, and made 0775 through the view (measured in qemu) |
| `CAP_SYS_RAWIO` | the daemon's alone: SMART through ATA pass-through (§23), and `CHECK POWER MODE` (§10) |

Until 2026-11-19 they also gave it the group `disk`, to read a device's head. It gave nothing more: `CAP_DAC_READ_SEARCH` reads every device node of the machine whatever its mode, as it reads every file, and `CAP_DAC_OVERRIDE` writes it, so it is through these two that the account reaches the pool's devices, a whole disk behind a USB bridge as much as a partition. A udev rule that gave `atoll` the pool's devices alone would likewise change nothing while it holds them; and a device policy of systemd (`DevicePolicy`, `DeviceAllow`) is left out of the units (below).

Measured in a Debian 13 container, as such a user with those capabilities and nothing else:
- it reads and writes mergerfs's runtime branches through `.mergerfs`;
- it runs `mount` and `umount` through util-linux.

The remount after a USB reset (`specs.md` §15.13), measured in qemu on Linux 6.12 and 7.3 as that user, with `NoNewPrivileges`:
- ext4 mounts again from its `/etc/fstab` entry;
- The kernel's type `ntfs` (Linux 7.1 and later, Debian 14's only kernel NTFS driver): mount it with `mount -i -t ntfs -o iocharset=utf8,errors=remount-ro,windows_names,umask=022`. Without `errors=remount-ro` the disk is `readonly` (`errors-continue`); without `windows_names`, a name holding `" * < > ?` that a program makes there sets the whole volume read-only once its file is closed (specs.md §22).
- ntfs-3g mounts again only on a mount point that `atoll` may write to: `mount.ntfs-3g` refuses one that root alone owns ("user has no write access to mountpoint"). Give each ntfs-3g disk's mount point to `atoll` while the disk is unmounted (`chown atoll /mnt/disk1`), or the disk stays `missing` after a reset until it is mounted by hand. The volume then shows every file as `atoll`'s, with `allow_other`, so that other accounts, a media server's included, still read it; Atoll takes the owner and the mode an ntfs-3g mount gives for the mount's, never for a change to carry to the other disks. The FUSE process `mount.ntfs-3g` starts is then the daemon's service's: `systemctl stop atoll`, or a restart, ends it, and the disk is unmounted (measured in qemu, `test/loopdev/units_test.go`); `mount /mnt/disk1` mounts it again.

The catalog's folder (`/var/lib/atoll`) belongs to `atoll`, with mode 0750: `StateDirectory` sees to it. The configuration's (`/etc/atoll`), which `ConfigurationDirectory` makes, stays root's, as systemd.exec(5) has it. Commands run by hand as root work on the same catalog; they wait while a pass holds it (§25).

Each disk's work folder, `Atoll/` at its top, must belong to root or to `atoll` (`specs.md` §10): `atoll disk add` refuses one that another account owns, on ext4, XFS and btrfs, and says the `chown` that gives it back, which a disk needs when it comes from a machine where `atoll` has another number. Keep the top of each disk closed to the accounts that must not read every file of the library: whoever owns it can make that folder again.

Prometheus reads the daemon's metrics on `/metrics` (`specs.md` §23.1): over TCP on `api.listen`, with the API's token, or on `metrics.listen`, which asks none and gives nothing else. A scrape job for each:

```yaml
scrape_configs:
  - job_name: atoll                       # api.listen = "127.0.0.1:7341"
    authorization: { credentials_file: /etc/prometheus/atoll.token }
    static_configs: [{ targets: ["127.0.0.1:7341"] }]
  - job_name: atoll-open                  # or metrics.listen = "127.0.0.1:9341", with no token
    static_configs: [{ targets: ["127.0.0.1:9341"] }]
```

The log goes to the journal, one record a line, in OpenTelemetry's log data model as JSON (`specs.md` §23.2): `journalctl -u atoll -o cat | jq` reads it, and `log_format = "text"` in `[telemetry]` brings back slog's text for a terminal. Vector reads both signals without OTLP, the log from the journal and the metrics from `metrics.listen`:

```toml
# vector.toml
[sources.atoll_log]
type          = "journald"
include_units = ["atoll.service", "atoll-mount.service"]

[transforms.atoll_records]
type   = "remap"
inputs = ["atoll_log"]
source = '. = merge(., object!(parse_json!(.message)))'

[sources.atoll_metrics]
type      = "prometheus_scrape"
endpoints = ["http://127.0.0.1:9341/metrics"]
```

Or over OTLP: with `otlp_endpoint`, `otlp_metrics` and `otlp_logs` in `[telemetry]`, the daemon sends both to a collector, Vector's `opentelemetry` source (`[sources.otlp] type = "opentelemetry"`, `http.address = "127.0.0.1:4318"`), Elastic Agent or the OpenTelemetry Collector, the metrics under their OpenTelemetry names, which a collector's Prometheus exporter writes back as `/metrics` names them. A token goes in `otlp_headers_file`, as `Authorization: ApiKey …` for Elastic, readable by `atoll` alone (`chmod 600`), over `https://` only.

The catalog's folder is closed to other accounts (`StateDirectoryMode=0750`). The catalog and its backups list every path of the library, its trash and its history, which the local API gives only through its socket, of mode 0660, or with its token (§25.1): a folder anyone could read would give them to every account of the machine. A member of the group `atoll` reads them, and may ask the socket.

## The hardening

Each unit sets what systemd offers that gives the service no mount namespace of its own (docs/threat-model.md, G18):

| Setting | What it does |
|---|---|
| `NoNewPrivileges=yes` | a setuid program, as `fusermount` or `mount`, gains nothing; the ambient capabilities still reach what the service runs |
| `SystemCallArchitectures=native` | calls of the machine's own architecture alone |
| `SystemCallFilter=@system-service @mount`, and `fanotify_init fanotify_mark open_by_handle_at` for the daemon | systemd's set for system services; `@mount` for `mount(8)`, `umount(8)` and `mount.ntfs-3g`, which the remount runs, for `open_tree`, through which the daemon reads whether a mount point is immutable, and for the view's own `mount(2)`; fanotify's calls, and the one that opens what a mark reports, for the watch (§15.1). No `bpf`, `ptrace`, `perf_event_open`, module, swap, reboot, `kexec`, raw port I/O, clock or `quotactl` |
| `SystemCallErrorNumber=EPERM` | any other call fails with `EPERM`, as systemd's own units have it, rather than ending the daemon in the middle of an operation |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` | the API's socket and `sd_notify`; the alerts, the media servers, NUT and OTLP; netlink, through which Go reads the machine's addresses |
| `RestrictNamespaces=yes` | no namespace made |
| `LockPersonality=yes`, `MemoryDenyWriteExecute=yes`, `RestrictRealtime=yes` | no other personality, no memory both writable and executable, which Go never needs, no realtime scheduling |
| `ProtectClock=yes` | the clock read only: a filter of calls, and no device policy |
| `ProtectHostname=yes` | the host's name changed for the service alone: a namespace of that name, and none of mounts |
| `UMask=0027` | what Atoll makes with no mode to copy, the folders of its trash, its catalog and its backups, is closed to other accounts. Copies, and the folders made for them or through the view, take the mode they copy whatever the mask; a folder of the library that no disk holds any more, made for a file moved there, is 0750 (§15.12) |

Each was measured in qemu on Linux 6.12 and 7.3: `test/loopdev/units_test.go` starts both units as they are, through systemd 257 as Debian 13 has it, as the account `atoll`, a call out of the filter ending the process rather than failing with `EPERM`, so that none goes by unseen; and checks that each runs in the host's mount namespace, with no device policy (test/qemu/README.md). The daemon gets ready; copies what was there and what is written after it starts, through the filesystem mark, a folder 2775 with its mode; answers on its socket; reads its configuration again on `systemctl reload`, through the unit's own `kill`; sets mergerfs's branches; remounts ext4, on a partition of a SCSI disk through device-mapper, and ntfs-3g, through `/etc/fstab` once their device comes back under another name; confirms each disk by reading its device; asks the whole disks below them whether they spin, through `SG_IO`; runs `smartctl`; and stops with exit 0. The view mounts both its points for every process of the host, reads, writes on a user's behalf, a folder 2775 too, and unmounts on stop. `ProtectClock` and `ProtectHostname` also give no mount namespace and no device policy with systemd 252, Debian 12's and openmediavault 7's.

Left out, as each breaks what Atoll does:
- `PrivateTmp`, `ProtectHome`, `ProtectSystem`, `PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectProc`, `ReadWritePaths` and their kin: each gives the service a mount namespace of its own. In one, what the daemon mounts stays its own, whatever `MountFlags` says (systemd.exec(5)), so the remount after a USB reset mounted the disk again for Atoll alone, while the host, mergerfs and the media servers kept the dead mount; and the view would be mounted for its service alone. The daemon's unit had `PrivateTmp` and `ProtectHome` until 2026-11-16; a test forbids every such setting in `deploy/`. What `ProtectKernelModules` would also do, the units have: no `CAP_SYS_MODULE`, and no module call in the filter.
- `RestrictSUIDSGID`: a copy, a folder made for it, a mode carried to the other copies and what a user makes through the view take setuid and setgid with the rest of their mode (`specs.md` §14, §15.12, §21.2), as a shared folder made 2775 does. Measured with it in both units: the daemon copied nothing of such a folder (`make folder "Shared": operation not permitted`), and `chmod 2775` through the view was refused.
- `DevicePolicy`, `DeviceAllow`: the remount opens the disk's block device for writing, under the number it comes back with after a USB reset, and systemd reads the device groups such a list names once, when the unit starts, so a list would refuse a disk whose driver loads after that; the capabilities reach every device all the same.

`systemd-analyze security` gives `atoll.service` 4.4, "OK", where it gave 7.3, "MEDIUM", before, and `atoll-mount.service` 4.2 where it gave 7.1, with systemd 257; 4.6 and 4.4 with systemd 252. What is left is the capabilities, which the work needs, the settings that make a mount namespace, and a network open to any address, which the alerts and the media servers need.

## openmediavault

On openmediavault, make the pool in the mergerfs plugin's form (Storage, mergerfs), not in `/etc/fstab`: `atoll mergerfs-config --omv` prints each field, as the form names it (`specs.md` §21.1). The plugin mounts the pool at `/srv/mergerfs/<name>`, so set `view.mount` to that path first; the command warns when it is not. The plugin keeps one minimum free space for the pool: by default the largest of the disks' own, or with `view.omv_min_free = "same"` none until every disk has the same. With `--json` too, its `omv.rpc` is what `omv-rpc -u admin MergerFS set` takes, with a `uuid` added: `fa4b1c66-ef79-11e5-87a0-0002b3a176b4` for a new pool. The plugin needs mergerfs 2.42 or later, which omv-extras installs with it.

## Atoll's own view in place of mergerfs

With `view.engine = "atoll"`, `atoll mount` serves the view that writes at `view.mount`, and the daemon runs no mergerfs (`specs.md` §21.2): unmount mergerfs there, and take its line out of `/etc/fstab`, before the unit starts. Each write reaches one disk: what is new the most free one, a file written its original; the daemon's passes take it to the other copies. A file deleted goes to the trash, `atoll trash restore` brings it back. Keep `atoll mergerfs-config`'s line at hand: plain mergerfs at `view.mount` still gives the same view once `atoll mount` is stopped.

## The read-only view

`atoll mount` serves Atoll's own view, read only, at `view.readonly_mount`, while mergerfs stays the view that writes, at `view.mount` (`specs.md` §21.2). Browsing it reads the catalog alone, and wakes no disk; each file reads from its original. It runs as a service of its own, `atoll-mount.service`, so that the view stays while the daemon stops or starts again.

- Set `view.readonly_mount`, such as `/srv/pool-ro`, and make that folder: Atoll makes none outside its disks.
- The unit gives `atoll` `CAP_SYS_ADMIN`, for `mount(2)`, and `CAP_DAC_READ_SEARCH`, to open each copy whatever its mode; for the view that writes, `CAP_DAC_OVERRIDE`, `CAP_CHOWN`, `CAP_FOWNER` and `CAP_FSETID`, as the daemon has, to write on a user's behalf where the library's owners allow `atoll` nothing, give what it makes that user's owner, and keep the setgid bit a user gives. `/dev/fuse` must be open to it: udev gives it mode 0666 on every system that has udev, Debian and openmediavault included; `atoll mount` says so when it is not. It has no setting that gives it a mount namespace of its own, or no other process would see the view; the test that holds `atoll.service` to that holds this unit too.
- Point the media server's libraries at the view, and tell its scans of folders as it sees them: `path_map = { "/srv/pool" = "/srv/pool-ro" }` in its table of `[notify]` (`specs.md` §21.5). Let it start after the view, with a drop-in of its own unit (`systemctl edit plexmediaserver`):

  ```ini
  [Unit]
  Wants=atoll-mount.service
  After=atoll-mount.service
  ```

- What the view presents follows the disks' states the catalog holds, which the daemon keeps. A disk whose mount check fails, which `atoll mount` runs every 30 s, is read from no more: a read of a copy there fails with `EIO` until the disk is back.
- A stop unmounts the view. A view still read from is detached, as `umount -l` does, and what is open there fails. A view whose process stopped short is detached at the next start; anything else mounted at the point is refused.

## A UPS through NUT

With `[ups] enabled = true`, the daemon reads the UPS that `ups.nut` names from NUT's `upsd` every 5 s, as upsmon does, over NUT's own protocol (`specs.md` §20): on battery, it starts no copy; with the battery low, it stops its work, writes out every disk, and waits for line power, which upsmon's shutdown ends with `SIGTERM` if it comes first.

- NUT's driver and `upsd` must run, on this machine or another: `upsc ups@localhost ups.status` says `OL` on line power. `upsd` listens on `127.0.0.1:3493` by default; for another machine, its `LISTEN` in `upsd.conf`, and `ups.nut = "ups@<host>"`. Reading takes no account in `upsd.users`.
- The unit starts after `nut-server.service` when NUT runs here. Atoll shuts nothing down: upsmon keeps that, as ever.
- A UPS that `upsd` says nothing of for 15 s is an alert, `ups-unreachable`, and the daemon goes on as on line power; one on battery that goes unheard counts as low, as upsmon counts it critical.
