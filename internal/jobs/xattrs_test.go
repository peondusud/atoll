package jobs

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/view/writer"
)

// xattrsOf gives the user. extended attributes of a path below a disk's top, and skips a test on a filesystem that keeps
// none.
func (b *bench) xattrsOf(id, path string) map[string]string {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], path)
	size, err := syscall.Listxattr(full, nil)
	if errors.Is(err, syscall.ENOTSUP) {
		b.t.Skip("this filesystem keeps no extended attribute")
	}
	if err != nil {
		b.t.Fatal(err)
	}
	list := make([]byte, size)
	if size, err = syscall.Listxattr(full, list); err != nil {
		b.t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range strings.Split(string(list[:size]), "\x00") {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		value := make([]byte, 1024)
		n, err := syscall.Getxattr(full, name, value)
		if err != nil {
			b.t.Fatal(err)
		}
		out[name] = string(value[:n])
	}
	return out
}

// setXattr sets a user. extended attribute directly on a disk, as a program does that writes there.
func (b *bench) setXattr(id, path, name, value string) {
	b.t.Helper()
	if err := syscall.Setxattr(filepath.Join(b.dirs[id], path), name, []byte(value), 0); err != nil {
		b.t.Fatal(err)
	}
}

// xattrPool gives a pool of two disks with Films/a.mkv on both, read whole, a view over it, the disk the view presents
// the file from and the other. Copies wait for no settle, and the clock is the real one, as setting an attribute moves
// the change time of a copy, which the disk gives.
func xattrPool(t *testing.T, more string) (b *bench, w *writer.Writer, disk, other string) {
	t.Helper()
	b = newBenchWith(t, "[defaults]\nsettle = \"0s\"\n"+more, "t1", "t2")
	b.now = time.Now().UnixNano()
	b.write("t1", "Films/a.mkv", "a film")
	b.write("t2", "Films/a.mkv", "a film")
	_ = b.xattrsOf("t1", "media/Films/a.mkv")
	b.scan(true)
	w = b.viewOf()
	n, err := w.Tree().Lookup(t.Context(), "Films/a.mkv")
	if err != nil || n == nil {
		t.Fatalf("the file presents %+v, %v", n, err)
	}
	return b, w, n.Disk, map[string]string{"t1": "t2", "t2": "t1"}[n.Disk]
}

// setThroughView sets an attribute of a file through the view, as Samba does.
func setThroughView(b *bench, w *writer.Writer, path, name, value string) {
	b.t.Helper()
	n, err := w.Tree().Lookup(b.t.Context(), path)
	if err != nil || n == nil {
		b.t.Fatalf("%s presents %+v, %v", path, n, err)
	}
	if err := w.SetXattr(b.t.Context(), path, n, name, []byte(value), 0); err != nil {
		b.t.Fatal(err)
	}
}

// settleAll runs passes, the debounce gone by between them, and gives every operation they ran.
func (b *bench) settleAll(passes int) []Operation {
	b.t.Helper()
	var all []Operation
	for range passes {
		all = append(all, b.pass().Operations...)
		b.now += 60_000_000_000
	}
	return all
}

func xattrOutcomes(list []Operation) []string {
	var out []string
	for i := range list {
		if list[i].Kind == KindXattr {
			out = append(out, list[i].Disk+":"+list[i].Outcome)
		}
	}
	return out
}

