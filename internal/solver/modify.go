package solver

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
)

// ModCopy is one copy of a file, as a modification sees it (specs.md §15.4). A pending copy is no copy here: its
// disk holds nothing of it yet.
type ModCopy struct {
	Disk     string
	Position int
	Verdict  CopyVerdict
	// Other says that a copy replaced holds what is no regular file, or the file another copy of its disk moved to.
	Other    bool
	State    CopyState
	Size     int64  // what its look found for a changed copy, what its row says otherwise
	Due      bool   // a changed copy's settle has passed since its change time
	Readable bool   // its disk can be read in this pass
	Hash     []byte // what a read gave, nil until it is read
}

// ModInput is a file a copy of which may have changed. Hash is the file's, nil when it has none, and Running says
// that a job of it is running.
type ModInput struct {
	State   State
	Size    int64
	Hash    []byte
	Running bool
	Copies  []ModCopy
}

// ModKind says what a modification decides.
type ModKind string

// What a modification decides (specs.md §15.4).
const (
	ModNone     ModKind = "none"
	ModWait     ModKind = "wait"
	ModRead     ModKind = "read"
	ModModified ModKind = "modified"
	ModMetadata ModKind = "metadata"
	ModConflict ModKind = "conflict"
)

// ModDecision is what a modification decides, each list by position. Read lists what to read before asking again;
// Reference is the copy read to compare with, where the file had no hash; Hash is what the file's hash becomes, nil
// when nothing says.
type ModDecision struct {
	Kind      ModKind
	Read      []string
	Changed   []string
	Stale     []string
	Reference string
	Hash      []byte
}

// Modify says what a pass does with a file whose copy is edited or replaced (specs.md §15.4). It reads nothing:
// asked with copies not yet read, it says which to read, and asked again with what they gave, it decides. An error
// means a value outside its closed list, a pending copy, or a disk or position that appears twice.
func Modify(in *ModInput) (ModDecision, error) {
	g, err := gate(in)
	if err != nil || g.kind != "" {
		return ModDecision{Kind: g.kind}, err
	}
	copies, changed, others := g.copies, g.changed, g.others
	d := ModDecision{Changed: disksOf(changed)}
	if slices.ContainsFunc(changed, func(c *ModCopy) bool { return c.State == CopyStale }) ||
		slices.ContainsFunc(changed, func(c *ModCopy) bool { return c.Size != changed[0].Size }) {
		d.Kind = ModConflict
		return d, nil
	}
	if changed[0].Size != in.Size && len(changed) == 1 {
		d.Kind, d.Stale = ModModified, disksOf(others)
		return d, nil
	}
	var reference *ModCopy
	if changed[0].Size == in.Size && in.Hash == nil && len(others) > 0 {
		at := slices.IndexFunc(others, func(c *ModCopy) bool {
			return c.Verdict == VerdictThere && (c.State == CopyOK || c.State == CopySurplus) && c.Readable
		})
		if at < 0 {
			return ModDecision{Kind: ModWait}, nil
		}
		reference = others[at]
	}
	if read := toRead(copies, changed, reference); len(read) > 0 {
		// What cannot be read now waits for a pass that can.
		if slices.ContainsFunc(read, func(c *ModCopy) bool { return !c.Readable }) {
			return ModDecision{Kind: ModWait}, nil
		}
		return ModDecision{Kind: ModRead, Read: disksOf(read)}, nil
	}
	content := changed[0].Hash
	if slices.ContainsFunc(changed, func(c *ModCopy) bool { return !bytes.Equal(c.Hash, content) }) {
		d.Kind = ModConflict
		return d, nil
	}
	ref := in.Hash
	if reference != nil {
		ref = reference.Hash
	}
	// A hash is never empty, so with no reference this is false.
	if changed[0].Size == in.Size && bytes.Equal(content, ref) {
		d.Kind, d.Hash = ModMetadata, content
		if reference != nil {
			d.Reference = reference.Disk
		}
		return d, nil
	}
	d.Kind, d.Stale, d.Hash = ModModified, disksOf(others), content
	return d, nil
}

