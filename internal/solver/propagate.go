package solver

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/value"
)

// FileLook is what a look found of a file, or what a row says of one.
type FileLook struct {
	Inode   string
	Size    int64
	MtimeNS int64
	CtimeNS int64
	BtimeNS *int64 // nil when the filesystem or driver gives none (specs.md §7)
}

// SameFile says whether two looks on one disk are of one file (specs.md §15.1). The change time is no
// part of it, as a rename moves it, and an inode alone is not enough, as a new file takes the inode of
// one just removed.
func SameFile(a, b *FileLook) bool {
	if a.Inode == "" || a.Inode != b.Inode {
		return false
	}
	if a.BtimeNS != nil && b.BtimeNS != nil {
		return *a.BtimeNS == *b.BtimeNS
	}
	return a.Size == b.Size && a.MtimeNS == b.MtimeNS
}

func hasNoBirth(a, b *FileLook) bool {
	return a.BtimeNS == nil || b.BtimeNS == nil
}

// CopyVerdict says what a look found of one copy (specs.md §15.12).
type CopyVerdict string

// The verdicts.
const (
	VerdictUnknown   CopyVerdict = "unknown"
	VerdictThere     CopyVerdict = "there"
	VerdictEdited    CopyVerdict = "edited"
	VerdictMoved     CopyVerdict = "moved"
	VerdictUncertain CopyVerdict = "uncertain"
	VerdictReplaced  CopyVerdict = "replaced"
	VerdictOwn       CopyVerdict = "own"
	VerdictGone      CopyVerdict = "gone"
)

// NamedLook is a file a walk found, and its name.
type NamedLook struct {
	Name string
	Look FileLook
}

// VerdictInput is what the verdict about one copy needs. Candidates are the files this walk found at a
// name with no row on this disk that is SameFile as it, with this copy's inode.
type VerdictInput struct {
	NotReached bool
	Row        FileLook
	AtName     *FileLook
	Candidates []NamedLook
	Own        bool // the same file is in the disk's own trash or tmp
}

// VerdictOf gives the verdict about one copy, and the target of one that moved.
func VerdictOf(in *VerdictInput) (CopyVerdict, string) {
	if in.NotReached {
		return VerdictUnknown, ""
	}
	if at := in.AtName; at != nil {
		switch {
		case SameFile(&in.Row, at):
			if in.Row.Size == at.Size && in.Row.MtimeNS == at.MtimeNS && in.Row.CtimeNS == at.CtimeNS {
				return VerdictThere, ""
			}
			return VerdictEdited, ""
		case in.Row.Inode != "" && in.Row.Inode == at.Inode && hasNoBirth(&in.Row, at):
			return VerdictEdited, ""
		}
	}
	same, doubtful := "", 0
	found := 0
	for i := range in.Candidates {
		c := &in.Candidates[i]
		switch {
		case SameFile(&in.Row, &c.Look):
			same = c.Name
			found++
		case in.Row.Inode != "" && in.Row.Inode == c.Look.Inode && hasNoBirth(&in.Row, &c.Look):
			doubtful++
		}
	}
	switch {
	case found == 1 && doubtful == 0:
		return VerdictMoved, same
	case found > 0 || doubtful > 0:
		return VerdictUncertain, ""
	case in.AtName != nil:
		return VerdictReplaced, ""
	case in.Own:
		return VerdictOwn, ""
	default:
		return VerdictGone, ""
	}
}

// PropCopy is one copy of a file, as a decision sees it.
type PropCopy struct {
	Disk        string
	Position    int
	Verdict     CopyVerdict
	Target      string // of a copy that moved
	Due         bool   // its debounce is over
	GoneNS      int64  // of a copy that moved
	CreatedBy   string
	FirstSeenNS int64
	Verified    bool
	OldPath     *string // where it sits, when that is not its file's path
	GuardHeld   bool    // the guard of its disk holds what is gone there
	Size        int64
}

// PropFile is one file to decide about. Follow is the new path it takes because a folder above it was
// renamed on a disk that holds no copy of it.
type PropFile struct {
	Path   string
	State  State
	Follow string
	Copies []PropCopy
}

