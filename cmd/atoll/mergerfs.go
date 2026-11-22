package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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

// mergerfsOutput is the output of api/schemas/mergerfs-config.schema.json. OMV is there only with --omv.
type mergerfsOutput struct {
	Mount    string           `json:"mount"`
	Branches []mergerfsBranch `json:"branches"`
	Options  []string         `json:"options"`
	Largest  *int64           `json:"largest_file_bytes"`
	Fstab    string           `json:"fstab"`
	OMV      *omvOutput       `json:"omv,omitempty"`
}

// omvRPC is what the openmediavault mergerfs plugin's RPC MergerFS set takes, as its form sends it: the omv.rpc object
// of api/schemas/mergerfs-config.schema.json.
type omvRPC struct {
	Name         string  `json:"name"`
	Paths        string  `json:"paths"`
	CreatePolicy string  `json:"createpolicy"`
	MinFree      *int64  `json:"minfreespace"`
	MinFreeUnit  *string `json:"minfreespaceunit"`
	Options      string  `json:"options"`
}

// omvOutput is the omv object of api/schemas/mergerfs-config.schema.json.
type omvOutput struct {
	RPC      omvRPC   `json:"rpc"`
	Mount    string   `json:"mount"`
	Warnings []string `json:"warnings"`
}

// The warnings of the omv object, a closed list, in byte order.
const (
	omvMinFreeDiffers = "minfreespace-differs"
	omvMinFreeRounded = "minfreespace-rounded"
	omvMountElsewhere = "mount-elsewhere"
)

// omvUnits are the labels of the form's unit, as it shows them: openmediavault translates none of them.
var omvUnits = map[string]string{"K": "Kilobytes", "M": "Megabytes", "G": "Gigabytes"}

// omvShifts are the powers of 1024 of the form's units, as mergerfs reads them.
var omvShifts = map[string]uint{"K": 10, "M": 20, "G": 30}

