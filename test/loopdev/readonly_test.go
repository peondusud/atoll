//go:build loopdev

package loopdev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/view/fusefs"
	"github.com/peondusud/atoll/internal/view/readonly"
)

// TestReadonlyMount: what atoll mount runs (specs.md §21.2), at a point where views whose process stopped short were
// left mounted: one that died before the kernel's first request was answered, and one killed while it served. Each is
// detached, and the view mounted there reads each file from its original. A second one at the same point is refused
// while the first serves. A disk unmounted is read from no more, and once mounted again is read
// from its new mount, within a check. A stop unmounts the view; the view unmounted by another process ends it with an
// error; anything else mounted at the point is refused.
func TestReadonlyMount(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
	write(t, b.keep, "Films/A.mkv", "from k")
	write(t, b.at, "Films/B.mkv", "from v")
	b.scan(false)
	catchUp(b)
	b.scan(false)
	point := mountPoint(t)
	mustDo(t, os.MkdirAll(point, 0o755))

	// Linux 6.12 answers ENOTCONN for a connection whose process never answered, 7.3 ECONNREFUSED.
	deadView(t, point)
	if _, err := os.Stat(point); !fsx.IsNotConnected(err) {
		t.Fatalf("a view whose process never answered: %v, want ENOTCONN or ECONNREFUSED", err)
	}
	killed := startView(t, b, point, nil)
	killed.mounted(t)
	_ = killed.cmd.Process.Kill()
	<-killed.exited
	// Not a stat, which the kernel answers from its cache for a second.
	if _, err := os.ReadDir(point); !errors.Is(err, syscall.ENOTCONN) {
		t.Fatalf("a view whose process was killed: %v, want ENOTCONN", err)
	}
	first, err := serveReadonly(t, b, point)
	mustDo(t, err)
	if got := mountsAt(t, point); len(got) != 1 || got[0] != fusefs.Type {
		t.Errorf("mounted at the point: %v, want the view alone", got)
	}
	for name, want := range map[string]string{"A.mkv": "from k", "B.mkv": "from v"} {
		if data, err := os.ReadFile(filepath.Join(point, "Films", name)); err != nil || string(data) != want {
			t.Errorf("%s: %q, %v", name, data, err)
		}
	}
	if _, err := serveReadonly(t, b, point); err == nil || !strings.Contains(err.Error(), "another process serves") {
		t.Errorf("a second view at the point: %v", err)
	}

	// B.mkv's original is v's copy: v unmounted, it fails; mounted again, it reads from the new mount.
	must(t, "umount", "-l", at)
	eventually(t, "B.mkv fails with EIO once v is unmounted", func() bool {
		_, err := os.ReadFile(filepath.Join(point, "Films", "B.mkv"))
		return errors.Is(err, syscall.EIO)
	})
	if data, err := os.ReadFile(filepath.Join(point, "Films", "A.mkv")); err != nil || string(data) != "from k" {
		t.Errorf("A.mkv with v away: %q, %v", data, err)
	}
	must(t, "mount", v.device, at)
	eventually(t, "B.mkv reads once v is mounted again", func() bool {
		data, err := os.ReadFile(filepath.Join(point, "Films", "B.mkv"))
		return err == nil && string(data) == "from v"
	})

	first.stop()
	if err := first.wait(); err != nil {
		t.Errorf("the view stopped: %v", err)
	}
	if got := mountsAt(t, point); len(got) != 0 {
		t.Errorf("mounted at the point once stopped: %v", got)
	}

	again, err := serveReadonly(t, b, point)
	mustDo(t, err)
	must(t, "umount", point)
	if err := again.wait(); !errors.Is(err, readonly.ErrUnmounted) {
		t.Errorf("the view unmounted by another process: %v", err)
	}

	must(t, "mount", "-t", "tmpfs", "tmpfs", point)
	defer func() { _ = attempt(t, "umount", point) }()
	if _, err := serveReadonly(t, b, point); err == nil || !strings.Contains(err.Error(), "tmpfs is mounted there") {
		t.Errorf("a tmpfs at the point: %v", err)
	}
}