// PropInput is what the decisions need. Taken lists the paths held by file rows that are not among the
// files given. Direct is deletes.direct.
type PropInput struct {
	Files  []PropFile
	Taken  []string
	Direct string
}

// PropKind says what was decided about a file.
type PropKind string

// The decisions (specs.md §15.12).
const (
	PropNone      PropKind = "none"
	PropWait      PropKind = "wait"
	PropHeld      PropKind = "held"
	PropRename    PropKind = "rename"
	PropPathTaken PropKind = "path-taken"
	PropDeletion  PropKind = "deletion"
	PropKept      PropKind = "kept"
	PropGone      PropKind = "gone"
)

// PropAction says what happens to one copy.
type PropAction string

// What can happen to a copy.
const (
	ActFollow      PropAction = "follow"       // it moved to the target: its row follows
	ActPending     PropAction = "pending"      // it stays where it is, and old_lpath says so
	ActClear       PropAction = "clear"        // it already sits at the target: old_lpath goes
	ActOwnFile     PropAction = "own-file"     // it did not exist when the file left: a file of its own at the old path
	ActOtherTarget PropAction = "other-target" // it moved elsewhere: a file of its own there
	ActStay        PropAction = "stay"         // nothing changes for it
	ActDrop        PropAction = "drop"         // its row goes
	ActKeepGone    PropAction = "keep-gone"    // gone, under a guard: its row stays, with old_lpath where it was
	ActTrash       PropAction = "trash"        // a trash job
)

// PropCopyAction is what happens to one copy.
type PropCopyAction struct {
	Disk   string
	Action PropAction
	Target string // of other-target
}

// PropDecision is what was decided about one file, and its copies by position.
type PropDecision struct {
	Path          string
	Kind          PropKind
	Target        string // of a rename, or what a path-taken wanted
	Src           string // of a deletion: the disk the job says the copy went from
	Copies        []PropCopyAction
	DropHash      bool
	ClearConflict bool
}

// Decide says what happens to each file given, by path in byte order (specs.md §15.12). It moves
// nothing. An error means that a value is outside its closed list, or that a path, a disk or a position
// appears twice.
func Decide(in *PropInput) ([]PropDecision, error) {
	if err := checkProp(in); err != nil {
		return nil, err
	}
	files := slices.Clone(in.Files)
	for i := range files {
		files[i].Copies = slices.Clone(files[i].Copies)
		slices.SortFunc(files[i].Copies, func(a, b PropCopy) int { return cmp.Compare(a.Position, b.Position) })
	}
	slices.SortFunc(files, func(a, b PropFile) int { return cmp.Compare(a.Path, b.Path) })
	targets := assignTargets(files, in.Taken)
	out := make([]PropDecision, 0, len(files))
	for i := range files {
		f := &files[i]
		if isRenameCandidate(f) {
			out = append(out, decideRename(f, targets))
			continue
		}
		out = append(out, decideRest(f, in.Direct))
	}
	return out, nil
}

func goneLike(v CopyVerdict) bool {
	return v == VerdictGone || v == VerdictUncertain || v == VerdictOwn
}

// isRenameCandidate says whether a file is about to take a new name, once its target is free.
func isRenameCandidate(f *PropFile) bool {
	moved := false
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Verdict == VerdictMoved {
			moved = true
		}
		if (c.Verdict == VerdictMoved || goneLike(c.Verdict)) && !c.Due {
			return false
		}
	}
	// A file that only follows waits for what the guard holds: with a new path that row would leave the guard's
	// count, and the hold would end with nobody answering (specs.md §15.12).
	return moved || (f.Follow != "" && !slices.ContainsFunc(f.Copies, func(c PropCopy) bool { return isHeldGone(&c) }))
}

// wanted gives the targets of a file, by the position of the copies that moved there.
func wanted(f *PropFile) []string {
	var list []string
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Verdict == VerdictMoved && !slices.Contains(list, c.Target) {
			list = append(list, c.Target)
		}
	}
	if len(list) == 0 && f.Follow != "" {
		list = append(list, f.Follow)
	}
	return list
}

