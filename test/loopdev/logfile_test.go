//go:build loopdev

package loopdev

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
)

// The journals the tests below write to a volume's $LogFile, as its two restart pages of a log page of 4 KiB, as
// Windows writes them, and mkntfs's all 0xFF. Windows was not run: these are restart pages that say what ntfs-3g reads
// (libntfs-3g/logfile.c), with no log record after them.
var logFiles = []struct {
	name string
	// major and minor are the version of both pages. open says that a client has the log in use and the volume is not
	// marked clean, as ntfs-3g's "unclean file system (0, 0)" says of a volume Windows had mounted when Fast Startup
	// ran; otherwise RESTART_VOLUME_IS_CLEAN is set. none leaves no client in use, which ntfs3 alone reads as clean.
	// broken gives both pages an update sequence array of the wrong count, which no driver reads.
	major, minor int16
	open, none   bool
	broken       bool
	empty        bool
}{
	{name: "empty", empty: true},
	{name: "clean 1.1", major: 1, minor: 1},
	{name: "clean 1.1 none in use", major: 1, minor: 1, none: true},
	{name: "unclean 1.1", major: 1, minor: 1, open: true},
	{name: "clean 2.0", major: 2, minor: 0},
	{name: "unclean 2.0", major: 2, minor: 0, open: true},
	{name: "invalid", major: 1, minor: 1, open: true, broken: true},
}

// restartPage is a restart page of 4 KiB, its update sequence applied as a write applies it, with its restart area
// and one client, NTFS.
func restartPage(major, minor int16, open, none, broken bool, lsn uint64, fileSize int64) []byte {
	const system = 4096
	p := make([]byte, system)
	le16 := binary.LittleEndian.PutUint16
	le32 := binary.LittleEndian.PutUint32
	le64 := binary.LittleEndian.PutUint64
	copy(p, "RSTR")
	le16(p[4:], 0x1e)
	le16(p[6:], 1+system/512)
	if broken {
		le16(p[6:], 2+system/512)
	}
	le32(p[16:], system)
	le32(p[20:], 4096)
	le16(p[24:], 0x30)
	le16(p[26:], uint16(minor))
	le16(p[28:], uint16(major))
	ra := p[0x30:]
	le64(ra[0:], lsn)
	le16(ra[8:], 1)
	le16(ra[10:], 0xffff)
	le16(ra[12:], 0)
	switch {
	case none:
		le16(ra[10:], 0)
		le16(ra[12:], 0xffff)
	case !open:
		le16(ra[14:], 0x0002)
	}
	le32(ra[16:], uint32(67-bits.Len64(uint64(fileSize))))
	le16(ra[20:], 0x30+0xa0)
	le16(ra[22:], 0x30)
	le64(ra[24:], uint64(fileSize))
	le16(ra[36:], 0x30)
	le16(ra[38:], 0x40)
	le32(ra[40:], 1)
	cr := ra[0x30:]
	le64(cr[0:], lsn)
	le64(cr[8:], lsn)
	le16(cr[16:], 0xffff)
	le16(cr[18:], 0xffff)
	le32(cr[28:], 8)
	for i, u := range utf16.Encode([]rune("NTFS")) {
		le16(cr[32+2*i:], u)
	}
	// The update sequence: the last two bytes of each 512 go to the array, and its number takes their place.
	usa := 0x1e
	le16(p[usa:], 1)
	for i := range system / 512 {
		end := (i+1)*512 - 2
		copy(p[usa+2+2*i:usa+4+2*i], p[end:end+2])
		le16(p[end:], 1)
	}
	return p
}

