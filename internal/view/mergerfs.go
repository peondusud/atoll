package view

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/value"
)

// Branch is one branch of mergerfs: the root folder of a data disk (specs.md §21.1).
type Branch struct {
	Disk string
	// Path is <mount>/<root>, not escaped.
	Path string
	// MinFree is the branch's own minfreespace in bytes, nil when it has none: its disk's capacity is
	// not known, and min_free holds only percentages.
	MinFree *int64
	// Mode is RW; NC for a draining disk, whose files stay but which takes no new one; RO for a failed disk,
	// which keeps showing what only it holds and is never written (specs.md §21.1).
	Mode string
}

// The modes of a branch.
const (
	ModeRW = "RW"
	ModeNC = "NC"
	ModeRO = "RO"
)

// Mergerfs is the configuration of the merged view in phase 1, and the break-glass one (specs.md §21.1).
type Mergerfs struct {
	Mount    string
	Branches []Branch
	// Options are key=value, in the order of specs.md §21.1.
	Options []string
}

// mergerfsOptions are the options of specs.md §21.1, in its order. category.create follows
// placement.new_files, and every value of it works as most-free until phase 2b (§24.4), which is mfs.
var mergerfsOptions = []string{
	"category.search=ff",
	"category.action=epall",
	"category.create=mfs",
	"moveonenospc=false",
	"rename-exdev=passthrough",
	"link-exdev=passthrough",
	"cache.attr=1",
	"cache.entry=1",
	"cache.negative_entry=0",
}

// NewMergerfs gives the configuration of the data disks of c, in its order. capacities holds the capacity
// the catalog recorded for each disk it knows one of, by id. An error names what mergerfs cannot take:
// no data disk, or a branch path holding ':' or ','.
func NewMergerfs(c *config.Config, minFree value.MinFree, capacities map[string]int64) (*Mergerfs, error) {
	mount, _ := c.Effective()["view.mount"].(string)
	m := &Mergerfs{Mount: mount, Options: append([]string(nil), mergerfsOptions...)}
	// A failed disk comes after the others, so that category.search=ff reads a copy it holds only when no other
	// disk holds one.
	var order []*config.Disk
	for _, failed := range []bool{false, true} {
		for i := range c.Disks {
			if (c.Disks[i].State == config.DiskFailed) == failed {
				order = append(order, &c.Disks[i])
			}
		}
	}
	for _, d := range order {
		if d.Role != "data" {
			continue // cache disks wait for phase 2b
		}
		b := Branch{Disk: d.ID, Path: path.Join(d.Mount, d.Root), Mode: ModeRW}
		switch d.State {
		case config.DiskDraining:
			b.Mode = ModeNC
		case config.DiskFailed:
			b.Mode = ModeRO
		}
		// mergerfs ends a branch at ':' and an option at ','; '=' is no trouble, as the mode always follows.
		if i := strings.IndexAny(b.Path, ":,"); i >= 0 {
			return nil, fmt.Errorf("disk %q: its branch %q holds %q, which mergerfs reads as a separator", d.ID, b.Path, b.Path[i:i+1])
		}
		if capacity, known := capacities[d.ID]; known {
			reserved, err := minFree.Reserved(capacity)
			if err != nil {
				return nil, fmt.Errorf("disk %q: %w", d.ID, err)
			}
			b.MinFree = &reserved
		} else if size, ok := minFree.SmallestSize(); ok {
			b.MinFree = &size
		}
		m.Branches = append(m.Branches, b)
	}
	if len(m.Branches) == 0 {
		return nil, errors.New("no data disk: mergerfs would have no branch")
	}
	return m, nil
}

// Fstab is the line of /etc/fstab, with no line feed (specs.md §21.1).
func (m *Mergerfs) Fstab() string {
	branches := make([]string, len(m.Branches))
	for i, b := range m.Branches {
		branches[i] = b.Path + "=" + b.Mode
		if b.MinFree != nil {
			branches[i] += "," + mergerfsSize(*b.MinFree)
		}
	}
	return fstabEscape(strings.Join(branches, ":")) + " " + fstabEscape(m.Mount) + " mergerfs " + strings.Join(m.Options, ",") + " 0 0"
}

// mergerfsSize writes a size in the largest unit that holds it exactly, as mergerfs reads them: powers of
// 1024. Otherwise in bytes.
func mergerfsSize(n int64) string {
	for _, u := range []struct {
		suffix string
		shift  uint
	}{{"T", 40}, {"G", 30}, {"M", 20}, {"K", 10}} {
		if n != 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// fstabEscape writes what fstab(5) would read as the end of a field, or as an escape, in octal.
var fstabEscape = strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace
