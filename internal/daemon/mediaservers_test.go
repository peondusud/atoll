package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/notify"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// The folders a media server is told of: those of the changes made other than through the view or by Atoll, and not
// of attributes alone (specs.md §21.5).
func TestChangedFolders(t *testing.T) {
	g := func(path string, folder bool, kinds, origins []string) watch.Group {
		return watch.Group{Disk: "t1", Path: path, Folder: folder, Kinds: kinds, Origins: origins}
	}
	got := ChangedFolders([]watch.Group{
		g("tv/a/1.mkv", false, []string{watch.KindWritten}, []string{watch.OriginUser}),
		g("tv/b/2.mkv", false, []string{watch.KindWritten}, []string{watch.OriginView}),
		g("tv/c", true, []string{watch.KindCreated}, []string{watch.OriginUnknown}),
		g("films/x.mkv", false, []string{watch.KindAttrib}, []string{watch.OriginUser}),
		g("", true, []string{watch.KindOverflow}, []string{watch.OriginUnknown}),
		g("top.mkv", false, []string{watch.KindDeleted}, []string{watch.OriginAtoll, watch.OriginUser}),
		g("tv/a/3.mkv", false, []string{watch.KindDeleted}, []string{watch.OriginAtoll}),
		g("tv/a/4.mkv", false, []string{watch.KindAttrib, watch.KindMovedTo}, []string{watch.OriginUser}),
	})
	if want := []string{"", "tv", "tv/a"}; !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}

// The folders an operation changed: a rename's two, and a trashed copy's; not what a dry run would do, nor a copy.
func TestOperationFolders(t *testing.T) {
	for _, tc := range []struct {
		op   jobs.Operation
		want []string
	}{
		{jobs.Operation{Kind: jobs.KindRename, Path: "a/b/c.mkv", To: "a/d/c.mkv", Outcome: trash.OutcomeRenamed}, []string{"a/b", "a/d"}},
		{jobs.Operation{Kind: jobs.KindFolder, Path: "tv/Old", To: "tv/New", Outcome: trash.OutcomeRenamed}, []string{"tv", "tv"}},
		{jobs.Operation{Kind: jobs.KindRename, Path: "a/b/c.mkv", To: "a/d/c.mkv", Outcome: "to-rename"}, nil},
		{jobs.Operation{Kind: jobs.KindTrash, Path: "films/x.mkv", Outcome: trash.OutcomeTrashed}, []string{"films"}},
		{jobs.Operation{Kind: jobs.KindTrash, Path: "x.mkv", Outcome: trash.OutcomeTrashed}, []string{""}},
		{jobs.Operation{Kind: jobs.KindTrash, Path: "films/x.mkv", Outcome: jobs.OutcomeHeld}, nil},
		{jobs.Operation{Kind: jobs.KindReplicate, Path: "films/x.mkv", Outcome: "replicated"}, nil},
	} {
		if got := OperationFolders(&tc.op); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %q", tc.op, got)
		}
	}
}

// fakeServer records what it is told, and fails while failing is set.
type fakeServer struct {
	told    [][]string
	failing bool
}

func (s *fakeServer) Name() string { return "notify.fake" }

func (s *fakeServer) Scan(_ context.Context, folders []string) error {
	if s.failing {
		return errors.New("unreachable")
	}
	s.told = append(s.told, slices.Clone(folders))
	return nil
}

func (s *fakeServer) Check(context.Context) error { return nil }

// tellLoop makes a loop over t1 online, t2 missing and t3 failed and missing, whose catalog holds tv/a/1.mkv on t1 and
// t2 and films/2.mkv on t1 and t3, with one fake media server.
func tellLoop(t *testing.T, apply bool) (*loop, *fakeServer) {
	t.Helper()
	l, _ := smartLoop(t, apply, false)
	db, err := catalog.OpenPool(t.Context(), l.set.catalog, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for path, on := range map[string][]string{"tv/a/1.mkv": {"t1", "t2"}, "films/2.mkv": {"t1", "t3"}} {
		res, err := db.ExecContext(t.Context(), `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 0, 0, 1, 'deferred', 'ok', 0, 0)`, []byte(path))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		for _, disk := range on {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (?, ?, 'user', 0, 'ok')`,
				id, disk); err != nil {
				t.Fatal(err)
			}
		}
	}
	l.disks = map[string]*diskState{"t1": {state: disks.Online}, "t2": {state: disks.Missing}, "t3": {state: disks.Missing}}
	s := &fakeServer{}
	l.set.servers = []notify.Server{s}
	l.queues = newQueues(l.set.servers, nil)
	l.changed = map[string]bool{}
	return l, s
}

// The daemon tells a media server of what changed, holds a folder whose file has a copy on a missing disk until it
// comes back, but not for a failed disk, tells a folder again only after the window, and tries a server it could not
// tell again later (specs.md §21.4, §21.5).
func TestTell(t *testing.T) {
	l, s := tellLoop(t, true)
	l.changed["films"] = true
	l.queueFolders(map[string]bool{"tv/a": true})
	if due := l.notifyDue(); due == nil || *due > l.opt.Machine.Now() {
		t.Fatalf("nothing due: %v", due)
	}
	l.tell(t.Context())
	if len(s.told) != 1 || !slices.Equal(s.told[0], []string{"films"}) {
		t.Fatalf("told %q", s.told)
	}
	if due := l.notifyDue(); due != nil || l.queues["notify.fake"].Held() != 1 {
		t.Errorf("due %v, %d held", due, l.queues["notify.fake"].Held())
	}
	// t2 comes back: the pass that follows lets the folder out.
	l.disks["t2"].state = disks.Online
	l.queueFolders(map[string]bool{})
	l.tell(t.Context())
	if len(s.told) != 2 || !slices.Equal(s.told[1], []string{"tv/a"}) {
		t.Fatalf("told %q", s.told)
	}
	// Changed again at once: told again after the window.
	l.queueFolders(map[string]bool{"films": true})
	l.tell(t.Context())
	if due := l.notifyDue(); len(s.told) != 2 || due == nil || *due <= l.opt.Machine.Now() {
		t.Errorf("told %q, due %v", s.told, due)
	}
	// A server that cannot be told: the folder stays, and waits notify.RetryAfter.
	l.queueFolders(map[string]bool{"music": true})
	s.failing = true
	before := l.opt.Machine.Now()
	l.tell(t.Context())
	if due := l.notifyDue(); due == nil || *due < before+notify.RetryAfter.Nanoseconds() {
		t.Errorf("due %v", due)
	}
	if got := l.queues["notify.fake"].Ready(before + notify.RetryAfter.Nanoseconds() + notify.Window.Nanoseconds()); !slices.Equal(got, []string{"films", "music"}) {
		t.Errorf("left %q", got)
	}
}

// In shadow mode, nothing is told, and what would be is not told again within the window.
func TestTellShadow(t *testing.T) {
	l, s := tellLoop(t, false)
	l.queueFolders(map[string]bool{"films": true})
	l.tell(t.Context())
	if len(s.told) != 0 || l.notifyDue() != nil {
		t.Errorf("told %q, due %v", s.told, l.notifyDue())
	}
}

// A reload keeps the queue of a server still set, and drops that of one no longer set.
func TestNewQueuesKeeps(t *testing.T) {
	s := &fakeServer{}
	had := newQueues([]notify.Server{s}, nil)
	had["notify.fake"].Add("x")
	if got := newQueues([]notify.Server{s}, had); got["notify.fake"] != had["notify.fake"] {
		t.Error("a queue lost")
	}
	if got := newQueues(nil, had); len(got) != 0 {
		t.Errorf("%v", got)
	}
}