// targetChoice says which name each file takes, and which other targets its copies keep to themselves.
type targetChoice struct {
	main  map[string]string          // by path of the file
	other map[string]map[string]bool // by path of the file: the other targets it gets
}

// assignTargets is a greatest fixed point: every candidate may leave its path, until one has no target
// that is free or held by a file that leaves, and that one holds its path after all, which may stop
// another. A target wanted twice goes to the first file by path.
func assignTargets(files []PropFile, taken []string) targetChoice {
	holder := make(map[string]*PropFile, len(files))
	in := map[string]bool{}
	for i := range files {
		holder[files[i].Path] = &files[i]
		if isRenameCandidate(&files[i]) {
			in[files[i].Path] = true
		}
	}
	for {
		for dropped := true; dropped; {
			dropped = false
			for i := range files {
				f := &files[i]
				if in[f.Path] && !slices.ContainsFunc(wanted(f), func(t string) bool { return isFree(t, f, holder, in, taken) }) {
					delete(in, f.Path)
					dropped = true
				}
			}
		}
		choice, lost := chooseTargets(files, holder, in, taken)
		if lost == "" {
			return choice
		}
		delete(in, lost)
	}
}

// isFree says whether a target can be had: nothing holds it, or what holds it leaves. A file's own
// path is free to it: a copy still at an old name has only caught up.
func isFree(t string, f *PropFile, holder map[string]*PropFile, in map[string]bool, taken []string) bool {
	if t == f.Path {
		return true
	}
	if slices.Contains(taken, t) {
		return false
	}
	// A holder whose first target is its own path is catching up with it, and does not leave.
	h := holder[t]
	return h == nil || (in[t] && wanted(h)[0] != t)
}

func chooseTargets(files []PropFile, holder map[string]*PropFile, in map[string]bool, taken []string) (targetChoice, string) {
	choice := targetChoice{main: map[string]string{}, other: map[string]map[string]bool{}}
	used := map[string]bool{}
	// A main target first, for every file, then what is left for the others.
	for i := range files {
		f := &files[i]
		if !in[f.Path] {
			continue
		}
		at := slices.IndexFunc(wanted(f), func(t string) bool { return !used[t] && isFree(t, f, holder, in, taken) })
		if at < 0 {
			return choice, f.Path
		}
		t := wanted(f)[at]
		choice.main[f.Path] = t
		used[t] = true
	}
	for i := range files {
		f := &files[i]
		if !in[f.Path] {
			continue
		}
		for _, t := range wanted(f) {
			if t != choice.main[f.Path] && !used[t] && isFree(t, f, holder, in, taken) {
				if choice.other[f.Path] == nil {
					choice.other[f.Path] = map[string]bool{}
				}
				choice.other[f.Path][t] = true
				used[t] = true
			}
		}
	}
	return choice, ""
}

