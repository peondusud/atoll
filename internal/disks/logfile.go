package disks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"slices"

	"github.com/peondusud/atoll/internal/fsx"
)

// LogFileName is NTFS's journal, a system file at the top of every volume, which each driver answers by that name
// (specs.md §10, §22).
const LogFileName = "$LogFile"

// LogFileHead is how much of its start a check reads: its two restart pages, of a system page of 64 KiB at most.
const LogFileHead = 128 << 10

// Journal is what $LogFile says of whether Windows left its volume in use (conformance/vectors/logfile.json).
type Journal string

// What a journal says.
const (
	// JournalEmpty: 0xFF wherever a restart page may be, as mkntfs makes it and every driver of Linux leaves it once it
	// mounted the volume read-write.
	JournalEmpty Journal = "empty"
	// JournalClean: its restart page has no client in use, or says the volume is clean, as Windows leaves it once it shut
	// down fully.
	JournalClean Journal = "clean"
	// JournalUnclean: a client has it in use and the volume is not marked clean, as Windows leaves a volume it had
	// mounted when Fast Startup ran, when it hibernated or when it stopped short.
	JournalUnclean Journal = "unclean"
	// JournalVersion2: clean, but of version 2.0, which Windows 8 and later leave on a volume they did not dismount, and
	// which ntfs-3g refuses to mount read-write, as Windows may keep its metadata cached.
	JournalVersion2 Journal = "version-2"
	// JournalInvalid: nothing proves it clean: no restart page that holds, the newer torn, a file too small, or a read
	// cut short.
	JournalInvalid Journal = "invalid"
)

// The layout of $LogFile's restart pages, as ntfs-3g's include/ntfs-3g/logfile.h gives it.
const (
	logBlock          = 512        // NTFS_BLOCK_SIZE, the sector an update sequence protects
	logPageSize       = 4096       // DefaultLogPageSize
	logMinRecordPages = 48         // MinLogRecordPages
	logMaxSize        = 1 << 32    // MaxLogFileSize
	logHeaderEnd      = 30         // the restart page header, up to its update sequence number
	logAreaSize       = 48         // a restart area
	logClientSize     = 160        // a client record
	logNoClient       = 0xffff     // LOGFILE_NO_CLIENT
	logVolumeIsClean  = 0x0002     // RESTART_VOLUME_IS_CLEAN
	logEmptyMagic     = 0xffffffff // a block never written
)

// errPastRead says that a restart page runs past what a read gave.
var errPastRead = errors.New("past what was read")

// restartPage is a restart page that holds: its LSN, its system page with its update sequence applied, and where its
// restart area lies in it.
type restartPage struct {
	lsn          int64
	page         []byte
	area         int
	major, minor int16
	torn         bool
}

// JournalOf says what $LogFile says, from the first bytes a read gave and the file's size, by the rules of ntfs-3g
// (libntfs-3g/logfile.c: ntfs_check_logfile and ntfs_is_logfile_clean; libntfs-3g/volume.c: the version 2.0 of
// ntfs_volume_check_logfile). Restart pages are looked for at 0, then at each power of two from 512, until a log record
// page, an empty block after one that is not, a second restart page, or the end of what was read; each is checked as
// ntfs-3g checks it, and of two the newer by LSN counts. Where it reads less than ntfs-3g, it says less: a restart page
// past what was read makes it invalid. The note says why, for a journal that is not empty or clean.
func JournalOf(size int64, head []byte) (Journal, string) {
	rounded := min(size, logMaxSize) &^ (logPageSize - 1)
	if rounded < 2*logPageSize || (rounded-2*logPageSize)/logPageSize < logMinRecordPages {
		return JournalInvalid, fmt.Sprintf("it holds %d bytes, too few for two restart pages and %d log pages", size, logMinRecordPages)
	}
	if want := min(rounded, LogFileHead); int64(len(head)) < want {
		return JournalInvalid, fmt.Sprintf("a read of its first %d bytes gave %d", want, len(head))
	}
	empty := true
	var first, second *restartPage
	refused := ""
scan:
	for pos := int64(0); pos < rounded && pos+logBlock <= int64(len(head)); pos = nextRestartPosition(pos) {
		magic := binary.LittleEndian.Uint32(head[pos:])
		switch {
		case magic != logEmptyMagic:
			empty = false
		case !empty:
			break scan
		}
		kind := string(head[pos : pos+4])
		if kind == "RCRD" {
			break
		}
		if kind != "RSTR" && kind != "CHKD" {
			continue
		}
		p, err := loadRestartPage(head, pos, size)
		switch {
		case errors.Is(err, errPastRead):
			return JournalInvalid, err.Error()
		case err != nil:
			refused = fmt.Sprintf("%v at %d", err, pos)
			continue
		case first == nil:
			first = p
		default:
			second = p
		}
		if pos != 0 {
			break
		}
	}
	switch {
	case empty:
		return JournalEmpty, ""
	case first == nil && refused != "":
		return JournalInvalid, "no restart page holds, the last one refused for " + refused
	case first == nil:
		return JournalInvalid, "no restart page, though it is not empty"
	}
	p := first
	if second != nil && second.lsn > first.lsn {
		p = second
	}
	inUse, flags := le16(p.page, p.area+12), le16(p.page, p.area+14)
	switch {
	case p.torn:
		return JournalInvalid, "its newer restart page is torn, as a write that did not finish leaves it"
	case inUse != logNoClient && flags&logVolumeIsClean == 0:
		return JournalUnclean, fmt.Sprintf("its restart area has client %d in use, and the volume is not marked clean", inUse)
	case p.major == 2 && p.minor == 0:
		return JournalVersion2, "its restart page is of version 2.0"
	default:
		return JournalClean, ""
	}
}