// logFileOf finds where $LogFile's data lies on an NTFS device: the first run of the $DATA of the third record of its
// MFT, and the size of that data.
func logFileOf(t *testing.T, device string) (start, length, size int64) {
	t.Helper()
	f, err := os.Open(device)
	mustDo(t, err)
	defer func() { _ = f.Close() }()
	boot := make([]byte, 512)
	_, err = f.ReadAt(boot, 0)
	mustDo(t, err)
	sector := int64(binary.LittleEndian.Uint16(boot[0x0B:]))
	cluster := sector * int64(boot[0x0D])
	record := int64(1) << -int8(boot[0x40])
	rec := make([]byte, record)
	_, err = f.ReadAt(rec, int64(binary.LittleEndian.Uint64(boot[0x30:]))*cluster+2*record)
	mustDo(t, err)
	usa := int(binary.LittleEndian.Uint16(rec[4:]))
	for i := range len(rec) / 512 {
		end := (i+1)*512 - 2
		copy(rec[end:end+2], rec[usa+2+2*i:usa+4+2*i])
	}
	for at := int(binary.LittleEndian.Uint16(rec[0x14:])); at+8 <= len(rec); {
		kind, n := binary.LittleEndian.Uint32(rec[at:]), int(binary.LittleEndian.Uint32(rec[at+4:]))
		if kind == 0xffffffff || n <= 0 {
			break
		}
		if kind == 0x80 && rec[at+9] == 0 && rec[at+8] == 1 {
			size = int64(binary.LittleEndian.Uint64(rec[at+0x30:]))
			pairs := rec[at+int(binary.LittleEndian.Uint16(rec[at+0x20:])):]
			lengthBytes, offsetBytes := int(pairs[0]&0xf), int(pairs[0]>>4)
			var runLength uint64
			for i := range lengthBytes {
				runLength |= uint64(pairs[1+i]) << (8 * i)
			}
			var lcn int64
			for i := range offsetBytes {
				lcn |= int64(pairs[1+lengthBytes+i]) << (8 * i)
			}
			if offsetBytes > 0 && pairs[lengthBytes+offsetBytes]&0x80 != 0 {
				lcn -= int64(1) << (8 * offsetBytes)
			}
			return lcn * cluster, int64(runLength) * cluster, size
		}
		at += n
	}
	t.Fatalf("%s: no $DATA of $LogFile", device)
	return 0, 0, 0
}

// writeLogFile writes the start of $LogFile on an NTFS device that nothing mounts, past every driver: its two restart
// pages, the second the newer, or 0xFF over them for an empty one. It gives the first 8 KiB it wrote.
func writeLogFile(t *testing.T, device string, major, minor int16, open, none, broken, empty bool) []byte {
	t.Helper()
	start, length, size := logFileOf(t, device)
	if length < disks.LogFileHead {
		t.Fatalf("%s: $LogFile's first run holds %d bytes", device, length)
	}
	head := bytes.Repeat([]byte{0xff}, 8192)
	if !empty {
		copy(head, restartPage(major, minor, open, none, broken, 0x10000, size))
		copy(head[4096:], restartPage(major, minor, open, none, broken, 0x10020, size))
	}
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	mustDo(t, err)
	defer func() { _ = f.Close() }()
	_, err = f.WriteAt(head, start)
	mustDo(t, err)
	mustDo(t, f.Sync())
	must(t, "blockdev", "--flushbufs", device)
	return head
}

// logFileOnDevice reads the first bytes of $LogFile from the device, past any driver, as many as a check reads, and
// gives them with the file's size.
func logFileOnDevice(t *testing.T, device string) (int64, []byte) {
	t.Helper()
	must(t, "blockdev", "--flushbufs", device)
	start, _, size := logFileOf(t, device)
	f, err := os.Open(device)
	mustDo(t, err)
	defer func() { _ = f.Close() }()
	head := make([]byte, disks.LogFileHead)
	_, err = f.ReadAt(head, start)
	mustDo(t, err)
	return size, head
}

// whatBecameOf says what a driver left of a journal: as it was written, emptied, or else.
func whatBecameOf(was, now []byte) string {
	now = now[:len(was)]
	switch {
	case bytes.Equal(was, now):
		return "left as it was"
	case bytes.Equal(now, bytes.Repeat([]byte{0xff}, len(now))):
		return "emptied (all 0xFF)"
	default:
		return fmt.Sprintf("rewritten, starting %x at 0 and %x at 4096", now[:8], now[4096:4104])
	}
}

