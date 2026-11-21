#!/bin/bash
# Runs test/loopdev as root inside qemu, on Debian's kernel and on a mainline one, from a throwaway
# debian:13 container: the host needs neither root, nor loop devices, nor ntfs3 (README.md beside this
# file). Each guest boots from an initramfs made of this container's own files, with no network, and
# its only disks are loop images in its own memory.
#
# It reads /qemu, which `make qemu-build` fills: loopdev.test, and repo/ with go.mod and conformance/,
# which the tests look for above their folder. Its exit code is the worst of the guests': 0 when every
# test passed, 1 when one failed, 2 when a guest did not say.
#
# CACHE, a folder that outlives the container, as a CI keeps one from a run to the next, holds what the
# run downloads: Debian's packages, and the mainline kernel's. What is there is not downloaded again.
set -u
here=$(cd "$(dirname "$0")" && pwd)
built=${BUILT:-/qemu}
kernels=${KERNELS:-debian mainline}
mainline=${MAINLINE:-v7.3.5}
cache=${CACHE:-}

if [ ! -x "$built/loopdev.test" ] || [ ! -f "$built/repo/go.mod" ]; then
	echo "no $built/loopdev.test or $built/repo/go.mod: run make qemu-build first" >&2
	exit 2
fi
# A binary older than the code beside it tests other code, and a test it lacks reports PASS, having run nothing.
newer=$(find "$here/../loopdev" "$here/../../internal" "$here/../../cmd" -name '*.go' -newer "$built/loopdev.test" -print -quit 2>/dev/null)
if [ -n "$newer" ]; then
	echo "$built/loopdev.test is older than ${newer#"$here/../../"}: run make qemu-build first" >&2
	exit 2
fi

export DEBIAN_FRONTEND=noninteractive
apt=(apt-get -qq)
if [ -n "$cache" ]; then
	# Below /var/cache, which the initramfs leaves out; out of reach of the image's docker-clean, which empties
	# /var/cache/apt/archives after each install.
	mkdir -p "$cache/apt/partial" || exit 2
	apt+=(-o "Dir::Cache::Archives=$cache/apt/")
	echo "cache: $cache, $(find "$cache" -type f -name '*.deb' | wc -l) packages in it"
fi
echo "installing the guest's tools and Debian's kernel"
"${apt[@]}" update >/dev/null || exit 2
# What the tests call, and what makes the initramfs. The kernel's package brings its modules. mergerfs brings
# mount.mergerfs, which the break-glass test mounts through, as a boot would; iproute2 the loopback the init brings up;
# systemd, PID 1 of the second run, smartmontools, whose smartctl the daemon runs there, and fdisk, whose sfdisk
# partitions a SCSI disk there; dbus, polkitd and udev, which the remount through systemd needs there, and which only
# the test of it starts (test/loopdev/units_test.go).
"${apt[@]}" install -y --no-install-recommends linux-image-amd64 ntfs-3g e2fsprogs xfsprogs btrfs-progs \
	util-linux dmsetup procps kmod cpio curl ca-certificates zstd mergerfs iproute2 systemd smartmontools \
	fdisk dbus polkitd udev >/dev/null || exit 2
versions=(/usr/lib/modules/*)
debian_version=${versions[0]##*/}
echo "debian kernel: $debian_version"

