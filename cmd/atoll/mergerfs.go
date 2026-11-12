package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/view"
)

// mergerfsBranch is one branch of api/schemas/mergerfs-config.schema.json.
type mergerfsBranch struct {
	Disk    string `json:"disk"`
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	MinFree *int64 `json:"minfreespace_bytes"`
}

// mergerfsOutput is the output of api/schemas/mergerfs-config.schema.json.
type mergerfsOutput struct {
	Mount    string           `json:"mount"`
	Branches []mergerfsBranch `json:"branches"`
	Options  []string         `json:"options"`
	Largest  *int64           `json:"largest_file_bytes"`
	Fstab    string           `json:"fstab"`
}

// runMergerfsConfig prints the mergerfs configuration of specs.md §21.1. It opens the catalog, for each
// disk's capacity, and looks at no disk.
func runMergerfsConfig(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("mergerfs-config: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "mergerfs-config: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out, err := mergerfsConfig(context.Background(), catalogPath(c), c)
	if err != nil {
		return failure(stderr, err)
	}
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, out.Fstab+"\n")
	}
	if err != nil {
		return failure(stderr, err)
	}
	code := exitOK
	for _, b := range out.Branches {
		switch {
		case b.MinFree == nil:
			report(stderr, fmt.Sprintf(T("disk %q: no minfreespace of its own, so mergerfs uses its global one: no scan has recorded the disk's capacity, and placement.min_free holds no size"), b.Disk))
			code = exitPartial
		case out.Largest != nil && *b.MinFree <= *out.Largest:
			report(stderr, fmt.Sprintf(T("warning: disk %q: its minfreespace of %d bytes is not larger than the largest file, of %d bytes"), b.Disk, *b.MinFree, *out.Largest))
		}
	}
	return code
}

// mergerfsConfig reads what the catalog recorded of the disks and of the largest file, and gives the
// configuration. An error is the configuration or the catalog that cannot be used, or a pool mergerfs
// cannot take.
func mergerfsConfig(ctx context.Context, file string, c *config.Config) (out *mergerfsOutput, err error) {
	db, err := catalog.OpenPool(ctx, file, c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, err
	}
	capacities := map[string]int64{}
	for _, d := range list {
		if d.HasCapacity {
			capacities[d.ID] = d.Capacity
		}
	}
	largest, known, err := catalog.LargestFile(ctx, db)
	if err != nil {
		return nil, err
	}
	minFree, err := c.MinFree()
	if err != nil {
		return nil, err
	}
	m, err := view.NewMergerfs(c, minFree, capacities)
	if err != nil {
		return nil, err
	}
	out = &mergerfsOutput{Mount: m.Mount, Branches: make([]mergerfsBranch, 0, len(m.Branches)), Options: m.Options, Fstab: m.Fstab()}
	for _, b := range m.Branches {
		out.Branches = append(out.Branches, mergerfsBranch{Disk: b.Disk, Path: b.Path, Mode: b.Mode, MinFree: b.MinFree})
	}
	if known {
		out.Largest = &largest
	}
	return out, nil
}
