package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// passPowered runs a pass of the daemon, which asks onBattery whether the UPS runs on battery before each copy and each
// transfer it would start (specs.md §20).
func (b *bench) passPowered(onBattery OnBattery) *Outcome {
	b.t.Helper()
	out, err := RunLaterOn(b.t.Context(), b.db, b.config, b, true, nil, nil, nil, onBattery)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

func always(on bool) OnBattery {
	return func() bool { return on }
}

// On battery, no copy starts: the file waits, counted, and with no time it is due at, as the daemon passes again once
// line power is back. Then the copy comes.
func TestNoCopyStartsOnBattery(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(false)
	out := b.passPowered(always(true))
	if got := outcomeOf(out, KindReplicate, "a"); got != "" || b.exists("t2", "media/a") {
		t.Fatalf("a copy on battery: %q", got)
	}
	if out.Waiting != 1 || out.NextDue.Valid {
		t.Errorf("waiting %d, next due %v", out.Waiting, out.NextDue)
	}
	out = b.passPowered(always(false))
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeReplicated || !b.exists("t2", "media/a") {
		t.Fatalf("on line power: %q", got)
	}
}

// The battery takes over while a copy runs: that copy finishes, verified and in its place, and the next one does not
// start.
func TestACopyUnderWayFinishesOnBattery(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "first")
	b.write("t1", "b", "second")
	b.scan(false)
	onBattery := false
	b.at["copied"] = func() { onBattery = true }
	out := b.passPowered(func() bool { return onBattery })
	var made, held []string
	for _, path := range []string{"a", "b"} {
		switch outcomeOf(out, KindReplicate, path) {
		case OutcomeReplicated:
			made = append(made, path)
		case "":
			held = append(held, path)
		}
	}
	if len(made) != 1 || len(held) != 1 {
		t.Fatalf("made %v, held %v: %+v", made, held, out.Operations)
	}
	data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", made[0]))
	if err != nil || string(data) != map[string]string{"a": "first", "b": "second"}[made[0]] {
		t.Errorf("the copy made: %q, %v", data, err)
	}
	if b.exists("t2", "media/"+held[0]) {
		t.Errorf("%s was copied on battery", held[0])
	}
	if b.count("SELECT count(*) FROM ops WHERE state = 'planned'") != 0 {
		t.Error("an operation left planned")
	}
}

// A dry run on battery lists no copy either.
func TestADryRunOnBatteryListsNoCopy(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(false)
	out, err := RunLaterOn(t.Context(), b.db, b.config, b, false, nil, nil, nil, always(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(out, KindReplicate, "a"); got != "" {
		t.Errorf("%q", got)
	}
	out, err = RunLaterOn(t.Context(), b.db, b.config, b, false, nil, nil, nil, always(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeToReplicate {
		t.Errorf("on line power: %q", got)
	}
}

// On battery, a file whose copies the data disks hold does not leave the SSD cache: its transfer waits, and runs on
// line power (specs.md §19, §20).
func TestATransferWaitsOnBattery(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	for _, id := range []string{"c", "t1", "t2"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	out := b.passPowered(always(true))
	if got := outcomeOf(out, KindTransfer, "a"); got != "" || !b.exists("c", "media/a") {
		t.Fatalf("a transfer on battery: %q", got)
	}
	if out.Waiting == 0 {
		t.Error("nothing waits")
	}
	out = b.passPowered(always(false))
	if got := outcomeOf(out, KindTransfer, "a"); got != OutcomeTransferred || b.exists("c", "media/a") {
		t.Fatalf("on line power: %q", got)
	}
}

// Balancing on battery starts no file, and says that some are left.
func TestBalancingStartsNothingOnBattery(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "a", "x")
	b.scan(true)
	if items, _ := b.replicate("a", ReplicateOptions{Now: true}); len(items) != 1 || items[0].Disk != "t2" {
		t.Fatalf("%+v", items)
	}
	b.base = strings.Replace(b.base, "id = \"t3\"\n", "id = \"t3\"\ntags = [\"usb\"]\n", 1)
	b.reconfigure("[[rule]]\nlabel = \"x\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	b.scan(false)
	moved := 0
	each := func(*BalanceItem) error {
		moved++
		return nil
	}
	sum, err := BalanceAtoll(t.Context(), b.db, b.config, b, 0, always(true), each)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 || !sum.Left || b.exists("t3", "media/a") {
		t.Fatalf("moved %d, left %v", moved, sum.Left)
	}
	if _, err := BalanceAtoll(t.Context(), b.db, b.config, b, 0, always(false), each); err != nil {
		t.Fatal(err)
	}
	if moved != 1 || !b.exists("t3", "media/a") {
		t.Errorf("on line power: moved %d", moved)
	}
}
