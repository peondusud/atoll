package watch

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/solver"
)

// DiskStatus is one disk, with what the catalog holds on it.
type DiskStatus struct {
	catalog.Disk
	Copies int
	Bytes  int64
}

// Status is what the catalog holds (specs.md §15.11). It says what the last scan saw.
type Status struct {
	Disks        []DiskStatus
	FilesTotal   int
	FilesAtLevel int
	BytesTotal   int64
	BytesAtLevel int64
	// BytesPlaced is the bytes of the files with no misplaced copy (specs.md §13.3).
	BytesPlaced int64
	ByState     map[string]int
}

// ReadStatus reads the catalog and touches no disk. A non-empty under keeps that path and what is
// below it in the counts, and gives each file it keeps to each, by path in byte order, with its
// copies by position. It keeps none of them, as a folder of 200,000 files would not fit in the
// memory of specs.md §26. misplaced holds the files with a misplaced copy, which the bytes correctly
// placed leave out.
func ReadStatus(ctx context.Context, db *sql.DB, under string, misplaced map[string]bool, each func(*catalog.FileRow) error) (*Status, error) {
	diskList, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := &Status{Disks: make([]DiskStatus, 0, len(diskList)), ByState: map[string]int{}}
	at := make(map[string]int, len(diskList))
	for i := range diskList {
		at[diskList[i].ID] = i
		out.Disks = append(out.Disks, DiskStatus{Disk: diskList[i]})
	}
	keep := map[string]bool{under: true}
	reader := catalog.NewFileReader(db)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return out, nil
		}
		for i := range page {
			f := &page[i]
			out.countCopies(f, at)
			if under != "" && !atOrBelow(keep, f.Path) {
				continue
			}
			out.countFile(f, at)
			if !misplaced[f.Path] {
				out.BytesPlaced += f.Size
			}
			if under == "" {
				continue
			}
			slices.SortFunc(f.Copies, func(a, b catalog.CopyRow) int { return cmp.Compare(at[a.Disk], at[b.Disk]) })
			if err := each(f); err != nil {
				return nil, fmt.Errorf("%w", err)
			}
		}
	}
}

// countCopies counts for the disks, whatever under keeps. Pages are read apart, so a scan running
// meanwhile can show a copy on a disk that was not in the list when it was read: such a copy counts
// nowhere.
func (s *Status) countCopies(f *catalog.FileRow, at map[string]int) {
	for i := range f.Copies {
		if d, known := at[f.Copies[i].Disk]; known {
			s.Disks[d].Copies++
			s.Disks[d].Bytes += f.Copies[i].Size.Int64
		}
	}
}

func (s *Status) countFile(f *catalog.FileRow, at map[string]int) {
	s.FilesTotal++
	s.BytesTotal += f.Size
	s.ByState[f.State]++
	if s.atLevel(f, at) {
		s.FilesAtLevel++
		s.BytesAtLevel += f.Size
	}
}

// atLevel counts what can be read now: unlike in the plan, a copy on a missing disk does not count,
// even before health.missing_grace (specs.md §15.11).
func (s *Status) atLevel(f *catalog.FileRow, at map[string]int) bool {
	reachable := 0
	for i := range f.Copies {
		c := &f.Copies[i]
		d, known := at[c.Disk]
		if !known || (c.State != string(solver.CopyOK) && c.State != string(solver.CopySurplus)) {
			continue
		}
		switch solver.DiskState(s.Disks[d].State) {
		case solver.DiskOnline, solver.DiskStandby, solver.DiskReadOnly:
			reachable++
		case solver.DiskMissing, solver.DiskDraining, solver.DiskFailed, solver.DiskRetired:
		}
	}
	return reachable >= f.ReplicasTarget+1
}
