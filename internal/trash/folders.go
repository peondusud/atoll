package trash

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/ops"
)

// Left records that a copy left the folder of a logical path on a disk through Atoll's own operation in this run:
// sent to the trash, or renamed away. The root is never one (specs.md §17.5).
func (s *Session) Left(disk, logical string) {
	folder := path.Dir(logical)
	if folder == "." || folder == "/" || folder == "" {
		return
	}
	if s.emptied[disk] == nil {
		s.emptied[disk] = map[string]bool{}
	}
	s.emptied[disk][folder] = true
}

// leftByEarlierRuns takes in the folders that the operations done since the last look left, which a run stopped
// before its end could not look at, as the journal says (specs.md §17.5).
func (s *Session) leftByEarlierRuns(ctx context.Context) error {
	from, err := catalog.FoldersTidiedThrough(ctx, s.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	upTo, err := catalog.HighestOp(ctx, s.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if err := catalog.LeftSince(ctx, s.db, from, upTo, s.Left); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	s.tidying = true
	return nil
}

// RemoveEmptied removes the folders Atoll made that its own operations of this run, or of a run stopped before its
// end, left empty, then each folder above one it removed that this left empty, the deepest first (specs.md §17.5). A
// folder that holds anything, or that is not the one Atoll made, stays, and so do those above it. A run that may not
// write removes nothing. Once every folder was looked at, on disks that could be written to, the journal records it.
func (s *Session) RemoveEmptied(ctx context.Context) error {
	if !s.apply {
		return nil
	}
	through, err := catalog.HighestOp(ctx, s.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	emptied := s.emptied
	s.emptied = map[string]map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(emptied)) {
		// By depth, so that every folder below one is decided before it is.
		byDepth := map[int]map[string]bool{}
		deepest := 0
		for folder := range emptied[id] {
			depth := strings.Count(folder, "/")
			if byDepth[depth] == nil {
				byDepth[depth] = map[string]bool{}
			}
			byDepth[depth][folder] = true
			deepest = max(deepest, depth)
		}
		for depth := deepest; depth >= 0; depth-- {
			for _, folder := range slices.Sorted(maps.Keys(byDepth[depth])) {
				removed, looked, err := s.removeFolder(ctx, id, folder)
				if err != nil {
					return err
				}
				s.untidy = s.untidy || !looked
				if parent := path.Dir(folder); removed && depth > 0 {
					if byDepth[depth-1] == nil {
						byDepth[depth-1] = map[string]bool{}
					}
					byDepth[depth-1][parent] = true
				}
			}
		}
	}
	// A folder of a disk that could not be written to, or lost meanwhile, is for the next run to look at.
	if s.tidying && !s.untidy {
		if err := catalog.SetFoldersTidiedThrough(ctx, s.db, through); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	return nil
}

// removeFolder removes one folder, when its row says Atoll made it and the catalog holds nothing of its disk below
// it, and says whether it went, and whether it was looked at: a folder of a disk that cannot be written to, or that
// is lost, was not.
func (s *Session) removeFolder(ctx context.Context, id, folder string) (removed, looked bool, err error) {
	if !s.writable(id) {
		return false, false, nil
	}
	key, own, err := catalog.OwnFolder(ctx, s.db, id, folder)
	if err != nil || !own {
		return false, true, wrapCatalog(err)
	}
	empty, err := catalog.NothingBelow(ctx, s.db, id, folder)
	if err != nil || !empty {
		return false, true, wrapCatalog(err)
	}
	handle, err := s.open(ctx, id)
	if err != nil {
		return false, false, err
	}
	if handle == nil || !s.writable(id) {
		return false, false, nil
	}
	op, err := catalog.BeginRmdir(ctx, s.db, id, folder, key, s.now)
	if err != nil {
		return false, false, fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("rmdir-journaled")
	err = handle.RemoveFolder(op)
	switch {
	case err == nil:
		if err := catalog.FinishRmdir(ctx, s.db, op); err != nil {
			return false, false, fmt.Errorf("catalog: %w", err)
		}
		return true, true, nil
	case errors.Is(err, ops.ErrDiskLost):
		// The operation stays planned, for the reprise to look at.
		return false, false, s.lose(ctx, id, err)
	case !errors.Is(err, ops.ErrNotEmpty) && !errors.Is(err, ops.ErrGone):
		slog.Warn("a folder Atoll made stays", "disk", id, "path", folder, "error", err)
	}
	if err := catalog.Undo(ctx, s.db, op, false, s.now); err != nil {
		return false, false, fmt.Errorf("catalog: %w", err)
	}
	return false, true, nil
}

func wrapCatalog(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("catalog: %w", err)
}
