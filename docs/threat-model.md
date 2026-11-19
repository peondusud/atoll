# Threat model

What could go wrong with Atoll's security, who could make it go wrong, what Atoll already does about it, and what is left. It covers the Go version on Linux, phases 1 to 2b (`specs.md` §28), as of 2026-11-19. It is a working document, not a contract: `specs.md` stays the source of truth, and the gaps at the end are proposals for the maintainer, none of them made. It was written from the code, the units in `deploy/`, the workflows and `specs.md`, read on that date; what could not be checked says so.

## What Atoll is, for this model

- A daemon that runs for good (`atoll daemon`, §15.13) and a CLI, in one binary, on a home server (ZimaBoard, M920q class) that one person administers as root. It watches the pool disks, copies files between them, sends copies to each disk's trash, and answers a local API.
- A second process, `atoll mount` (§21.2), that serves Atoll's own FUSE view of the library: read only for the media servers, and with `view.engine = "atoll"` the view that writes, in place of mergerfs.
- Both run as the account `atoll`, with capabilities (§26, `deploy/atoll.service`, `deploy/atoll-mount.service`). Root administers: `/etc/atoll` and the units are root's, and the CLI is run by root or by the account `atoll`.
- Other accounts of the machine are expected: the media servers' (`plex`, `jellyfin`), members of the household writing through a Samba or NFS share or the view, containers with a bind mount of the library.

## Assets

| Asset | What matters | Where |
|---|---|---|
| The user's media and its copies | integrity and availability: never the last good version lost (invariants 1, 2, 10) | `<disk>/media/` on each disk |
| The trash | availability: it is the way back from a deletion or an overwrite for `trash.retention` (§17) | `<disk>/Atoll/trash/` |
| The catalog and its backups | integrity, as it decides what is in sync and what is deleted; confidentiality, as it lists every path of the library, its history and its `user.` attributes | `/var/lib/atoll/` (§9); `<disk>/Atoll/catalog/` with `maintenance.catalog_on_disks` |
| The secrets | confidentiality | `api.token_file`; `notify.plex.token_file`; `notify.jellyfin.api_key_file` and `notify.emby.api_key_file`; `alerts.ntfy` (the topic in the URL is the secret) and `alerts.ntfy_token_file`; `alerts.gotify_token_file`; `alerts.smtp_password_file`; `telemetry.otlp_headers_file` |
| The daemon's privileges | not lent to anyone: the account `atoll` with `CAP_SYS_ADMIN`, `CAP_DAC_OVERRIDE`, `CAP_DAC_READ_SEARCH`, `CAP_CHOWN`, `CAP_FOWNER`, `CAP_SYS_RAWIO` and the group `disk` is close to root | the units |
| The guards' answers | integrity: an answer lets held deletions or modifications go (§15.6) | the catalog's events |
| The releases | integrity: what users install is what the repository holds | GitHub releases, `.tar.gz`, `.deb` (§26) |

## Adversaries

| | Who | Can |
|---|---|---|
| A1 | A local account with no privilege: a media server's, a household member's, a container's | use what the permissions of the machine give it: the view, a share, the files it owns on a disk |
| A2 | A member of the group `atoll` | open the API's socket (mode 0660) |
| A3 | Whoever can write a pool disk: its owner account, a share's users, someone who plugs in a USB disk written elsewhere | choose every name, depth, link, attribute and file below `media/`, and anything in `Atoll/` when it owns that folder |
| A4 | A device on the tailnet, a process on the host, a web page in a browser on the host | reach `api.listen` or `metrics.listen` |
| A5 | A media server, an alert service, an OTLP collector, a UPS server, or the network between | answer what Atoll asks, or read what it sends |
| A6 | The supply chain: a dependency, a GitHub Action, the build | change the binary or the package before a user installs it |

Out of scope: root on the host, who can do everything Atoll can; a physical attacker with the machine itself; bugs of the kernel, of the filesystem drivers and of FUSE, beyond what Atoll checks (§10, §22); ransomware as such, which §3 leaves to backups ("the trash recovers from mistakes, not from ransomware").

## Trust boundaries and entry points

