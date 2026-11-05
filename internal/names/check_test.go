package names

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type checkDiskVector struct {
	ID    *string `json:"id"`
	Mount *string `json:"mount"`
	Root  *string `json:"root"`
}

type checkFindingVector struct {
	Kind    FindingKind `json:"kind"`
	Path    *string     `json:"path"`
	PathB64 *string     `json:"path_b64"`
	FS      Filesystem  `json:"fs"`
	Reasons []Reason    `json:"reasons"`
	With    []string    `json:"with"`
	Disk    string      `json:"disk"`
}

type checkVector struct {
	Paths    *[]string             `json:"paths"`
	PathsB64 []string              `json:"paths_b64"`
	Disks    *[]checkDiskVector    `json:"disks"`
	Under    *string               `json:"under"`
	UnderB64 *string               `json:"under_b64"`
	Checked  *int                  `json:"checked"`
	Findings *[]checkFindingVector `json:"findings"`
	Error    bool                  `json:"error"`
	Note     string                `json:"note"`
}

// input turns a case into the arguments of Check.
func (v checkVector) input() ([]string, []CheckedDisk, string, error) {
	paths := slices.Clone(*v.Paths)
	for i := range v.PathsB64 {
		p, err := vectors.Path(nil, &v.PathsB64[i])
		if err != nil {
			return nil, nil, "", err
		}
		paths = append(paths, p)
	}
	disks := make([]CheckedDisk, 0, len(*v.Disks))
	for _, d := range *v.Disks {
		disks = append(disks, CheckedDisk{*d.ID, *d.Mount, *d.Root})
	}
	under := ""
	if v.Under != nil || v.UnderB64 != nil {
		u, err := vectors.Path(v.Under, v.UnderB64)
		if err != nil {
			return nil, nil, "", err
		}
		under = u
	}
	return paths, disks, under, nil
}

func (v checkVector) wantReport() (Report, error) {
	want := Report{Checked: *v.Checked, Findings: []Finding{}}
	for _, f := range *v.Findings {
		path, err := vectors.Path(f.Path, f.PathB64)
		if err != nil {
			return Report{}, err
		}
		want.Findings = append(want.Findings, Finding{f.Kind, path, f.FS, f.Reasons, f.With, f.Disk})
	}
	return want, nil
}

func TestCheckVectors(t *testing.T) {
	for i, v := range vectors.Load[checkVector](t, "names_check.json") {
		if v.Paths == nil || v.Disks == nil {
			t.Errorf("vector %d (%s): want paths and disks", i, v.Note)
			continue
		}
		if v.Error == (v.Findings != nil) || (v.Checked != nil) != (v.Findings != nil) {
			t.Errorf("vector %d (%s): want checked and findings, or error", i, v.Note)
			continue
		}
		if slices.ContainsFunc(*v.Disks, func(d checkDiskVector) bool { return d.ID == nil || d.Mount == nil || d.Root == nil }) {
			t.Errorf("vector %d (%s): a disk needs id, mount and root", i, v.Note)
			continue
		}
		paths, disks, under, err := v.input()
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		got, err := Check(paths, disks, under)
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
		want, err := v.wantReport()
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, got, want)
		}
	}
}

// The report and the solver's check agree (specs.md §15.9): for each path, Find
// on a target holding every other path gives what the report says about the
// first of its prefixes with a collision finding.
func TestCheckAgreesWithTargetNames(t *testing.T) {
	components := []string{
		"a", "A", "b", "B", "café", "café", "CAFÉ",
		"ς", "σ", "ß", "ẞ", "x\xff", "X\xff",
	}
	state := uint64(0x9E3779B97F4A7C15)
	next := func(n int) int {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return int(state % uint64(n))
	}
	for range 3000 {
		var paths []string
		for range 1 + next(10) {
			parts := make([]string, 1+next(3))
			for i := range parts {
				parts[i] = components[next(len(components))]
			}
			paths = append(paths, strings.Join(parts, "/"))
		}
		report, err := Check(paths, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		with := map[string][]string{}
		for _, f := range report.Findings {
			if f.Kind == FindingCollision {
				with[f.Path] = f.With
			}
		}
		for _, p := range paths {
			target := NewTargetNames()
			for _, other := range paths {
				if other != p {
					if err := target.Add(other); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := target.Find(p)
			if err != nil {
				t.Fatal(err)
			}
			var want *Collision
			parts := strings.Split(p, "/")
			for depth := range parts {
				if w, ok := with[strings.Join(parts[:depth+1], "/")]; ok {
					want = &Collision{Component: depth, With: w}
					break
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("paths %+q: Find(%+q) = %+v, the report says %+v", paths, p, got, want)
			}
		}
	}
}

// A folder with many files below it gives as many findings as a folder with one.
func TestCheckFindingsDoNotGrowWithFiles(t *testing.T) {
	disks := []CheckedDisk{{"t1", "/mnt/t1", "media"}}
	one, err := Check([]string{"a:b/1"}, disks, "")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 1000)
	for i := range 1000 {
		paths = append(paths, "a:b/"+strings.Repeat("x", 1+i%100)+string(rune('a'+i%26)))
	}
	many, err := Check(paths, disks, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one.Findings, many.Findings) {
		t.Errorf("one file gives %+v, many give %+v", one.Findings, many.Findings)
	}
}
