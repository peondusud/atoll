package disks

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/vectors"
)

type mountinfoCase struct {
	Text       *string          `json:"text"`
	TextB64    *string          `json:"text_b64"`
	Entries    []mountinfoEntry `json:"entries"`
	Unreadable int              `json:"unreadable"`
	Find       *uint64          `json:"find"`
	Finds      string           `json:"finds"`
	Note       string           `json:"note"`
}

type mountinfoEntry struct {
	ID            uint64   `json:"id"`
	Major         uint32   `json:"major"`
	Minor         uint32   `json:"minor"`
	Root          string   `json:"root"`
	MountPoint    *string  `json:"mount_point"`
	MountPointB64 *string  `json:"mount_point_b64"`
	MountOptions  []string `json:"mount_options"`
	FSType        string   `json:"fstype"`
	Source        string   `json:"source"`
	SuperOptions  []string `json:"super_options"`
}

func TestParseMountInfo(t *testing.T) {
	for _, c := range vectors.Load[mountinfoCase](t, "mountinfo.json") {
		text, err := vectors.Bytes("text", c.Text, c.TextB64)
		if err != nil {
			t.Fatalf("%q: %v", c.Note, err)
		}
		got := ParseMountInfo([]byte(text))
		if got.Unreadable != c.Unreadable {
			t.Errorf("%q: %d lines unreadable, want %d (%s)", text, got.Unreadable, c.Unreadable, c.Note)
		}
		want := make([]MountEntry, 0, len(c.Entries))
		for _, e := range c.Entries {
			point, err := vectors.Bytes("mount_point", e.MountPoint, e.MountPointB64)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, MountEntry{e.ID, e.Major, e.Minor, e.Root, point, e.MountOptions, e.FSType, e.Source, e.SuperOptions})
		}
		if !reflect.DeepEqual(got.Entries, want) {
			t.Errorf("%q:\n got %+v\nwant %+v (%s)", text, got.Entries, want, c.Note)
		}
		if c.Find != nil {
			if finds := findsOf(got.Find(*c.Find)); finds != c.Finds {
				t.Errorf("%q: finding %d gives %s, want %s (%s)", text, *c.Find, finds, c.Finds, c.Note)
			}
		}
	}
}

func findsOf(seen MountSeen) string {
	switch {
	case seen.Path == PathIOError:
		return "unknown"
	case seen.Entry == nil:
		return "nothing"
	default:
		return strconv.FormatUint(seen.Entry.ID, 10)
	}
}

type superblockCase struct {
	FS     string `json:"fs"`
	Length int    `json:"length"`
	Bytes  []struct {
		At  int    `json:"at"`
		Hex string `json:"hex"`
	} `json:"bytes"`
	ID            string `json:"id"`
	Variant       string `json:"variant"`
	NoJournal     bool   `json:"no_journal"`
	TimePrecision int64  `json:"time_precision_ns"`
	Error         bool   `json:"error"`
	Note          string `json:"note"`
}

// head builds what a case says a device starts with. A little longer, then cut: bytes past the end of a head must
// not count.
func (c *superblockCase) head(t *testing.T) []byte {
	t.Helper()
	head := make([]byte, max(c.Length, HeadSize))
	for _, b := range c.Bytes {
		raw, err := hex.DecodeString(b.Hex)
		if err != nil {
			t.Fatal(err)
		}
		copy(head[b.At:], raw)
	}
	return head[:c.Length]
}

