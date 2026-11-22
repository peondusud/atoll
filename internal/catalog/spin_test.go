package catalog

import "testing"

// The daemon's check records a disk in standby, or online again, and nothing else does: a sync that finds the disk
// mounted keeps standby, as a command's mount check cannot tell a disk asleep; one that finds it missing, readonly or
// draining writes that state, which the daemon then leaves alone (specs.md §10).
func TestTheSpinStateIsTheDaemons(t *testing.T) {
	db, _ := newPool(t)
	sync := func(state string) {
		t.Helper()
		d := oneDisk("a", "ua", 0)
		d.State = state
		if err := SyncDisks(t.Context(), db, []Disk{d}); err != nil {
			t.Fatal(err)
		}
	}
	stateOf := func() string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(t.Context(), "SELECT state FROM disks WHERE id = 'a'").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	set := func(standby bool) bool {
		t.Helper()
		ok, err := SetSpin(t.Context(), db, "a", standby)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if set(true) {
		t.Error("a disk the catalog does not hold took a state")
	}
	sync("online")
	if !set(true) || stateOf() != "standby" {
		t.Fatalf("standby: %s", stateOf())
	}
	sync("online")
	if stateOf() != "standby" {
		t.Errorf("a sync that finds it mounted: %s", stateOf())
	}
	if !set(false) || stateOf() != "online" {
		t.Errorf("online again: %s", stateOf())
	}
	for _, state := range []string{"missing", "readonly", "draining"} {
		sync(state)
		if set(true) || stateOf() != state {
			t.Errorf("%s: the daemon wrote %s", state, stateOf())
		}
	}
	sync("online")
	if stateOf() != "online" {
		t.Errorf("found again: %s", stateOf())
	}
}
