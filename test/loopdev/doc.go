// Package loopdev holds the tests that need root: real ext4, XFS, btrfs and
// NTFS filesystems on loop devices. Their files carry the loopdev build tag:
// run them with sudo make test-loopdev, or with make test-qemu on any host
// with Docker, which boots Debian's kernel and a mainline one in qemu
// (test/qemu/README.md).
package loopdev
