package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/value"
)

// scrubDiskOutput is one disk of api/schemas/scrub.schema.json.
type scrubDiskOutput struct {
	ID          string     `json:"id"`
	Scrubbed    bool       `json:"scrubbed"`
	CopiesRead  int64      `json:"copies_read"`
	BytesRead   int64      `json:"bytes_read"`
	Corrupt     []pathItem `json:"corrupt"`
	Quarantined []pathItem `json:"quarantined"`
	Kept        []pathItem `json:"kept"`
	Changed     []pathItem `json:"changed"`
	Left        bool       `json:"left"`
}

// scrubOutput is the output of api/schemas/scrub.schema.json.
type scrubOutput struct {
	Applied bool              `json:"applied"`
	Disks   []scrubDiskOutput `json:"disks"`
}

type scrubArgs struct {
	disk, under         string
	budget              time.Duration
	dryRun, yes, asJSON bool
}

// runScrub runs atoll scrub (specs.md §16, §25): it reads the copies due and checks them against their hashes,
// records what it finds, and with --yes moves a corrupt copy to the trash when another copy is healthy.
func runScrub(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseScrub(args)
	if err != nil {
		return usage(stderr, "scrub: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.disk != "" {
		if _, err := diskOfConfig(c, parsed.disk); err != nil {
			return failure(stderr, fmt.Errorf("scrub: %w", err))
		}
	}
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	if !parsed.dryRun {
		release, err := lockCatalog(ctx, c, stderr)
		if err != nil {
			return failure(stderr, err)
		}
		defer release()
	}
	out, err := scrub(ctx, c, m, &parsed)
	if err != nil {
		return failure(stderr, err)
	}
	result := scrubOutput{Applied: !parsed.dryRun, Disks: make([]scrubDiskOutput, 0, len(out.Disks))}
	quarantine, kept := false, false
	for i := range out.Disks {
		d := &out.Disks[i]
		result.Disks = append(result.Disks, scrubDiskOutput{ID: d.ID, Scrubbed: d.Scrubbed, CopiesRead: d.Read, BytesRead: d.Bytes,
			Corrupt: pathItems(d.Corrupt), Quarantined: pathItems(d.Quarantined), Kept: pathItems(d.Kept), Changed: pathItems(d.Changed), Left: d.Left})
		quarantine = quarantine || len(d.Quarantined) > 0
		kept = kept || len(d.Kept) > 0
	}
	if parsed.asJSON {
		err = writeJSON(stdout, result)
	} else {
		err = writeText(stdout, scrubText(&result, parsed.yes))
	}
	if err != nil {
		return failure(stderr, err)
	}
	switch {
	case out.Partial || kept:
		return exitPartial
	case quarantine && !parsed.yes && !parsed.dryRun:
		return exitConfirm
	default:
		return exitOK
	}
}

func scrub(ctx context.Context, c *config.Config, m disks.Machine, p *scrubArgs) (out *jobs.ScrubOutcome, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	out, err = jobs.Scrub(ctx, db, c, m, jobs.ScrubOptions{Disk: p.disk, Under: p.under, Budget: p.budget, Apply: !p.dryRun, Quarantine: p.yes})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return out, nil
}

// parseScrub reads the arguments of scrub: --disk, --path and --budget with a value, --dry-run, --yes and --json, each
// once, --dry-run never with --yes (specs.md §25).
func parseScrub(args []string) (scrubArgs, error) {
	var out scrubArgs
	given := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--dry-run":
			if err := setFlag(&out.dryRun, a); err != nil {
				return out, err
			}
			continue
		case "--yes":
			if err := setFlag(&out.yes, a); err != nil {
				return out, err
			}
			continue
		case "--json":
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
			continue
		}
		name, v, inline := strings.Cut(a, "=")
		if name != "--disk" && name != "--budget" && name != "--path" {
			return out, fmt.Errorf("unknown argument %q", a)
		}
		if !inline {
			if i+1 == len(args) {
				return out, fmt.Errorf("%s needs a value", name)
			}
			i++
			v = args[i]
		}
		if given[name] {
			return out, fmt.Errorf("%s comes twice", name)
		}
		given[name] = true
		switch name {
		case "--disk":
			if !layout.IsIdentifier(v) {
				return out, fmt.Errorf("--disk: %q is no disk id", v)
			}
			out.disk = v
			continue
		case "--path":
			// Only its form counts here, as for status.
			if _, err := names.CheckPath(v, names.Ext4); err != nil {
				return out, fmt.Errorf("--path: %w", err)
			}
			out.under = v
			continue
		}
		budget, err := value.ParseDuration(v)
		if err != nil || budget <= 0 {
			return out, fmt.Errorf("--budget: %q is no positive duration", v)
		}
		out.budget = budget
	}
	if out.dryRun && out.yes {
		return out, errors.New("--dry-run and --yes do not go together")
	}
	return out, nil
}

func scrubText(out *scrubOutput, yes bool) string {
	var b bytes.Buffer
	for _, d := range out.Disks {
		if !d.Scrubbed {
			fmt.Fprintf(&b, "disk %s: not scrubbed\n", d.ID)
			continue
		}
		fmt.Fprintf(&b, "disk %s: %d copies read, %d bytes", d.ID, d.CopiesRead, d.BytesRead)
		if d.Left {
			b.WriteString(", the budget ran out: the next scrub goes on from there")
		}
		b.WriteString("\n")
		for _, list := range []struct {
			what  string
			paths []pathItem
		}{{"corrupt", d.Corrupt}, {"kept, no healthy copy elsewhere", d.Kept}, {"changed, for the next pass", d.Changed}} {
			for _, p := range list.paths {
				fmt.Fprintf(&b, "  %s: %s\n", list.what, pathText(p))
			}
		}
		for _, p := range d.Quarantined {
			if yes {
				fmt.Fprintf(&b, "  moved to the trash: %s\n", pathText(p))
			} else {
				fmt.Fprintf(&b, "  to move to the trash with --yes: %s\n", pathText(p))
			}
		}
	}
	return b.String()
}

func pathText(p pathItem) string {
	if p.PathB64 != "" {
		return "(base64) " + p.PathB64
	}
	return p.Path
}
