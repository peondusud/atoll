package disks

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/vectors"
)

// logFileCase is a case of logfile.json: a $LogFile's size, the first bytes a read gave of it, and what they say.
type logFileCase struct {
	Note   string `json:"note"`
	Size   int64  `json:"size"`
	Length int    `json:"length"`
	Fill   string `json:"fill"`
	Bytes  []struct {
		At    int     `json:"at"`
		Hex   *string `json:"hex"`
		Zeros *int    `json:"zeros"`
	} `json:"bytes"`
	Journal Journal `json:"journal"`
}

// head builds what a read gave: fill, then each run in its order.
func (c *logFileCase) head(t testing.TB) []byte {
	t.Helper()
	fill := byte(0)
	if c.Fill != "" {
		b, err := hex.DecodeString(c.Fill)
		if err != nil || len(b) != 1 {
			t.Fatalf("%s: fill %q", c.Note, c.Fill)
		}
		fill = b[0]
	}
	head := bytes.Repeat([]byte{fill}, c.Length)
	for _, b := range c.Bytes {
		var raw []byte
		switch {
		case b.Hex != nil && b.Zeros == nil:
			var err error
			if raw, err = hex.DecodeString(*b.Hex); err != nil {
				t.Fatalf("%s: %v", c.Note, err)
			}
		case b.Zeros != nil && b.Hex == nil:
			raw = make([]byte, *b.Zeros)
		default:
			t.Fatalf("%s: a run of hex or zeros, not both", c.Note)
		}
		if b.At < 0 || b.At+len(raw) > len(head) {
			t.Fatalf("%s: bytes past what was read: %d and %d", c.Note, b.At, len(raw))
		}
		copy(head[b.At:], raw)
	}
	return head
}

func logFileCases(t testing.TB) []logFileCase {
	t.Helper()
	return vectors.Load[logFileCase](t, "logfile.json")
}

// journalCase finds the first case of logfile.json whose note begins so.
func journalCase(t testing.TB, note string) *logFileCase {
	t.Helper()
	cases := logFileCases(t)
	for i := range cases {
		if strings.HasPrefix(cases[i].Note, note) {
			return &cases[i]
		}
	}
	t.Fatalf("no case %q in logfile.json", note)
	return nil
}

