package view

import (
	"fmt"
	"path"
	"strings"

	"github.com/peondusud/atoll/internal/names"
)

// The openmediavault mergerfs plugin, as docs/research/omv-mergerfs.md measured its version 8.0.5 with mergerfs 2.42.0
// (specs.md §21.1): its form makes the pool, and its RPC MergerFS set takes what the form sends.
const (
	// OMVMountDir is where the plugin mounts a pool, as <OMVMountDir>/<name>: <OMV_MOUNT_DIR>/mergerfs, /srv by default.
	OMVMountDir = "/srv/mergerfs"
	// OMVMaxCount is the most the form's minimum free space takes, in its unit.
	OMVMaxCount = 99999
)

// The values of view.omv_min_free (specs.md §24): what the plugin's one minimum free space stands for.
const (
	// OMVLargest is the largest of the branches' own, so that no disk takes a new file past its min_free.
	OMVLargest = "largest"
	// OMVSame gives none when the branches' own differ, so that the user sets one min_free for every disk.
	OMVSame = "same"
)

// omvRefused are the characters the plugin refuses in a path, as a shell or a systemd unit would read them, besides
// ':' and ',', which mergerfs reads as separators and NewMergerfs refuses first, and the line breaks.
const omvRefused = "\"'`$\\;|&<>"

// OMVSize is the plugin's one minimum free space: a count of K, M or G, which mergerfs reads as powers of 1024.
type OMVSize struct {
	Count int64
	Unit  string
	// Bytes is what the pool keeps free. Count of Unit is that much, or more when Rounded.
	Bytes   int64
	Rounded bool
}

// OMV is the pool as the plugin's form takes it, for the same branches and options as the line (specs.md §21.1).
type OMV struct {
	// Name is the pool's name. Mount, where the plugin mounts the pool, is OMVMountDir/Name; Elsewhere says that it is
	// not view.mount.
	Name      string
	Mount     string
	Elsewhere bool
	// Paths are the branches as <path>=<mode>, a line of the form each, in the order of the line.
	Paths []string
	// CreatePolicy is category.create, which the plugin takes from its own field, and Options the other options of
	// the line, in their order.
	CreatePolicy string
	Options      []string
	// MinFree is nil when no branch that counts has a minfreespace of its own, or when Differs.
	MinFree *OMVSize
	// Differs holds, with OMVSame, the branches that count when their minfreespace are not all the same.
	Differs []Branch
}

// OMVRefusal is a branch path that holds a character the plugin refuses.
type OMVRefusal struct {
	Disk, Path, Char string
}

func (e *OMVRefusal) Error() string {
	return fmt.Sprintf("disk %q: its branch %q holds %q, which the openmediavault mergerfs plugin refuses", e.Disk, e.Path, e.Char)
}

// OMVTooLarge is a minimum free space the form cannot hold: more than OMVMaxCount G.
type OMVTooLarge struct {
	Bytes int64
}

func (e *OMVTooLarge) Error() string {
	return fmt.Sprintf("a minimum free space of %d bytes is more than the openmediavault mergerfs plugin's form holds, %dG", e.Bytes, OMVMaxCount)
}

// OMVRefused gives the first character of p that the plugin refuses: one of omvRefused, or a control or a format
// character, line breaks and U+202E RIGHT-TO-LEFT OVERRIDE included, which the human output could not show as it is,
// and so not as the form is to take it (names.Unshowable, specs.md §21.1, §25). ':' and ',' are NewMergerfs's.
func OMVRefused(p string) (string, bool) {
	for _, r := range p {
		if strings.ContainsRune(omvRefused, r) || names.Unshowable(r) {
			return string(r), true
		}
	}
	return "", false
}

// OMVName gives the pool's name, and whether view.mount, mount, is where the plugin mounts it: its last component when
// it is OMVMountDir/<name>, a name the form takes. Otherwise the name proposed is that last component still, or the
// pool's id when the form would refuse it, which an identifier never is (specs.md §29.5).
func OMVName(mount, poolID string) (string, bool) {
	name := path.Base(mount)
	if !omvNameTaken(name) {
		return poolID, false
	}
	return name, path.Dir(mount) == OMVMountDir
}

// omvNameTaken says whether the form takes name as a pool's, as its pattern ^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$ does.
func omvNameTaken(name string) bool {
	for i := range len(name) {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		case (c == '.' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return name != ""
}

// OMVSizeOf writes n bytes as the form takes them: in the largest of G, M and K that holds n exactly with a count of at
// most OMVMaxCount, and otherwise rounded up, in the smallest of them that holds the count, as a larger reserve is the
// safe side. False when even G cannot hold it.
func OMVSizeOf(n int64) (OMVSize, bool) {
	units := []struct {
		name  string
		shift uint
	}{{"G", 30}, {"M", 20}, {"K", 10}}
	for _, u := range units {
		if n%(1<<u.shift) == 0 && n>>u.shift <= OMVMaxCount {
			return OMVSize{Count: n >> u.shift, Unit: u.name, Bytes: n}, true
		}
	}
	for i := len(units) - 1; i >= 0; i-- {
		u := units[i]
		count := n >> u.shift
		if n%(1<<u.shift) != 0 {
			count++
		}
		if count <= OMVMaxCount {
			return OMVSize{Count: count, Unit: u.name, Bytes: n, Rounded: true}, true
		}
	}
	return OMVSize{}, false
}

// OMV gives the pool as the plugin's form takes it, for poolID and view.omv_min_free, minFree. The one minimum free
// space stands for the branches that take new files, RW, as mergerfs reads minfreespace only to place one; every
// branch when none does. An error is a branch path the plugin refuses, an *OMVRefusal, or a minimum free space its form
// cannot hold, an *OMVTooLarge.
func (m *Mergerfs) OMV(poolID, minFree string) (*OMV, error) {
	o := &OMV{Paths: make([]string, 0, len(m.Branches))}
	for _, b := range m.Branches {
		if c, refused := OMVRefused(b.Path); refused {
			return nil, &OMVRefusal{Disk: b.Disk, Path: b.Path, Char: c}
		}
		o.Paths = append(o.Paths, b.Path+"="+b.Mode)
	}
	var there bool
	o.Name, there = OMVName(m.Mount, poolID)
	o.Mount = OMVMountDir + "/" + o.Name
	o.Elsewhere = !there
	for _, option := range m.Options {
		if policy, ok := strings.CutPrefix(option, "category.create="); ok {
			o.CreatePolicy = policy
			continue
		}
		o.Options = append(o.Options, option)
	}
	var counted []Branch
	for _, b := range m.Branches {
		if b.Mode == ModeRW {
			counted = append(counted, b)
		}
	}
	if len(counted) == 0 {
		counted = m.Branches
	}
	var known []Branch
	for _, b := range counted {
		if b.MinFree != nil {
			known = append(known, b)
		}
	}
	if len(known) == 0 {
		return o, nil
	}
	reserve := *known[0].MinFree
	differ := false
	for _, b := range known[1:] {
		differ = differ || *b.MinFree != reserve
		reserve = max(reserve, *b.MinFree)
	}
	if differ && minFree == OMVSame {
		o.Differs = known
		return o, nil
	}
	size, ok := OMVSizeOf(reserve)
	if !ok {
		return nil, &OMVTooLarge{Bytes: reserve}
	}
	o.MinFree = &size
	return o, nil
}