func TestReadIdentity(t *testing.T) {
	for _, c := range vectors.Load[superblockCase](t, "superblock.json") {
		got, err := ReadIdentity(names.Filesystem(c.FS), c.head(t))
		if c.Error {
			if err == nil {
				t.Errorf("%s: read as %+v, want it refused (%s)", c.FS, got, c.Note)
			} else if !errors.Is(err, ErrNoFilesystem) {
				t.Errorf("%s: %v (%s)", c.FS, err, c.Note)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v (%s)", c.FS, err, c.Note)
			continue
		}
		if got.ID != c.ID || got.Variant != Variant(c.Variant) || got.NoJournal != c.NoJournal {
			t.Errorf("%s: got %+v, want %s %q, no journal %v (%s)", c.FS, got, c.ID, c.Variant, c.NoJournal, c.Note)
		}
	}
}

func TestTimePrecisionVectors(t *testing.T) {
	for _, c := range vectors.Load[superblockCase](t, "time_precision.json") {
		got, err := ReadIdentity(names.Filesystem(c.FS), c.head(t))
		switch {
		case c.Error:
			if !errors.Is(err, ErrNoFilesystem) {
				t.Errorf("%s: %+v, %v, want it refused (%s)", c.FS, got, err, c.Note)
			}
		case err != nil:
			t.Errorf("%s: %v (%s)", c.FS, err, c.Note)
		case c.TimePrecision == 0:
			t.Errorf("%s: a case needs time_precision_ns or error (%s)", c.FS, c.Note)
		case got.TimePrecisionNS != c.TimePrecision:
			t.Errorf("%s: %d ns, want %d (%s)", c.FS, got.TimePrecisionNS, c.TimePrecision, c.Note)
		}
	}
}

// What only the kind of a filesystem says: NTFS counts in 100 ns, and the others hold nanoseconds.
func TestTimePrecisionOfAKind(t *testing.T) {
	for fs, want := range map[names.Filesystem]int64{names.Ext4: 1, names.XFS: 1, names.Btrfs: 1, names.NTFS: 100} {
		if got := TimePrecision(fs); got != want {
			t.Errorf("%s: %d, want %d", fs, got, want)
		}
	}
}

// TestHeadSize: what a check reads must reach the last byte a layout looks at.
func TestHeadSize(t *testing.T) {
	head := make([]byte, HeadSize)
	copy(head[65600:], "_BHRfS_M")
	if _, err := ReadIdentity(names.Btrfs, head); err != nil {
		t.Error(err)
	}
}

type checkEntry struct {
	MountPoint   *string   `json:"mount_point"`
	Root         *string   `json:"root"`
	FSType       *string   `json:"fstype"`
	MountOptions *[]string `json:"mount_options"`
	SuperOptions *[]string `json:"super_options"`
}

type checkCase struct {
	FS         *string     `json:"fs"`
	UUID       *string     `json:"uuid"`
	Mount      *string     `json:"mount"`
	Path       *string     `json:"path"`
	NoEntry    bool        `json:"no_entry"`
	Entry      *checkEntry `json:"entry"`
	Device     *string     `json:"device"`
	NoMagic    bool        `json:"no_magic"`
	ID         *string     `json:"id"`
	Variant    string      `json:"variant"`
	NoJournal  bool        `json:"no_journal"`
	RootFolder *string     `json:"root_folder"`
	OtherCase  bool        `json:"other_case"`
	// Hiberfil is what the top of the mount holds at hiberfil.sys; a file has its size, the bytes of its content that are
	// not zero, and how many of its first bytes a read gave.
	Hiberfil      *string   `json:"hiberfil"`
	HiberfilSize  int64     `json:"hiberfil_size"`
	HiberfilBytes []bytesAt `json:"hiberfil_bytes"`
	HiberfilRead  *int      `json:"hiberfil_read"`
	// LogFile is what the top of the mount holds at $LogFile, and Journal what that file says.
	LogFile    *string         `json:"logfile"`
	Journal    *string         `json:"journal"`
	Marker     json.RawMessage `json:"marker"`
	State      string          `json:"state"`
	Reason     string          `json:"reason"`
	MountOwner bool            `json:"mount_owner"`
	Note       string          `json:"note"`
}

func or[T any](p *T, fallback T) T {
	if p != nil {
		return *p
	}
	return fallback
}

// bytesAt is a run of bytes at an offset, in hexadecimal.
type bytesAt struct {
	At  int    `json:"at"`
	Hex string `json:"hex"`
}

// answers plays a machine from a case of disk_check.json, and notes what was asked.
type answers struct {
	seen     MountSeen
	device   DeviceSeen
	root     RootSeen
	hiberfil HiberfilSeen
	logFile  LogFileSeen
	asked    []string
}

func (a *answers) Mount(string) MountSeen {
	a.asked = append(a.asked, "mount")
	return a.seen
}

func (a *answers) Device(names.Filesystem, *MountEntry) DeviceSeen {
	a.asked = append(a.asked, "device")
	return a.device
}

func (a *answers) Root(string, string) RootSeen {
	a.asked = append(a.asked, "root")
	return a.root
}

func (a *answers) Hiberfil(string, *MountEntry) HiberfilSeen {
	a.asked = append(a.asked, "hiberfil")
	return a.hiberfil
}

func (a *answers) LogFile(string, *MountEntry) LogFileSeen {
	a.asked = append(a.asked, "logfile")
	return a.logFile
}

// logFileOf gives what a case says the top holds at $LogFile: a file whose journal is empty when it says nothing.
func logFileOf(c *checkCase) LogFileSeen {
	seen := LogFileSeen{File: LogFileAnswer(or(c.LogFile, "file"))}
	switch {
	case seen.File == LogFileFile:
		seen.Journal = Journal(or(c.Journal, "empty"))
		if seen.Journal != JournalEmpty && seen.Journal != JournalClean {
			seen.Note = "what the case says"
		}
	case seen.File != LogFileAbsent:
		seen.Note = "what the case says"
	}
	return seen
}

// hiberfilOf gives what a case says the top holds at hiberfil.sys: a file's content is zeros but for its bytes, and a
// read gives its first 4 KiB, or as much as the case says.
func hiberfilOf(t *testing.T, c *checkCase) HiberfilSeen {
	t.Helper()
	seen := HiberfilSeen{File: HiberfilAnswer(or(c.Hiberfil, "absent"))}
	if seen.File != HiberfilFile {
		return seen
	}
	read := or(c.HiberfilRead, int(min(c.HiberfilSize, HiberfilHeader)))
	content := make([]byte, 2*HiberfilHeader)
	for _, b := range c.HiberfilBytes {
		raw, err := hex.DecodeString(b.Hex)
		if err != nil {
			t.Fatal(err)
		}
		if b.At+len(raw) > len(content) || b.At+len(raw) > int(c.HiberfilSize) {
			t.Fatalf("bytes past the file: %+v (%s)", b, c.Note)
		}
		copy(content[b.At:], raw)
	}
	if read > len(content) || int64(read) > c.HiberfilSize {
		t.Fatalf("a read of %d bytes (%s)", read, c.Note)
	}
	seen.Size, seen.Head = c.HiberfilSize, content[:read]
	return seen
}

func answersFor(t *testing.T, c *checkCase) (*config.Disk, *answers) {
	t.Helper()
	fs := names.Filesystem(or(c.FS, "ext4"))
	uuid := "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"
	fstype, super := string(fs), []string{"rw"}
	if fs == names.NTFS {
		uuid, fstype, super = "01D9F3A2B4C5E6F7", "ntfs3", []string{"rw", "iocharset=utf8"}
	}
	uuid = or(c.UUID, uuid)
	d := &config.Disk{ID: "t1", UUID: uuid, Mount: or(c.Mount, "/mnt/t1"), FS: fs, Root: "media"}
	e := checkEntry{}
	if c.Entry != nil {
		e = *c.Entry
	}
	entry := &MountEntry{
		MountPoint:   or(e.MountPoint, d.Mount),
		Root:         or(e.Root, "/"),
		FSType:       or(e.FSType, fstype),
		MountOptions: or(e.MountOptions, []string{"rw"}),
		SuperOptions: or(e.SuperOptions, super),
	}
	if c.NoEntry {
		entry = nil
	}
	a := &answers{seen: MountSeen{Path: PathAnswer(or(c.Path, "ok")), Entry: entry}}
	a.device = DeviceSeen{Device: DeviceAnswer(or(c.Device, "ok"))}
	if !c.NoMagic {
		a.device.Identity = &Identity{ID: or(c.ID, uuid), Variant: Variant(c.Variant), NoJournal: c.NoJournal, TimePrecisionNS: 100}
	}
	a.root = RootSeen{Folder: FolderAnswer(or(c.RootFolder, "ok")), OtherCase: c.OtherCase}
	a.hiberfil = hiberfilOf(t, c)
	a.logFile = logFileOf(c)
	return d, a
}

func TestCheck(t *testing.T) {
	cases := vectors.Load[checkCase](t, "disk_check.json")
	for i := range cases {
		c := &cases[i]
		d, a := answersFor(t, c)
		got := Check(a, d)
		if c.Marker != nil {
			got = MarkerVerdict(got, markerText(t, c.Marker), true, "p", d)
		}
		if string(got.State) != c.State || string(got.Reason) != c.Reason {
			t.Errorf("case %d: got %s %q, want %s %q (%s)", i, got.State, got.Reason, c.State, c.Reason, c.Note)
		}
		if (got.State == Online) != (got.Message == "") {
			t.Errorf("case %d: %s with the message %q (%s)", i, got.State, got.Message, c.Note)
		}
		// What the head said of the time goes with a disk that passed, and nothing with one that did not (specs.md §9).
		if want := map[bool]int64{true: 0, false: 100}[got.State == Missing]; got.TimePrecisionNS != want {
			t.Errorf("case %d: %s with a precision of %d (%s)", i, got.State, got.TimePrecisionNS, c.Note)
		}
		if got.MountOwner != c.MountOwner {
			t.Errorf("case %d: the mount's owner %v, want %v (%s)", i, got.MountOwner, c.MountOwner, c.Note)
		}
	}
}

// An entry of the mount table that is not the mount path's is either a mount the path leads to through a symlink, or
// the mount that holds the path, as an empty mount point gives its parent's entry (specs.md §10, question 2): the
// message says which, so that an empty mount point under /tmp, seen in qemu, does not read as a symlink. Only whole
// components make a parent.
func TestNotMountedSaysWhatIsMountedThere(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		mount, entry string
		want         string
	}{
		{"/mnt/t1", "/", `nothing is mounted at that path: it is part of the mount at "/"`},
		{"/mnt/t1", "/mnt", `nothing is mounted at that path: it is part of the mount at "/mnt"`},
		{"/tmp/atoll/t1", "/tmp", `nothing is mounted at that path: it is part of the mount at "/tmp"`},
		{"/mnt/t1", "/srv/t1", `the mount is at "/srv/t1": write that path, not one through a symlink`},
		{"/mnt/t1", "/mnt/t", `the mount is at "/mnt/t": write that path, not one through a symlink`},
		{"/mnt/t1", "/mnt/T1", `the mount is at "/mnt/T1": write that path, not one through a symlink`},
		{"/mnt/t1", "/mnt/t1/", `the mount is at "/mnt/t1/": write that path, not one through a symlink`},
		{"/mnt/t1", "/mnt/t1/sub", `the mount is at "/mnt/t1/sub": write that path, not one through a symlink`},
	} {
		d, a := answersFor(t, &checkCase{Mount: text(tc.mount), Entry: &checkEntry{MountPoint: text(tc.entry)}})
		got := Check(a, d)
		if got.State != Missing || got.Reason != NotMounted || got.Message != tc.want {
			t.Errorf("%s with the entry of %s: %s %s %q, want %q", tc.mount, tc.entry, got.State, got.Reason, got.Message, tc.want)
		}
	}
}