// writeJournal writes a file of a case's size at path, its first bytes the case's, the rest zeros.
func writeJournal(t *testing.T, path, note string) {
	t.Helper()
	c := journalCase(t, note)
	if err := os.WriteFile(path, c.head(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, c.Size); err != nil {
		t.Fatal(err)
	}
}

// TestJournalVectors: what the first bytes of $LogFile say, by ntfs-3g's rules (specs.md §10, §22); a journal that is
// neither empty nor clean says why.
func TestJournalVectors(t *testing.T) {
	for i, c := range logFileCases(t) {
		got, note := JournalOf(c.Size, c.head(t))
		if got != c.Journal {
			t.Errorf("case %d (%s): %s, %q, want %s", i, c.Note, got, note, c.Journal)
		}
		if (got == JournalEmpty || got == JournalClean) != (note == "") {
			t.Errorf("case %d (%s): %s with the note %q", i, c.Note, got, note)
		}
	}
}

// TestJournalVectorsCoverEveryJournal: a journal no vector gives is one no other version has to agree on.
func TestJournalVectorsCoverEveryJournal(t *testing.T) {
	seen := map[Journal]bool{}
	for _, c := range logFileCases(t) {
		seen[c.Journal] = true
	}
	for _, j := range []Journal{JournalEmpty, JournalClean, JournalUnclean, JournalVersion2, JournalInvalid} {
		if !seen[j] {
			t.Errorf("no vector gives %q", j)
		}
		delete(seen, j)
	}
	for j := range seen {
		t.Errorf("a vector gives %q, which is no journal", j)
	}
}

// FuzzJournalOf: any bytes of a $LogFile, of any size, read without a panic and leave them as they were; the same
// bytes say the same; a journal that is neither empty nor clean says why; a read shorter than a check makes, of a file
// that holds more, proves nothing; and bytes of 0xFF are an empty journal. Seeded from conformance/vectors/logfile.json.
func FuzzJournalOf(f *testing.F) {
	for _, c := range logFileCases(f) {
		f.Add(c.Size, c.head(f))
	}
	f.Fuzz(func(t *testing.T, size int64, head []byte) {
		was := slices.Clone(head)
		got, note := JournalOf(size, head)
		if !bytes.Equal(head, was) {
			t.Fatal("the bytes read changed")
		}
		if again, againNote := JournalOf(size, head); again != got || againNote != note {
			t.Fatalf("%s %q, then %s %q", got, note, again, againNote)
		}
		switch got {
		case JournalEmpty, JournalClean:
			if note != "" {
				t.Fatalf("%s with the note %q", got, note)
			}
		case JournalUnclean, JournalVersion2, JournalInvalid:
			if note == "" {
				t.Fatalf("%s with no note", got)
			}
		default:
			t.Fatalf("%q is no journal", got)
		}
		rounded := min(size, logMaxSize) &^ (logPageSize - 1)
		if int64(len(head)) < min(rounded, LogFileHead) && got != JournalInvalid {
			t.Fatalf("%d bytes of a file of %d read %s", len(head), size, got)
		}
		if size >= 1<<20 {
			if got, _ := JournalOf(size, bytes.Repeat([]byte{0xff}, LogFileHead)); got != JournalEmpty {
				t.Fatalf("0xFF of a file of %d bytes reads %s", size, got)
			}
		}
	})
}

// TestJournalReadsNoFurther: what lies past the first bytes a check reads changes nothing, nor does a longer read
// than LogFileHead; a byte of a restart page changes what it says.
func TestJournalReadsNoFurther(t *testing.T) {
	c := journalCase(t, "unclean 1.1")
	head := c.head(t)
	longer := append(slices.Clone(head), bytes.Repeat([]byte{0x52}, LogFileHead)...)
	if got, _ := JournalOf(c.Size, longer); got != JournalUnclean {
		t.Errorf("a longer read: %s", got)
	}
	// The flags of the second restart area, whose LSN is the greater: RESTART_VOLUME_IS_CLEAN set, it is clean.
	clean := slices.Clone(head)
	clean[4096+0x30+14] |= 0x02
	if got, _ := JournalOf(c.Size, clean); got != JournalClean {
		t.Errorf("the newer page marked clean: %s", got)
	}
	// The same on the older page leaves it unclean.
	older := slices.Clone(head)
	older[0x30+14] |= 0x02
	if got, _ := JournalOf(c.Size, older); got != JournalUnclean {
		t.Errorf("the older page marked clean: %s", got)
	}
}

// TestReadLogFile: what the top of a disk holds at $LogFile, read through a handle on it and never followed elsewhere;
// an error that says the disk is unknown is never a file absent (invariant 4), and reading writes nothing, not even the
// file's access time (specs.md §10).
func TestReadLogFile(t *testing.T) {
	top := t.TempDir()
	at := filepath.Join(top, LogFileName)
	open := func(t *testing.T) fsx.Dir {
		t.Helper()
		dir, err := fsx.OpenDir(top)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		return dir
	}
	clear := func(t *testing.T) {
		t.Helper()
		if err := os.RemoveAll(at); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("absent", func(t *testing.T) {
		got := ReadLogFile(open(t))
		if got.File != LogFileAbsent || judgeLogFile(got).Reason != Hibernated {
			t.Errorf("%+v", got)
		}
	})
	for _, tc := range []struct {
		note  string
		want  Journal
		state State
	}{
		{"empty", JournalEmpty, Online},
		{"clean 1.1", JournalClean, Online},
		{"unclean 1.1", JournalUnclean, Readonly},
		{"unclean 2.0", JournalUnclean, Readonly},
		{"clean 2.0", JournalVersion2, Readonly},
		{"both pages with an update sequence of the wrong count", JournalInvalid, Readonly},
	} {
		t.Run(tc.note, func(t *testing.T) {
			t.Cleanup(func() { clear(t) })
			writeJournal(t, at, tc.note)
			got := ReadLogFile(open(t))
			if got.File != LogFileFile || got.Journal != tc.want {
				t.Fatalf("%+v", got)
			}
			if v := judgeLogFile(got); v.State != tc.state || (v.State == Readonly) != (v.Reason == Hibernated) {
				t.Errorf("judged %+v", v)
			}
		})
	}
	t.Run("a file that is not a journal", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := os.WriteFile(at, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadLogFile(open(t)); got.File != LogFileFile || got.Journal != JournalInvalid || got.Note == "" {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a folder", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := os.Mkdir(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ReadLogFile(open(t)); got.File != LogFileNotAFile || judgeLogFile(got).Reason != Hibernated {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a symbolic link to an empty journal", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		empty := filepath.Join(t.TempDir(), "empty")
		writeJournal(t, empty, "empty")
		if err := os.Symlink(empty, at); err != nil {
			t.Fatal(err)
		}
		if got := ReadLogFile(open(t)); got.File != LogFileNotAFile {
			t.Errorf("not followed: %+v", got)
		}
	})
	t.Run("a FIFO, without waiting", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := syscall.Mkfifo(at, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadLogFile(open(t)); got.File != LogFileNotAFile {
			t.Errorf("%+v", got)
		}
	})
	for _, tc := range []struct {
		name string
		err  error
		want LogFileAnswer
	}{
		{"EIO", syscall.EIO, LogFileIOError},
		{"ENOTCONN", syscall.ENOTCONN, LogFileIOError},
		{"ENODEV", syscall.ENODEV, LogFileIOError},
		{"ENXIO", syscall.ENXIO, LogFileIOError},
		{"EACCES", syscall.EACCES, LogFileUnreadable},
		{"EMFILE", syscall.EMFILE, LogFileUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fsx.Fault(open(t), func(call, name string) error {
				if call == "readstart" && name == LogFileName {
					return tc.err
				}
				return nil
			})
			got := ReadLogFile(dir)
			if got.File != tc.want || got.Note == "" {
				t.Errorf("%+v", got)
			}
			if v := judgeLogFile(got); (tc.want == LogFileIOError) != (v.State == Missing) || v.State == Online {
				t.Errorf("judged %+v", v)
			}
		})
	}
	t.Run("nothing written", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		writeJournal(t, at, "clean 1.1")
		// An access time a day and more ago, before the modification: any read would move it under relatime.
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(at, old, old.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		before := accessTime(t, at)
		if got := ReadLogFile(open(t)); got.Journal != JournalClean {
			t.Fatalf("%+v", got)
		}
		if after := accessTime(t, at); !after.Equal(before) {
			t.Errorf("the access time moved from %v to %v", before, after)
		}
	})
}

// TestJudgeLogFile: each answer, and what a check makes of it; an answer no machine gives is no reading, and the disk
// is written to no more.
func TestJudgeLogFile(t *testing.T) {
	for _, tc := range []struct {
		seen   LogFileSeen
		state  State
		reason Reason
	}{
		{LogFileSeen{File: LogFileFile, Journal: JournalEmpty}, Online, ""},
		{LogFileSeen{File: LogFileFile, Journal: JournalClean}, Online, ""},
		{LogFileSeen{File: LogFileFile, Journal: JournalUnclean, Note: "n"}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileFile, Journal: JournalVersion2, Note: "n"}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileFile, Journal: JournalInvalid, Note: "n"}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileFile, Journal: "what"}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileFile}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileAbsent}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileNotAFile}, Readonly, Hibernated},
		{LogFileSeen{File: LogFileIOError, Note: "EIO"}, Missing, IOError},
		{LogFileSeen{File: LogFileUnreadable, Note: "EACCES"}, Readonly, Hibernated},
		{LogFileSeen{}, Readonly, Hibernated},
		{LogFileSeen{File: "what", Journal: JournalClean}, Readonly, Hibernated},
	} {
		got := judgeLogFile(tc.seen)
		if got.State != tc.state || got.Reason != tc.reason || (got.State == Online) != (got.Message == "") {
			t.Errorf("%+v: %+v, want %s %q", tc.seen, got, tc.state, tc.reason)
		}
	}
}