// mountCapabilities are those deploy/atoll-mount.service gives the dedicated user, and no others.
const mountCapabilities = "-all,+sys_admin,+dac_read_search"

// childEnv names the configuration TestReadonlyMountChild serves the view of, at the point childPointEnv names.
const (
	childEnv      = "ATOLL_READONLY_CHILD"
	childPointEnv = "ATOLL_READONLY_POINT"
)

// TestReadonlyMountAsTheUnit: the view served as deploy/atoll-mount.service serves it, as another account than root,
// with the group disk, CAP_SYS_ADMIN and CAP_DAC_READ_SEARCH alone, and no new privileges. It confirms the disks,
// mounts by mount(2), and reads a copy only root may read for root; the kernel refuses that file to another account
// and gives it the others, as the view presents them. SIGTERM unmounts it, with exit 0.
func TestReadonlyMountAsTheUnit(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skip("no setpriv")
	}
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
	write(t, b.keep, "Films/A.mkv", "from k")
	write(t, b.at, "Films/secret.mkv", "root's alone")
	mustDo(t, os.Chmod(filepath.Join(b.at, "media", "Films", "secret.mkv"), 0o600))
	b.scan(false)
	catchUp(b)
	b.scan(false)
	// The catalog's folder is the dedicated user's, as StateDirectory makes it; /dev/fuse is open to every account, as
	// udev makes it (50-udev-default.rules), where the guest of test/qemu, which has no udev, gives it to root alone.
	must(t, "chown", "-R", "990:990", filepath.Dir(b.catalog))
	fuse, err := os.Stat("/dev/fuse")
	mustDo(t, err)
	mustDo(t, os.Chmod("/dev/fuse", 0o666))
	t.Cleanup(func() { _ = os.Chmod("/dev/fuse", fuse.Mode().Perm()) })
	// Where another account may reach it, as a media server reaches /srv/pool-ro: a test's own folders are root's alone.
	point, err := os.MkdirTemp("", "atoll-view-")
	mustDo(t, err)
	mustDo(t, os.Chmod(point, 0o755))
	t.Cleanup(func() { _ = os.Remove(point) })

	unit := []string{"setpriv", "--reuid=990", "--regid=990", "--groups=6", "--no-new-privs",
		"--inh-caps=" + mountCapabilities, "--ambient-caps=" + mountCapabilities, "--bounding-set=" + mountCapabilities, "--"}
	view := startView(t, b, point, unit)
	view.mounted(t)
	for name, want := range map[string]string{"A.mkv": "from k", "secret.mkv": "root's alone"} {
		if data, err := os.ReadFile(filepath.Join(point, "Films", name)); err != nil || string(data) != want {
			t.Errorf("%s, as root: %q, %v", name, data, err)
		}
	}
	other := func(name string) error {
		return attempt(t, "setpriv", "--reuid=991", "--regid=991", "--clear-groups", "cat", filepath.Join(point, "Films", name))
	}
	if err := other("A.mkv"); err != nil {
		t.Errorf("A.mkv, as another account: %v", err)
	}
	if err := other("secret.mkv"); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("secret.mkv, as another account: %v, want it refused", err)
	}
	mustDo(t, view.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-view.exited:
		if view.err != nil {
			t.Errorf("the view stopped: %v\n%s", view.err, view.out.String())
		}
	case <-time.After(10 * time.Second):
		t.Errorf("the view did not stop on SIGTERM:\n%s", view.said())
	}
	if got := mountsAt(t, point); len(got) != 0 {
		t.Errorf("mounted at the point once stopped: %v", got)
	}
}

// TestReadonlyMountChild serves the view for startView, as atoll mount does, until SIGTERM; it is skipped otherwise.
func TestReadonlyMountChild(t *testing.T) {
	file := os.Getenv(childEnv)
	if file == "" {
		t.Skip("run by startView")
	}
	data, err := os.ReadFile(file)
	mustDo(t, err)
	c, err := config.Parse(data)
	mustDo(t, err)
	path, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(context.Background(), path, c.PoolID)
	mustDo(t, err)
	defer func() { _ = db.Close() }()
	machine, err := disks.NewMachine()
	mustDo(t, err)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	mustDo(t, readonly.Run(ctx, readonly.Options{
		Config: c, DB: db, Machine: machine, Point: os.Getenv(childPointEnv), Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}))
}