// TestCheckStopsAtTheFirstFailure: nothing past a failure is read, as reading a device or a
// folder may wake a disk, or hang on one that is dying.
func TestCheckStopsAtTheFirstFailure(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		c    checkCase
		want []string
	}{
		{"healthy", checkCase{}, []string{"mount", "device", "root"}},
		{"absent", checkCase{Path: text("absent")}, []string{"mount"}},
		{"I/O error", checkCase{Path: text("io-error")}, []string{"mount"}},
		{"not a mount", checkCase{NoEntry: true}, []string{"mount"}},
		{"another path", checkCase{Entry: &checkEntry{MountPoint: text("/x")}}, []string{"mount"}},
		{"a folder", checkCase{Entry: &checkEntry{Root: text("/x")}}, []string{"mount"}},
		{"another type", checkCase{Entry: &checkEntry{FSType: text("xfs")}}, []string{"mount"}},
		{"device gone", checkCase{Device: text("gone")}, []string{"mount", "device"}},
		{"device unreadable", checkCase{Device: text("unreadable")}, []string{"mount", "device"}},
		{"no magic", checkCase{NoMagic: true}, []string{"mount", "device"}},
		{"another disk", checkCase{ID: text("x")}, []string{"mount", "device"}},
		{"shut down", checkCase{Entry: &checkEntry{SuperOptions: &[]string{"rw", "shutdown"}}}, []string{"mount", "device"}},
		// hiberfil.sys, then $LogFile, are the only files of a disk a check reads: on an NTFS disk alone, once questions 1
		// to 10 passed, and only when a driver's options leave the disk online; $LogFile once hiberfil.sys did (specs.md
		// §10).
		{"healthy ntfs", checkCase{FS: text("ntfs")}, []string{"mount", "device", "root", "hiberfil", "logfile"}},
		{"ntfs hibernated", checkCase{FS: text("ntfs"), Hiberfil: text("not-a-file")}, []string{"mount", "device", "root", "hiberfil"}},
		{"ntfs whose hiberfil.sys cannot be read", checkCase{FS: text("ntfs"), Hiberfil: text("io-error")},
			[]string{"mount", "device", "root", "hiberfil"}},
		{"ntfs with an unclean journal", checkCase{FS: text("ntfs"), Journal: text("unclean")},
			[]string{"mount", "device", "root", "hiberfil", "logfile"}},
		{"ntfs without its root", checkCase{FS: text("ntfs"), RootFolder: text("absent")}, []string{"mount", "device", "root"}},
		{"ntfs gone", checkCase{FS: text("ntfs"), Device: text("gone")}, []string{"mount", "device"}},
		{"ntfs3 ads", checkCase{FS: text("ntfs"), Entry: &checkEntry{SuperOptions: &[]string{"rw", "iocharset=utf8", "ads"}}}, []string{"mount", "device", "root"}},
		{"ntfs errors=continue", checkCase{FS: text("ntfs"), Entry: &checkEntry{FSType: text("ntfs"), SuperOptions: &[]string{"rw", "iocharset=utf8"}}},
			[]string{"mount", "device", "root"}},
		{"ntfs-3g read-only", checkCase{FS: text("ntfs"), Entry: &checkEntry{FSType: text("fuseblk"), MountOptions: &[]string{"ro"}, SuperOptions: &[]string{"ro"}}},
			[]string{"mount", "device", "root", "hiberfil", "logfile"}},
		{"ntfs3 ignoring case", checkCase{FS: text("ntfs"), OtherCase: true}, []string{"mount", "device", "root", "hiberfil", "logfile"}},
	} {
		d, a := answersFor(t, &tc.c)
		Check(a, d)
		if !slices.Equal(a.asked, tc.want) {
			t.Errorf("%s: asked %v, want %v", tc.name, a.asked, tc.want)
		}
	}
}

