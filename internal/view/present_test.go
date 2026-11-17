package view

import (
	"testing"

	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/vectors"
)

type viewDiskVector struct {
	ID       string           `json:"id"`
	Position int              `json:"position"`
	State    solver.DiskState `json:"state"`
	Role     string           `json:"role"`
	Capacity *int64           `json:"capacity"`
	Free     *int64           `json:"free"`
}

type viewCopyVector struct {
	Disk        string           `json:"disk"`
	State       solver.CopyState `json:"state"`
	Size        *int64           `json:"size"`
	MtimeNS     *int64           `json:"mtime_ns"`
	Mode        *uint32          `json:"mode"`
	UID         *uint32          `json:"uid"`
	GID         *uint32          `json:"gid"`
	Nlink       *int64           `json:"nlink"`
	CreationNS  *int64           `json:"creation_ns"`
	FirstSeenNS int64            `json:"first_seen_ns"`
	GoneKind    string           `json:"gone_kind"`
	OldPath     string           `json:"old_path"`
}

type viewFileVector struct {
	State        solver.State     `json:"state"`
	Original     string           `json:"original"`
	ConflictHeld bool             `json:"conflict_held"`
	Size         int64            `json:"size"`
	MtimeNS      int64            `json:"mtime_ns"`
	Copies       []viewCopyVector `json:"copies"`
}

type viewEntryVector struct {
	Disk    string           `json:"disk"`
	Kind    solver.EntryKind `json:"kind"`
	Mode    *uint32          `json:"mode"`
	UID     *uint32          `json:"uid"`
	GID     *uint32          `json:"gid"`
	Size    *int64           `json:"size"`
	MtimeNS *int64           `json:"mtime_ns"`
	Target  string           `json:"target"`
}

type viewNodeVector struct {
	CacheInView bool              `json:"cache_in_view"`
	Disks       []viewDiskVector  `json:"disks"`
	File        *viewFileVector   `json:"file"`
	Entries     []viewEntryVector `json:"entries"`
	Node        *struct {
		Kind    NodeKind `json:"kind"`
		Disk    string   `json:"disk"`
		Mode    uint32   `json:"mode"`
		UID     uint32   `json:"uid"`
		GID     uint32   `json:"gid"`
		Size    int64    `json:"size"`
		MtimeNS int64    `json:"mtime_ns"`
		Nlink   int64    `json:"nlink"`
		Target  string   `json:"target"`
		Ignored bool     `json:"ignored"`
	} `json:"node"`
	Note string `json:"note"`
}

// disksOf gives the disks of a vector: a cache disk is left out of the view but with cacheInView (specs.md §19).
func disksOf(list []viewDiskVector, cacheInView bool) []Disk {
	out := make([]Disk, 0, len(list))
	for _, d := range list {
		out = append(out, Disk{ID: d.ID, Position: d.Position, State: d.State, Cache: d.Role == "cache" && !cacheInView, Capacity: d.Capacity, Free: d.Free})
	}
	return out
}

// TestPresentVectors: what a path presents, from its rows (specs.md §21.2).
func TestPresentVectors(t *testing.T) {
	self := Owner{UID: 4242, GID: 4343}
	for i, v := range vectors.Load[viewNodeVector](t, "view_node.json") {
		var file *File
		if v.File != nil {
			file = &File{State: v.File.State, Original: v.File.Original, ConflictHeld: v.File.ConflictHeld, Size: v.File.Size, MtimeNS: v.File.MtimeNS}
			for _, c := range v.File.Copies {
				file.Copies = append(file.Copies, Copy(c))
			}
		}
		var entries []Entry
		for _, e := range v.Entries {
			entries = append(entries, Entry(e))
		}
		got, err := Present(disksOf(v.Disks, v.CacheInView), file, entries, self)
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		switch {
		case v.Node == nil && got != nil:
			t.Errorf("vector %d (%s): presents %+v, want nothing", i, v.Note, *got)
		case v.Node != nil && got == nil:
			t.Errorf("vector %d (%s): presents nothing", i, v.Note)
		case v.Node != nil:
			want := Node{Kind: v.Node.Kind, Disk: v.Node.Disk, Ignored: v.Node.Ignored, Mode: v.Node.Mode, UID: v.Node.UID, GID: v.Node.GID, Size: v.Node.Size,
				MtimeNS: v.Node.MtimeNS, Nlink: v.Node.Nlink, Target: v.Node.Target}
			if *got != want {
				t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, *got, want)
			}
		}
	}
}

type viewStatfsVector struct {
	CacheInView bool             `json:"cache_in_view"`
	Disks       []viewDiskVector `json:"disks"`
	Statfs      struct {
		Bsize   uint64 `json:"bsize"`
		Blocks  uint64 `json:"blocks"`
		Bfree   uint64 `json:"bfree"`
		Bavail  uint64 `json:"bavail"`
		Namemax uint64 `json:"namemax"`
	} `json:"statfs"`
	Note string `json:"note"`
}

// TestStatfsVectors: statfs of the view, from the catalog (specs.md §21.2).
func TestStatfsVectors(t *testing.T) {
	for i, v := range vectors.Load[viewStatfsVector](t, "view_statfs.json") {
		got := StatfsOf(disksOf(v.Disks, v.CacheInView))
		want := Statfs{Bsize: v.Statfs.Bsize, Blocks: v.Statfs.Blocks, Bfree: v.Statfs.Bfree, Bavail: v.Statfs.Bavail, Namemax: v.Statfs.Namemax}
		if got != want {
			t.Errorf("vector %d (%s): %+v, want %+v", i, v.Note, got, want)
		}
	}
}
