package watch

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/solver"
)

// CopySeen is the verdict about a copy that is not as its row says (specs.md §15.12), kept small, as a
// rename of a whole pool has one for each copy. Target and what follows it are of a copy that moved: what
// the walk found at the target, and SameDate says that its size and date are what the row says. AsRow is of a
// copy unknown for being at a name only a reprise can tell of: what is there is what its row says, or nothing, as
// that operation moved it.
type CopySeen struct {
	FileID   int64
	Verdict  solver.CopyVerdict
	Target   string
	CtimeNS  int64
	Nlink    int64
	SameDate bool
	AsRow    bool
}

// folderID is what makes a folder the same one.
type folderID struct {
	key   string
	btime int64
}

// FolderMove is a folder that a look found renamed, the same one at another name.
type FolderMove struct {
	From string
	To   string
}

// compared is what comparing a walk with the rows of a disk holds while it runs.
type compared struct {
	notReached map[string]bool
	r          *Report
	found      func(*FoundFile)
	// quiet is a pass's look: nothing reads what a scan prints, and it holds a line for each copy.
	quiet bool
	// rowless holds the files found at a name no row holds, in byte order, and others those at a name
	// whose row is another file: only these can be where a copy went.
	rowless []*FoundFile
	others  []*FoundFile
	// open holds the rows whose name does not hold them, by pointer, as a slice of these would be copied
	// as it grows, on a look where all of them moved.
	open []*openRow
	// unreached holds what the rows the walk could not get to say.
	unreached []solver.FileLook
	// planned holds the names only a reprise can tell of.
	planned map[string]bool
	// behind holds the paths of files whose copy on this disk still sits elsewhere.
	behind map[string]bool
	clocks []catalog.CopyClock
}

// openRow is a row whose copy is not where it says, until resolve says what became of it. A rename of a whole disk
// holds one for each of its copies, so its row is kept compact (specs.md §26).
type openRow struct {
	k       knownRow
	at      *FoundFile
	verdict solver.CopyVerdict
	target  *FoundFile
}

// knownRow is a catalog.KnownCopy without the room each nullable field takes: 120 bytes instead of 168.
type knownRow struct {
	path, filePath, fileKey, goneKind       string
	fileID, size, mtime, ctime, btime, gone int64
	valid                                   uint8
}

// What valid says holds a value.
const (
	validSize uint8 = 1 << iota
	validMtime
	validCtime
	validBtime
	validGone
	validKey
	validKind
)

func compactRow(k *catalog.KnownCopy) knownRow {
	r := knownRow{path: k.Path, filePath: k.FilePath, fileKey: k.FileKey.String, goneKind: k.GoneKind.String, fileID: k.FileID,
		size: k.Size.Int64, mtime: k.MtimeNS.Int64, ctime: k.CtimeNS.Int64, btime: k.BtimeNS.Int64, gone: k.GoneNS.Int64}
	for _, f := range []struct {
		valid bool
		bit   uint8
	}{{k.Size.Valid, validSize}, {k.MtimeNS.Valid, validMtime}, {k.CtimeNS.Valid, validCtime}, {k.BtimeNS.Valid, validBtime},
		{k.GoneNS.Valid, validGone}, {k.FileKey.Valid, validKey}, {k.GoneKind.Valid, validKind}} {
		if f.valid {
			r.valid |= f.bit
		}
	}
	return r
}

// known gives the row as the catalog had it.
func (r *knownRow) known() catalog.KnownCopy {
	return catalog.KnownCopy{
		Path: r.path, FilePath: r.filePath, FileID: r.fileID,
		Size:     sql.NullInt64{Int64: r.size, Valid: r.valid&validSize != 0},
		MtimeNS:  sql.NullInt64{Int64: r.mtime, Valid: r.valid&validMtime != 0},
		CtimeNS:  sql.NullInt64{Int64: r.ctime, Valid: r.valid&validCtime != 0},
		BtimeNS:  sql.NullInt64{Int64: r.btime, Valid: r.valid&validBtime != 0},
		GoneNS:   sql.NullInt64{Int64: r.gone, Valid: r.valid&validGone != 0},
		FileKey:  sql.NullString{String: r.fileKey, Valid: r.valid&validKey != 0},
		GoneKind: sql.NullString{String: r.goneKind, Valid: r.valid&validKind != 0},
	}
}

