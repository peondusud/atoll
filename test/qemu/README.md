# test/loopdev in qemu

`test/loopdev` needs root, loop devices, device-mapper, every filesystem driver Atoll meets (ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g), and mergerfs for the break-glass test. A Docker container has none of that, as it shares its host's kernel: Docker Desktop's has no ntfs3. So `run.sh` boots kernels of its own in qemu, inside a throwaway `debian:13` container, and runs the tests there as root. It needs Docker and nothing else, on Linux and on Windows alike.

- Two kernels by default: Debian 13's (Linux 6.12) and a mainline one from kernel.ubuntu.com (v7.3.1), the two the measurements of specs.md were made on.
- Each guest boots from an initramfs made of the container's own files, with no network. Its only disks are loop images in its own memory: nothing touches a real disk.
- The guest sets `ATOLL_QEMU=1`: a test that skips on a machine without mergerfs fails in the guest, where `run.sh` installs it.
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
| `COUNT` | `1` | how many times each test runs |
| `VERBOSE` | `0` | `1` lists every test as it passes, fails or is skipped |
| `TIMEOUT` | `3000s` | the tests' own timeout, in each guest |
| `QEMU_TIMEOUT` | `3600` | seconds before a guest is stopped |
| `QEMU_MEMORY` | `6144` | the guest's memory in MiB; the loop images take up to 2 GiB of it |

For example, the tests of a device that fails during an operation, twice, on Debian's kernel only:

```powershell
docker run --rm -e KERNELS=debian -e RUN=TestADeviceThatFails -e COUNT=2 -v "${PWD}:/src:ro" -v "${PWD}/bin/qemu:/qemu:ro" debian:13 bash /src/test/qemu/run.sh
```
