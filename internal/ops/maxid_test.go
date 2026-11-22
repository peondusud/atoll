package ops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/watch"
)

// An id a name of the work folder gives past those a catalog gives, catalog.MaxID, is none of Atoll's: a new catalog
// starts its operations and its files past the others alone, and a rebuild lists no entry of the trash at it, but
// says so and leaves it. Taken, one such name made the catalog's counter reach the largest id, and the catalog could
// record no other operation or file (scrutineer F020, 2026-11-22). The id catalog.MaxID itself is still one.
func TestNoIDPastThoseACatalogGivesIsTaken(t *testing.T) {
	mount := t.TempDir()
	for _, file := range []string{
		"trash/2026-01-02/9007199254740991/a/at.mkv",
		"trash/2026-01-02/9007199254740992/past.mkv",
		"trash/2026-01-02/9223372036854775807/largest.mkv",
		"trash/2026-01-02/9223372036854775807.chunks/c.mkv/00000000",
		"tmp/9223372036854775807.part",
		"tmp/9007199254740992.3.part",
		"links/9223372036854775807",
		"links/9007199254740992",
		"links/12",
	} {
		path := filepath.Join(mount, "Atoll", filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := Open(&onDisk{}, &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, err := d.HighestOpID(); err != nil || got != 9007199254740991 {
		t.Errorf("the highest operation: %d, %v", got, err)
	}
	if got, err := d.HighestLinkID(); err != nil || got != 12 {
		t.Errorf("the highest file: %d, %v", got, err)
	}
	var left []string
	found, err := d.FoundTrash(func(place, why string) { left = append(left, place+": "+why) }, watch.Unbounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].OpID != 9007199254740991 || found[0].Path != "a/at.mkv" {
		t.Errorf("found %+v", found)
	}
	slices.Sort(left)
	want := []string{
		"2026-01-02/9007199254740992: an id past those a catalog gives",
		"2026-01-02/9223372036854775807.chunks: an id past those a catalog gives",
		"2026-01-02/9223372036854775807: an id past those a catalog gives",
	}
	if !slices.Equal(left, want) {
		t.Errorf("left %q", strings.Join(left, "; "))
	}
}
