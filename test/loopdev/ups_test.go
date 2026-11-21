//go:build loopdev

package loopdev

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// upsd answers each GET VAR with the status the test last set.
type upsd struct {
	mu     sync.Mutex
	status string
	port   int
}

func newUpsd(t *testing.T, status string) *upsd {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	mustDo(t, err)
	t.Cleanup(func() { _ = l.Close() })
	u := &upsd{status: status, port: l.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil || line == "LOGOUT\n" {
						return
					}
					u.mu.Lock()
					answer := "VAR ups ups.status \"" + u.status + "\"\n"
					u.mu.Unlock()
					if _, err := conn.Write([]byte(answer)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return u
}

func (u *upsd) set(status string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = status
}

// sectorsWritten reads what a loop device has written, in sectors of 512 bytes, from its counters.
func sectorsWritten(t *testing.T, device string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(device), "stat"))
	mustDo(t, err)
	fields := strings.Fields(string(data))
	if len(fields) < 7 {
		t.Fatalf("the counters of %s: %q", device, data)
	}
	n, err := strconv.ParseInt(fields[6], 10, 64)
	mustDo(t, err)
	return n
}

// TestALowBatteryWritesOutRealDisks: the daemon reads a UPS through NUT. With its battery low, it asks each disk to
// write out what its filesystem holds in memory, syncfs(2), on ext4 and XFS: files written there without an fsync,
// which the kernel would hold for 30 s and more, reach both devices at once. Then it does nothing, not even for a new
// file, until line power is back, when it passes as at a start (specs.md §20).
func TestALowBatteryWritesOutRealDisks(t *testing.T) {
	needRoot(t)
	// The kernel's own writeback leaves dirty pages younger than this alone: only the daemon's syncfs writes them.
	const expire = "/proc/sys/vm/dirty_expire_centisecs"
	was, err := os.ReadFile(expire)
	mustDo(t, err)
	mustDo(t, os.WriteFile(expire, []byte("360000"), 0o644))
	t.Cleanup(func() { _ = os.WriteFile(expire, was, 0o644) })
	loopbackUp(t)

	u := newUpsd(t, "OL")
	kept, v := newVolume(t, names.Ext4), newVolume(t, names.XFS)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchKept(t, kept, v, at, benchGuards, fmt.Sprintf("[ups]\nenabled = true\nnut = \"ups@127.0.0.1:%d\"\n", u.port)+
		"[[rule]]\nlabel = \"solo\"\npath = \"solo/**\"\nreplicas = 0\n")
	mustDo(t, b.db.Close())
	passes := make(chan []string, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			Config: b.config, Machine: b.machine, Apply: true, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
			CheckEvery: 300 * time.Millisecond, UPSEvery: 100 * time.Millisecond, UPSDead: time.Second,
			OnPass: func(because []string, _ *jobs.Outcome) { passes <- because },
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the daemon stopped: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	wait := func(why string) {
		t.Helper()
		for deadline := time.After(60 * time.Second); ; {
			select {
			case because := <-passes:
				for _, w := range because {
					if w == why {
						return
					}
				}
			case <-deadline:
				t.Fatalf("no pass because %s", why)
			}
		}
	}
	wait(daemon.BecauseStart)

	// 8 MiB on each disk, through the page cache, never synced.
	data := strings.Repeat("a film ", 8<<20/7)
	devices := map[string]string{b.keep: kept.device, at: v.device}
	before := map[string]int64{}
	for top, device := range devices {
		mustDo(t, os.MkdirAll(filepath.Join(top, "media", "solo"), 0o755))
		mustDo(t, os.WriteFile(filepath.Join(top, "media", "solo", "film.mkv"), []byte(data), 0o644))
		before[device] = sectorsWritten(t, device)
	}
	u.set("OB DISCHRG LB")
	for _, device := range devices {
		waitFor(t, "the files written out to "+device, func() bool {
			return sectorsWritten(t, device)-before[device] >= int64(len(data))/512
		})
	}

	// Parked: a new file calls for no pass.
	for len(passes) > 0 {
		<-passes
	}
	write(t, b.keep, "solo/later.mkv", "later")
	select {
	case because := <-passes:
		t.Fatalf("a pass while parked: %v", because)
	case <-time.After(2 * time.Second):
	}
	u.set("OL CHRG")
	wait(daemon.BecauseStart)
}