// le16 reads the little-endian 16-bit value at at.
func le16(b []byte, at int) int {
	return int(binary.LittleEndian.Uint16(b[at:]))
}

// nextRestartPosition is where a restart page may be after pos: 512 after 0, then each power of two.
func nextRestartPosition(pos int64) int64 {
	if pos == 0 {
		return logBlock
	}
	return pos << 1
}

// loadRestartPage checks the restart page at pos as ntfs-3g's ntfs_check_and_load_restart_page does, and gives it with
// its update sequence applied. size is the file's.
func loadRestartPage(head []byte, pos, size int64) (*restartPage, error) {
	rp := head[pos : pos+logBlock]
	system, logPage := int64(binary.LittleEndian.Uint32(rp[16:])), int64(binary.LittleEndian.Uint32(rp[20:]))
	if system < logBlock || logPage < logBlock || bits.OnesCount64(uint64(system)) != 1 || bits.OnesCount64(uint64(logPage)) != 1 {
		return nil, fmt.Errorf("a system page of %d bytes and a log page of %d", system, logPage)
	}
	if pos != 0 && pos != system {
		return nil, fmt.Errorf("a restart page of a system page of %d bytes, in no place one may be", system)
	}
	major, minor := int16(binary.LittleEndian.Uint16(rp[28:])), int16(binary.LittleEndian.Uint16(rp[26:]))
	if (major != 1 || minor != 1) && (major != 2 || minor != 0) {
		return nil, fmt.Errorf("version %d.%d", major, minor)
	}
	chkd := string(rp[:4]) == "CHKD"
	usaAt, usaCount := le16(rp, 4), le16(rp, 6)
	hasUSA := !chkd || usaCount != 0
	areaMin := logHeaderEnd
	if hasUSA {
		end := usaAt + 2*usaCount
		if int64(usaCount) != 1+system/logBlock || usaAt < logHeaderEnd || end > logBlock-2 {
			return nil, fmt.Errorf("an update sequence of %d at %d", usaCount, usaAt)
		}
		areaMin = end
	}
	at := le16(rp, 24)
	if at&7 != 0 || at < areaMin || int64(at) > system {
		return nil, fmt.Errorf("a restart area at %d", at)
	}
	if !chkd && binary.LittleEndian.Uint64(rp[8:]) != 0 {
		return nil, errors.New("a chkdsk LSN on a page chkdsk did not mark")
	}
	if err := checkRestartArea(rp, at, system); err != nil {
		return nil, err
	}
	if system > size {
		return nil, fmt.Errorf("a system page of %d bytes, past the file", system)
	}
	if pos+system > int64(len(head)) {
		return nil, fmt.Errorf("a restart page of %d bytes at %d: %w", system, pos, errPastRead)
	}
	page := slices.Clone(head[pos : pos+system])
	area := rp[at:]
	torn := hasUSA && !applyUpdateSequence(page, usaAt, usaCount)
	if torn && at+le16(area, 20) > logBlock-2 {
		return nil, errors.New("torn past its first sector, where its restart area runs")
	}
	if !chkd && le16(area, 12) != logNoClient {
		if err := checkClients(page, at, system); err != nil {
			return nil, err
		}
	}
	lsn := int64(binary.LittleEndian.Uint64(area[0:]))
	if chkd {
		lsn = int64(binary.LittleEndian.Uint64(rp[8:]))
	}
	return &restartPage{lsn: lsn, page: page, area: at, major: major, minor: minor, torn: torn}, nil
}

