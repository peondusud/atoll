package view

import (
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type attrsVector struct {
	Kind NodeKind `json:"kind"`
	Mode uint32   `json:"mode"`
	UID  uint32   `json:"uid"`
	GID  uint32   `json:"gid"`
}

func (a attrsVector) attrs() Attrs { return Attrs(a) }

type accessVector struct {
	Asks   AccessKind `json:"asks"`
	Caller struct {
		UID    uint32   `json:"uid"`
		GID    uint32   `json:"gid"`
		Groups []uint32 `json:"groups"`
		Caps   []string `json:"caps"`
	} `json:"caller"`
	Presented       attrsVector  `json:"presented"`
	Disk            attrsVector  `json:"disk"`
	PresentedFolder *attrsVector `json:"presented_folder"`
	DiskFolder      *attrsVector `json:"disk_folder"`
	Mode            *uint32      `json:"mode"`
	UID             *uint32      `json:"uid"`
	GID             *uint32      `json:"gid"`
	Refused         string       `json:"refused"`
	Applied         *uint32      `json:"applied"`
	Note            string       `json:"note"`
}

// The capabilities by the names the vectors give them.
var capNames = map[string]Cap{
	"chown": CapChown, "dac_override": CapDACOverride, "dac_read_search": CapDACReadSearch, "fowner": CapFowner, "fsetid": CapFsetid,
}

// TestAccessVectors: what the view checks on a disk before it acts there for a caller (specs.md §21.2).
func TestAccessVectors(t *testing.T) {
	kinds := map[AccessKind]bool{}
	for i, v := range vectors.Load[accessVector](t, "view_access.json") {
		kinds[v.Asks] = true
		who := &Credentials{UID: v.Caller.UID, GID: v.Caller.GID, Groups: v.Caller.Groups}
		for _, name := range v.Caller.Caps {
			c, ok := capNames[name]
			if !ok {
				t.Fatalf("vector %d: no capability %q", i, name)
			}
			who.Caps |= 1 << c
		}
		a := &Access{Kind: v.Asks, Presented: v.Presented.attrs(), Disk: v.Disk.attrs(), UID: v.UID, GID: v.GID}
		if (v.PresentedFolder != nil) != (v.Asks == AccessDelete) || (v.DiskFolder != nil) != (v.Asks == AccessDelete) {
			t.Fatalf("vector %d: a folder is given for a deletion, and only then", i)
		}
		if v.PresentedFolder != nil {
			a.PresentedFolder, a.DiskFolder = v.PresentedFolder.attrs(), v.DiskFolder.attrs()
		}
		if (v.Mode != nil) != (v.Asks == AccessChmod) || (v.Applied != nil) != (v.Asks == AccessChmod && v.Refused == "") {
			t.Fatalf("vector %d: a mode is asked of a chmod, and applied by one allowed, and only then", i)
		}
		if v.Mode != nil {
			a.Mode = *v.Mode
		}
		refused, applied := a.Check(who)
		want := map[string]syscall.Errno{"": 0, "EACCES": syscall.EACCES, "EPERM": syscall.EPERM}[v.Refused]
		if refused != want {
			t.Errorf("vector %d (%s): refused %v, want %q", i, v.Note, refused, v.Refused)
		}
		if v.Applied != nil && applied != *v.Applied {
			t.Errorf("vector %d (%s): applied %#o, want %#o", i, v.Note, applied, *v.Applied)
		}
	}
	for _, k := range []AccessKind{AccessWrite, AccessXattr, AccessCreate, AccessDelete, AccessChmod, AccessChown, AccessTimes, AccessTouch, AccessLink} {
		if !kinds[k] {
			t.Errorf("no vector of %s", k)
		}
	}
}

type procStatusVector struct {
	Status string   `json:"status"`
	FSUID  uint32   `json:"fsuid"`
	FSGID  uint32   `json:"fsgid"`
	Groups []uint32 `json:"groups"`
	CapEff uint64   `json:"cap_eff"`
	Error  bool     `json:"error"`
	Note   string   `json:"note"`
}

// TestProcStatusVectors: what the view reads of the thread that asks it an operation (specs.md §21.2).
func TestProcStatusVectors(t *testing.T) {
	for i, v := range vectors.Load[procStatusVector](t, "proc_status.json") {
		got, err := ParseProcStatus([]byte(v.Status))
		switch {
		case v.Error:
			if err == nil {
				t.Errorf("vector %d (%s): read %+v, want an error", i, v.Note, got)
			}
		case err != nil:
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
		case got.FSUID != v.FSUID || got.FSGID != v.FSGID || !slices.Equal(got.Groups, v.Groups) || uint64(got.CapEff) != v.CapEff:
			t.Errorf("vector %d (%s): %+v, want %d:%d %v %#x", i, v.Note, got, v.FSUID, v.FSGID, v.Groups, v.CapEff)
		}
	}
}