// TestDiskCheckCoversEveryReason: a reason no vector gives would be a reason no other version
// has to agree on. unsupported-driver is one no check gives since the type ntfs was supported, and no vector may give
// it either (specs.md §10).
func TestDiskCheckCoversEveryReason(t *testing.T) {
	seen := map[string]bool{}
	cases := vectors.Load[checkCase](t, "disk_check.json")
	for i := range cases {
		seen[cases[i].Reason] = true
	}
	for _, r := range []Reason{
		NotMounted, IOError, PartialMount, WrongFilesystem, DeviceGone, DeviceUnread, WrongIdentifier, NoRoot,
		ADS, NoCase, IOCharset, ErrorsContinue, Hibernated, IgnoreCase, Casefold, ASCIICI, NoJournal, ReadOnlyMount, OtherMarker,
	} {
		if !seen[string(r)] {
			t.Errorf("no vector gives %q", r)
		}
	}
	if seen[string(UnsupportedDriver)] {
		t.Errorf("a vector gives %q, which no check gives any more", UnsupportedDriver)
	}
}

// The type ntfs is the one whose EINVAL from renameat2 with RENAME_NOREPLACE is a name refused, where on the other
// drivers link tells (specs.md §17.1), and the one whose link moves its file's modification time (§22).
func TestWhatTheTypeNTFSDoesOtherwise(t *testing.T) {
	for fstype, want := range map[string]bool{"ntfs": true, "ntfs3": false, "fuseblk": false, "ext4": false, "xfs": false, "btrfs": false, "": false} {
		if got := RenameTellsNames(fstype); got != want {
			t.Errorf("a rename on %q: %v", fstype, got)
		}
		if got := LinkMovesTimes(fstype); got != want {
			t.Errorf("a link on %q: %v", fstype, got)
		}
	}
}

