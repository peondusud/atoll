package disks

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/peondusud/atoll/internal/names"
)

// HeadSize is how much of a device's start a check reads: up to the end of the btrfs magic, the
// highest offset of specs.md §10.
const HeadSize = 65608

// Variant is what a superblock says about name rules (specs.md §10, §15.8).
type Variant string

// The variants of a superblock.
const (
	VariantNone     Variant = ""
	VariantCasefold Variant = "casefold"
	VariantASCIICI  Variant = "ascii-ci"
)

// Identity is what the head of a device says about its filesystem. TimePrecisionNS is how finely it holds a
// time, in nanoseconds (specs.md §9), 0 when nothing says.
type Identity struct {
	ID              string
	Variant         Variant
	TimePrecisionNS int64
}

// The precisions of a time, in nanoseconds (specs.md §9).
const (
	nanosecond   = 1
	ntfsTick     = 100
	wholeSecond  = 1_000_000_000
	oldInodeSize = 128
)

// TimePrecision gives how finely a filesystem holds a time when nothing but its kind is known: an ext4 is taken to
// have the inodes mkfs.ext4 makes, which hold nanoseconds (specs.md §9).
func TimePrecision(fs names.Filesystem) int64 {
	if fs == names.NTFS {
		return ntfsTick
	}
	return nanosecond
}

// ErrNoFilesystem says that a head does not hold the filesystem it was read as.
var ErrNoFilesystem = errors.New("no such filesystem")

// ReadIdentity reads a head as the layout of fs, and looks at nothing else, so that what an
// earlier filesystem left at another offset changes nothing.
func ReadIdentity(fs names.Filesystem, head []byte) (Identity, error) {
	var id Identity
	var err error
	switch fs {
	case names.Ext4:
		id, err = readExt4(head)
	case names.XFS:
		id, err = readXFS(head)
	case names.Btrfs:
		id, err = readBtrfs(head)
	case names.NTFS:
		id, err = readNTFS(head)
	default:
		return Identity{}, fmt.Errorf("%w: unknown %q", ErrNoFilesystem, fs)
	}
	if err != nil {
		return Identity{}, err
	}
	if id.TimePrecisionNS == 0 {
		id.TimePrecisionNS = TimePrecision(fs)
	}
	return id, nil
}

func readExt4(head []byte) (Identity, error) {
	const (
		magicAt     = 1080
		revisionAt  = 1100
		inodeSizeAt = 1112
		featuresAt  = 1120
		uuidAt      = 1128
		casefold    = 0x20000
	)
	if len(head) < uuidAt+16 || !bytes.Equal(head[magicAt:magicAt+2], []byte{0x53, 0xEF}) {
		return Identity{}, fmt.Errorf("%w: ext4", ErrNoFilesystem)
	}
	id := Identity{ID: formatUUID(head[uuidAt : uuidAt+16]), TimePrecisionNS: nanosecond}
	if binary.LittleEndian.Uint32(head[featuresAt:])&casefold != 0 {
		id.Variant = VariantCasefold
	}
	// Revision 0 has 128-byte inodes, whatever the size field holds; an inode that small has no room for the
	// nanoseconds of its times.
	if binary.LittleEndian.Uint32(head[revisionAt:]) == 0 || binary.LittleEndian.Uint16(head[inodeSizeAt:]) <= oldInodeSize {
		id.TimePrecisionNS = wholeSecond
	}
	return id, nil
}

func readXFS(head []byte) (Identity, error) {
	const (
		uuidAt    = 32
		versionAt = 100
		borgBit   = 0x4000
	)
	if len(head) < versionAt+2 || !bytes.HasPrefix(head, []byte("XFSB")) {
		return Identity{}, fmt.Errorf("%w: xfs", ErrNoFilesystem)
	}
	id := Identity{ID: formatUUID(head[uuidAt : uuidAt+16])}
	if binary.BigEndian.Uint16(head[versionAt:])&borgBit != 0 {
		id.Variant = VariantASCIICI
	}
	return id, nil
}

func readBtrfs(head []byte) (Identity, error) {
	const (
		uuidAt  = 65568
		magicAt = 65600
	)
	if len(head) < magicAt+8 || !bytes.Equal(head[magicAt:magicAt+8], []byte("_BHRfS_M")) {
		return Identity{}, fmt.Errorf("%w: btrfs", ErrNoFilesystem)
	}
	return Identity{ID: formatUUID(head[uuidAt : uuidAt+16])}, nil
}

func readNTFS(head []byte) (Identity, error) {
	const (
		magicAt  = 3
		serialAt = 72
	)
	if len(head) < serialAt+8 || !bytes.Equal(head[magicAt:magicAt+8], []byte("NTFS    ")) {
		return Identity{}, fmt.Errorf("%w: ntfs", ErrNoFilesystem)
	}
	return Identity{ID: fmt.Sprintf("%016X", binary.LittleEndian.Uint64(head[serialAt:]))}, nil
}

func formatUUID(raw []byte) string {
	text := hex.EncodeToString(raw)
	return strings.Join([]string{text[:8], text[8:12], text[12:16], text[16:20], text[20:]}, "-")
}
