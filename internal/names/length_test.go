package names

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type branchPathVector struct {
	Mount   *string `json:"mount"`
	Root    *string `json:"root"`
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Fits    *bool   `json:"fits"`
	Error   bool    `json:"error"`
	Note    string  `json:"note"`
}

func TestFitsBranchVectors(t *testing.T) {
	for i, v := range vectors.Load[branchPathVector](t, "branch_path.json") {
		if v.Mount == nil || v.Root == nil {
			t.Errorf("vector %d: want mount and root", i)
			continue
		}
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Errorf("vector %d: %v", i, err)
			continue
		}
		got, err := FitsBranch(*v.Mount, *v.Root, path)
		checkFits(t, i, v.Note, v.Fits, v.Error, got, err)
	}
}

type windowsPathVector struct {
	Root    *string `json:"root"`
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Fits    *bool   `json:"fits"`
	Error   bool    `json:"error"`
	Note    string  `json:"note"`
}

func TestFitsWindowsVectors(t *testing.T) {
	for i, v := range vectors.Load[windowsPathVector](t, "windows_path.json") {
		if v.Root == nil {
			t.Errorf("vector %d: want root", i)
			continue
		}
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Errorf("vector %d: %v", i, err)
			continue
		}
		got, err := FitsWindows(*v.Root, path)
		checkFits(t, i, v.Note, v.Fits, v.Error, got, err)
	}
}

// checkFits names a failing case by its note: the paths run to 4 KiB.
func checkFits(t *testing.T, i int, note string, want *bool, wantErr, got bool, err error) {
	t.Helper()
	if wantErr == (want != nil) {
		t.Errorf("vector %d (%s): want exactly one of fits and error", i, note)
		return
	}
	switch {
	case wantErr && err == nil:
		t.Errorf("vector %d (%s): got %v, want an error", i, note, got)
	case !wantErr && err != nil:
		t.Errorf("vector %d (%s): %v", i, note, err)
	case !wantErr && got != *want:
		t.Errorf("vector %d (%s): got %v, want %v", i, note, got, *want)
	}
}
