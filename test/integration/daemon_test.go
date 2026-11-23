//go:build integration

package integration

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The daemon that runs for good copies at its start what the catalog holds below its level, then a file its watcher
// hears, with no walk asking for it: no rescan comes due in a week, and only the event calls for a pass (specs.md
// §15.1, §15.13). SIGTERM stops it with 0, its log in JSON lines on standard error, every copy at its source's path and
// modification time, and no hidden file on a disk. The scenarios cannot say this: their daemon runs --until-idle, its
// clock standing still, and changes reach it through rescans (§29.4).
func TestTheDaemonCopiesWhatItHears(t *testing.T) {
	p := newPool(t, map[string]string{"Films/a.mkv": "x"})
	d := p.start()
	p.waitFor("the copy of Films/a.mkv made at the start", func() bool { return p.holds("t2", "Films/a.mkv", "x") })
	p.write("t1", "Séries/Été/b.mkv", "made while the daemon runs")
	p.waitFor("the copy of the file it heard", func() bool { return p.holds("t2", "Séries/Été/b.mkv", "made while the daemon runs") })
	// The copy is at its name before the catalog records it (invariant 7): a stop in between leaves the operation to
	// the reprise of the next start.
	p.waitFor("the catalog to record both copies", func() bool { total, atLevel := p.status(); return total == 2 && atLevel == 2 })
	if code := d.stop(syscall.SIGTERM); code != 0 {
		t.Errorf("SIGTERM: exit %d\n%s", code, d.stderr.String())
	}
	d.logIsJSONLines()
	p.sameTimes("Films/a.mkv")
	p.sameTimes("Séries/Été/b.mkv")
	if total, atLevel := p.status(); total != 2 || atLevel != 2 {
		t.Errorf("%d files, %d at their level", total, atLevel)
	}
	p.noHiddenFile()
}

// What changes while the daemon is stopped, its first pass takes in when it starts again: a file made then is copied,
// and one removed then has its copy on the other disk sent to that disk's trash, never removed (invariant 10). SIGINT
// stops it as SIGTERM does.
func TestTheDaemonStartsAgainOnWhatChangedMeanwhile(t *testing.T) {
	p := newPool(t, map[string]string{"a": "kept", "gone": "removed while stopped"})
	d := p.start()
	p.waitFor("the copies made at the start", func() bool { total, atLevel := p.status(); return total == 2 && atLevel == 2 })
	if !p.holds("t2", "a", "kept") || !p.holds("t2", "gone", "removed while stopped") {
		t.Fatal("the catalog holds copies the disk does not")
	}
	if code := d.stop(syscall.SIGINT); code != 0 {
		t.Fatalf("SIGINT: exit %d\n%s", code, d.stderr.String())
	}
	p.write("t1", "new", "made while stopped")
	must(t, os.Remove(filepath.Join(p.mount("t1"), "media", "gone")))
	d = p.start()
	p.waitFor("the copy of the file made while it was stopped", func() bool { return p.holds("t2", "new", "made while stopped") })
	p.waitFor("the copy of the file removed to go to the trash", func() bool {
		_, err := os.Lstat(filepath.Join(p.mount("t2"), "media", "gone"))
		return os.IsNotExist(err) && p.inTrash("t2", "gone", "removed while stopped")
	})
	p.waitFor("the catalog to record the new file's copies", func() bool { total, atLevel := p.status(); return total == 2 && atLevel == 2 })
	if code := d.stop(syscall.SIGTERM); code != 0 {
		t.Errorf("SIGTERM: exit %d\n%s", code, d.stderr.String())
	}
	if !p.holds("t1", "a", "kept") || !p.holds("t2", "a", "kept") {
		t.Error("a file nothing changed lost a copy")
	}
	d.logIsJSONLines()
	p.noHiddenFile()
}

// The local API answers over its socket, mode 0660, what each command's --json gives, byte for byte, while the daemon
// runs (specs.md §25.1); SIGHUP makes it read its configuration again, and its answers follow the new one: a disk
// added there is listed, missing, as the folder is no disk of the control file (§15.13).
func TestTheAPIAnswersOverItsSocket(t *testing.T) {
	p := newPool(t, map[string]string{"a": "x"})
	d := p.start()
	p.waitFor("the copy of a", func() bool { return p.holds("t2", "a", "x") })
	st, err := os.Stat(p.socket)
	must(t, err)
	if st.Mode().Type() != os.ModeSocket || st.Mode().Perm() != 0o660 {
		t.Errorf("the socket: %v", st.Mode())
	}
	asTheCommand := func(path string, args ...string) bool {
		code, body := p.get(path)
		return code == http.StatusOK && body == p.run(0, args...)
	}
	for path, args := range map[string][]string{
		"/v1/status": {"status", "--json"}, "/v1/disks": {"disk", "list", "--json"}, "/v1/disks/t1": {"disk", "show", "t1", "--json"},
		"/v1/trash": {"trash", "list", "--json"}, "/v1/guards": {"guard", "list", "--json"},
	} {
		p.waitFor(path+" to answer as atoll "+args[0], func() bool { return asTheCommand(path, args...) })
	}
	if code, body := p.get("/v1/nothing"); code != http.StatusNotFound {
		t.Errorf("/v1/nothing: %d %s", code, body)
	}
	third := filepath.Join(p.dir, "disks", "t3")
	must(t, os.MkdirAll(filepath.Join(third, "media"), 0o755))
	p.text += "[[disk]]\nid = \"t3\"\nuuid = \"9c8b7a65-4321-4fed-8cba-0987654321fe\"\nmount = " + quote(third) + "\nfs = \"ext4\"\n"
	must(t, os.WriteFile(p.config, []byte(p.text), 0o644))
	must(t, d.cmd.Process.Signal(syscall.SIGHUP))
	p.waitFor("/v1/disks to list the disk added", func() bool { return asTheCommand("/v1/disks", "disk", "list", "--json") })
	if code := d.stop(syscall.SIGTERM); code != 0 {
		t.Errorf("SIGTERM: exit %d\n%s", code, d.stderr.String())
	}
	d.logIsJSONLines()
}

// quote writes a path as a TOML basic string holds it: those of the tests hold no quote, backslash or control.
func quote(path string) string {
	return "\"" + path + "\""
}
