package ops

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// FuzzResolve: the reprise of an operation left planned decides from what each of its names holds now (specs.md
// §17.2), which anyone who can write the disk may have changed. Whatever it reads, a word it does not know is an
// error, and a decision never removes a name that holds anything but the file the operation moved, nor the only name
// left holding it, nor moves a file back over a name that holds one; seeded from conformance/vectors/op_recovery.json.
func FuzzResolve(f *testing.F) {
	for _, v := range vectors.Load[recoveryVector](f, "op_recovery.json") {
		var target, source string
		if v.Target != nil {
			target = string(*v.Target)
		}
		if v.Source != nil {
			source = string(*v.Source)
		}
		f.Add(*v.Kind, target, source, v.Changed)
	}
	f.Fuzz(func(t *testing.T, kind, target, source string, changed bool) {
		var d Decision
		var err error
		var names []string
		switch kind {
		case "trash":
			d, err = ResolveTrash(Seen(target), Seen(source), changed)
			names = []string{target, source}
		case "restore":
			d, err = ResolveRestore(Seen(target), Seen(source))
			names = []string{target, source}
		case "rename":
			d, err = ResolveRename(Seen(target), Seen(source))
			names = []string{target, source}
		case "unlink":
			d, err = ResolveUnlink(Seen(source))
			names, target = []string{source}, ""
		case "rmdir":
			d, err = ResolveRmdir(Seen(source))
			names, target = []string{source}, ""
		case "mkdir":
			d, err = ResolveMkdir(Seen(target))
			names, source = []string{target}, ""
		default:
			return
		}
		known := true
		for _, n := range names {
			known = known && (n == string(SeenAbsent) || n == string(SeenSame) || n == string(SeenOther))
		}
		if (err == nil) != known {
			t.Fatalf("%s of %q, %q: %+v, %v", kind, target, source, d, err)
		}
		if err != nil {
			return
		}
		same := func(s string) bool { return s == string(SeenSame) }
		switch {
		case d.RemoveSource && d.RemoveTarget:
			t.Fatalf("%s of %q, %q: both names removed: %+v", kind, target, source, d)
		case d.RemoveSource && !same(target), d.RemoveTarget && !same(source):
			t.Fatalf("%s of %q, %q: a name removed that the other does not stand in for: %+v", kind, target, source, d)
		case d.RemoveSource && !same(source), d.RemoveTarget && !same(target):
			t.Fatalf("%s of %q, %q: a name removed that holds what the operation did not move: %+v", kind, target, source, d)
		case d.MoveBack && source != string(SeenAbsent):
			t.Fatalf("%s of %q, %q: a move back onto a name that holds something: %+v", kind, target, source, d)
		case d.Finish && (d.RemoveTarget || d.MoveBack):
			t.Fatalf("%s of %q, %q: finished and undone: %+v", kind, target, source, d)
		case (kind == "unlink" || kind == "rmdir") && d.Finish && source != string(SeenAbsent):
			t.Fatalf("%s of %q: finished while its name still holds something: %+v", kind, source, d)
		case kind == "mkdir" && d.Finish && !same(target):
			t.Fatalf("mkdir of %q: finished with no folder there: %+v", target, d)
		}
	})
}