func decideRename(f *PropFile, targets targetChoice) PropDecision {
	target, ok := targets.main[f.Path]
	if !ok {
		first := wanted(f)[0]
		return PropDecision{Path: f.Path, Kind: PropPathTaken, Target: first, Copies: everyCopy(f, ActStay)}
	}
	d := PropDecision{Path: f.Path, Kind: PropRename, Target: target}
	// Only a copy that sat at the file's path says when the file left it.
	leftSince, moved := int64(0), false
	for i := range f.Copies {
		if c := &f.Copies[i]; c.Verdict == VerdictMoved && c.OldPath == nil && (!moved || c.GoneNS < leftSince) {
			leftSince, moved = c.GoneNS, true
		}
	}
	var left, dropped []*PropCopy
	parted := false
	// A row is never behind itself.
	atTarget := func(c *PropCopy) bool {
		return (c.OldPath != nil && *c.OldPath == target) || (c.OldPath == nil && f.Path == target)
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		action := PropCopyAction{Disk: c.Disk}
		switch {
		case c.Verdict == VerdictMoved && c.Target == target:
			action.Action = ActFollow
			left = append(left, c)
		case c.Verdict == VerdictMoved && targets.other[f.Path][c.Target]:
			action.Action, action.Target = ActOtherTarget, c.Target
			parted = true
		case c.Verdict == VerdictMoved:
			// Its target is taken. It stays where its row says, or the row would say it is at the file's new
			// path, where it is not.
			action.Action = ActPending
			if atTarget(c) {
				action.Action = ActClear
			}
			left = append(left, c)
		case c.Verdict == VerdictGone && c.GuardHeld:
			// Held where it sat, unless that is where the file now is.
			action.Action = ActKeepGone
			if atTarget(c) {
				action.Action = ActClear
			}
			left = append(left, c)
		case goneLike(c.Verdict):
			action.Action = ActDrop
			dropped = append(dropped, c)
		case moved && c.OldPath == nil && c.FirstSeenNS >= leftSince && !atTarget(c):
			action.Action = ActOwnFile
			parted = true
		case atTarget(c):
			action.Action = ActClear
			left = append(left, c)
		default:
			action.Action = ActPending
			left = append(left, c)
		}
		d.Copies = append(d.Copies, action)
	}
	d.ClearConflict = clearsConflict(f, left)
	d.DropHash = (len(dropped) > 0 && laterThanWhatWent(dropped, left)) || ((len(dropped) > 0 || parted) && noneVerified(left))
	return d
}

func everyCopy(f *PropFile, action PropAction) []PropCopyAction {
	out := make([]PropCopyAction, 0, len(f.Copies))
	for i := range f.Copies {
		out = append(out, PropCopyAction{Disk: f.Copies[i].Disk, Action: action})
	}
	return out
}

// clearsConflict says that what remains of a file in conflict can no longer differ by what is known:
// the next --full finds any difference of content again (specs.md §15.12).
func clearsConflict(f *PropFile, left []*PropCopy) bool {
	if f.State != StateConflict || len(left) == len(f.Copies) || len(left) == 0 {
		return false
	}
	return !slices.ContainsFunc(left, func(c *PropCopy) bool { return c.Size != left[0].Size })
}

func decideRest(f *PropFile, direct string) PropDecision {
	if mustWait(f) {
		return PropDecision{Path: f.Path, Kind: PropWait, Copies: everyCopy(f, ActStay)}
	}
	var gone, left []*PropCopy
	for i := range f.Copies {
		c := &f.Copies[i]
		if goneLike(c.Verdict) {
			gone = append(gone, c)
		} else {
			left = append(left, c)
		}
	}
	switch {
	case len(gone) == 0:
		return PropDecision{Path: f.Path, Kind: PropNone, Copies: everyCopy(f, ActStay)}
	case slices.ContainsFunc(gone, isHeldGone):
		return PropDecision{Path: f.Path, Kind: PropHeld, Copies: everyCopy(f, ActStay)}
	case len(left) == 0:
		return PropDecision{Path: f.Path, Kind: PropGone, Copies: everyCopy(f, ActDrop)}
	case isDeletion(f, gone, left, direct):
		d := PropDecision{Path: f.Path, Kind: PropDeletion, Src: gone[0].Disk}
		for i := range f.Copies {
			action := ActTrash
			if goneLike(f.Copies[i].Verdict) {
				action = ActDrop
			}
			d.Copies = append(d.Copies, PropCopyAction{Disk: f.Copies[i].Disk, Action: action})
		}
		return d
	}
	d := PropDecision{Path: f.Path, Kind: PropKept, ClearConflict: clearsConflict(f, left), DropHash: laterThanWhatWent(gone, left) || noneVerified(left)}
	for i := range f.Copies {
		action := ActStay
		if goneLike(f.Copies[i].Verdict) {
			action = ActDrop
		}
		d.Copies = append(d.Copies, PropCopyAction{Disk: f.Copies[i].Disk, Action: action})
	}
	return d
}

func mustWait(f *PropFile) bool {
	return slices.ContainsFunc(f.Copies, func(c PropCopy) bool { return (goneLike(c.Verdict) || c.Verdict == VerdictMoved) && !c.Due })
}

