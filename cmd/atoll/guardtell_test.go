package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// An answer to the mass-modification guard holds for the guard's window (specs.md §15.6): with no daemon to tell of
// it, the command says how long a pass has to start. The answer is recorded all the same, and with nothing to answer
// there is nothing to say.
func TestAGuardAnswerSaysHowLongItHolds(t *testing.T) {
	configPath, c := apiPool(t, "[modifies.mass_guard]\nwindow = \"7m\"\n")
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.WriteModifyGuardEvent(t.Context(), db, "t1", 1_000); err != nil {
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
}
