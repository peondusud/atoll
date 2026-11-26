package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// poolMachine is a pool's disks as folders, each mounted healthy as ext4 with the identifier its [[disk]] gives, but
// those a test takes away, whose mount point has nothing mounted. Its clock moves a second at each question.
type poolMachine struct {
	mu   sync.Mutex
	uuid map[string]string
	away map[string]bool
	now  int64
	// fail makes calls through a handle on a disk fail, as fsx.Fault does, when set: given the disk's mount, the call
	// and the name it is about.
	fail func(mount, call, name string) error
	// at runs at each checkpoint a command reaches, when set: a test stops there, as kill -9 would, or acts there.
	at func(point string)
}

// faulty wraps a handle on a disk in the machine's fail, when it has one.
func (m *poolMachine) faulty(mount string, d fsx.Dir, err error) (fsx.Dir, error) {
	m.mu.Lock()
	fail := m.fail
	m.mu.Unlock()
	if err != nil || fail == nil {
		return d, err
	}
	return fsx.Fault(d, func(call, name string) error { return fail(mount, call, name) }), nil
}

// advance moves the machine's clock on.
func (m *poolMachine) advance(by time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now += int64(by)
}

func (m *poolMachine) setAway(mount string, away bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.away[mount] = away
}

func (m *poolMachine) Mount(mount string) disks.MountSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.uuid[mount]; !known || m.away[mount] {
		return disks.MountSeen{Path: disks.PathOK}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{Root: "/", MountPoint: mount, FSType: "ext4",
		MountOptions: []string{"rw"}, SuperOptions: []string{"rw"}, Source: mount}}
}

func (m *poolMachine) Device(_ names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: m.uuid[e.MountPoint], TimePrecisionNS: 1}}
}
func (m *poolMachine) Root(string, string) disks.RootSeen {
	return disks.RootSeen{Folder: disks.FolderOK}
}
func (m *poolMachine) Hiberfil(string, *disks.MountEntry) disks.HiberfilSeen {
	return disks.HiberfilSeen{File: disks.HiberfilAbsent}
}
func (m *poolMachine) LogFile(string, *disks.MountEntry) disks.LogFileSeen {
	return disks.LogFileSeen{File: disks.LogFileFile, Journal: disks.JournalEmpty}
}
func (m *poolMachine) Marker(mount, workdir string, _ *disks.MountEntry, _ bool) disks.MarkerSeen {
	return disks.ReadMarkerAt(mount, workdir)
}
func (m *poolMachine) Now() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now += int64(time.Second)
	return m.now
}
func (m *poolMachine) Space(string) (int64, int64, error) { return 1 << 30, 1 << 29, nil }
func (m *poolMachine) OpenRoot(mount, root string) (fsx.Dir, error) {
	d, err := fsx.OpenDir(filepath.Join(mount, root))
	return m.faulty(mount, d, err)
}
func (m *poolMachine) OpenTop(mount string) (fsx.Dir, error) {
	d, err := fsx.OpenDir(mount)
	return m.faulty(mount, d, err)
}
func (m *poolMachine) ReadPastCache(fs names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, e)
}
func (m *poolMachine) Checkpoint(point string) {
	m.mu.Lock()
	at := m.at
	m.mu.Unlock()
	if at != nil {
		at(point)
	}
}

// poolBench is a pool of disks t1, t2… as folders, its configuration and its catalog's path, with each command run in
// the test's process on the bench's machine.
type poolBench struct {
	t          *testing.T
	dir        string
	configPath string
	mounts     []string
	machine    *poolMachine
}

// poolUUIDs are the identifiers of the bench's disks, in order.
var poolUUIDs = []string{"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d", "9c8b7a6d-5e4f-4321-8765-0fedcba98765"}

// newPoolBench makes a pool of n disks, more added to the configuration after them, with the bench's machine given to
// every command until the test ends. Nothing is in the catalog: init makes it.
func newPoolBench(t *testing.T, n int, more string) *poolBench {
	t.Helper()
	return newPoolBenchViewing(t, n, "", more)
}

// newPoolBenchViewing is newPoolBench with more keys of [view], each on a line of its own.
func newPoolBenchViewing(t *testing.T, n int, view, more string) *poolBench {
	t.Helper()
	dir := t.TempDir()
	b := &poolBench{t: t, dir: dir, configPath: filepath.Join(dir, "atoll.toml"),
		machine: &poolMachine{uuid: map[string]string{}, away: map[string]bool{}, now: 1_700_000_000_000_000_000}}
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(filepath.Join(dir, "catalog.db")) + "\n[view]\nmount = " +
		strconv.Quote(filepath.Join(dir, "pool")) + "\n" + view
	for i := range n {
		id := fmt.Sprintf("t%d", i+1)
		mount := filepath.Join(dir, "disks", id)
		mustMkdir(t, filepath.Join(mount, "media"))
		b.mounts = append(b.mounts, mount)
		b.machine.uuid[mount] = poolUUIDs[i]
		text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = %q\nmount = %s\nfs = \"ext4\"\n", id, poolUUIDs[i], strconv.Quote(mount))
	}
	text += more
	if err := os.WriteFile(b.configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	was := newMachine
	newMachine = func() (disks.Machine, error) { return b.machine, nil }
	t.Cleanup(func() { newMachine = was })
	return b
}

// write puts a file below media/ on the disk numbered from 1, with its parents.
func (b *poolBench) write(disk int, path, text string) {
	b.t.Helper()
	full := filepath.Join(b.mounts[disk-1], "media", path)
	mustMkdir(b.t, filepath.Dir(full))
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// read gives a file below media/ on a disk, "absent" when there is none.
func (b *poolBench) read(disk int, path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.mounts[disk-1], "media", path))
	if err != nil {
		return "absent"
	}
	return string(data)
}

// atoll runs a command with the bench's configuration, and gives its exit code and what it wrote.
func (b *poolBench) atoll(args ...string) (int, string, string) {
	b.t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--config", b.configPath}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// ok runs a command that must exit with the code given, and gives what it wrote on standard output.
func (b *poolBench) ok(want int, args ...string) string {
	b.t.Helper()
	code, stdout, stderr := b.atoll(args...)
	if code != want {
		b.t.Fatalf("atoll %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, want, stdout, stderr)
	}
	return stdout
}

// json runs a command with --json that must exit with the code given, and decodes what it wrote.
func (b *poolBench) json(want int, args ...string) map[string]any {
	b.t.Helper()
	out := b.ok(want, append(args, "--json")...)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		b.t.Fatalf("atoll %s --json: %v\n%s", strings.Join(args, " "), err, out)
	}
	return got
}
