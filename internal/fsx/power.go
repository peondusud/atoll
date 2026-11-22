package fsx

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sgIOHdr is struct sg_io_hdr of <scsi/sg.h>, as 64-bit Linux lays it out: Go aligns its fields as C does there.
type sgIOHdr struct {
	interfaceID    int32
	dxferDirection int32
	cmdLen         uint8
	mxSbLen        uint8
	iovecCount     uint16
	dxferLen       uint32
	dxferp         unsafe.Pointer
	cmdp           *byte
	sbp            *byte
	timeout        uint32
	flags          uint32
	packID         int32
	usrPtr         unsafe.Pointer
	status         uint8
	maskedStatus   uint8
	msgStatus      uint8
	sbLenWr        uint8
	hostStatus     uint16
	driverStatus   uint16
	resid          int32
	duration       uint32
	info           uint32
}

// SG_IO and what it is told: no data moves.
const (
	sgIO        = 0x2285
	sgDxferNone = -1
)

// checkPowerModeCDB is ATA PASS-THROUGH (16) of CHECK POWER MODE (E5h): the non-data protocol, with CK_COND set, so
// that the disk's answer, its count, comes back in the sense data (SAT).
var checkPowerModeCDB = [16]byte{0x85, 3 << 1, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xe5, 0}

// CheckPowerMode asks a whole disk its power mode, ATA CHECK POWER MODE through SCSI ATA pass-through, and gives the
// sense data of the answer, which disks.PowerModeOf reads (specs.md §10). The command wakes no disk. A device that is no
// SCSI disk, or a USB bridge with no pass-through, gives an error or sense data that says nothing. It takes
// CAP_SYS_RAWIO, as smartctl does.
func CheckPowerMode(device string, timeout time.Duration) ([]byte, error) {
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", device, err)
	}
	defer func() { _ = unix.Close(fd) }()
	cdb := new([16]byte)
	*cdb = checkPowerModeCDB
	sense := new([32]byte)
	hdr := &sgIOHdr{
		interfaceID: 'S', dxferDirection: sgDxferNone, cmdLen: uint8(len(cdb)), mxSbLen: uint8(len(sense)),
		cmdp: &cdb[0], sbp: &sense[0], timeout: uint32(timeout.Milliseconds()),
	}
	// SG_IO takes its header by address, which only unsafe gives, as x/sys passes its own: the buffers the header points
	// to are kept alive over the call, and the length read back is bounded by the buffer's.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), sgIO, uintptr(unsafe.Pointer(hdr))) // nosemgrep: go.lang.security.audit.unsafe.use-of-unsafe-block
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return nil, fmt.Errorf("%s: SG_IO: %w", device, errno)
	}
	return sense[:min(int(hdr.sbLenWr), len(sense))], nil
}

// sysBlockOf is where Linux describes a whole disk by its kernel name.
func sysBlockOf(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '/') || name == "." || name == ".." {
		return "", fmt.Errorf("%q is no disk name", name)
	}
	return filepath.Join("/sys/block", name), nil
}

// Rotational says whether a whole disk has a spindle, as /sys/block/<name>/queue/rotational says.
func Rotational(name string) (bool, error) {
	dir, err := sysBlockOf(name)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "queue", "rotational"))
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	switch strings.TrimSpace(string(data)) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("%s: rotational is %q", name, data)
}

// ReadBlockStat gives the content of /sys/block/<name>/stat, a whole disk's I/O counters.
func ReadBlockStat(name string) (string, error) {
	dir, err := sysBlockOf(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(data), nil
}