// RowLook says what a row says of a file, to compare with a look.
func RowLook(k *catalog.KnownCopy) solver.FileLook {
	look := solver.FileLook{Inode: k.FileKey.String, Size: k.Size.Int64, MtimeNS: k.MtimeNS.Int64, CtimeNS: k.CtimeNS.Int64}
	if k.BtimeNS.Valid {
		look.BtimeNS = &k.BtimeNS.Int64
	}
	return look
}

// FoundLook says what a walk found of a file.
func FoundLook(f *FoundFile) solver.FileLook {
	return solver.FileLook{Inode: f.FileKey, Size: f.Size, MtimeNS: f.MtimeNS, CtimeNS: f.CtimeNS, BtimeNS: f.BtimeNS}
}

// clock says what a verdict does to a copy's clock, when that changes anything.
func (c *compared) clock(k *catalog.KnownCopy, v solver.CopyVerdict) {
	switch v {
	case solver.VerdictMoved, solver.VerdictGone, solver.VerdictUncertain, solver.VerdictOwn:
		if !k.GoneNS.Valid || k.GoneKind.String != string(v) {
			c.clocks = append(c.clocks, catalog.CopyClock{FileID: k.FileID, Kind: string(v)})
		}
	case solver.VerdictThere, solver.VerdictEdited, solver.VerdictReplaced:
		if k.GoneNS.Valid {
			c.clocks = append(c.clocks, catalog.CopyClock{FileID: k.FileID})
		}
	case solver.VerdictUnknown:
	}
}

// row looks at one row, and what its name holds. What takes a look elsewhere waits until every row has
// been looked at.
func (c *compared) row(k *catalog.KnownCopy, at *FoundFile) {
	if k.Path != k.FilePath {
		if c.behind == nil {
			c.behind = map[string]bool{}
		}
		c.behind[k.FilePath] = true
	}
	if atOrBelow(c.notReached, k.Path) {
		seen := CopySeen{FileID: k.FileID, Verdict: solver.VerdictUnknown}
		if at != nil {
			c.found(at)
			// That row says nothing of what else came to its name, and the row it left must not read as gone.
			rowLook, look := RowLook(k), FoundLook(at)
			if !solver.SameFile(&rowLook, &look) {
				c.others = append(c.others, at)
			}
			v, _ := solver.VerdictOf(&solver.VerdictInput{Row: rowLook, AtName: &look})
			seen.AsRow = v == solver.VerdictThere
		} else {
			// On its own copy of the row: the next row is read into this one, and a look points into it.
			row := *k
			c.unreached = append(c.unreached, RowLook(&row))
			seen.AsRow = atOrBelow(c.planned, k.Path)
		}
		c.r.Seen = append(c.r.Seen, seen)
		return
	}
	if at != nil {
		c.found(at)
		look := FoundLook(at)
		v, _ := solver.VerdictOf(&solver.VerdictInput{Row: RowLook(k), AtName: &look})
		if v == solver.VerdictThere {
			at.home = true
			c.r.Known++
			c.clock(k, v)
			return
		}
		if v == solver.VerdictEdited {
			at.home = true
			c.r.Changed = append(c.r.Changed, k.Path)
			c.r.Seen = append(c.r.Seen, CopySeen{FileID: k.FileID, Verdict: v})
			c.clock(k, v)
			return
		}
		c.others = append(c.others, at)
	}
	c.open = append(c.open, &openRow{k: compactRow(k), at: at})
}

