package jobs

import (
	"maps"
	"testing"

	"github.com/peondusud/atoll/internal/config"
)

// A start of the daemon records the physical disks of each disk it reads; a dry run records nothing, and neither does a
// pass of the daemon that runs for good, after its start (specs.md §10, R-06).
func TestAStartOfTheDaemonRecordsThePhysicalDisks(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.scan(false)
	m := &topology{bench: b, physical: map[string][]string{b.dirs["t1"]: {"sda"}, b.dirs["t2"]: {"sdb"}}}
	none := map[string]string{"t1": "", "t2": ""}
	if _, err := Run(t.Context(), b.db, b.config, m, false); err != nil {
		t.Fatal(err)
	}
	if got := b.devicesOf(); !maps.Equal(got, none) {
		t.Errorf("a dry run recorded %v", got)
	}
	if _, err := RunClosed(t.Context(), b.db, b.config, m, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.devicesOf(); !maps.Equal(got, none) {
		t.Errorf("a pass after the start recorded %v", got)
	}
	if _, err := Run(t.Context(), b.db, b.config, m, true); err != nil {
		t.Fatal(err)
	}
	if got := b.devicesOf(); !maps.Equal(got, map[string]string{"t1": "sda", "t2": "sdb"}) {
		t.Errorf("a start recorded %v", got)
	}
}

// A pass reads every disk after one the configuration marks failed, which it never reads again and gives no line; and
// one of the daemon that walks only some disks spares each disk before the one it walks (specs.md §15.13, §18).
func TestAPassGoesOnPastADiskItLeaves(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.scan(false)
	b.config.Disks[0].State = config.DiskFailed
	out := b.pass()
	if lineOf(out, "t1") != nil || lineOf(out, "t2") == nil || !lineOf(out, "t2").Walked || lineOf(out, "t3") == nil || !lineOf(out, "t3").Walked {
		t.Errorf("past a failed disk: %+v", out.Disks)
	}
	b.config.Disks[0].State = ""
	out = b.passOn("t3")
	if t1, t2, t3 := lineOf(out, "t1"), lineOf(out, "t2"), lineOf(out, "t3"); t1 == nil || !t1.Spared || t2 == nil || !t2.Spared || t3 == nil || !t3.Walked {
		t.Errorf("only t3 asked for: %+v", out.Disks)
	}
}

// A pass sums the bytes of every copy it makes (specs.md §15.12).
func TestAPassSumsTheBytesItCopies(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yyy")
	b.scan(true)
	if out := b.pass(); out.Copied != 4 {
		t.Errorf("copied %d bytes: %+v", out.Copied, out.Operations)
	}
}
