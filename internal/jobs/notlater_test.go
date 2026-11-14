package jobs

import (
	"slices"
	"testing"
)

// TestACopyNoUserChangedIsAConflict: a changed copy whose change time did not move past what its row says is no
// user's change, as any change moves the change time forward and no program sets it back: a power cut that left a
// file empty or short, or a disk put back from an older image (specs.md §15.4, §15.6). Its file is a conflict, and the
// other copy stays as it is, where it was made stale and overwritten with what the power cut left.
func TestACopyNoUserChangedIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		// text is what the copy on t1 holds now; ahead how far its row's change time is past its own.
		text  string
		ahead int64
		want  string
	}{
		{"emptied, the row's change time the copy's", "", 0, "conflict"},
		{"shorter, the row's change time later", "x", 1_000_000_000, "conflict"},
		{"of the file's size, the row's change time later", "zzzzzzz", 1_000_000_000, "conflict"},
		// What moved the change time forward is a user's change.
		{"emptied, the change time later than the row's", "", -1, "modified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.write("t1", "a", "content")
			b.write("t2", "a", "content")
			b.scan(true)
			b.changeAgain("t1", "a", tc.text)
			ctime := b.ctimeOf("t1", "a")
			// The row recorded a change time at least as late as what the disk holds now.
			b.exec("UPDATE copies SET ctime_ns = ? WHERE disk_id = 't1'", ctime+tc.ahead)
			b.now = ctime + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: tc.want}}) {
				t.Fatalf("%+v", out)
			}
			if tc.want == "modified" {
				return
			}
			if len(out.Operations) != 0 {
				t.Errorf("operations: %+v", out.Operations)
			}
			if got := b.text("SELECT state FROM files"); got != "conflict" {
				t.Errorf("the file is %s", got)
			}
			if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
				t.Errorf("the other copy is %s", got)
			}
			if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
				t.Errorf("%d jobs", got)
			}
			// Nothing reaches the other copy at the next pass either: every version stays until the conflict is
			// resolved.
			b.now += settleNS
			b.pass()
			if b.read("t2", "a") != "content" || b.read("t1", "a") != tc.text {
				t.Errorf("t1 holds %q and t2 %q", b.read("t1", "a"), b.read("t2", "a"))
			}
		})
	}
}
