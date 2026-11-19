# test/loopdev in qemu

`test/loopdev` needs root, loop devices, device-mapper, every filesystem driver Atoll meets (ext4, XFS, btrfs, ntfs3, the type `ntfs` of Linux 7.1 and later, ntfs-3g and lowntfs-3g), and mergerfs for the break-glass test. The tests of the type `ntfs` skip on Debian 13's 6.12, which has no such driver, and run on the mainline kernel. A Docker container has none of that, as it shares its host's kernel: Docker Desktop's has no ntfs3. So `run.sh` boots kernels of its own in qemu, inside a throwaway `debian:13` container, and runs the tests there as root. It needs Docker and nothing else, on Linux and on Windows alike.

- Two kernels by default: Debian 13's (Linux 6.12) and a mainline one from kernel.ubuntu.com (v7.3.1), the two the measurements of specs.md were made on.
- Each guest boots from an initramfs made of the container's own files, with no network. Its only disks are loop images in its own memory: nothing touches a real disk.
- The guest sets `ATOLL_QEMU=1`: a test that skips on a machine without mergerfs fails in the guest, where `run.sh` installs it.
- Each guest runs the tests twice. The first run has a shell for PID 1, and no service manager. The second, once the first is over, has systemd for PID 1, as a machine has, and runs the tests that start `deploy/atoll.service` and `deploy/atoll-mount.service` as they are, the real `atoll` at `/usr/local/bin`, as the account `atoll`: the units' hardening is measured there (`test/loopdev/units_test.go`, `*UnderSystemd`). Those tests skip in the first run, and anywhere but in the guest, as they install the units over any of the machine. Nothing of `sysinit.target` runs, as its tmpfiles would empty `/tmp`, where the tests' disks are: `run.sh` masks it. Nor do the system bus, polkit and udev, which a machine runs: `TestTheRemountUnderSystemd` starts them for itself, with drop-ins that keep `sysinit.target` out, as the daemon's remount after a USB reset has systemd start the disk's mount unit through them, as the account `atoll`, with Atoll's polkit rule installed as the `.deb` installs it, then without it.
- Without KVM, as on Docker Desktop, qemu emulates the processor: a whole run takes about 20 minutes, the tests about 5 of them on each kernel. With `/dev/kvm` they should take far less.
- The exit code is the worst of the guests': 0 when every test passed, 1 when one failed, 2 when a guest did not say, a download failed or nothing was built.

## Running it

On Linux, with Go and Docker:

```bash
make test-qemu
```

On Windows, from PowerShell at the root of the repository, with Docker Desktop. The first command builds the tests in the Go image into `bin/qemu/`, the second boots them:

```powershell
docker run --rm -v "${PWD}:/src" -w /src golangci/golangci-lint:v2.15.0 make qemu-build
docker run --rm -v "${PWD}:/src:ro" -v "${PWD}/bin/qemu:/qemu:ro" debian:13 bash /src/test/qemu/run.sh
```

## Choosing what runs

Environment variables of the second command, passed with `-e NAME=value` (`make test-qemu` passes them on):

| Variable | Default | What it does |
|---|---|---|
| `KERNELS` | `debian mainline` | the kernels to boot, in that order |
| `MAINLINE` | `v7.3.1` | the mainline version, as kernel.ubuntu.com names its folders |
| `RUN` | every test | a `-test.run` pattern, such as `TestADeviceThatFails` |
| `SYSTEMD_RUN` | `UnderSystemd$`, or none when `RUN` is set | the `-test.run` pattern of the second run, under systemd; empty for no second run. `RUN='^$' SYSTEMD_RUN='UnderSystemd$'` runs only the tests of the units |
| `COUNT` | `1` | how many times each test runs |
| `VERBOSE` | `0` | `1` lists every test as it passes, fails or is skipped |
| `TIMEOUT` | `3000s` | the tests' own timeout, in each guest |
| `QEMU_TIMEOUT` | `3600` | seconds before a guest is stopped |
| `QEMU_MEMORY` | `6144` | the guest's memory in MiB; the loop images take up to 2 GiB of it |
| `SOAK` | none | runs `TestDaemonSoak` for that long, such as `4h`: the daemon that runs for good under a writer, its memory logged every tenth of the run. Raise `TIMEOUT` and `QEMU_TIMEOUT` past it, by 20 minutes for the first pass |
| `SOAK_FILES` | `2000` | the files each disk of the soak starts with; past 2,000, its volumes are sized for them, and its first pass has 10 ms a file more |
| `SCALE` | none | runs `TestScale` with that many files a disk, such as `200000`: the first scan, `atoll status` with a strict rule, and the passes of the link guard, each timed with its memory. Raise `TIMEOUT` and `QEMU_TIMEOUT` as for a soak |
| `CACHE` | none | a folder of the container that outlives it, such as `-v <folder>:/var/cache/atoll-qemu -e CACHE=/var/cache/atoll-qemu`: Debian's packages and the mainline kernel are kept there, and what it holds is not downloaded again. `make test-qemu QEMU_CACHE=<folder>` mounts a folder of the host there; the CI's qemu job keeps one from a run to the next |

The run says whether qemu has KVM (`accelerator: KVM`) or emulates the processor.

For example, the tests of a device that fails during an operation, twice, on Debian's kernel only:

```powershell
docker run --rm -e KERNELS=debian -e RUN=TestADeviceThatFails -e COUNT=2 -v "${PWD}:/src:ro" -v "${PWD}/bin/qemu:/qemu:ro" debian:13 bash /src/test/qemu/run.sh
```

And the daemon's soak, for four hours:

```powershell
docker run --rm -e KERNELS=debian -e RUN=TestDaemonSoak -e VERBOSE=1 -e SOAK=4h -e TIMEOUT=16000s -e QEMU_TIMEOUT=16500 -v "${PWD}:/src:ro" -v "${PWD}/bin/qemu:/qemu:ro" debian:13 bash /src/test/qemu/run.sh
```
