package solver

import (
	"math"
	"reflect"
	"testing"

	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/vectors"
)

type purgeVector struct {
	NowNS       *int64  `json:"now_ns"`
	OlderThanNS *int64  `json:"older_than_ns"`
	MaxSize     *string `json:"max_size"`
	Disks       []struct {
		ID       *string `json:"id"`
		Capacity *int64  `json:"capacity"`
	} `json:"disks"`
	Entries []struct {
		Disk      *string `json:"disk"`
		Op        *int64  `json:"op"`
		Reason    *string `json:"reason"`
		Size      *int64  `json:"size"`
		DeletedNS *int64  `json:"deleted_ns"`
		ExpiresNS *int64  `json:"expires_ns"`
		Held      *bool   `json:"held"`
	} `json:"entries"`
	Purge *[]struct {
		Op  int64    `json:"op"`
		Why PurgeWhy `json:"why"`
	} `json:"purge"`
	Full  *[]string `json:"full"`
	Error bool      `json:"error"`
	Note  string    `json:"note"`
}

func TestPurgeVectors(t *testing.T) {
	for i, v := range vectors.Load[purgeVector](t, "trash_purge.json") {
		if v.NowNS == nil || v.MaxSize == nil || v.Disks == nil || v.Entries == nil {
			t.Errorf("vector %d: want now_ns, max_size, disks and entries", i)
			continue
		}
		if v.Error == (v.Purge != nil) || (v.Purge == nil) != (v.Full == nil) {
			t.Errorf("vector %d: want purge and full, or error", i)
			continue
		}
		maxSize, err := value.ParsePercent(*v.MaxSize)
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		in := PurgeInput{NowNS: *v.NowNS, OlderThanNS: v.OlderThanNS, MaxSize: maxSize}
		for j := range v.Disks {
			d := &v.Disks[j]
			if d.ID == nil {
				t.Fatalf("vector %d: disk %d: want an id", i, j)
			}
			in.Disks = append(in.Disks, TrashDisk{ID: *d.ID, Capacity: deref(d.Capacity), HasCapacity: d.Capacity != nil})
		}
		for j := range v.Entries {
			e := &v.Entries[j]
			if e.Disk == nil || e.Op == nil || e.Reason == nil || e.DeletedNS == nil || e.ExpiresNS == nil || e.Held == nil {
				t.Fatalf("vector %d: entry %d: every field but size is required", i, j)
			}
			in.Entries = append(in.Entries, TrashEntry{
				Disk: *e.Disk, OpID: *e.Op, Reason: *e.Reason, Size: deref(e.Size), HasSize: e.Size != nil,
				DeletedNS: *e.DeletedNS, ExpiresNS: *e.ExpiresNS, Held: *e.Held,
			})
		}
		got, err := ChoosePurge(&in)
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): got %+v, want an error", i, v.Note, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		want := &PurgeResult{Purge: []PurgeChoice{}, Full: *v.Full}
		for _, p := range *v.Purge {
			want.Purge = append(want.Purge, PurgeChoice{OpID: p.Op, Why: p.Why})
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, got, want)
		}
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// The choice leaves its input as it was: the caller lists the entries in its own order.
func TestChoosePurgeKeepsItsInput(t *testing.T) {
	maxSize, _ := value.ParsePercent("5%")
	in := PurgeInput{
		NowNS: 10, MaxSize: maxSize, Disks: []TrashDisk{{ID: "t2", Capacity: 1000, HasCapacity: true}, {ID: "t1", Capacity: 1000, HasCapacity: true}},
		Entries: []TrashEntry{
			{Disk: "t1", OpID: 9, Reason: "surplus", Size: 600, HasSize: true, ExpiresNS: math.MaxInt64},
			{Disk: "t2", OpID: 2, Reason: "deleted", Size: 600, HasSize: true, ExpiresNS: math.MaxInt64},
		},
	}
	before := reflect.ValueOf(in.Entries).Index(0).Interface()
	got, err := ChoosePurge(&in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &PurgeResult{Purge: []PurgeChoice{{OpID: 9, Why: PurgeMaxSize}}, Full: []string{"t2"}}) {
		t.Errorf("%+v", got)
	}
	if in.Entries[0] != before || in.Disks[0].ID != "t2" {
		t.Errorf("the input changed: %+v", in)
	}
}

// Sums past the largest integer stay there, and a cut-off before the smallest one stays there too.
func TestPurgeClamps(t *testing.T) {
	used := TrashUsed([]TrashEntry{
		{Disk: "t1", Size: math.MaxInt64 - 1, HasSize: true}, {Disk: "t1", Size: 5, HasSize: true}, {Disk: "t1", Size: 7},
	})
	if used["t1"] != math.MaxInt64 {
		t.Errorf("%d", used["t1"])
	}
	if got := subtractClamped(math.MinInt64+3, 10); got != math.MinInt64 {
		t.Errorf("%d", got)
	}
	if got := subtractClamped(5, 10); got != -5 {
		t.Errorf("%d", got)
	}
}