// mountLogged mounts a volume as remount does, and gives what the driver said, which tells why it refused, or why it
// mounted read-only: its output, and what the kernel logged of NTFS meanwhile.
func mountLogged(t *testing.T, v *volume, driver, at string, options []string) (string, error) {
	t.Helper()
	var command []string
	switch driver {
	case "ntfs-3g", "lowntfs-3g":
		command = []string{driver}
	case "ntfs":
		command = []string{"mount", "-i", "-t", driver}
	default:
		command = []string{"mount", "-t", driver}
	}
	if len(options) > 0 {
		command = append(command, "-o", strings.Join(options, ","))
	}
	command = append(command, v.device, at)
	_ = exec.CommandContext(t.Context(), "dmesg", "-C").Run()
	out, err := exec.CommandContext(t.Context(), command[0], command[1:]...).CombinedOutput()
	kernel, _ := exec.CommandContext(t.Context(), "dmesg").CombinedOutput()
	said := strings.Join(strings.Fields(string(out)), " ")
	for line := range strings.Lines(string(kernel)) {
		if strings.Contains(strings.ToLower(line), "ntfs") {
			said += " | " + strings.TrimSpace(line)
		}
	}
	if err != nil {
		return said, fmt.Errorf("%v: %w", command, err)
	}
	return said, nil
}

// readOnlyMount says whether the mount table shows a mount read-only, as the check reads it.
func readOnlyMount(e *disks.MountEntry) bool {
	return slices.Contains(e.MountOptions, "ro") || slices.Contains(e.SuperOptions, "ro")
}

// quiet waits until a device has no request under way and its count of reads holds still for 200 ms.
func quiet(t *testing.T, device string) {
	t.Helper()
	inflight := filepath.Join("/sys/block", filepath.Base(device), "inflight")
	last := readsOf(t, device)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		data, err := os.ReadFile(inflight)
		mustDo(t, err)
		now := readsOf(t, device)
		if now == last && slices.Equal(strings.Fields(string(data)), []string{"0", "0"}) {
			return
		}
		last = now
	}
	t.Fatalf("%s was not quiet within 10 s", device)
}

// TestALogFileIsReadOncePerMount: the daemon's machine reads $LogFile once for each mount, as hiberfil.sys, so that its
// check every 30 s reads nothing of it again; a machine of its own reads it at once; mounted anew, read-only once its
// journal is unclean, it is read again (specs.md §10).
func TestALogFileIsReadOncePerMount(t *testing.T) {
	needRoot(t)
	driver, options := ntfs3Type(t), ntfs3Options(t)
	v := hibernatedVolume(t, nil)
	at := mountPoint(t)
	mustDo(t, os.MkdirAll(at, 0o755))
	mustDo(t, remount(t, v, driver, at, options))
	letGo(t, at)
	d := diskOf(v, at)
	daemon, err := disks.NewMachine()
	mustDo(t, err)
	want(t, disks.Check(daemon, d), disks.Online, "")

	// The read of its first 128 KiB set the kernel's readahead going past them, which may end after the check: what is
	// counted is what the next check reads once the device is quiet.
	logFile := filepath.Join(at, disks.LogFileName)
	quiet(t, v.device)
	dropPages(t, logFile)
	reads := readsOf(t, v.device)
	want(t, disks.Check(daemon, d), disks.Online, "")
	if got := readsOf(t, v.device); got != reads {
		t.Errorf("a check of the same mount read the device: %d reads, then %d", reads, got)
	}
	other, err := disks.NewMachine()
	mustDo(t, err)
	want(t, disks.Check(other, d), disks.Online, "")
	if got := readsOf(t, v.device); got == reads {
		t.Log("a new machine read $LogFile from the page cache: the measure above may be blind")
	}

	must(t, "umount", at)
	writeLogFile(t, v.device, 2, 0, true, false, false, false)
	mustDo(t, remount(t, v, driver, at, append([]string{"ro"}, options...)))
	want(t, disks.Check(daemon, d), disks.Readonly, disks.Hibernated)
}

