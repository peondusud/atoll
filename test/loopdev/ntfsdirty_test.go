//go:build loopdev

package loopdev

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// TestNTFSDirtyOnARealVolume: the reading of $Volume holds on a volume mkntfs made: clean once made, and still clean
// while ntfs3 has it mounted for writing, as ntfs3 does not mark it so. Marked dirty as Windows marks it, it reads
// dirty and ntfs3 refuses to mount it; ntfsfix -d, which clears the flag, makes it clean, and it mounts again
// (specs.md §22).
func TestNTFSDirtyOnARealVolume(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.NTFS)
	read := func(off int64, n int) ([]byte, error) { return fsx.ReadAt(v.device, off, n) }
	dirtyNow := func(when string) bool {
		t.Helper()
		must(t, "blockdev", "--flushbufs", v.device)
		dirty, err := disks.NTFSDirty(read)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		return dirty
	}
	if dirtyNow("made") {
		t.Error("dirty as mkntfs made it")
	}
	driver := ntfs3Type(t)
	at := mountPoint(t)
	mountVolume(t, v, driver, at, ntfs3Options(t)...)
	if dirtyNow("mounted") {
		t.Error("dirty while ntfs3 has it mounted: then a clean unplugging would read dirty")
	}
	must(t, "umount", at)
	markDirty(t, v.device)
	if !dirtyNow("marked") {
		t.Fatal("clean once marked dirty")
	}
	if err := attempt(t, "mount", "-t", driver, v.device, at); err == nil {
		_ = attempt(t, "umount", at)
		t.Error("ntfs3 mounted a volume marked dirty")
	}
	must(t, "ntfsfix", "-d", v.device)
	if dirtyNow("fixed") {
		t.Error("dirty once ntfsfix -d cleared it")
	}
	mountVolume(t, v, driver, at, ntfs3Options(t)...)
	must(t, "umount", at)
}

// markDirty sets VOLUME_IS_DIRTY in $Volume as Windows does, writing its record back with its update sequence.
func markDirty(t *testing.T, device string) {
	t.Helper()
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	mustDo(t, err)
	defer func() { _ = f.Close() }()
	boot := make([]byte, 512)
	_, err = f.ReadAt(boot, 0)
	mustDo(t, err)
	sector := int64(binary.LittleEndian.Uint16(boot[0x0B:]))
	cluster := sector * int64(boot[0x0D])
	record := int64(1) << -int8(boot[0x40])
	off := int64(binary.LittleEndian.Uint64(boot[0x30:]))*cluster + 3*record
	rec := make([]byte, record)
	_, err = f.ReadAt(rec, off)
	mustDo(t, err)
	usa := int(binary.LittleEndian.Uint16(rec[4:]))
	blocks := len(rec) / 512
	for i := range blocks {
		end := (i+1)*512 - 2
		copy(rec[end:end+2], rec[usa+2+2*i:usa+4+2*i])
	}
	at := int(binary.LittleEndian.Uint16(rec[0x14:]))
	for binary.LittleEndian.Uint32(rec[at:]) != 0x70 {
		at += int(binary.LittleEndian.Uint32(rec[at+4:]))
	}
	flags := at + int(binary.LittleEndian.Uint16(rec[at+0x14:])) + 0x0A
	binary.LittleEndian.PutUint16(rec[flags:], binary.LittleEndian.Uint16(rec[flags:])|1)
	for i := range blocks {
		end := (i+1)*512 - 2
		copy(rec[usa+2+2*i:usa+4+2*i], rec[end:end+2])
		copy(rec[end:end+2], rec[usa:usa+2])
	}
	_, err = f.WriteAt(rec, off)
	mustDo(t, err)
	mustDo(t, f.Sync())
}