// runMergerfsConfig prints the mergerfs configuration of specs.md §21.1: the fstab line, or with --omv the fields of the
// openmediavault mergerfs plugin's form. It opens the catalog, for each disk's capacity, and looks at no disk.
func runMergerfsConfig(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON, omv := false, false
	for _, a := range args {
		var flag *bool
		switch a {
		case "--json":
			flag = &asJSON
		case "--omv":
			flag = &omv
		default:
			return usage(stderr, fmt.Sprintf(T("mergerfs-config: unknown argument %q"), a))
		}
		if err := setFlag(flag, a); err != nil {
			return usage(stderr, "mergerfs-config: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out, m, err := mergerfsConfig(context.Background(), catalogPath(c), c)
	if err != nil {
		return failure(stderr, err)
	}
	if omv {
		return runOMV(c, m, out, asJSON, stdout, stderr)
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

// runOMV prints the pool as the openmediavault mergerfs plugin's form takes it, a field a line, or with --json the
// output of the line with its omv object (specs.md §21.1). It exits with 1, printing nothing, when the plugin cannot take
// the pool; with 4 when the form lacks what Atoll would give each disk.
func runOMV(c *config.Config, m *view.Mergerfs, out *mergerfsOutput, asJSON bool, stdout, stderr io.Writer) int {
	minFree, _ := c.Effective()["view.omv_min_free"].(string)
	o, err := m.OMV(c.PoolID, minFree)
	if err != nil {
		var refusal *view.OMVRefusal
		var large *view.OMVTooLarge
		switch {
		case errors.As(err, &refusal):
			report(stderr, fmt.Sprintf(T("disk %q: its branch %s holds %q, which the openmediavault mergerfs plugin refuses"), refusal.Disk, shownPath(refusal.Path), refusal.Char))
		case errors.As(err, &large):
			report(stderr, fmt.Sprintf(T("the pool's minimum free space, %d bytes, is more than the openmediavault mergerfs plugin's form holds, 99999 G"), large.Bytes))
		default:
			report(stderr, err.Error())
		}
		return exitError
	}
	out.OMV = omvOf(o)
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, omvText(o))
	}
	if err != nil {
		return failure(stderr, err)
	}
	code := exitOK
	for _, b := range out.Branches {
		if b.MinFree == nil {
			report(stderr, fmt.Sprintf(T("disk %q: no minfreespace of its own, so the pool's minimum free space stands for it: no scan has recorded the disk's capacity, and placement.min_free holds no size"), b.Disk))
			code = exitPartial
		}
	}
	switch {
	case o.Differs != nil:
		values := make([]string, len(o.Differs))
		for i, b := range o.Differs {
			values[i] = fmt.Sprintf(T("%q (%d bytes)"), b.Disk, *b.MinFree)
		}
		report(stderr, fmt.Sprintf(T("view.omv_min_free = \"same\", and the disks that take new files keep different minfreespace: %s. The plugin has one for the pool: set one placement.min_free for every disk, or view.omv_min_free = \"largest\""), strings.Join(values, ", ")))
		code = exitPartial
	case o.MinFree == nil:
		report(stderr, T("the pool has no minimum free space to give, as no disk has a minfreespace of its own: the plugin's form keeps the one it holds"))
	default:
		if o.MinFree.Rounded {
			report(stderr, fmt.Sprintf(T("warning: the pool's minimum free space, %d bytes, is rounded up to %d%s, as no count of K, M or G up to 99999 holds it exactly"), o.MinFree.Bytes, o.MinFree.Count, o.MinFree.Unit))
		}
		if reserved := o.MinFree.Count << omvShifts[o.MinFree.Unit]; out.Largest != nil && reserved <= *out.Largest {
			report(stderr, fmt.Sprintf(T("warning: the pool's minimum free space of %d%s is not larger than the largest file, of %d bytes"), o.MinFree.Count, o.MinFree.Unit, *out.Largest))
		}
	}
	if o.Elsewhere {
		report(stderr, fmt.Sprintf(T("warning: view.mount is %s, but the openmediavault mergerfs plugin mounts the pool at %s: set view.mount to that path, so that Atoll and the media servers find the pool there"), shownPath(m.Mount), o.Mount))
	}
	return code
}

// omvOf gives the omv object of the output, as the plugin's RPC takes the form's fields.
func omvOf(o *view.OMV) *omvOutput {
	out := &omvOutput{
		RPC:      omvRPC{Name: o.Name, Paths: strings.Join(o.Paths, "\n"), CreatePolicy: o.CreatePolicy, Options: strings.Join(o.Options, ",")},
		Mount:    o.Mount,
		Warnings: []string{},
	}
	if o.Differs != nil {
		out.Warnings = append(out.Warnings, omvMinFreeDiffers)
	}
	if o.MinFree != nil {
		count, unit := o.MinFree.Count, o.MinFree.Unit
		out.RPC.MinFree, out.RPC.MinFreeUnit = &count, &unit
		if o.MinFree.Rounded {
			out.Warnings = append(out.Warnings, omvMinFreeRounded)
		}
	}
	if o.Elsewhere {
		out.Warnings = append(out.Warnings, omvMountElsewhere)
	}
	return out
}

// omvText gives each field of the form a line, named as the form names it, the paths one a line, as its hint asks, and
// the mount point the pool will have. Without a minimum free space to give, its two fields are left out.
func omvText(o *view.OMV) string {
	var b strings.Builder
	// A Builder takes every write.
	_, _ = fmt.Fprintf(&b, T("Name: %s\n"), o.Name)
	b.WriteString(T("Paths:\n"))
	for _, p := range o.Paths {
		b.WriteString(shownPath(p) + "\n")
	}
	_, _ = fmt.Fprintf(&b, T("Create policy: %s\n"), o.CreatePolicy)
	if o.MinFree != nil {
		_, _ = fmt.Fprintf(&b, T("Minimum free space: %d\n"), o.MinFree.Count)
		_, _ = fmt.Fprintf(&b, T("Unit: %s\n"), omvUnits[o.MinFree.Unit])
	}
	_, _ = fmt.Fprintf(&b, T("Options: %s\n"), strings.Join(o.Options, ","))
	_, _ = fmt.Fprintf(&b, T("Mount point: %s\n"), o.Mount)
	return b.String()
}

// mergerfsConfig reads what the catalog recorded of the disks and of the largest file, and gives the
// configuration, as the output of --json and as view gives it. An error is the configuration or the catalog that
// cannot be used, or a pool mergerfs cannot take.
func mergerfsConfig(ctx context.Context, file string, c *config.Config) (out *mergerfsOutput, m *view.Mergerfs, err error) {
	db, err := catalog.OpenPool(ctx, file, c.PoolID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	capacities := map[string]int64{}
	for _, d := range list {
		if d.HasCapacity {
			capacities[d.ID] = d.Capacity
		}
	}
	largest, known, err := catalog.LargestFile(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	minFree, err := c.MinFree()
	if err != nil {
		return nil, nil, err
	}
	m, err = view.NewMergerfs(c, minFree, capacities)
	if err != nil {
		return nil, nil, err
	}
	out = &mergerfsOutput{Mount: m.Mount, Branches: make([]mergerfsBranch, 0, len(m.Branches)), Options: m.Options, Fstab: m.Fstab()}
	for _, b := range m.Branches {
		out.Branches = append(out.Branches, mergerfsBranch{Disk: b.Disk, Path: b.Path, Mode: b.Mode, MinFree: b.MinFree})
	}
	if known {
		out.Largest = &largest
	}
	return out, m, nil
}
