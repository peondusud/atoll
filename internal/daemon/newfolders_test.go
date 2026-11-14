package daemon

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/jobs"
)

// The folders where a pass's walks found new copies are told to the media servers with those of its operations
// (specs.md §21.5).
func TestAPassTellsTheFoldersOfNewCopies(t *testing.T) {
	s := &passSink{folders: map[string]bool{}}
	if err := s.Disks([]jobs.DiskLine{{ID: "t1", NewFolders: []string{"Films", ""}}, {ID: "t2", NewFolders: []string{"Films", "Shows/S1"}}}); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(s.folders); !slices.Equal(got, []string{"", "Films", "Shows/S1"}) {
		t.Errorf("%q", got)
	}
}