```
            tailnet / loopback                               the host
  A4 ──► api.listen (token) ─┐                  ┌── A2 ──► /run/atoll/atoll.sock (0660)
  A4 ──► metrics.listen ─────┤                  │
                             ▼                  ▼
                     ┌───────────────────────────────┐     root ──► /etc/atoll/*.toml, secret files
                     │ atoll daemon (account atoll,  │◄────────────── configuration, reload
                     │ CAP_SYS_ADMIN, DAC_OVERRIDE…) │──► smartctl, mount, umount (PATH)
                     └──────┬───────────────┬────────┘──► media servers, ntfy, Gotify, SMTP, OTLP ──► A5
                 openat, no │               │ catalog (SQLite)                 ▲ answers
                 link taken ▼               ▼                                  │
  A3 ──► <disk>/media/, <disk>/Atoll/      /var/lib/atoll (0750)          upsd (NUT, cleartext) ◄── A5
                     ▲
  A1 ──► Atoll's FUSE view (allow_other, default_permissions) ──► atoll mount (account atoll, DAC_OVERRIDE…)
  A6 ──► GitHub Actions ──► GoReleaser ──► .tar.gz, .deb (postinst as root) ──► root installs
```

Each section below takes one entry point: its threats by STRIDE (spoofing, tampering, repudiation, information disclosure, denial of service, elevation of privilege), what Atoll does about each, with the section of `specs.md`, the invariant or the test that holds it, and what is left. The gaps are numbered, and listed with their mitigations and priorities at the end.

### 1. The local API over the Unix socket

`api.socket`, `/run/atoll/atoll.sock` by default (§25.1). It answers the questions of `status`, `disk list` and `show`, `trash list`, `guard list` and `/metrics`, and takes one write: an answer to a guard.

- **Spoofing, elevation.** Whoever can open the socket is trusted, with no token: the socket is made mode 0660, owner and group `atoll`, in a folder systemd makes for the service (`internal/fsx/socket.go`, `ListenSocket`: the umask set to 0177 around `listen`, then `chmod` 0660). An earlier socket at the path is replaced; anything else there, a symbolic link included, is left and is an error. So A1 cannot connect; A2 can, and can answer a guard, which lets held deletions go to the trash. Tests: `TestTheAPISocket`, `TestTheAPIAnswersAGuardAsTheCommand`.
- **Repudiation.** The daemon's log records each answer, with the request's remote address (`cmd/atoll/apiguard.go`); over the socket that address is empty, and nothing says which account answered (gap G7).
- **Tampering.** The answer's body is one JSON object of 4 KiB at most, every field named once and in its case, `yes` required, and `up_to` naming the guard answered, so that an answer given on what was shown never lets go of what tripped since (§25.1, §15.6; `TestTheAPIRefusesAnAnswerItCannotTake`, `TestTheAPIAnswersTheGuardItWasShown`). It goes through the same code, lock and events as `atoll guard confirm`.
- **Denial of service.** Header, whole-request (15 s) and answer timeouts, header size, one catalog answer at a time, one mount check at a time for `/v1/disks` and `/v1/disks/{id}`, and 32 connections at once on each listener, past which a connection waits, unaccepted, until one closes (gap G6, done; `TestTheAPIServerLimits`, `TestASlowClientTimesOut`, `TestTheConnectionsAreCapped`, `TestTheAPIWaitsPastItsConnections`, `TestTheAPIReadsTheCatalogOneAnswerAtATime`, `TestTheAPIChecksTheMountsOneAnswerAtATime`). A client that holds the 32 places of `api.listen` keeps other TCP clients waiting, never the socket's.
- **Information disclosure.** The answers are what the CLI's `--json` gives: paths, states, the trash. Only the socket's group reads them.

### 2. The local API over TCP, and `metrics.listen`