// ModifyGate says what Modify says before anything depends on a size: ModNone with nothing to decide, ModWait
// while the file waits, and ModRead when a look at its changed copies comes next, for their size now.
func ModifyGate(in *ModInput) (ModKind, error) {
	g, err := gate(in)
	if err != nil {
		return "", err
	}
	if g.kind == "" {
		return ModRead, nil
	}
	return g.kind, nil
}

// gated is a file's copies by position, the changed ones apart, and what stops a decision before any size counts.
type gated struct {
	kind                    ModKind
	copies, changed, others []*ModCopy
}

func gate(in *ModInput) (gated, error) {
	if err := checkMod(in); err != nil {
		return gated{}, err
	}
	var g gated
	g.copies = make([]*ModCopy, 0, len(in.Copies))
	for i := range in.Copies {
		g.copies = append(g.copies, &in.Copies[i])
	}
	slices.SortFunc(g.copies, func(a, b *ModCopy) int { return cmp.Compare(a.Position, b.Position) })
	for _, c := range g.copies {
		if (c.Verdict == VerdictEdited || c.Verdict == VerdictReplaced) && !c.Other {
			g.changed = append(g.changed, c)
		} else {
			g.others = append(g.others, c)
		}
	}
	switch {
	case len(g.changed) == 0 || in.State == StateConflict || in.State == StateCorrupt:
		g.kind = ModNone
	case mustWaitMod(in, g.copies):
		g.kind = ModWait
	}
	return g, nil
}

// mustWaitMod says that what becomes of a file is decided elsewhere first, or later (specs.md §15.4).
func mustWaitMod(in *ModInput, copies []*ModCopy) bool {
	if in.Running {
		return true
	}
	return slices.ContainsFunc(copies, func(c *ModCopy) bool {
		switch c.Verdict {
		case VerdictMoved, VerdictGone, VerdictUncertain, VerdictOwn:
			return true
		case VerdictEdited:
			return !c.Due
		case VerdictReplaced:
			return c.Other || !c.Due
		case VerdictUnknown, VerdictThere:
		}
		return false
	})
}

// toRead gives the copies a decision still needs to have read, by position.
func toRead(copies, changed []*ModCopy, reference *ModCopy) []*ModCopy {
	var out []*ModCopy
	for _, c := range copies {
		if c.Hash == nil && (slices.Contains(changed, c) || c == reference) {
			out = append(out, c)
		}
	}
	return out
}

func disksOf(list []*ModCopy) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Disk)
	}
	return out
}

func checkMod(in *ModInput) error {
	if _, err := stateRank(in.State); err != nil {
		return fmt.Errorf("modification: %w", err)
	}
	disks := make(map[string]bool, len(in.Copies))
	positions := make(map[int]bool, len(in.Copies))
	for i := range in.Copies {
		c := &in.Copies[i]
		if disks[c.Disk] || positions[c.Position] {
			return fmt.Errorf("modification: disk %q or position %d appears twice", c.Disk, c.Position)
		}
		disks[c.Disk], positions[c.Position] = true, true
		switch c.Verdict {
		case VerdictUnknown, VerdictThere, VerdictEdited, VerdictMoved, VerdictUncertain, VerdictReplaced, VerdictOwn, VerdictGone:
		default:
			return fmt.Errorf("modification: unknown verdict %q", c.Verdict)
		}
		if c.Other && c.Verdict != VerdictReplaced {
			return fmt.Errorf("modification: %q holds another file, and is %s", c.Disk, c.Verdict)
		}
		switch c.State {
		case CopyOK, CopySurplus, CopyStale, CopyCorrupt, CopyUnavailable:
		case CopyPending:
			return fmt.Errorf("modification: %q is pending, and has nothing on its disk", c.Disk)
		default:
			return fmt.Errorf("modification: unknown copy state %q", c.State)
		}
	}
	return nil
}
