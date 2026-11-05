// Package faults holds the tests on real filesystems: ext4, NTFS, XFS and btrfs
// on loop devices, and USB drops with dm-error or dm-flakey. They need root.
// Their files carry the loopdev build tag: run them with sudo make test-loopdev.
package faults