// viewProcess is a view TestReadonlyMountChild serves, in a process of its own.
type viewProcess struct {
	cmd    *exec.Cmd
	out    *bytes.Buffer
	exited chan struct{}
	// err is what the process ended with, once exited is closed.
	err   error
	point string
}

// startView serves the view at the point in another process, run through the command before, if any, such as
// setpriv: the process ends at the end of the test, and the view is detached.
func startView(t *testing.T, b *passBench, point string, before []string) *viewProcess {
	t.Helper()
	file := filepath.Join(filepath.Dir(b.catalog), "atoll.toml")
	mustDo(t, os.WriteFile(file, []byte(b.configText), 0o644))
	args := append(append([]string{}, before...), os.Args[0], "-test.run=^TestReadonlyMountChild$", "-test.v")
	p := &viewProcess{out: &bytes.Buffer{}, exited: make(chan struct{}), point: point}
	p.cmd = exec.CommandContext(context.Background(), args[0], args[1:]...)
	p.cmd.Env = append(os.Environ(), childEnv+"="+file, childPointEnv+"="+point)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	mustDo(t, p.cmd.Start())
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = p.said()
		_ = syscall.Unmount(point, syscall.MNT_DETACH)
	})
	return p
}

// said gives what the process wrote, once it has exited: killed first when it has not.
func (p *viewProcess) said() string {
	_ = p.cmd.Process.Kill()
	<-p.exited
	return p.out.String()
}

// mounted waits until the view answers at its point.
func (p *viewProcess) mounted(t *testing.T) {
	t.Helper()
	eventually(t, "the view mounted", func() bool {
		_, err := os.Stat(filepath.Join(p.point, "Films"))
		return err == nil
	})
	if t.Failed() {
		t.Fatalf("the view's process said:\n%s", p.said())
	}
}

// served is a view readonly.Run serves.
type served struct {
	stop context.CancelFunc
	done chan error
}

// wait gives what Run returned, within 10 s.
func (s *served) wait() error {
	select {
	case err := <-s.done:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("readonly.Run did not return")
	}
}

// serveReadonly runs readonly.Run at the point, checking the disks every 100 ms, until it says the view is mounted,
// or returns.
func serveReadonly(t *testing.T, b *passBench, point string) (*served, error) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	s := &served{stop: stop, done: make(chan error, 1)}
	ready := make(chan struct{})
	opts := readonly.Options{
		Config: b.config, DB: b.db, Machine: b.machine, Point: point, Every: 100 * time.Millisecond,
		Ready: func() { close(ready) }, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	go func() { s.done <- readonly.Run(ctx, opts) }()
	select {
	case <-ready:
		t.Cleanup(func() {
			stop()
			_ = s.wait()
		})
		return s, nil
	case err := <-s.done:
		stop()
		return nil, err
	case <-time.After(10 * time.Second):
		stop()
		return nil, errors.New("readonly.Run did not mount the view")
	}
}

// deadView leaves at the point a mount of the view whose process is gone: mounted on a FUSE connection whose only
// descriptor is then closed, which the kernel answers ENOTCONN for.
func deadView(t *testing.T, point string) {
	t.Helper()
	dev, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	mustDo(t, err)
	data := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0", dev.Fd())
	mustDo(t, syscall.Mount("atoll", point, fusefs.Type, syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_RDONLY, data))
	mustDo(t, dev.Close())
	t.Cleanup(func() { _ = syscall.Unmount(point, syscall.MNT_DETACH) })
}

// mountsAt gives the type of each mount at a path, the one on top last.
func mountsAt(t *testing.T, path string) []string {
	t.Helper()
	data, err := fsx.ReadMountTable()
	mustDo(t, err)
	var types []string
	for _, e := range disks.ParseMountInfo(data).Entries {
		if e.MountPoint == path {
			types = append(types, e.FSType)
		}
	}
	return types
}

// eventually waits up to 5 s for a condition, past the kernel's caches of the view and a check of the disks.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Errorf("not within 5 s: %s", what)
}
