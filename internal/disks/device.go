package disks

import "github.com/peondusud/atoll/internal/fsx"

// DeviceIdentifier says what a device's name leads to, for what a mount check cannot ask: whether the device a disk
// was mounted from left (specs.md §15.13). A machine need not have it: then no device is ever said to have left.
type DeviceIdentifier interface {
	DeviceNode(path string) (fsx.DeviceNode, error)
}

// DeviceLeft says whether the device a disk was mounted from left, from what its name led to at the last check that
// found the disk mounted, and what it leads to now: nowhere, to no block device, to other numbers, or to the same
// numbers with another sequence number of its disk, both known, which a disk plugged again gets anew (specs.md
// §15.13, conformance/vectors/device_left.json). Nothing known before, or nothing that tells the same device from one
// plugged anew, is no departure: an administrator's umount leaves the device where it was.
func DeviceLeft(before, now fsx.DeviceNode) bool {
	switch {
	case !before.There || !before.Block:
		return false
	case !now.There || !now.Block || now.Major != before.Major || now.Minor != before.Minor:
		return true
	default:
		return before.Seq != 0 && now.Seq != 0 && now.Seq != before.Seq
	}
}