// markStraddled finds a file renamed while the walk went: found at a name no row holds, with one link, and
// the same file was found at a name a row holds, or a row of it could not be reached. Two names with one
// link are one file, and so are two with as many links as each other and another change time, as a link outside
// the root leaves: a new name adds to the links, and a rename only moves that time. Given a row, the row it
// left could only read as gone at the next look, as a file with a row is no candidate; given none, that look
// reads the rename (specs.md §15.10). A name recorded as an ignored entry has its row, and is none of these.
func (c *compared) markStraddled(ctx context.Context, db *sql.DB, d *config.Disk, w *Walked) error {
	// A first look has rows for none of what it found, and nothing to compare.
	if len(c.rowless) == 0 || (len(c.rowless) == len(w.Files) && len(c.unreached) == 0) {
		return nil
	}
	ignored, err := catalog.IgnoredPaths(ctx, db, d.ID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	index := make(map[string]*FoundFile, len(c.rowless))
	for _, f := range c.rowless {
		if !ignored[f.Path] {
			index[f.FileKey] = f
		}
	}
	for i := range w.Files {
		f := &w.Files[i]
		if g := index[f.FileKey]; g != nil && !f.rowless && (g.Nlink == 1 || (g.Nlink == f.Nlink && g.CtimeNS != f.CtimeNS)) {
			fLook, gLook := FoundLook(f), FoundLook(g)
			g.straddled = g.straddled || solver.SameFile(&fLook, &gLook)
		}
	}
	for i := range c.unreached {
		look := &c.unreached[i]
		if g := index[look.Inode]; g != nil && (g.Nlink == 1 || g.CtimeNS != look.CtimeNS) {
			gLook := FoundLook(g)
			g.straddled = g.straddled || solver.SameFile(look, &gLook)
		}
	}
	return nil
}

// resolve gives each open row its verdict, now that every file no row holds is known. A name recorded as
// an ignored entry has a row of its own, and so is no place a copy went to. A name that now holds a folder,
// a symlink or another kind of file holds another file: an empty look says so.
func (c *compared) resolve(ctx context.Context, db *sql.DB, d *config.Disk, w *Walked, look *Look) error {
	if len(c.open) == 0 {
		return nil
	}
	ignored, err := catalog.IgnoredPaths(ctx, db, d.ID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	index := make(map[string]*FoundFile, len(c.rowless)+len(c.others))
	for _, list := range [][]*FoundFile{c.rowless, c.others} {
		for _, f := range list {
			if !ignored[f.Path] {
				f.sameInode = index[f.FileKey]
				index[f.FileKey] = f
			}
		}
	}
	entries := make(map[string]bool, len(w.Entries))
	links := map[string]bool{}
	for i := range w.Entries {
		entries[w.Entries[i].Path] = true
		if w.Entries[i].Kind == solver.EntrySymlink {
			links[w.Entries[i].Path] = true
		}
	}
	gone := map[string]bool{}
	for _, o := range c.open {
		known := o.k.known()
		in := solver.VerdictInput{Row: RowLook(&known)}
		if o.at != nil {
			at := FoundLook(o.at)
			in.AtName = &at
		} else if entries[o.k.path] || atOrBelow(links, o.k.path) {
			in.AtName = &solver.FileLook{}
		}
		for f := index[o.k.fileKey]; f != nil; f = f.sameInode {
			if f != o.at {
				in.Candidates = append(in.Candidates, solver.NamedLook{Name: f.Path, Look: FoundLook(f)})
			}
		}
		v, target := solver.VerdictOf(&in)
		if v == solver.VerdictGone && carriedOutOfReach(c.r.FolderMoves, c.notReached, o.k.path) {
			v = solver.VerdictUnknown
		}
		o.verdict = v
		if v == solver.VerdictMoved {
			o.target = findFile(index[o.k.fileKey], target)
			o.target.claims++
		}
		if v == solver.VerdictGone {
			gone[o.k.fileKey] = true
		}
	}
	own, err := c.ownFiles(d, look, gone)
	if err != nil {
		return err
	}
	// A list grown copy by copy is twice the size of what it holds for a moment, once a disk holds 200,000.
	c.r.Seen = slices.Grow(c.r.Seen, len(c.open))
	c.clocks = slices.Grow(c.clocks, len(c.open))
	for _, o := range c.open {
		c.finish(o, own)
	}
	return nil
}

// carriedOutOfReach says that a row sits below a folder renamed, at what is now a name the walk did not enter, as
// a mount below a folder goes with it: it says nothing of the file, at that name as at the old one.
func carriedOutOfReach(moves []FolderMove, notReached map[string]bool, path string) bool {
	for i := range moves {
		m := &moves[i]
		if strings.HasPrefix(path, m.From+"/") && atOrBelow(notReached, m.To+strings.TrimPrefix(path, m.From)) {
			return true
		}
	}
	return false
}

func findFile(first *FoundFile, name string) *FoundFile {
	for f := first; f != nil; f = f.sameInode {
		if f.Path == name {
			return f
		}
	}
	return nil
}

func (c *compared) ownFiles(d *config.Disk, look *Look, gone map[string]bool) (map[string][]solver.FileLook, error) {
	if look.Own == nil || len(gone) == 0 {
		return nil, nil //nolint:nilnil // a look that does not ask finds nothing
	}
	own, err := look.Own(d, gone)
	if err != nil {
		return nil, fmt.Errorf("own folders of %q: %w", d.ID, err)
	}
	return own, nil
}

func (c *compared) finish(o *openRow, own map[string][]solver.FileLook) {
	v, target := o.verdict, o.target
	k := o.k.known()
	seen := CopySeen{FileID: k.FileID}
	switch {
	case v == solver.VerdictMoved && target.claims > 1:
		// Two rows say that one name is theirs, as hard links seen as two files can.
		v = solver.VerdictUncertain
	case v == solver.VerdictMoved:
		seen.Target, seen.CtimeNS, seen.Nlink = target.Path, target.CtimeNS, target.Nlink
		seen.SameDate = k.Size.Valid && k.Size.Int64 == target.Size && k.MtimeNS.Valid && k.MtimeNS.Int64 == target.MtimeNS
		target.isTarget = true
		if !c.quiet {
			c.r.Moved = append(c.r.Moved, MovedCopy{Path: k.Path, To: target.Path})
		}
	case v == solver.VerdictGone && isOwn(&k, own):
		v = solver.VerdictOwn
	case v == solver.VerdictGone && k.GoneKind.String == string(solver.VerdictUncertain):
		// Its names got rows of their own, and they say no more than the first look did (specs.md §15.12).
		v = solver.VerdictUncertain
	}
	seen.Verdict = v
	switch v {
	case solver.VerdictReplaced:
		c.r.Changed = append(c.r.Changed, k.Path)
	case solver.VerdictGone, solver.VerdictUncertain, solver.VerdictOwn:
		if !c.quiet {
			c.r.Absent = append(c.r.Absent, k.Path)
		}
	case solver.VerdictThere, solver.VerdictEdited, solver.VerdictMoved, solver.VerdictUnknown:
	}
	c.r.Seen = append(c.r.Seen, seen)
	c.clock(&k, v)
}

func isOwn(k *catalog.KnownCopy, own map[string][]solver.FileLook) bool {
	row := RowLook(k)
	for i, list := 0, own[k.FileKey.String]; i < len(list); i++ {
		if solver.SameFile(&row, &list[i]) {
			return true
		}
	}
	return false
}

// moveFolders finds the folders renamed: a folder row the walk did not find, or found another folder in
// place of, and the same folder with no row. The row and all below it stay, and the new name and all below
// it get none, until the files below are decided (specs.md §15.12). It says which new names those are.
func moveFolders(ctx context.Context, db *sql.DB, disk string, w *Walked, r *Report, out *catalog.Walk, notReached map[string]bool) (map[string]bool, error) {
	walked := make(map[string]*FoundEntry, len(w.Entries))
	for i := range w.Entries {
		if e := &w.Entries[i]; e.Kind == solver.EntryDir {
			walked[e.Path] = e
		}
	}
	var absent []catalog.EntryIdentity
	rows := make(map[string]bool, len(walked))
	// The folders found at their own names. An inode number alone is no folder: each btrfs subvolume has its own.
	here := make(map[folderID]bool, len(walked))
	err := catalog.EachFolderRow(ctx, db, disk, func(e *catalog.EntryIdentity) {
		rows[e.Path] = true
		if atOrBelow(notReached, e.Path) {
			return
		}
		if isAnotherFolder(walked[e.Path], e) {
			absent = append(absent, *e)
		} else {
			here[folderID{e.FileKey.String, e.BtimeNS.Int64}] = true
		}
	})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	byKey := map[folderID]*FoundEntry{}
	tops := map[string]bool{}
	for path, e := range walked {
		if rows[path] || e.FileKey == nil || e.BtimeNS == nil {
			continue
		}
		// Where the walk did not enter it is no name a rename can be read from, as a bind mount shows one folder at
		// two. Of several, the first in byte order, as map order must decide nothing.
		if atOrBelow(notReached, path) {
			continue
		}
		id := folderID{*e.FileKey, *e.BtimeNS}
		if prev := byKey[id]; prev == nil || path < prev.Path {
			byKey[id] = e
		}
		// One folder at two names: renamed once the walk had read the old one. No row, and the next look reads it.
		if here[id] {
			tops[path] = true
		}
	}
	if len(absent) == 0 {
		return tops, nil
	}
	out.Kept = map[string]bool{}
	for i := range absent {
		a := &absent[i]
		e := byKey[folderID{a.FileKey.String, a.BtimeNS.Int64}]
		// Found at neither name, as one moved during the walk from what it had not listed to what it had read. Its row
		// stays while a copy of that disk still sits below it: a look that finds the new name reads the rename by it.
		if e == nil && walked[a.Path] == nil && a.BtimeNS.Valid {
			copies, err := catalog.CopiesBelow(ctx, db, disk, a.Path)
			if err != nil {
				return nil, fmt.Errorf("%w", err)
			}
			if copies > 0 {
				if err := keepBelow(ctx, db, disk, a.Path, out); err != nil {
					return nil, err
				}
				r.UnfoundFolders = append(r.UnfoundFolders, a.Path)
			}
			continue
		}
		// Below itself is no rename another disk could make, and its followers would read as still below it.
		if e == nil || !a.BtimeNS.Valid || strings.HasPrefix(e.Path, a.Path+"/") || holdsItsFile(w, a.Path) {
			continue
		}
		r.FolderMoves = append(r.FolderMoves, FolderMove{From: a.Path, To: e.Path})
		tops[e.Path] = true
		if err := keepBelow(ctx, db, disk, a.Path, out); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(r.FolderMoves, func(a, b FolderMove) int { return cmp.Compare(a.From, b.From) })
	return tops, nil
}

func keepBelow(ctx context.Context, db *sql.DB, disk, folder string, out *catalog.Walk) error {
	out.Kept[folder] = true
	below, err := catalog.EntriesBelow(ctx, db, disk, folder)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	for _, p := range below {
		out.Kept[p] = true
	}
	return nil
}

// isAnotherFolder says that a folder row's name holds no folder, or one made since: an application that
// recreates its folder must not hide the rename of the one it had.
func isAnotherFolder(found *FoundEntry, e *catalog.EntryIdentity) bool {
	return found == nil || (found.FileKey != nil && *found.FileKey != e.FileKey.String) || (found.BtimeNS != nil && *found.BtimeNS != e.BtimeNS.Int64)
}

// holdsItsFile says that a file below a folder is what the row at its name says: the folder at that name is no
// folder renamed away with all it held, or that file went back there. The files of a walk are in byte order
// by now.
func holdsItsFile(w *Walked, folder string) bool {
	prefix := folder + "/"
	at, _ := slices.BinarySearchFunc(w.Files, prefix, func(f FoundFile, p string) int { return cmp.Compare(f.Path, p) })
	for ; at < len(w.Files) && strings.HasPrefix(w.Files[at].Path, prefix); at++ {
		if w.Files[at].home {
			return true
		}
	}
	return false
}

// SeenOf finds the verdict about one copy of a file, and nil when it has none: as its row says.
func SeenOf(list []CopySeen, fileID int64) *CopySeen {
	at, found := slices.BinarySearchFunc(list, fileID, func(s CopySeen, id int64) int { return cmp.Compare(s.FileID, id) })
	if !found {
		return nil
	}
	return &list[at]
}

// SortSeen puts the verdicts in the order SeenOf looks them up in.
func SortSeen(list []CopySeen) {
	slices.SortFunc(list, func(a, b CopySeen) int { return cmp.Compare(a.FileID, b.FileID) })
}
