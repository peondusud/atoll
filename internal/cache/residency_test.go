package cache

import (
	"reflect"
	"testing"

	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/vectors"
)

type residencyVector struct {
	Note       string           `json:"note"`
	NowNS      int64            `json:"now_ns"`
	Enabled    *bool            `json:"enabled"`
	WindowOpen bool             `json:"window_open"`
	MaxFill    string           `json:"max_fill"`
	Disks      []residencyDisk  `json:"disks"`
	Files      []residencyFile  `json:"files"`
	Leave      []residencyLeave `json:"leave"`
	Error      bool             `json:"error"`
}

type residencyDisk struct {
	ID       string `json:"id"`
	Position int    `json:"position"`
	Capacity *int64 `json:"capacity"`
	Free     *int64 `json:"free"`
}

type residencyFile struct {
	Path     *string  `json:"path"`
	PathB64  *string  `json:"path_b64"`
	Size     int64    `json:"size"`
	Disks    []string `json:"disks"`
	SinceNS  int64    `json:"since_ns"`
	MinAgeNS int64    `json:"min_age_ns"`
	Cache    *bool    `json:"cache"`
	Held     bool     `json:"held"`
	Released bool     `json:"released"`
}

type residencyLeave struct {
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Why     string  `json:"why"`
}

func TestResidencyVectors(t *testing.T) {
	for i, v := range vectors.Load[residencyVector](t, "cache_residency.json") {
		maxFill, err := value.ParsePercent(v.MaxFill)
		if err != nil {
			t.Fatalf("vector %d (%s): %v", i, v.Note, err)
		}
		in := Input{NowNS: v.NowNS, Enabled: v.Enabled == nil || *v.Enabled, WindowOpen: v.WindowOpen, MaxFill: maxFill}
		for _, d := range v.Disks {
			in.Disks = append(in.Disks, Disk(d))
		}
		for _, f := range v.Files {
			path, err := vectors.Path(f.Path, f.PathB64)
			if err != nil {
				t.Fatal(err)
			}
			in.Files = append(in.Files, File{Path: path, Size: f.Size, Disks: f.Disks, SinceNS: f.SinceNS, MinAgeNS: f.MinAgeNS,
				Cache: f.Cache == nil || *f.Cache, Held: f.Held, Released: f.Released})
		}
		got, err := Leaving(&in)
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
		want := []Leave{}
		for _, l := range v.Leave {
			path, err := vectors.Path(l.Path, l.PathB64)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, Leave{Path: path, Why: Why(l.Why)})
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("vector %d (%s): leave %+v, want %+v", i, v.Note, got, want)
		}
	}
}

// When a file on the cache is first due: its residency's end, now with cache = false, the cache not enabled or named by
// atoll replicate, and never while it waits for that command.
func TestDueAt(t *testing.T) {
	in := &Input{NowNS: 100, Enabled: true}
	for _, tc := range []struct {
		name string
		f    File
		at   int64
		due  bool
	}{
		{"its residency's end", File{SinceNS: 50, Cache: true, MinAgeNS: 200}, 250, true},
		{"over already", File{SinceNS: 10, Cache: true, MinAgeNS: 20}, 30, true},
		{"cache = false", File{SinceNS: 50, Cache: false, MinAgeNS: 200}, 100, true},
		{"held", File{SinceNS: 50, Cache: true, MinAgeNS: 200, Held: true}, 0, false},
		{"held and named", File{SinceNS: 50, Cache: true, MinAgeNS: 200, Held: true, Released: true}, 100, true},
		{"past the last nanosecond", File{SinceNS: 1 << 62, Cache: true, MinAgeNS: 1 << 62}, 1<<63 - 1, true},
	} {
		if at, due := DueAt(in, &tc.f); at != tc.at || due != tc.due {
			t.Errorf("%s: %d, %v; want %d, %v", tc.name, at, due, tc.at, tc.due)
		}
	}
	if at, due := DueAt(&Input{NowNS: 100}, &File{SinceNS: 50, Cache: true, MinAgeNS: 200}); at != 100 || !due {
		t.Errorf("the cache not enabled: %d, %v", at, due)
	}
}