// An extended attribute set or removed through Atoll's own view, on the copy it presents, reaches the file's other copy
// at the next pass, one operation xattr, with nothing copied again and no copy made stale (specs.md §21.2); the pass
// then takes the view's operation in, and a pass after it has nothing to do.
func TestAPassGivesTheOtherCopyAnAttributeSetThroughTheView(t *testing.T) {
	b, w, disk, other := xattrPool(t, "")
	setThroughView(b, w, "Films/a.mkv", "user.DOSATTRIB", "0x20")
	setThroughView(b, w, "Films/a.mkv", "user.org.netatalk.Metadata", "")
	if got := b.xattrsOf(other, "media/Films/a.mkv"); len(got) != 0 {
		t.Fatalf("the other copy took it at once: %v", got)
	}
	ops := b.settleAll(2)
	if got := strings.Join(xattrOutcomes(ops), " "); got != other+":applied" {
		t.Errorf("operations xattr %q, want one applied on %s", got, other)
	}
	want := map[string]string{"user.DOSATTRIB": "0x20", "user.org.netatalk.Metadata": ""}
	for _, id := range []string{disk, other} {
		if got := b.xattrsOf(id, "media/Films/a.mkv"); len(got) != 2 || got["user.DOSATTRIB"] != "0x20" || got["user.org.netatalk.Metadata"] != "" {
			t.Errorf("%s holds %v, want %v", id, got, want)
		}
	}
	if got := b.holders("Films/a.mkv"); got != "t1:user:ok t2:user:ok" {
		t.Errorf("copies %s", got)
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d jobs: something is copied again", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
	if again := b.pass(); len(again.Operations) != 0 || len(again.Decisions) != 0 {
		t.Errorf("a pass after: %+v, %+v", again.Decisions, again.Operations)
	}

	n, err := w.Tree().Lookup(t.Context(), "Films/a.mkv")
	if err != nil || n == nil {
		t.Fatal(err)
	}
	if err := w.RemoveXattr(t.Context(), "Films/a.mkv", n, "user.org.netatalk.Metadata"); err != nil {
		t.Fatal(err)
	}
	b.settleAll(2)
	if got := b.xattrsOf(other, "media/Films/a.mkv"); len(got) != 1 || got["user.DOSATTRIB"] != "0x20" {
		t.Errorf("after a removal through the view, %s holds %v", other, got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
}

// A copy whose filesystem refuses an attribute keeps what it held, kept, and is never made stale for it: the copy holds
// the same file all the same (specs.md §21.2).
func TestACopyThatRefusesAnAttributeIsKept(t *testing.T) {
	b, w, _, other := xattrPool(t, "")
	b.fail[b.dirs[other]] = func(call, _ string) error {
		if call == "setxattr" {
			return syscall.ENOTSUP
		}
		return nil
	}
	setThroughView(b, w, "Films/a.mkv", "user.DOSATTRIB", "0x20")
	ops := b.settleAll(2)
	if got := strings.Join(xattrOutcomes(ops), " "); got != other+":kept" {
		t.Errorf("operations xattr %q, want one kept on %s", got, other)
	}
	if got := b.xattrsOf(other, "media/Films/a.mkv"); len(got) != 0 {
		t.Errorf("%s holds %v", other, got)
	}
	if got := b.holders("Films/a.mkv"); got != "t1:user:ok t2:user:ok" {
		t.Errorf("copies %s", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
	delete(b.fail, b.dirs[other])
	if again := b.settleAll(1); len(xattrOutcomes(again)) != 0 {
		t.Errorf("given again: %+v", again)
	}
}

// A disk that fails as a copy takes an attribute is lost, and its operation stays planned; a crash once the attributes
// are on the copy leaves it planned too. The reprise undoes each, as nothing says which attribute reached the copy, and
// the next pass gives what still differs, with the view's operation open until then (specs.md §17.2, §21.2).
func TestAnAttributeGivenAgainAfterAFailureOrACrash(t *testing.T) {
	t.Run("EIO", func(t *testing.T) {
		b, w, _, other := xattrPool(t, "")
		b.fail[b.dirs[other]] = func(call, _ string) error {
			if call == "setxattr" {
				return syscall.EIO
			}
			return nil
		}
		setThroughView(b, w, "Films/a.mkv", "user.DOSATTRIB", "0x20")
		ops := b.settleAll(1)
		if got := strings.Join(xattrOutcomes(ops), " "); got != other+":io-error" {
			t.Errorf("operations xattr %q, want io-error on %s", got, other)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'xattr' AND state = 'planned'"); got != 1 {
			t.Errorf("%d operations left planned, want 1", got)
		}
		if open := b.openViewOps(); len(open) != 1 {
			t.Errorf("taken in while a copy lacks it: %+v", open)
		}
		delete(b.fail, b.dirs[other])
		b.settleAll(3)
		if got := b.xattrsOf(other, "media/Films/a.mkv"); got["user.DOSATTRIB"] != "0x20" {
			t.Errorf("%s holds %v once back", other, got)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'xattr' AND state = 'planned'"); got != 0 {
			t.Errorf("%d operations left planned", got)
		}
		if open := b.openViewOps(); len(open) != 0 {
			t.Errorf("not taken in: %+v", open)
		}
	})
	t.Run("kill -9", func(t *testing.T) {
		b, w, _, other := xattrPool(t, "")
		setThroughView(b, w, "Films/a.mkv", "user.DOSATTRIB", "0x20")
		b.crashAt("xattrs-set")
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'xattr' AND state = 'planned'"); got != 1 {
			t.Fatalf("%d operations left planned, want 1", got)
		}
		b.settleAll(3)
		if got := b.xattrsOf(other, "media/Films/a.mkv"); got["user.DOSATTRIB"] != "0x20" {
			t.Errorf("%s holds %v", other, got)
		}
		if got := b.text("SELECT group_concat(state, ' ') FROM (SELECT state FROM ops WHERE kind = 'xattr' ORDER BY id)"); got != "undone" {
			t.Errorf("the operations xattr: %s, want the one the reprise undid, and none more", got)
		}
		if got := b.holders("Films/a.mkv"); got != "t1:user:ok t2:user:ok" {
			t.Errorf("copies %s", got)
		}
		if open := b.openViewOps(); len(open) != 0 {
			t.Errorf("not taken in: %+v", open)
		}
	})
}

// A replica takes its source's user. extended attributes, as it restores its metadata, before it is synced and read
// back; a target that refuses one still gets the copy, and an event xattr-kept says what it went without (specs.md
// §14).
func TestAReplicaTakesItsSourcesAttributes(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "content")
	_ = b.xattrsOf("t1", "media/a")
	b.setXattr("t1", "media/a", "user.DOSATTRIB", "0x20")
	b.setXattr("t1", "media/a", "user.b", "two")
	b.scan(false)
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if got := b.xattrsOf("t2", "media/a"); len(got) != 2 || got["user.DOSATTRIB"] != "0x20" || got["user.b"] != "two" {
		t.Errorf("the replica holds %v", got)
	}

	refused := newBench(t, "t1", "t2")
	refused.write("t1", "a", "content")
	refused.setXattr("t1", "media/a", "user.b", "two")
	refused.scan(false)
	refused.fail[refused.dirs["t2"]] = func(call, _ string) error {
		if call == "setxattr" {
			return syscall.E2BIG
		}
		return nil
	}
	items, _ = refused.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if got := refused.xattrsOf("t2", "media/a"); len(got) != 0 {
		t.Errorf("the replica holds %v", got)
	}
	if got := refused.text("SELECT json_extract(data, '$.names') FROM events WHERE kind = 'xattr-kept' AND json_extract(data, '$.disk') = 't2'"); got != `["user.b"]` {
		t.Errorf("the event: %s", got)
	}
}
