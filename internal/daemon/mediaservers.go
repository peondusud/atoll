package daemon

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/notify"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// ChangedFolders gives the folders of the view whose content changed other than through the view or by Atoll, as the
// ripe groups say: the folder that holds each group's path, once each, in byte order. A media server sees what changes
// through the view on its host, and nothing else (specs.md §21.5). A change of attributes alone, an overflow, which
// names no path, and the top of a disk name none.
func ChangedFolders(ripe []watch.Group) []string {
	set := map[string]bool{}
	for _, g := range ripe {
		if g.Path == "" {
			continue
		}
		if !slices.ContainsFunc(g.Origins, func(o string) bool { return o == watch.OriginUser || o == watch.OriginUnknown }) {
			continue
		}
		if !slices.ContainsFunc(g.Kinds, func(k string) bool { return k != watch.KindAttrib && k != watch.KindOverflow }) {
			continue
		}
		set[parentOf(g.Path)] = true
	}
	return sortedKeys(set)
}

// OperationFolders gives the folders of the view an operation of a pass changed: those of a rename's two paths, and
// that of a copy moved to the trash (specs.md §21.5). A copy made, repaired or given its attributes changes no name.
func OperationFolders(op *jobs.Operation) []string {
	switch {
	case (op.Kind == jobs.KindRename || op.Kind == jobs.KindFolder) && op.Outcome == trash.OutcomeRenamed:
		return []string{parentOf(op.Path), parentOf(op.To)}
	case op.Kind == jobs.KindTrash && op.Outcome == trash.OutcomeTrashed:
		return []string{parentOf(op.Path)}
	default:
		return nil
	}
}

// parentOf gives the folder that holds a path of the view, "" for its top.
func parentOf(p string) string {
	if at := strings.LastIndexByte(p, '/'); at >= 0 {
		return p[:at]
	}
	return ""
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// newQueues gives each media server set its queue, keeping the one it had.
func newQueues(servers []notify.Server, had map[string]*notify.Queue) map[string]*notify.Queue {
	out := make(map[string]*notify.Queue, len(servers))
	for _, s := range servers {
		if q := had[s.Name()]; q != nil {
			out[s.Name()] = q
		} else {
			out[s.Name()] = notify.NewQueue()
		}
	}
	return out
}

// queueFolders gives the folders a pass changed to each media server's queue, with those the ripe groups named, and
// lets out what was held, to be looked at again: a pass follows a disk that comes back.
func (l *loop) queueFolders(folders map[string]bool) {
	if len(l.queues) == 0 {
		return
	}
	for f := range l.changed {
		folders[f] = true
	}
	clear(l.changed)
	list := sortedKeys(folders)
	for _, q := range l.queues {
		q.Release()
		q.Add(list...)
	}
}

// notifyDue gives when a media server may next be told of a folder: nil when none is waiting.
func (l *loop) notifyDue() *int64 {
	var due *int64
	for _, q := range l.queues {
		if next, ok := q.Next(); ok && (due == nil || next < *due) {
			due = &next
		}
	}
	return due
}

// tell tells each media server of the folders it may be told of now (specs.md §21.5). A folder below which a file has
// a copy on a missing disk is held until a disk comes back, as a scan would find that file gone (specs.md §21.4). A
// server that cannot be told is tried again after notify.RetryAfter. A daemon in shadow mode logs what it would tell.
func (l *loop) tell(ctx context.Context) {
	now := l.opt.Machine.Now()
	ready := map[string][]string{}
	all := map[string]bool{}
	for _, s := range l.set.servers {
		ready[s.Name()] = l.queues[s.Name()].Ready(now)
		for _, f := range ready[s.Name()] {
			all[f] = true
		}
	}
	if missing := l.missingDisks(); len(missing) > 0 && len(all) > 0 {
		var held []string
		_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
			var err error
			held, err = catalog.FoldersWithCopiesOn(ctx, db, sortedKeys(all), missing)
			return nil, err
		})
		if err != nil {
			if ctx.Err() == nil {
				l.log.Warn("the folders to tell the media servers of could not be looked at: they are tried again later", "error", err)
			}
			for _, q := range l.queues {
				q.Failed(now)
			}
			return
		}
		for _, s := range l.set.servers {
			q := l.queues[s.Name()]
			q.Hold(held)
			ready[s.Name()] = slices.DeleteFunc(ready[s.Name()], func(f string) bool { return slices.Contains(held, f) })
		}
		if len(held) > 0 {
			l.log.Info("folders held from the media servers while a disk is missing", "folders", len(held), "missing", missing)
		}
	}
	for _, s := range l.set.servers {
		folders := ready[s.Name()]
		if len(folders) == 0 {
			continue
		}
		q := l.queues[s.Name()]
		if !l.opt.Apply {
			l.log.Info("a media server would be told; a dry run tells none", "server", s.Name(), "folders", len(folders))
			q.Told(folders, now)
			continue
		}
		if err := s.Scan(ctx, folders); err != nil {
			if ctx.Err() == nil {
				l.log.Warn("a media server could not be told: it is tried again later", "server", s.Name(), "folders", len(folders),
					"retry", notify.RetryAfter.String(), "error", err)
			}
			q.Failed(now)
			continue
		}
		q.Told(folders, now)
		l.log.Info("a media server was told", "server", s.Name(), "folders", len(folders))
	}
}

// missingDisks gives the disks of the configuration the last check found missing, but a failed one, which no longer
// counts: a scan may forget what was only on it.
func (l *loop) missingDisks() []string {
	var out []string
	for i := range l.opt.Config.Disks {
		d := &l.opt.Config.Disks[i]
		if st := l.disks[d.ID]; st != nil && st.state == disks.Missing && d.State != config.DiskFailed {
			out = append(out, d.ID)
		}
	}
	return out
}
