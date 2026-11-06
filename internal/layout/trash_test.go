package layout

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/vectors"
)

type trashPathVector struct {
	StartedNS    *int64  `json:"started_ns"`
	OpID         *int64  `json:"op_id"`
	Path         *string `json:"path"`
	PathB64      *string `json:"path_b64"`
	TrashPath    *string `json:"trash_path"`
	TrashPathB64 *string `json:"trash_path_b64"`
	Error        bool    `json:"error"`
	Note         string  `json:"note"`
}

func oneOf(t *testing.T, text, b64 *string) (string, bool) {
	t.Helper()
	switch {
	case text != nil && b64 != nil:
		t.Fatal("both forms")
	case text != nil:
		return *text, true
	case b64 != nil:
		raw, err := base64.StdEncoding.DecodeString(*b64)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw), true
	}
	return "", false
}

// The day is the day in UTC, wherever the machine is: both versions must find the same folder.
func TestTrashPathIsInUTC(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("far", 14*3600)
	t.Cleanup(func() { time.Local = local })
	got, err := TrashPath(1767268800000000000, 1, "a") // 12:00 on 2026-01-01 in UTC, 02:00 on the 2nd there
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-01-01/1/a" {
		t.Errorf("%q", got)
	}
}

func TestTrashPathVectors(t *testing.T) {
	for i, v := range vectors.Load[trashPathVector](t, "trash_path.json") {
		path, ok := oneOf(t, v.Path, v.PathB64)
		if !ok || v.StartedNS == nil || v.OpID == nil {
			t.Fatalf("vector %d: want started_ns, op_id and a path", i)
		}
		want, hasWant := oneOf(t, v.TrashPath, v.TrashPathB64)
		if v.Error == hasWant {
			t.Fatalf("vector %d: want exactly one of trash_path and error", i)
		}
		got, err := TrashPath(*v.StartedNS, *v.OpID, path)
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): %q, want an error", i, v.Note, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("vector %d (%s): %q, %v, want %q", i, v.Note, got, err, want)
		}
	}
}