if [[ " $kernels " == *" mainline "* ]]; then
	base=https://kernel.ubuntu.com/mainline/$mainline/amd64
	debs=/dl
	[ -n "$cache" ] && debs=$cache/mainline/$mainline
	mkdir -p "$debs" /dl/x || exit 2
	image=$(find "$debs" -maxdepth 1 -name 'linux-image-unsigned-*-generic_*_amd64.deb' | sort | head -1)
	modules=$(find "$debs" -maxdepth 1 -name 'linux-modules-*-generic_*_amd64.deb' | sort | head -1)
	if [ -n "$image" ] && [ -n "$modules" ]; then
		echo "the $mainline kernel, from the cache"
	else
		echo "downloading the $mainline kernel"
		list=$(curl -sSf "$base/") || { echo "cannot list $base" >&2; exit 2; }
		image=$(grep -o 'linux-image-unsigned-[^"]*-generic_[^"]*_amd64\.deb' <<<"$list" | sort -u | head -1)
		modules=$(grep -o 'linux-modules-[^"]*-generic_[^"]*_amd64\.deb' <<<"$list" | sort -u | head -1)
		if [ -z "$image" ] || [ -z "$modules" ]; then
			echo "no kernel image or modules listed at $base" >&2
			exit 2
		fi
		image=$debs/$image modules=$debs/$modules
		# Through another name first, so that a download cut short is never taken for a whole one.
		for deb in "$image" "$modules"; do
			if ! curl -sSf -o "$deb.part" "$base/${deb##*/}" || ! mv "$deb.part" "$deb"; then
				echo "cannot download the $mainline kernel" >&2
				exit 2
			fi
		done
	fi
	# Into a folder of its own: extracted over /, tar could replace the /lib symlink.
	dpkg-deb -x "$image" /dl/x && dpkg-deb -x "$modules" /dl/x || exit 2
	for d in /dl/x/lib/modules/* /dl/x/usr/lib/modules/*; do
		[ -d "$d" ] && cp -a "$d" /usr/lib/modules/
	done
	kernel_files=(/dl/x/boot/vmlinuz-*)
	mainline_file=${kernel_files[0]##*/}
	mainline_version=${mainline_file#vmlinuz-}
	cp "/dl/x/boot/$mainline_file" /boot/
	echo "mainline kernel: $mainline_version"
fi

# Only the modules the tests can load: the initramfs lives in the guest's memory.
for v in /usr/lib/modules/*; do
	find "$v/kernel" -mindepth 1 -maxdepth 1 ! -name fs ! -name lib ! -name crypto ! -name drivers ! -name arch -exec rm -rf {} +
	find "$v/kernel/drivers" -mindepth 1 -maxdepth 1 ! -name block ! -name md ! -name scsi -exec rm -rf {} +
	depmod -a "$(basename "$v")"
done

# The guest's files: its init, the tests, and what they read. A bind mount would be left out of the
# initramfs, which stays on one filesystem.
cp "$here/init" /init && chmod +x /init
mkdir -p /opt/atoll && cp "$built/loopdev.test" /opt/atoll/ && cp -r "$built/repo" /opt/atoll/repo || exit 2
mkdir -p /opt/atoll/repo/test/loopdev
# atoll where deploy/'s units start it, for the second run.
if [ -x "$built/atoll" ]; then
	cp "$built/atoll" /usr/local/bin/atoll || exit 2
fi
common=(-test.count="${COUNT:-1}" -test.timeout="${TIMEOUT:-3000s}")
[ "${VERBOSE:-0}" = 1 ] && common+=(-test.v)
args=("${common[@]}")
[ -n "${RUN:-}" ] && args+=(-test.run="$RUN")
# The second run, under systemd as PID 1: the tests SYSTEMD_RUN names, those of deploy/'s units by default when RUN
# names none; when RUN names some, only those SYSTEMD_RUN names; none when it is empty.
units_default='UnderSystemd$'
if [ -n "${RUN:-}" ]; then
	systemd_run=${SYSTEMD_RUN-}
else
	systemd_run=${SYSTEMD_RUN-$units_default}
fi
rm -f /opt/atoll/units-args
if [ -n "$systemd_run" ]; then
	printf '%s\n' "${common[@]}" -test.run="$systemd_run" >/opt/atoll/units-args
	cp "$here/units" /opt/atoll/units && chmod +x /opt/atoll/units || exit 2
	install -m 644 "$here/atoll-qemu-units.service" /etc/systemd/system/ || exit 2
	# Nothing of sysinit.target, which no unit of the tests may pull in: its tmpfiles would empty /tmp.
	ln -sf /dev/null /etc/systemd/system/sysinit.target || exit 2
	echo "second run, under systemd: $systemd_run"
fi
# The soak of the daemon that runs for good (test/loopdev/soak_test.go): SOAK=30m, SOAK_FILES=2000.
[ -n "${SOAK:-}" ] && args+=(-atoll.soak="$SOAK" -atoll.soak-files="${SOAK_FILES:-2000}")
# What grows with the pool and no other test reaches (test/loopdev/scale_test.go): SCALE=200000 files a disk.
[ -n "${SCALE:-}" ] && args+=(-atoll.scale="$SCALE")
printf '%s\n' "${args[@]}" >/opt/atoll/args

initramfs() {
	local keep=$1 out=$2
	(cd / && find . -xdev \( -path ./proc -o -path ./sys -o -path ./dev -o -path ./tmp -o -path ./run -o -path ./boot \
		-o -path ./dl -o -path ./var/cache -o -path ./var/lib/apt -o -path ./usr/share/doc -o -path ./usr/share/man \
		-o -path ./usr/share/locale \) -prune -o \( -path './usr/lib/modules/*' -a -not -path "./usr/lib/modules/$keep*" \) -prune -o -print |
		cpio -o -H newc --quiet | gzip -1 >"$out")
}
mkdir -p /dl
initramfs "$debian_version" /dl/debian.gz || exit 2
if [ -n "${mainline_version:-}" ]; then
	initramfs "$mainline_version" /dl/mainline.gz || exit 2
fi

# Installed last, so that qemu stays out of the initramfs.
"${apt[@]}" install -y --no-install-recommends qemu-system-x86 >/dev/null || exit 2
# What can no longer be downloaded, an older version of a package, leaves the cache.
[ -n "$cache" ] && "${apt[@]}" autoclean >/dev/null
if [ -w /dev/kvm ]; then
	accel=(-accel kvm -cpu host)
	echo "accelerator: KVM"
else
	# shellcheck disable=SC2054 # one option, whose commas are its own
	accel=(-accel tcg,thread=multi -cpu max)
	echo "accelerator: none, no /dev/kvm: qemu emulates the processor"
fi
cpus=$(nproc)
[ "$cpus" -gt 8 ] && cpus=8

worst=0
boot() {
	local name=$1 kernel=$2 initrd=$3
	echo "######## $name"
	timeout "${QEMU_TIMEOUT:-3600}" qemu-system-x86_64 "${accel[@]}" -smp "$cpus" -m "${QEMU_MEMORY:-6144}" \
		-no-reboot -nic none -display none -monitor none -serial stdio \
		-kernel "$kernel" -initrd "$initrd" -append "console=ttyS0 rdinit=/init panic=-1 loglevel=3" | tee /tmp/console
	local code
	code=$(grep -ao 'ATOLL-QEMU-EXIT=[0-9]*' /tmp/console | tail -1 | cut -d= -f2)
	if [ -z "$code" ]; then
		echo "######## $name said nothing of its tests"
		code=2
	elif [ "$code" -ne 0 ]; then
		code=1
	fi
	[ "$code" -gt "$worst" ] && worst=$code
	echo "######## $name: exit $code"
}
for k in $kernels; do
	case $k in
	debian) boot "Linux $debian_version" "/boot/vmlinuz-$debian_version" /dl/debian.gz ;;
	mainline) boot "Linux $mainline_version" "/boot/$mainline_file" /dl/mainline.gz ;;
	*)
		echo "unknown kernel $k: debian or mainline" >&2
		worst=2
		;;
	esac
done
exit "$worst"