// TestAnUncleanLogFileOnEachDriver: what each NTFS driver does with a volume whose $LogFile Windows left unclean, at a
// mount read-write and, refused, read-only, and what the check makes of it (specs.md §10, §22). Each driver answers
// $LogFile by its name, as a regular file whose bytes are those of the device; the check reads it without moving its
// access time and writes nothing to it, and gives hibernated for a journal it reads unclean, of version 2.0 or invalid,
// whatever the mount, after what a driver's options say. What each driver did with the journal is logged.
func TestAnUncleanLogFileOnEachDriver(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("ntfs-3g"); err != nil {
		t.Skip("no ntfs-3g to make the volume")
	}
	for _, driver := range []struct {
		name, driver string
		options      func(t *testing.T) []string
		// first is the reason a driver's options give before hibernated, where they give one.
		first disks.Reason
	}{
		{"ntfs3", "ntfs3", ntfs3Options, ""},
		{"ntfs", "ntfs", func(*testing.T) []string { return ntfsOptions }, ""},
		{"ntfs errors=continue", "ntfs", func(*testing.T) []string { return []string{"iocharset=utf8"} }, disks.ErrorsContinue},
		{"ntfs-3g", "ntfs-3g", func(*testing.T) []string { return nil }, ""},
		{"lowntfs-3g", "lowntfs-3g", func(*testing.T) []string { return nil }, ""},
		// recover, their default, empties a journal left in use as they mount it read-write; norecover does not.
		{"ntfs-3g norecover", "ntfs-3g", func(*testing.T) []string { return []string{"norecover"} }, ""},
		{"lowntfs-3g norecover", "lowntfs-3g", func(*testing.T) []string { return []string{"norecover"} }, ""},
	} {
		for _, log := range logFiles {
			t.Run(driver.name+" "+log.name, func(t *testing.T) {
				needDriver(t, driver.driver)
				v := hibernatedVolume(t, nil)
				was := writeLogFile(t, v.device, log.major, log.minor, log.open, log.none, log.broken, log.empty)
				at := mountPoint(t)
				mustDo(t, os.MkdirAll(at, 0o755))
				options := driver.options(t)
				said, err := mountLogged(t, v, driver.driver, at, options)
				how := "read-write"
				if err != nil {
					_, after := logFileOnDevice(t, v.device)
					t.Logf("refused read-write: %v: %s; the journal after: %s", err, said, whatBecameOf(was, after))
					how = "read-only, once refused read-write"
					if said, err = mountLogged(t, v, driver.driver, at, append([]string{"ro"}, options...)); err != nil {
						t.Fatalf("refused read-only too: %v: %s", err, said)
					}
				}
				letGo(t, at)
				m, err := disks.NewMachine()
				mustDo(t, err)
				entry := m.Mount(at).Entry
				if entry == nil {
					t.Fatalf("%s is not mounted", at)
				}
				size, onDevice := logFileOnDevice(t, v.device)
				journal, note := disks.JournalOf(size, onDevice)
				t.Logf("mounted %s as %s: %s; the journal once mounted: %s, which reads %s %s", how, mountedAs(t, m, at), said,
					whatBecameOf(was, onDevice), journal, note)

				// What the mount shows at $LogFile is the device's journal.
				top, err := fsx.OpenDir(at)
				mustDo(t, err)
				seen := disks.ReadLogFile(top)
				mustDo(t, top.Close())
				if seen.File != disks.LogFileFile || seen.Journal != journal {
					t.Errorf("through the mount: %+v, on the device: %s %s", seen, journal, note)
				}
				logFile := filepath.Join(at, disks.LogFileName)
				var before unix.Stat_t
				mustDo(t, unix.Lstat(logFile, &before))

				state, why := disks.Online, disks.Reason("")
				switch {
				case driver.first != "":
					state, why = disks.Readonly, driver.first
				case journal != disks.JournalEmpty && journal != disks.JournalClean:
					state, why = disks.Readonly, disks.Hibernated
				case readOnlyMount(entry):
					state, why = disks.Readonly, disks.ReadOnlyMount
				}
				got := disks.Check(m, diskOf(v, at))
				want(t, got, state, why)
				t.Logf("the check: %s %q (%s)", got.State, got.Reason, got.Message)

				var after unix.Stat_t
				mustDo(t, unix.Lstat(logFile, &after))
				if after.Atim != before.Atim {
					t.Errorf("the access time of $LogFile moved from %v to %v", before.Atim, after.Atim)
				}
				if _, again := logFileOnDevice(t, v.device); !bytes.Equal(again, onDevice) {
					t.Errorf("the journal changed during the check: %s", whatBecameOf(onDevice[:8192], again))
				}
				unmountFor(t, driver.driver, at)
				_, last := logFileOnDevice(t, v.device)
				t.Logf("the journal once let go: %s", whatBecameOf(was, last))
			})
		}
	}
}