// checkRestartArea checks a restart area as ntfs-3g's ntfs_check_restart_area does: whatever it holds before its
// clients lies in the first sector, which no update sequence number changes, and it fits its page. Lengths are counted
// without wrapping, where ntfs-3g counts one in 16 bits.
func checkRestartArea(rp []byte, at int, system int64) error {
	if at+logAreaSize > logBlock-2 {
		return fmt.Errorf("a restart area at %d, past its first sector", at)
	}
	area := rp[at:]
	clientsAt := le16(area, 22)
	if clientsAt&7 != 0 || at+clientsAt > logBlock-2 {
		return fmt.Errorf("a client array at %d", clientsAt)
	}
	clients, length := le16(area, 8), le16(area, 20)
	need := clientsAt + clients*logClientSize
	if int64(at+need) > system || int64(at+length) > system || need > length {
		return fmt.Errorf("a restart area of %d bytes, for %d clients", length, clients)
	}
	free, inUse := le16(area, 10), le16(area, 12)
	if (free != logNoClient && free >= clients) || (inUse != logNoClient && inUse >= clients) {
		return fmt.Errorf("client lists at %d and %d, of %d clients", free, inUse, clients)
	}
	fileSize := binary.LittleEndian.Uint64(area[24:])
	if want := 67 - bits.Len64(fileSize); int(binary.LittleEndian.Uint32(area[16:])) != want {
		return fmt.Errorf("%d sequence number bits, for a file of %d bytes", binary.LittleEndian.Uint32(area[16:]), fileSize)
	}
	if le16(area, 36)&7 != 0 || le16(area, 38)&7 != 0 {
		return errors.New("a log record header or a log page data offset that is no multiple of 8")
	}
	return nil
}

// applyUpdateSequence undoes what a write of a multi-sector record does: the last two bytes of each sector must be the
// update number, and take back what the array kept of them. A sector that ends otherwise was torn, and then nothing is
// changed, as ntfs-3g's ntfs_mst_post_read_fixup changes nothing. An array at an odd offset is taken for torn too.
func applyUpdateSequence(page []byte, at, count int) bool {
	sectors := len(page) / logBlock
	if at&1 != 0 || count-1 != sectors || at+2*count > len(page) {
		return false
	}
	number := [2]byte(page[at : at+2])
	for i := range sectors {
		if end := (i+1)*logBlock - 2; [2]byte(page[end:end+2]) != number {
			return false
		}
	}
	for i := range sectors {
		end := (i+1)*logBlock - 2
		copy(page[end:end+2], page[at+2+2*i:at+4+2*i])
	}
	return true
}

// checkClients checks the client array of a restart page that has a client in use, as ntfs-3g's
// ntfs_check_log_client_array does: the free list, then the list in use, each client within the array and the page,
// the first of a list with none before it, and no more of them in all than the array holds, so that a loop ends.
func checkClients(page []byte, at int, system int64) error {
	area := page[at:]
	base := at + le16(area, 22)
	if int64(at+logAreaSize) > system || int64(base) >= system {
		return errors.New("a client array past its page")
	}
	clients := le16(area, 8)
	left := clients
	for _, idx := range []int{le16(area, 10), le16(area, 12)} {
		for first := true; idx != logNoClient; first = false {
			client := base + idx*logClientSize
			if left == 0 || idx >= clients || int64(client+logClientSize) > system {
				return errors.New("a client array that is corrupt")
			}
			if first && le16(page, client+16) != logNoClient {
				return errors.New("a client array that is corrupt")
			}
			left--
			idx = le16(page, client+18)
		}
	}
	return nil
}

// LogFileAnswer is what the top of an NTFS disk's mount gave at $LogFile.
type LogFileAnswer string

// The answers about $LogFile.
const (
	// LogFileAbsent: no such name.
	LogFileAbsent LogFileAnswer = "absent"
	// LogFileFile: a regular file, whose first bytes were read.
	LogFileFile LogFileAnswer = "file"
	// LogFileNotAFile: a folder, a symbolic link or anything else at that name.
	LogFileNotAFile LogFileAnswer = "not-a-file"
	// LogFileIOError: an error that says the disk cannot be relied on, as EIO (invariant 4).
	LogFileIOError LogFileAnswer = "io-error"
	// LogFileUnreadable: any other error, as a permission refused: nothing can be told.
	LogFileUnreadable LogFileAnswer = "unreadable"
)

