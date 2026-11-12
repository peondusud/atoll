package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

func TestFstabHas(t *testing.T) {
	table := []byte("# /etc/fstab\nUUID=1 / ext4 defaults 0 1\n\n  UUID=2   /mnt/t1  ext4 defaults,nofail 0 2\n/dev/sdc1 /mnt/my\\040disk ntfs3 iocharset=utf8 0 0\n#UUID=3 /mnt/off ext4 defaults 0 0\nbroken\n")
	for mount, want := range map[string]bool{"/mnt/t1": true, "/mnt/t1/": true, "/mnt/my disk": true, "/mnt/off": false, "/mnt/t2": false, "/": true, "broken": false} {
		if got := fstabHas(table, mount); got != want {
			t.Errorf("%q: %v", mount, got)
		}
	}
	if got := fstabUnescape(`a\040b\134c\011d\0`); got != "a b\\c\td\\0" {
		t.Errorf("%q", got)
	}
}

func TestSdNotify(t *testing.T) {
	if err := sdNotify("READY=1"); err != nil {
		t.Errorf("with no socket: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", socket)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Errorf("%q, %v", buf[:n], err)
	}
}

// bench makes a configuration of disks that are not mounted, and its catalog.
func bench(t *testing.T, more string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "t1")) + "\nfs = \"ext4\"\n" + more
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	return c
}

// The loop on a disk that is not mounted: it tells the service manager it is ready, runs its first pass, checks
// again and again, takes no branch from a mergerfs that is not there, and stops with nil when asked, telling the
// service manager first (specs.md §15.13).
func TestRunStartsAndStops(t *testing.T) {
	c := bench(t, "")
	socket := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", socket)
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	passes := make(chan []string, 16)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: machine, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: 50 * time.Millisecond,
			OnPass: func(because []string, o *jobs.Outcome) {
				if len(o.Disks) != 1 || o.Disks[0].Verdict.State != disks.Missing {
					t.Errorf("the pass saw %+v", o.Disks)
				}
				passes <- because
			}})
	}()
	select {
	case because := <-passes:
		if !slices.Equal(because, []string{BecauseStart}) {
			t.Errorf("the first pass: %v", because)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no first pass")
	}
	// Checks go on; nothing calls for another pass.
	select {
	case because := <-passes:
		t.Errorf("another pass: %v", because)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopped: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	var said []string
	buf := make([]byte, 64)
	for range 2 {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		said = append(said, string(buf[:n]))
	}
	if !slices.Equal(said, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("told the service manager %v", said)
	}
}

// With no catalog, or another pool's, the daemon does not start.
func TestRunNeedsItsCatalog(t *testing.T) {
	c := bench(t, "")
	file, _ := c.Effective()["catalog"].(string)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), Options{Config: c, Machine: machine, Log: slog.New(slog.DiscardHandler)})
	if !errors.Is(err, ErrStartup) {
		t.Errorf("%v", err)
	}
	if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
		t.Errorf("a catalog was made: %v", statErr)
	}
}
