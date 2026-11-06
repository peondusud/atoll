package disks

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/peondusud/atoll/internal/config"
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
	ID      string `json:"id"`
	Variant string `json:"variant"`
	Error   bool   `json:"error"`
	Note    string `json:"note"`
}

func TestReadIdentity(t *testing.T) {
	for _, c := range vectors.Load[superblockCase](t, "superblock.json") {
		// A little longer, then cut: bytes past the end of a head must not count.
		head := make([]byte, max(c.Length, HeadSize))
		for _, b := range c.Bytes {
			raw, err := hex.DecodeString(b.Hex)
			if err != nil {
				t.Fatal(err)
			}
			copy(head[b.At:], raw)
		}
		got, err := ReadIdentity(names.Filesystem(c.FS), head[:c.Length])
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
		if want := (Identity{c.ID, Variant(c.Variant)}); got != want {
			t.Errorf("%s: got %+v, want %+v (%s)", c.FS, got, want, c.Note)
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
	RootFolder *string     `json:"root_folder"`
	OtherCase  bool        `json:"other_case"`
	State      string      `json:"state"`
	Reason     string      `json:"reason"`
	Note       string      `json:"note"`
}

func or[T any](p *T, fallback T) T {
	if p != nil {
		return *p
	}
	return fallback
}

// answers plays a machine from a case of disk_check.json, and notes what was asked.
type answers struct {
	seen   MountSeen
	device DeviceSeen
	root   RootSeen
	asked  []string
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

func answersFor(c *checkCase) (*config.Disk, *answers) {
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
		a.device.Identity = &Identity{or(c.ID, uuid), Variant(c.Variant)}
	}
	a.root = RootSeen{Folder: FolderAnswer(or(c.RootFolder, "ok")), OtherCase: c.OtherCase}
	return d, a
}

func TestCheck(t *testing.T) {
	cases := vectors.Load[checkCase](t, "disk_check.json")
	for i := range cases {
		c := &cases[i]
		d, a := answersFor(c)
		got := Check(a, d)
		if string(got.State) != c.State || string(got.Reason) != c.Reason {
			t.Errorf("case %d: got %s %q, want %s %q (%s)", i, got.State, got.Reason, c.State, c.Reason, c.Note)
		}
		if (got.State == Online) != (got.Message == "") {
			t.Errorf("case %d: %s with the message %q (%s)", i, got.State, got.Message, c.Note)
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
	} {
		d, a := answersFor(&tc.c)
		Check(a, d)
		if !slices.Equal(a.asked, tc.want) {
			t.Errorf("%s: asked %v, want %v", tc.name, a.asked, tc.want)
		}
	}
}

// TestDiskCheckCoversEveryReason: a reason no vector gives would be a reason no other version
// has to agree on.
func TestDiskCheckCoversEveryReason(t *testing.T) {
	seen := map[string]bool{}
	cases := vectors.Load[checkCase](t, "disk_check.json")
	for i := range cases {
		seen[cases[i].Reason] = true
	}
	for _, r := range []Reason{
		NotMounted, IOError, PartialMount, WrongFilesystem, DeviceGone, DeviceUnread, WrongIdentifier, NoRoot,
		UnsupportedDriver, ADS, NoCase, IOCharset, IgnoreCase, Casefold, ASCIICI, ReadOnlyMount,
	} {
		if !seen[string(r)] {
			t.Errorf("no vector gives %q", r)
		}
	}
}

func TestCheckAll(t *testing.T) {
	d, a := answersFor(&checkCase{})
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