// TestLogFileIsReadOncePerMount: what $LogFile gave is kept for its mount as hiberfil.sys's is, and an error, or a mount
// that changed during the read, is not (specs.md §10).
func TestLogFileIsReadOncePerMount(t *testing.T) {
	var c logFileCache
	unique := uint64(7)
	var failing error
	uniqueOf := func(string) (uint64, bool, error) { return unique, true, failing }
	reads := 0
	onDisk := LogFileSeen{File: LogFileFile, Journal: JournalUnclean, Note: "n"}
	var during func()
	ask := func() LogFileSeen {
		return c.answer("/mnt/t1", &MountEntry{ID: 300, Major: 8, Minor: 17}, uniqueOf, func() LogFileSeen {
			reads++
			if during != nil {
				during()
			}
			return onDisk
		})
	}
	if got := ask(); got.Journal != JournalUnclean || reads != 1 {
		t.Fatalf("%+v after %d reads", got, reads)
	}
	onDisk = LogFileSeen{File: LogFileFile, Journal: JournalClean}
	if got := ask(); got.Journal != JournalUnclean || reads != 1 {
		t.Errorf("the same mount: %+v after %d reads", got, reads)
	}
	unique++
	if got := ask(); got.Journal != JournalClean || reads != 2 {
		t.Errorf("mounted anew: %+v after %d reads", got, reads)
	}
	c.forget("/mnt/t1")
	onDisk = LogFileSeen{File: LogFileIOError, Note: "EIO"}
	if got := ask(); got.File != LogFileIOError || reads != 3 {
		t.Errorf("an error: %+v after %d reads", got, reads)
	}
	onDisk = LogFileSeen{File: LogFileFile, Journal: JournalEmpty}
	if got := ask(); got.Journal != JournalEmpty || reads != 4 {
		t.Errorf("not kept: %+v after %d reads", got, reads)
	}
	c.forget("/mnt/t1")
	during = func() { unique++ }
	if got := ask(); got.File != LogFileUnreadable || got.Note != mountChanged.Note || reads != 5 {
		t.Errorf("mounted anew during the read: %+v after %d reads", got, reads)
	}
	during = nil
	if got := ask(); got.Journal != JournalEmpty || reads != 6 {
		t.Errorf("read again: %+v after %d reads", got, reads)
	}
	c.forget("/mnt/t1")
	for _, tc := range []struct {
		err  error
		want LogFileAnswer
	}{{syscall.EIO, LogFileIOError}, {syscall.EACCES, LogFileUnreadable}} {
		failing = tc.err
		if got := ask(); got.File != tc.want || got.Note != logFileError(tc.err).Note || reads != 6 {
			t.Errorf("the unique id does not answer, %v: %+v after %d reads", tc.err, got, reads)
		}
	}
}
