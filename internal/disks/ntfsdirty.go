package disks

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// The parts of an NTFS volume that say it is dirty (specs.md §22): the fourth record of the MFT is $Volume, whose
// $VOLUME_INFORMATION attribute holds the flag.
const (
	ntfsVolumeRecord  = 3
	ntfsVolumeInfo    = 0x70
	ntfsAttributesEnd = 0xffffffff
	ntfsDirtyFlag     = 0x0001
	ntfsBlock         = 512
)

// ErrNotNTFS says that a device holds no NTFS volume that can be read so.
var ErrNotNTFS = errors.New("no NTFS volume")

// NTFSDirty reads an NTFS volume, a few sectors through read, and says whether it is marked dirty, as an unclean
// drop leaves it (specs.md §22). read gives n bytes of the device at off.
func NTFSDirty(read func(off int64, n int) ([]byte, error)) (bool, error) {
	boot, err := read(0, ntfsBlock)
	if err != nil {
		return false, fmt.Errorf("boot sector: %w", err)
	}
	if len(boot) < ntfsBlock || !bytes.Equal(boot[3:11], []byte("NTFS    ")) {
		return false, ErrNotNTFS
	}
	sector := int64(binary.LittleEndian.Uint16(boot[0x0B:]))
	if sector < 256 || sector > 4096 || bits.OnesCount64(uint64(sector)) != 1 {
		return false, fmt.Errorf("%w: %d bytes a sector", ErrNotNTFS, sector)
	}
	perCluster := int64(boot[0x0D])
	if perCluster > 0x80 {
		// A power of two, 2^(256 - n). Past 2^12 no volume has it, and 2^63 is no count that 64 bits hold: it would
		// read as a negative one, which is under any limit, and leave clusters of 0 bytes.
		power := 256 - perCluster
		if power > 12 {
			return false, fmt.Errorf("%w: 2^%d sectors a cluster", ErrNotNTFS, power)
		}
		perCluster = 1 << power
	}
	if perCluster <= 0 || bits.OnesCount64(uint64(perCluster)) != 1 || perCluster > 1<<12 {
		return false, fmt.Errorf("%w: %d sectors a cluster", ErrNotNTFS, perCluster)
	}
	cluster := sector * perCluster
	record := int64(0)
	// As an integer of its own width, -128 is its own opposite, and would pass for a small power.
	if n := int(int8(boot[0x40])); n < 0 {
		if -n > 20 {
			return false, fmt.Errorf("%w: a record of 2^%d bytes", ErrNotNTFS, -n)
		}
		record = 1 << -n
	} else {
		record = int64(n) * cluster
	}
	if record < ntfsBlock || record > 1<<16 || record%ntfsBlock != 0 {
		return false, fmt.Errorf("%w: a record of %d bytes", ErrNotNTFS, record)
	}
	mft := binary.LittleEndian.Uint64(boot[0x30:])
	if mft > uint64(1<<62)/uint64(cluster) {
		return false, fmt.Errorf("%w: the MFT at cluster %d", ErrNotNTFS, mft)
	}
	rec, err := read(int64(mft)*cluster+ntfsVolumeRecord*record, int(record))
	if err != nil {
		return false, fmt.Errorf("$Volume: %w", err)
	}
	if int64(len(rec)) < record {
		return false, fmt.Errorf("%w: $Volume cut short", ErrNotNTFS)
	}
	if err := fixUp(rec); err != nil {
		return false, err
	}
	return volumeDirty(rec)
}

// fixUp applies a record's update sequence: the last two bytes of each block must be the update number, and take
// back what the array kept of them. A block that ends otherwise was torn by a write that did not finish.
func fixUp(rec []byte) error {
	if !bytes.Equal(rec[:4], []byte("FILE")) {
		return fmt.Errorf("%w: $Volume is no record", ErrNotNTFS)
	}
	at := int(binary.LittleEndian.Uint16(rec[4:]))
	count := int(binary.LittleEndian.Uint16(rec[6:]))
	blocks := len(rec) / ntfsBlock
	if count != blocks+1 || at < 8 || at+2*count > len(rec) {
		return fmt.Errorf("%w: an update sequence that does not fit its record", ErrNotNTFS)
	}
	number := [2]byte(rec[at : at+2])
	for i := range blocks {
		end := (i+1)*ntfsBlock - 2
		if [2]byte(rec[end:end+2]) != number {
			return fmt.Errorf("%w: $Volume torn in block %d", ErrNotNTFS, i)
		}
		copy(rec[end:end+2], rec[at+2+2*i:at+4+2*i])
	}
	return nil
}

// volumeDirty walks a record's attributes to $VOLUME_INFORMATION, which is always resident, and reads its flag.
func volumeDirty(rec []byte) (bool, error) {
	at := int(binary.LittleEndian.Uint16(rec[0x14:]))
	for {
		if at+8 > len(rec) {
			return false, fmt.Errorf("%w: attributes past the record", ErrNotNTFS)
		}
		kind := binary.LittleEndian.Uint32(rec[at:])
		if kind == ntfsAttributesEnd {
			return false, fmt.Errorf("%w: no volume information", ErrNotNTFS)
		}
		length := int(binary.LittleEndian.Uint32(rec[at+4:]))
		if length < 0x18 || at+length > len(rec) {
			return false, fmt.Errorf("%w: an attribute of %d bytes", ErrNotNTFS, length)
		}
		if kind == ntfsVolumeInfo {
			if rec[at+8] != 0 {
				return false, fmt.Errorf("%w: volume information not resident", ErrNotNTFS)
			}
			size := int(binary.LittleEndian.Uint32(rec[at+0x10:]))
			value := int(binary.LittleEndian.Uint16(rec[at+0x14:]))
			if size < 12 || value+size > length {
				return false, fmt.Errorf("%w: volume information of %d bytes", ErrNotNTFS, size)
			}
			return binary.LittleEndian.Uint16(rec[at+value+0x0A:])&ntfsDirtyFlag != 0, nil
		}
		at += length
	}
}

// DeviceReader reads a block device that holds no mounted filesystem, by path, for what a mount check cannot ask.
// A machine need not have it: then nothing reads a device so.
type DeviceReader interface {
	// ReadDevice gives n bytes of the device at path, from off; an error that wraps fs.ErrNotExist when there is no
	// such device.
	ReadDevice(path string, off int64, n int) ([]byte, error)
}

// ErrNoDevice says that a disk's device cannot be found, or read, on this machine.
var ErrNoDevice = errors.New("no device to read")

// ProbeNTFSDirty says whether the NTFS volume of a disk is marked dirty, reading it through its identifier's name
// under /dev/disk/by-uuid, which udev keeps as the volume's serial (specs.md §22). It is for a disk that is not
// mounted: ErrNoDevice when the machine reads no device or the name is not there.
func ProbeNTFSDirty(m Machine, d *config.Disk) (bool, error) {
	reader, ok := m.(DeviceReader)
	if !ok || d.FS != names.NTFS {
		return false, ErrNoDevice
	}
	path := "/dev/disk/by-uuid/" + d.UUID
	dirty, err := NTFSDirty(func(off int64, n int) ([]byte, error) { return reader.ReadDevice(path, off, n) })
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return dirty, nil
}