`api.listen` and `metrics.listen` (§25.1, §23.1): a literal address, loopback or the Tailscale network (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`), refused otherwise when the configuration loads (`internal/value/listen.go`, `conformance/vectors/listen.json`, now `FuzzListen` against `net/netip`).

- **Spoofing.** Every request on `api.listen` carries `Authorization: Bearer`, compared in constant time with what `api.token_file` holds, read at each request (`cmd/atoll/api.go`, `withToken`); an unreadable or empty file refuses everything; `api.listen` without a token file does not load (`TestTheAPIOverTCPAsksForItsToken`). The token has no minimum length, and wrong tokens cost nothing but a round trip (gap G9). `100.64.0.0/10` is also the carrier-grade NAT range: the address is taken as Tailscale's from its range alone (gap G12).
- **Elevation.** An answer to a guard over TCP needs a token file that only root and the daemon's account can change and other accounts cannot read, else 403 (gap G1, done): the file it read belongs to root or to the account the daemon runs as, with no write for its group or for other accounts and no read for other accounts; each folder that holds its name, or the name of a symbolic link on the way, belongs to root or that account, with no write for other accounts, sticky or not, and none for a group other than root's or the daemon's (`fsx.SecretFolders`, `tokenDoubts`). The 403 says each reason (`TestTheAPIAnswerTakesATokenOnlyItsAccountsCanSet`, `TestTheAPIAnswerLooksAtTheFoldersOfALink`, `TestTokenDoubts`). A group that is not Atoll's may still read the file, as Prometheus's may to scrape: its members can answer.
- **Tampering, CSRF.** A browser cannot send the answer's `application/json` body to another site unasked (415 otherwise), and it has no token. `metrics.listen` asks for no token and checks no `Host`: a page in a browser on the host could read it through DNS rebinding (gap G10). It writes nothing.
- **Information disclosure.** The metrics give the version, disk ids, filesystems, mount paths, states, capacities, SMART and temperature, alerts by name, the UPS; no file name (`api/metrics.json`). The log never holds the token; it holds the token file's path when that cannot be read, once a minute at most, with the requests refused since (gap G6, done; `TestAnUnreadableTokenIsLoggedOnceAMinute`).

### 3. Atoll's own FUSE view

`atoll mount` (§21.2): mounted `allow_other` and `default_permissions` by `mount(2)` (`internal/view/fusefs/fusefs.go`), `nosuid` and `nodev` by go-fuse's default for a direct mount (checked in its `mount_linux.go`), read only for the media servers, and writing with `view.engine = "atoll"`.

- **Elevation, tampering.** The kernel checks each caller against the mode, owner and group the view presents, which come from the catalog's rows (`default_permissions`); the process then writes with `CAP_DAC_OVERRIDE`, `CAP_CHOWN` and `CAP_FOWNER`, and gives what it makes to the caller's uid and gid. So what the kernel allows is exactly what the view decides. Three ways that can differ from the disk:
  - a `chmod` or `chown` made directly on a disk is presented once a pass records it: until then the view grants the old access, with the process's own override behind it (gap G2);
  - a folder presents the first disk's entry, and a change of a folder's mode is applied to every disk: a stricter folder on another disk is not what the kernel checks (gap G11);
  - POSIX ACLs are neither presented nor checked, and replicas do not carry them (§14; gap G14).
  No test writes through the view as an account other than the owner (`TestReadonlyMountAsTheUnit` checks that the kernel refuses another account a read; gap G3).
- **Tampering by name.** The kernel gives one component at a time; `names.CheckPath` refuses empty, `.`, `..`, a slash and NUL, and the names a target filesystem refuses (§15.8; `FuzzCheckPath`). A symbolic link is stored as given and never followed by Atoll; no FIFO or device can be made (no `Mknod`); `RENAME_EXCHANGE` is refused.
- **Extended attributes.** Only the `user.` namespace is set, kept and copied (`solver.KeptXattr`); `security.`, `trusted.` and `system.` are refused with `ENOTSUP` (`TestViewXattrs`). So no file capability or SELinux label travels through the view or into a replica.
- **Elevation, a window.** A file the view makes is created with the mode asked, setuid and setgid included, owned by `atoll`, then given to the caller and its mode set again (`ops.(*Disk).CreateUserFile`); the copy pipeline creates its `.part` private first (`TestAPartIsPrivateAndTakesItsOwnerFirst`). For that moment an empty setuid file of `atoll` exists on the disk (gap G13).
- **Repudiation.** `view_ops` journals what the view did, not who asked it (gap G15).
- **Denial of service.** The view serves from the catalog in memory; a client can hold files open, as on any filesystem.

### 4. What the disks hold

Anyone who can write a disk controls its tree (A3): names, depth, symbolic and hard links, special files, attributes, times, and, if the account owns `Atoll/`, the work folder.

- **Spoofing a disk.** A disk is its filesystem's UUID or NTFS serial, read from the superblock (§10, invariant 8; `conformance/vectors/superblock.json`); two with one identifier, or two pool filesystems on one physical disk, are refused. `Atoll/disk.toml` is read strictly: exactly four keys, format 1, each value of its grammar, 64 KiB at most (`layout.ParseMarker`; `TestMarker`, now `FuzzMarker`); one that names another pool, disk or filesystem makes the disk `readonly`, `other-marker`. But the marker is optional, and a disk with no marker passes: a disk made with the UUID of a pool disk and mounted in its place is taken as that disk (gap G8). Its content then reads as modifications and deletions, which the guards hold past their thresholds (§15.6).
- **Tampering through links.** Every step below a disk's root is an `openat` with `O_NOFOLLOW`; a file opened is a regular file, opened `O_NONBLOCK` (`internal/fsx/dir.go`; `TestDirNeverFollowsASymlink`, `TestOpenFileRefusesAFifoWithoutWaiting`, `TestANamedLookFollowsNoLink`); a symbolic link, a FIFO, a socket or a device is an entry that is never copied (§15.10). A hard link is never taken for a rename (§27). A hard link to a file of another account on the same filesystem is copied with its owner and mode, so it discloses nothing the disk did not; Linux's `fs.protected_hardlinks` keeps such a link from being made by someone who cannot read the file.
- **Tampering with content.** A deletion or modification made on a disk propagates to the other copies only to the trash (invariants 1, 10), past the mass-deletion and mass-modification guards (§15.6). A change below the guards' thresholds, spread over windows, goes through, and the trash, once past `trash.max_size`, purges overwritten versions before their retention ends (§17): a tool that encrypts the library slowly enough can end with no clean version. §3 makes backups the answer to ransomware; the alert `versions-purged:<id>` now says it is happening, at priority 4, naming the files that lost a version (gap G5, done; `TestTheDaemonTellsOfVersionsPurgedEarly`).
- **Tampering with Atoll's own folder.** `disk add` refuses a work folder another account owns, and each pass raises `workdir-owner` for one (`TestDiskAddRefusesAWorkFolderOfAnotherAccount`, `TestForeignWorkdir`); `links/` is 0700 and `tmp/` closed; a file planted in `trash/` is seen only by `catalog rebuild`, with the reason `unknown`, and restored only by a command (§17.4); `links/` names Atoll did not make are never removed.
- **Elevation through copies.** A replica takes its source's owner, then its mode, setuid and setgid included, which are dropped when the owner cannot be set (`TestSetgidIsPartOfTheMode`); only `user.` attributes travel. Atoll never copies what the disk did not already hold with that owner.
- **Information disclosure, terminals.** A name with a control character or that is not UTF-8 is shown in base64 on a terminal and in an alert (`TestANameWritesNoLineOfItsOwn`, now `FuzzEncodePath`). Characters that reorder or hide text, U+202E and the like, are shown as they are (gap G16).
- **Denial of service.** A walk goes no deeper than 2,048 folders and some 4 MiB of paths by default (§15.8, §26); names past a filesystem's limits are `name-incompatible`; the mount table is read line by line, a line it cannot read counted and never taken for an absence (`FuzzParseMountInfo`). An empty mount point is a missing disk, never a mass deletion (invariant 4, `TestAFailedMountIsAMissingDisk`).
- **Attribution.** The watcher takes an event for the view's or Atoll's own by the process: `comm` "mergerfs" with the view's mount in its arguments, or Atoll's own binary (`internal/watch/watcher.go`). Any account can name a process so: its change is then found by the next walk rather than at once (invariant 11 holds; gap G17).

### 5. The configuration and the secret files

`/etc/atoll/atoll.toml` and the files it names (§24). Root writes them.

- **Tampering.** The configuration is read as a tree and checked against its schema; unknown keys are errors; values have their grammars (§24.3, §29.5; `TestConfigVectors`, now `FuzzParse` and `FuzzEdit`). Nothing in it is executed: no hook, no command. Absolute paths hold no `.` or `..`. `atoll init` writes it 0640 whatever the umask, and the edits of `disk add`, `remove`, `fail` and `replace` keep its mode and owner. The daemon never checks the mode or owner of the configuration itself; whoever can write it controls the daemon, and through it the disks.
- **Information disclosure.** Each secret but the ntfy topic sits in a file of its own, read when it is used (§24). An error about a URL never quotes it (`TestAnErrorShowsNoURL`); the OTLP headers file is never quoted (`TestReadHeaders`). The API's token file is checked at each answer to a guard (G1, done); every secret file gets a §24.4 warning, from `atoll rules check` and in the daemon's log at its start and each reload, when other accounts may read or write it or it belongs to an account none of Atoll's, and each is read at most 4 KiB at a time (16 KiB for the OTLP headers), a regular file only, a FIFO refused without waiting (gap G4, done). A warning does not refuse: version 1 takes the configuration. The daemon reads them with `CAP_DAC_OVERRIDE`, so a path set there reads whatever file it names: `telemetry.otlp_headers_file = "/etc/shadow"` would send its lines as headers. Only root writes the configuration, so this lends root's reach to root.
- **Information disclosure on the wire.** The tokens of Plex, Jellyfin, Emby, ntfy and Gotify may go to an `http://` URL on any host, in clear, as may ntfy's topic and a user written in an address: each gets a §24.4 warning when its host is not this machine as written, `localhost`, `127.0.0.0/8` or `[::1]` (gap G4, done; `conformance/vectors/config.json`); the OTLP headers file refuses `http://` (`internal/config/relations.go`) and the SMTP password never goes in clear. A host of the Tailscale network gets the warning too, though WireGuard carries it.

### 6. What goes out

Notifications to the media servers (§21.5), alerts to ntfy, Gotify and SMTP (§23), the metrics and the log over OTLP (§23.2).

- **Information disclosure.** Each secret goes in a header, never in a URL: `X-Plex-Token`, `Authorization: MediaBrowser Token=…`, `X-Emby-Token`, `Authorization: Bearer` for ntfy, `X-Gotify-Key`; a key holding a quote, CR or LF is refused. What goes out holds paths: a folder to Plex in `?path=`, to Jellyfin and Emby in the body, in alerts in base64 when needed, in the OTLP log. That is their purpose, to services the configuration names.
- **Spoofing, tampering.** TLS is verified, with no option to skip it; SMTP takes TLS 1.2 at least; no redirect is followed by notify, alerts or telemetry (`TestMediaServersDoNotFollowARedirect`, `TestAlertsDoNotFollowARedirect`, `TestARedirectIsNotFollowed`). Without a user, SMTP's STARTTLS is taken when offered and can be stripped by the network, unless `alerts.smtp_require_tls = true`, which sends nothing without TLS (gap G4, done; `TestSMTPRequiresTLSWhenAsked`). It is false by default, so as to change nothing for a configuration of version 1.
- **Denial of service.** Each request has 10 s; a collector that answers 500 or never holds nothing up (§27).

### 7. What comes in

- The media servers' answers: Plex's JSON is decoded into a structure and its paths only compared; another answer gives at most 512 bytes of text to the log and to the CLI, through `shownText`. Bodies are read with a limit: Plex 4 MiB, Jellyfin and Emby 512 bytes, the alert services 512 bytes, SMTP 256 KiB a conversation.
- NUT (§20): `upsd` speaks in clear and takes no login. An answer forged on the network can park the daemon, as on a low battery, or hide that the machine runs on one. `ParseNUT` takes a host of two colons or more whole, a bracket in it too, which no resolver takes (found by `FuzzNUT`; harmless, as upsd is then not reached).
- The OTLP headers file is read with its 16 KiB limit, never whole first, and only when it is a regular file (gap G4, done).

### 8. The daemon's privileges, the units and what they run

`deploy/atoll.service` and `deploy/atoll-mount.service`, as the `.deb` installs them (§26).

- **Elevation.** `User=atoll`, `NoNewPrivileges=yes`, and `CAP_SYS_ADMIN` (fanotify, the remount, mergerfs's runtime branches, the view's `mount(2)`), `CAP_DAC_READ_SEARCH`, `CAP_DAC_OVERRIDE`, `CAP_CHOWN`, `CAP_FOWNER`, `CAP_SYS_RAWIO` (SMART, `CHECK POWER MODE`), and the group `disk` to read a device's head. The group `disk` writes every block device, the system disk too, and `CAP_SYS_ADMIN` is broad: a flaw that runs code in the daemon, in its parsing of a disk or of HTTP, has the machine (gap G18). No setting may give the service a mount namespace of its own (`TestNoUnitKeepsItsMountsToItself`), as a remount there would stay the daemon's: that rules out `ProtectSystem`, `ProtectHome`, `PrivateTmp` and their kin, but not `SystemCallFilter`, `SystemCallArchitectures`, `RestrictAddressFamilies`, `RestrictNamespaces`, `LockPersonality`, `MemoryDenyWriteExecute`, `RestrictRealtime`, `RestrictSUIDSGID` or `UMask`, none of which the units set.
- **Tampering through PATH.** `smartctl`, `mount` and `umount` are found on the service's `PATH` (`internal/disks/observer_delivered.go`, `internal/daemon/run.go`), as is go-fuse's `fusermount` fallback; their arguments come from the configuration and `/sys`. The default `PATH` of a unit holds root's folders only (gap G19). mergerfs's runtime branches are set through `<view>/.mergerfs` with `getxattr` and `setxattr`, which follow a link, and nothing checks first that the mount there is mergerfs (gap G19).
- **The conformance build** reads folders as mounted disks and its time from a control file (§29.4); the delivered binary has none of that code (`TestDeliveredBinaryIgnoresTheControlFile`), so it can never skip a mount check (invariant 3).
- **The `.deb`'s scripts** run as root: `postinst` makes the group and account `atoll` once and reloads systemd; `postrm` reloads systemd. Neither touches a path a user controls, enables a unit or writes a configuration.

### 9. The catalog, its backups and the trash

- **Information disclosure.** `/var/lib/atoll` is 0750 (`StateDirectoryMode`, `TestTheUnitClosesTheCatalogsFolder`). A `catalog` path elsewhere is made by `fsx.MkdirAll` with 0755, and the database, its WAL and its backups take SQLite's mode under the umask, 0644 with the default: the library's index readable by every account (gap G20). The copy on each disk, `Atoll/catalog/catalog.db`, is written 0600, which an NTFS mount may not honour: there it is as readable as the mount's options make it, and it leaves the machine with the disk. The catalog holds no secret.
- **Tampering.** The catalog is a cache rebuilt from the disks (§9); a forged copy on a disk is used only by `catalog rebuild` from it, which root runs. The trash is purged by its own rules (§17), and a file planted there is never taken for one of Atoll's (§17.4).

### 10. The release supply chain

- **Tampering.** Every action is pinned by commit, with its version in a comment, and Dependabot proposes their updates and the Go modules' each week, to `beta`; `permissions` are `contents: read` but for the release job, which has `contents: write`, `id-token: write` and `attestations: write`; no workflow runs on `pull_request_target`, and every checkout keeps no credentials. `go.sum` pins the modules; `govulncheck` runs on each push. GoReleaser and syft are pinned; the binary is static, `-trimpath`, with the commit's time; `SHA256SUMS`, an SBOM for each archive, and GitHub's build provenance attestation for each archive and package, which `gh attestation verify` checks; the release is a draft the maintainer publishes (§26).
- What is left: the job that builds also attests and publishes, with write permissions throughout, and the release does not wait for the CI of its commit (gap G21); the container images of the CI are pinned by tag, not digest, and Dependabot does not update them; the Go toolchain of a release is the latest patch of its minor version, unpinned; the `.deb` has no SBOM of its own; whether tags `v*` are protected on GitHub is not visible from the repository (not verified).

## Gaps and proposed mitigations

Each is the maintainer's to decide; those decided and done say so in their priority, and what was made of them is under **Done**, below the table. A change to the configuration, the API or the schema is a contract change (§29.3), noted as such. Priorities: **high**, a local account or a disk's writer can lose data or gain the daemon's reach; **medium**, a realistic misuse or a weakness of defence in depth; **low**, hardening.

| Gap | What | Proposed mitigation | Priority |
|---|---|---|---|
| G1 | The API's 403 for a loose token file checks only that others cannot read it; a token file others may write, owned by another account, or in a folder others may write, lets any local account choose the token it sends, and answer a guard over TCP. | Take a token file for an answer only when it is owned by root or `atoll`, its mode has no `g+w`/`o+rw`, and its folder is not writable by others; say why in the 403. A test per case. | medium; **done** 2026-11-19 |
| G2 | The writing view checks callers against the catalog's mode and owner, and writes with `CAP_DAC_OVERRIDE`: a `chmod` or `chown` made on a disk is not enforced until a pass records it. | On open, create, unlink, rename and setattr, `fstatat` the copy on its disk and refuse with `EACCES` when its mode or owner is stricter than what is presented, or present the stricter of the two. | medium |
| G3 | No test writes through the view as an account other than the files' owner: every authorization of the writing view rests on `default_permissions`, untested there. | Tests in `test/loopdev` with `setpriv`, as `TestReadonlyMountAsTheUnit`: a create in a folder the caller cannot write, an unlink in a sticky folder, a `chmod` and a `chown` of another's file, a hard link `fs.protected_hardlinks` refuses, a setuid bit set by a non-owner. | medium |
| G4 | Secret files other than the API's token have no check of mode or owner, and some are read with no size limit; tokens may go to `http://` on any host; SMTP's STARTTLS can be stripped without a user. | A §24.4 warning for a secret file others may read, or not owned by root or `atoll`; read each with `fsx.ReadSecretFile`'s limit; a §24.4 warning, not a refusal (version 1 takes it), for a token sent over `http://` to a host that is not loopback; a key to require TLS for SMTP is a contract change. | medium; **done** 2026-11-19 |
| G5 | A tool that rewrites the library slowly, under the mass-modification guard's threshold in each window, goes through, and once the trash passes `trash.max_size` its overwritten versions are purged before their retention ends. | An alert when the trash purges `overwritten` entries for room before their retention, which says that versions are being lost; and, for the maintainer to weigh, a guard on what a day changed, or never purging `overwritten` for room while modifications run high. §3 keeps backups as the answer. | medium; the alert **done** 2026-11-19 |
| G6 | No read timeout for a request's body and no cap on connections; a warning logged per request when the token cannot be read; `/v1/disks` runs mount checks per request unserialized. | `ReadTimeout` of some 15 s and a listener that caps connections (`golang.org/x/net/netutil` is a new dependency; a counter of open connections is not); log an unreadable token once a minute; serialize `/v1/disks` as the catalog's answers are. | medium; **done** 2026-11-19 |
| G7 | Over the socket, nothing records which account answered a guard. | Read `SO_PEERCRED` and log the uid and pid with the answer; the event could carry it, which is a schema change. | low |
| G8 | The marker is optional: a disk with a pool disk's UUID and no marker, mounted in its place, is taken for it. | Once `disk add` wrote a marker (`layout.marker = true`, the default), a confirmation that finds none makes the disk `readonly` with a reason of its own; a new reason is an addition to a closed list (§29.3). | low |
| G9 | The API's token has no minimum length, and failed tokens cost nothing. | Refuse a token under 32 bytes when the request reads it (a 401 and a log line), or warn at load; a short delay after a 401. | low |
| G10 | `metrics.listen` answers any `Host`, so a page in a browser on the host can read it through DNS rebinding. | Answer only a `Host` that is the listen address, or `localhost` for a loopback one. | low |
| G11 | A folder presents the first disk's entry, and a `chmod` of a folder through the view applies to every disk: a stricter folder on another disk is not enforced. | Present the strictest mode of a folder's entries, or alert when they differ. | low |
| G12 | `100.64.0.0/10` is also carrier-grade NAT: an address there is taken for Tailscale's by its range alone. | At start, check that the address belongs to an interface named `tailscale*`, or say in §25.1 that the range is trusted as such. | low |
| G13 | A file the view makes is created with its final mode, setuid included, owned by `atoll`, before its owner is set. | Create it 0600, `fchown`, then `fchmod`, as the copy pipeline makes its `.part`. | low |
| G14 | POSIX ACLs are neither presented nor enforced by the view, nor copied to replicas. | Say so in §21.2 and §14; an alert for a file that holds an ACL. | low |
| G15 | `view_ops` records what the view did, not who asked. | The caller's uid and pid in the journal: a schema change, so format 2 or a new nullable column. | low |
| G16 | A name with characters that reorder or hide text (U+202E, U+2066 to U+2069, zero-width) is shown as it is. | Show in base64 a name with a `Cf` character, as one with a control character. | low |
| G17 | A process of any account named `mergerfs`, or running Atoll's binary, makes its changes look like the view's or Atoll's own. | Also compare the process's uid with the view's, or read the executable's path rather than its `comm`. | low |
| G18 | The units set none of the systemd hardening that makes no mount namespace, and the group `disk` writes every block device. | Add `SystemCallArchitectures=native`, `SystemCallFilter=@system-service @mount fanotify_init fanotify_mark` (to measure), `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`, `RestrictNamespaces=yes`, `LockPersonality=yes`, `MemoryDenyWriteExecute=yes`, `RestrictRealtime=yes`, `RestrictSUIDSGID=yes`, `UMask=0027`; replace the group `disk` with read access to the pool's devices alone (a udev rule). Each to measure in qemu with the loop-device tests, as a remount needs what it needs. | medium |
| G19 | `smartctl`, `mount` and `umount` come from `PATH`; `.mergerfs` is used through a link with no check that the mount is mergerfs. | Absolute paths, or a `PATH` set in the units; check `fuse.mergerfs` in the mount table first, and `lgetxattr`/`lsetxattr`. | low |
| G20 | A `catalog` path outside `/var/lib/atoll` gets a folder 0755 and a database 0644. | Make the folder 0700 and the database, its WAL and its backups 0600, whatever the umask, as `atoll init` does for the configuration. | low |
| G21 | The release builds, attests and publishes in one job with write permissions, and does not wait for the CI; images by tag; the toolchain floats. | A build job with `contents: read` handing its files to an attest-and-draft job; the release on a commit the CI passed; images by digest; a `toolchain` line in `go.mod`; a ruleset that protects `v*`. | low |

By priority: medium, G1 to G6 and G18; low, the rest.

### Done

What the maintainer decided on 2026-11-19, and what was made of it.

- **G1.** An answer to a guard over TCP takes a token file that belongs to root or to the account the daemon runs as (its effective uid), with no `g+w`, `o+r` or `o+w`; and each folder that holds its name, and the name of each symbolic link on the way to it, belongs to root or that account, with no `o+w`, sticky or not, and `g+w` only when its group is root's (gid 0) or the daemon's own (its effective gid). The 403 names each reason, in that order, with the `chmod` or `chown` that ends it, and the log says it too. Questions and dry runs are answered as before. The folders of every link are looked at, not only the file's, as whoever writes a link's folder points the link elsewhere; the folders above them are not, as a file or folder put in place by another account would be that account's, which the owner checks refuse. `cmd/atoll/api.go` (`tokenDoubts`, `tokenRefusal`), `internal/fsx/secret.go`; §25.1.
- **G4.** (a) A §24.4 warning at the key of each secret file, the eight of `config.SecretFiles`, that other accounts may read or write, or that belongs to an account none of Atoll's (root's, the one running, the catalog's owner, as `ops.TrustedOwners` says); `atoll rules check` gives it, and the daemon logs every warning of its configuration at its start and at each reload (`cmd/atoll/secrets.go`; scenarios `rules-check-secret-files`, `rules-check-secret-file-owner`). (b) Every secret file is read through `fsx.ReadSmallFile`: 4 KiB for a token, a key or a password, 16 KiB for the OTLP headers, a regular file only, so a FIFO no longer holds a notification, an alert or an export. (c) A §24.4 warning for a secret sent over `http://` to a host that is not this machine as written: a media server with its token or key, ntfy whatever its token (its topic is its secret), Gotify with its token, and any address with a user; OTLP is refused already (`internal/config/relations.go`, `value.LoopbackHost`; 30 config vectors). (d) `alerts.smtp_require_tls`, false by default: when true, no mail goes without TLS, implicit or by STARTTLS, and the error says why (`TestSMTPRequiresTLSWhenAsked`). §23, §24.1, §24.4.
- **G5.** The alert `versions-purged:<id>`, at priority 4: the `overwritten` entries the daemon's purge took from a disk's trash for room before their retention ended, counted from the first until a day goes by with none more, naming the first ten paths; it says those versions are lost, that a library rewritten slowly ends so with no clean version, and to look at what changes them and raise `trash.max_size`. It is counted in the daemon's memory as its purge goes, so a restart starts again from none; `atoll_alert` gives it, as every condition. Nothing else changes in the purge; the guard on what a day changed, and keeping `overwritten` entries while modifications run high, stay for the maintainer to weigh. `internal/alerts/conditions.go`, `internal/daemon/run.go` (`notePurged`, `versionsPurged`); `TestVersionsPurgedEarlyAreCounted`, `TestTheDaemonTellsOfVersionsPurgedEarly`; §17, §23.
- **G6.** `ReadTimeout` of 15 s on every server of the API, socket, TCP and `metrics.listen`, beside the 10 s of headers, the 60 s of an answer and of an idle connection; each listener holds 32 connections at once through Atoll's own small listener (`cmd/atoll/apilimit.go`, no new dependency), and a connection past them waits, unaccepted, in the kernel's queue, until one closes; the warning for a token file that cannot be read goes to the log once a minute at most, with the requests refused since; `/v1/disks` and `/v1/disks/{id}` check the mounts one at a time, and `/v1/disks/{id}`, which reads the catalog too, takes its turn there first, as the other catalog answers do. §25.1.

## Accepted risks

- Root, and whoever writes `/etc/atoll`, controls Atoll; so does a member of the group `atoll`, for the guards (§26).
- A disk's writer controls that disk's content, which Atoll copies with its owner and mode; the guards and the trash bound what propagates, they do not authenticate it.
- Ransomware is for backups (§3). In phase 1, a deletion through mergerfs reaches every copy at once, and only Atoll's own view sends it to the trash (§15.6).
- The metrics, and the paths sent to the services the configuration names, are disclosed by design.

## When to look again

At a new entry point (a new write of the API, a new service told, a web interface in phase 5), a change to the units or to the view's permission model, the Rust version (§29.2), and Windows (phase 4), whose service account, named pipe and WinFsp drive are each an entry point of their own.