// LogFileSeen answers whether Windows left an NTFS volume in use (specs.md §10): what $LogFile at the top of the mount
// is, and for a file, what its journal says. Note says why, for an answer that is no clean reading.
type LogFileSeen struct {
	File    LogFileAnswer
	Journal Journal
	Note    string
}

// settled says whether an answer holds for as long as its mount does: a reading, not an error.
func (s LogFileSeen) settled() bool {
	return s.File == LogFileAbsent || s.File == LogFileFile || s.File == LogFileNotAFile
}

// withError and changedMount are what the cache gives of a failure to tell the mount at a path.
func (LogFileSeen) withError(err error) LogFileSeen { return logFileError(err) }
func (LogFileSeen) changedMount() LogFileSeen {
	return LogFileSeen{File: LogFileUnreadable, Note: mountChanged.Note}
}

// judgeLogFile makes a verdict of what $LogFile gave: online for a journal that is empty or clean. An error that says
// the disk cannot be relied on makes it missing (invariant 4); anything else that is no clean journal leaves whether
// Windows has the volume in use unknown at best, and the disk is written to no more until a read tells.
func judgeLogFile(s LogFileSeen) Verdict {
	const after = ": start Windows and shut it down fully, or turn Fast Startup off, then mount it again"
	switch {
	case s.File == LogFileIOError:
		return missing(IOError, LogFileName+" at its top does not answer: "+s.Note)
	case s.File == LogFileFile && (s.Journal == JournalEmpty || s.Journal == JournalClean):
		return Verdict{State: Online}
	case s.File == LogFileFile && s.Journal == JournalUnclean:
		return readOnly(Hibernated, "Windows left this volume in use, as it does one it had mounted when Fast Startup ran or "+
			"when it hibernated, and what is written to it is lost when Windows resumes: its journal, "+LogFileName+", is unclean ("+
			s.Note+")"+after)
	case s.File == LogFileFile && s.Journal == JournalVersion2:
		return readOnly(Hibernated, "Windows may hold this volume's metadata in its memory, as Fast Startup does: its journal, "+
			LogFileName+", is of version 2.0, which Windows leaves on a volume it did not dismount, and which ntfs-3g refuses to "+
			"mount read-write"+after)
	case s.File == LogFileFile:
		return readOnly(Hibernated, "whether Windows left this volume in use cannot be told: its journal, "+LogFileName+", proves "+
			"nothing ("+s.Note+")"+after)
	case s.File == LogFileAbsent:
		return readOnly(Hibernated, "whether Windows left this volume in use cannot be told: its top does not answer at "+
			LogFileName+", its journal, which every NTFS volume holds")
	case s.File == LogFileNotAFile:
		return readOnly(Hibernated, "whether Windows left this volume in use cannot be told: "+LogFileName+" at its top is not a file")
	default:
		return readOnly(Hibernated, "whether Windows left this volume in use cannot be told, as "+LogFileName+" at its top cannot be "+
			"read: "+s.Note)
	}
}

// ReadLogFile looks at $LogFile through a handle on the top of a disk's mount, and writes nothing: its size and its
// first LogFileHead bytes, read without changing its access time where the process may, and what they say (specs.md
// §10).
func ReadLogFile(top fsx.Dir) LogFileSeen {
	size, head, err := fsx.ReadStart(top, LogFileName, LogFileHead)
	switch {
	case err == nil:
		journal, note := JournalOf(size, head)
		return LogFileSeen{File: LogFileFile, Journal: journal, Note: note}
	case fsx.IsAbsent(err):
		return LogFileSeen{File: LogFileAbsent, Note: err.Error()}
	case fsx.IsUnreadableName(err):
		return LogFileSeen{File: LogFileNotAFile, Note: err.Error()}
	default:
		return logFileError(err)
	}
}

// logFileError is what an error says of $LogFile: the disk unknown, or nothing.
func logFileError(err error) LogFileSeen {
	if fsx.IsDiskLost(err) {
		return LogFileSeen{File: LogFileIOError, Note: err.Error()}
	}
	return LogFileSeen{File: LogFileUnreadable, Note: err.Error()}
}
