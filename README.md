<img src="docs/logo.png" alt="The logo of Atoll: four islets on one reef around a lagoon, seen from the air" width="128" align="right">

# Atoll

Atoll adds file-level redundancy to heterogeneous disks (SATA, USB DAS) that hold a media library (Plex, Emby, Jellyfin), without moving existing files.

**Under development and not ready for real data.** The alpha, phase 1a, is in progress: see [ROADMAP.md](ROADMAP.md).

## How it works

- **Copies.** Each file under a disk's `media/` folder can have 0 to 2 full copies on other disks, at the same relative path and under the same name. Every disk stays a normal folder tree, readable on its own.
- **The merged view.** A merged view presents each file once: mergerfs in phase 1, Atoll's own FUSE view from phase 2a. Media servers read that view. Atoll's own view already serves them read only beside mergerfs (`atoll mount`): browsing it wakes no disk, and each file reads from its original copy.
- **Break glass.** Stopping or uninstalling Atoll breaks nothing. Existing paths stay as they were, no disk holds a hidden file, and plain mergerfs, mounted from the line `atoll mergerfs-config` prints, gives the same view.
- **No data lost.** Atoll never deletes or overwrites the last verified copy of a file. What it replaces goes to a trash on the same disk first. When in doubt, it keeps both versions and reports a conflict.
- **Checked.** Every copy is verified by a BLAKE3 hash before it takes its place.
- **Disks.** A disk is known by its filesystem's identifier. Atoll writes to it only once it has confirmed that identifier.
- **The catalog.** The SQLite catalog is a cache that can be rebuilt: the truth is on the disks.

The Go version runs on Linux, amd64 and arm64. A Rust rewrite with the same contracts follows, then Windows.

## Documentation

| File | What it holds |
|---|---|
| [specs.md](specs.md) | The specification, and the source of truth |
| [ROADMAP.md](ROADMAP.md) | The milestones, where the work stands, and what remains before each |
| [TODO.md](TODO.md), [DONE.md](DONE.md) | Open work, and finished work with how it was verified |
| [LESSONS.md](LESSONS.md) | What the work taught |
| [docs/research/](docs/research/) | The measurements behind open design decisions |
| [conformance/](conformance/README.md) | Vectors and scenarios shared by the Go and Rust versions |
| [api/](api/README.md) | The `--json` schemas of the command line |
| [deploy/](deploy/) | A sample configuration, the systemd units, and the `.deb`'s scripts |

## Installing a release

Each [release](https://github.com/peondusud/atoll/releases) holds, for amd64 and arm64, a `.deb` and a `.tar.gz`, `SHA256SUMS`, an SBOM of each archive, and the provenance GitHub attested when it built each archive and package. The archive holds the static binary, the systemd units, the sample configuration, `LICENSE` and this README. Check a download before installing it, the second line with [gh](https://cli.github.com/):

```bash
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify atoll_0.1.0~beta.1_amd64.deb --repo peondusud/atoll
```

Installing either changes nothing on its own: no unit is enabled or started, and no configuration is written.

- **The `.deb`**, for Debian 13 and the systems built on it, tried on Debian 13: `sudo apt install ./atoll_<version>_amd64.deb`. It installs `/usr/bin/atoll` and the units in `/usr/lib/systemd/system/`, makes the account `atoll` they run as, and brings mergerfs and smartmontools, which it recommends. The sample configuration is `/usr/share/doc/atoll/examples/atoll.toml`.
- **The `.tar.gz`**: copy `atoll` to `/usr/local/bin/`, where the units of `deploy/` start it, then make the account and install the units as `deploy/README.md` says.

Then `sudo atoll init --pool-id <id> --view-mount <path>` writes `/etc/atoll/atoll.toml` and the catalog, `sudo atoll disk add <mount point>` adds each disk, and `sudo systemctl enable --now atoll` starts the daemon (`deploy/README.md`). `atoll --version` says which release runs. Removing the package leaves the account, `/etc/atoll`, `/var/lib/atoll`, the disks and a unit that runs as they are: stop and disable the units first.

## Building and testing

Go 1.27 or later:

```bash
make build              # bin/atoll
go test ./...           # unit and property tests, with the conformance vectors
make conformance        # the black-box scenarios against a conformance build
make test-qemu          # test/loopdev as root in qemu, on Linux 6.12 and 7.3, with only Docker on the host
test/ci/run.sh [REV]    # every check of a commit, with only git and Docker
make release-snapshot   # a release as a tag makes one, into dist/, with only Docker: nothing is uploaded
```

GitHub Actions runs the same checks on each push to `beta` and `main` and each pull request (`.github/workflows/ci.yml`), the unit tests on arm64 too, and the media-server test once a week.

## Releasing

The maintainer alone pushes a tag `vX.Y.Z`, or `vX.Y.Z-beta.N` for a prerelease, on a commit the CI passed: `git tag -a v0.1.0-beta.1 -m v0.1.0-beta.1 && git push origin v0.1.0-beta.1`. `.github/workflows/release.yml` builds it with GoReleaser (`.goreleaser.yaml`), drafts the release, marked a prerelease for `-alpha`, `-beta` and `-rc`, and attests each archive and package. Once that run has passed, publish the draft. `make release-snapshot` tries the same on this machine, from a clone; on Windows, from PowerShell at the root of one:

```powershell
docker run --rm -v "${PWD}:/src" -w /src -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src goreleaser/goreleaser:v2.20.0 release --snapshot --clean
```

## License

SPDX-License-Identifier: GPL-3.0-or-later

The full text of version 3 is in [LICENSE](LICENSE).
