package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// A confirm of the mass-modification guard holds for the guard's window (specs.md §15.6): with no daemon to tell of
// it, the command says how long a pass has to start. The answer is recorded all the same, and with nothing to answer
// there is nothing to say. A reject keeps what it covered for good (flaw 27, reject-kept): it has no window to say.
func TestAGuardAnswerSaysHowLongItHolds(t *testing.T) {
	configPath, c := apiPool(t, "[modifies.mass_guard]\nwindow = \"7m\"\n")
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.WriteModifyGuardEvent(t.Context(), db, "t1", "", 1_000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	confirm := []string{"--config", configPath, "guard", "confirm", "--disk", "t1", "--kind", "modifications"}
	var stdout, stderr bytes.Buffer
	if code := run(confirm, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "t1: confirm up to ") || !strings.Contains(stderr.String(), "no daemon runs: the answer holds for 7m") {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(confirm, &stdout, &stderr); code != exitOK || !strings.Contains(stdout.String(), "nothing to confirm") || stderr.Len() != 0 {
		t.Errorf("then exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	db, err = catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.WriteModifyGuardEvent(t.Context(), db, "t1", "", 2_000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	reject := []string{"--config", configPath, "guard", "reject", "--disk", "t1", "--kind", "modifications"}
	if code := run(reject, &stdout, &stderr); code != exitOK || !strings.Contains(stdout.String(), "t1: reject up to ") || stderr.Len() != 0 {
		t.Errorf("a reject: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// A hold the day tripped says so in atoll guard list, with the day's count and the files it counted, and an answer's
// dry run speaks of those; a hold the window tripped says nothing of a window, as before (specs.md §15.6).
func TestGuardListSaysTheDayTripped(t *testing.T) {
	configPath, c := apiPool(t, "[modifies.mass_guard]\nwindow = \"10m\"\n[modifies.day_guard]\nwindow = \"24h\"\n")
	db, err := catalog.OpenPool(t.Context(), catalogPath(c), "p")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	for i, at := range []int64{now - int64(30*time.Hour), now - int64(5*time.Hour), now - int64(2*time.Hour), now - int64(time.Minute)} {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO events(ts_ns, level, kind, data) VALUES (?, 'info', 'modified', ?)",
			at, `{"disk":"t1","path":"`+string(rune('a'+i))+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.WriteModifyGuardEvent(t.Context(), db, "t1", catalog.WindowDay, now-int64(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	held := []pathItem{{Path: "b"}, {Path: "c"}, {Path: "d"}}
	out, code := runCLI(t, "--config", configPath, "guard", "list", "--json")
	var list struct {
		Disks []guardItem `json:"disks"`
	}
	want := []guardItem{
		{Disk: "t1", Kind: "deletions", Paths: []pathItem{}},
		{Disk: "t1", Kind: "modifications", Tripped: true, Count: 3, Paths: held, Window: "day"},
	}
	if err := json.Unmarshal([]byte(out), &list); code != exitOK || err != nil || !reflect.DeepEqual(list.Disks, want) {
		t.Errorf("exit %d, %v:\n%s", code, err, out)
	}
	if out, code := runCLI(t, "--config", configPath, "guard", "list"); code != exitOK || !strings.Contains(out, "t1, modifications: tripped, 3 within a day\n") {
		t.Errorf("exit %d: %q", code, out)
	}
	out, code = runCLI(t, "--config", configPath, "guard", "confirm", "--disk", "t1", "--kind", "modifications", "--dry-run", "--json")
	var answer guardAnswer
	if err := json.Unmarshal([]byte(out), &answer); code != exitOK || err != nil || answer.Paths == nil || !reflect.DeepEqual(*answer.Paths, held) {
		t.Errorf("the dry run: exit %d, %v: %s", code, err, out)
	}
}