func TestCheckAll(t *testing.T) {
	d, a := answersFor(t, &checkCase{})
	second := *d
	second.UUID = "another"
	got, err := CheckAll(context.Background(), a, []config.Disk{*d, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].State != Online || got[1].Reason != WrongIdentifier {
		t.Errorf("not in the order given: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CheckAll(ctx, a, []config.Disk{*d}); !errors.Is(err, context.Canceled) {
		t.Errorf("a check whose context is over: %v", err)
	}
}

func TestSwapCase(t *testing.T) {
	for in, want := range map[string]string{
		"media": "MEDIA", "Media": "mEDIA", "MEDIA": "media", "m1-x": "M1-X", "123": "123", "médias": "MéDIAS", "媒体": "媒体",
		"a\xffb": "A\xffB", "@[`{": "@[`{",
	} {
		if got := swapCase(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// markerText writes what a case's marker says the disk.toml holds.
func markerText(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var odd string
	if json.Unmarshal(raw, &odd) == nil {
		return []byte("format_version = 9\n")
	}
	var m struct {
		PoolID string `json:"pool_id"`
		DiskID string `json:"disk_id"`
		FSUUID string `json:"fs_uuid"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	text, err := layout.MarkerText(layout.Marker{PoolID: m.PoolID, DiskID: m.DiskID, FSUUID: m.FSUUID})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(text)
}