// isHeldGone says what the guard holds: a copy gone from a disk it speaks for, wherever it sat, as what
// comes back must find its row.
func isHeldGone(c *PropCopy) bool {
	return c.Verdict == VerdictGone && c.GuardHeld
}

func isDeletion(f *PropFile, gone, left []*PropCopy, direct string) bool {
	if direct != "propagate" || f.State == StateConflict || f.State == StateCorrupt {
		return false
	}
	earliest := gone[0].FirstSeenNS
	for _, c := range gone {
		if c.Verdict != VerdictGone || c.OldPath != nil {
			return false
		}
		earliest = min(earliest, c.FirstSeenNS)
	}
	allVerified := !slices.ContainsFunc(gone, func(c *PropCopy) bool { return !c.Verified })
	for _, c := range left {
		if c.Verdict != VerdictThere && c.Verdict != VerdictUnknown {
			return false
		}
		peer := c.CreatedBy == CreatedByAtoll || (c.Verified && allVerified && c.FirstSeenNS <= earliest)
		if !peer {
			return false
		}
	}
	return true
}

// laterThanWhatWent says that every copy left came after what went, so that what a hash describes went
// with those: an atomic save of the same size must not read as a conflict (specs.md §15.12).
func laterThanWhatWent(gone, left []*PropCopy) bool {
	earliest := gone[0].FirstSeenNS
	for _, c := range gone {
		earliest = min(earliest, c.FirstSeenNS)
	}
	return !slices.ContainsFunc(left, func(c *PropCopy) bool { return c.FirstSeenNS <= earliest })
}

// noneVerified says that no copy left was read against the hash, which so describes only what went: the next
// --full must not compare what is left with a version no copy holds (specs.md §15.12).
func noneVerified(left []*PropCopy) bool {
	return !slices.ContainsFunc(left, func(c *PropCopy) bool { return c.Verified })
}

func checkProp(in *PropInput) error {
	if in.Direct != "propagate" && in.Direct != "restore" {
		return fmt.Errorf("propagation: direct %q", in.Direct)
	}
	paths := make(map[string]bool, len(in.Files))
	for i := range in.Files {
		f := &in.Files[i]
		if paths[f.Path] {
			return fmt.Errorf("propagation: path %q appears twice", f.Path)
		}
		paths[f.Path] = true
		if _, err := stateRank(f.State); err != nil {
			return fmt.Errorf("propagation: %w", err)
		}
		if err := checkPropCopies(f); err != nil {
			return err
		}
	}
	return nil
}

func checkPropCopies(f *PropFile) error {
	disks := make(map[string]bool, len(f.Copies))
	positions := make(map[int]bool, len(f.Copies))
	for i := range f.Copies {
		c := &f.Copies[i]
		if disks[c.Disk] || positions[c.Position] {
			return fmt.Errorf("propagation: disk %q or position %d appears twice", c.Disk, c.Position)
		}
		disks[c.Disk], positions[c.Position] = true, true
		switch c.Verdict {
		case VerdictMoved:
			if c.Target == "" {
				return fmt.Errorf("propagation: %q moved with no target", f.Path)
			}
		case VerdictUnknown, VerdictThere, VerdictEdited, VerdictUncertain, VerdictReplaced, VerdictOwn, VerdictGone:
		default:
			return fmt.Errorf("propagation: unknown verdict %q", c.Verdict)
		}
		if c.CreatedBy != CreatedByUser && c.CreatedBy != CreatedByAtoll {
			return fmt.Errorf("propagation: created by %q", c.CreatedBy)
		}
	}
	return nil
}

// GuardInput is what the guard of one disk looks at (specs.md §15.6). Count is what disappeared, and
// CountPercent that less what has nothing left to send to the trash, which counts toward Files only.
type GuardInput struct {
	Count        int64
	CountPercent int64
	Rows         int64
	Files        int64
	Percent      value.Percent
}

// GuardTrips says whether the guard of a disk holds what is gone there, compared exactly.
func GuardTrips(in *GuardInput) bool {
	return in.Count > in.Files || in.Percent.Exceeded(in.CountPercent, in.Rows)
}
