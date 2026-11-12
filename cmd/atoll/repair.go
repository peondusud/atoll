package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// repairCopyOutput is one copy of api/schemas/repair.schema.json.
type repairCopyOutput struct {
	Disk string `json:"disk"`
	pathItem
	Mode    string `json:"mode"`
	Chunks  int    `json:"chunks"`
	Outcome string `json:"outcome"`
}

// repairOutput is the output of api/schemas/repair.schema.json.
type repairOutput struct {
	Applied bool               `json:"applied"`
	Copies  []repairCopyOutput `json:"copies"`
}

type repairArgs struct {
	under, disk, mode   string
	dryRun, yes, asJSON bool
}

// runRepair runs atoll repair (specs.md §16.1, §25): it repairs the corrupt copies of the files at a path from
// healthy ones, with --yes, and says what it would do without it.
func runRepair(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseRepair(args)
	if err != nil {
		return usage(stderr, "repair: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.disk != "" {
		if _, err := diskOfConfig(c, parsed.disk); err != nil {
			return failure(stderr, fmt.Errorf("repair: %w", err))
		}
	}
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	release, err := lockCatalog(ctx, c, stderr)
	if err != nil {
		return failure(stderr, err)
	}
	defer release()
	out, err := repair(ctx, c, m, &parsed)
	if err != nil {
		return failure(stderr, err)
	}
	result := repairOutput{Applied: out.Applied, Copies: make([]repairCopyOutput, 0, len(out.Items))}
	toRepair, stays := false, false
	for i := range out.Items {
		it := &out.Items[i]
		result.Copies = append(result.Copies, repairCopyOutput{Disk: it.Disk, pathItem: pathItems([]string{it.Path})[0], Mode: it.Mode, Chunks: it.Chunks, Outcome: it.Outcome})
		switch it.Outcome {
		case jobs.OutcomeToRepair:
			toRepair = true
		case trash.OutcomeRepaired, jobs.OutcomeVerified:
		default:
			stays = true
		}
	}
	if parsed.asJSON {
		err = writeJSON(stdout, result)
	} else {
		err = writeText(stdout, repairText(&result))
	}
	if err != nil {
		return failure(stderr, err)
	}
	switch {
	case out.Partial || stays:
		return exitPartial
	case toRepair && !parsed.yes && !parsed.dryRun:
		return exitConfirm
	default:
		return exitOK
	}
}

func repair(ctx context.Context, c *config.Config, m disks.Machine, p *repairArgs) (out *jobs.RepairSummary, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	out, err = jobs.Repair(ctx, db, c, m, jobs.RepairOptions{Under: p.under, Disk: p.disk, Mode: p.mode, Apply: p.yes, Record: !p.dryRun})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return out, nil
}

// parseRepair reads the arguments of repair: a path, --disk and --mode with a value, --dry-run, --yes and --json, each
// once, --dry-run never with --yes (specs.md §25).
func parseRepair(args []string) (repairArgs, error) {
	var out repairArgs
	given := map[string]bool{}
	path := false
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
		if !strings.HasPrefix(a, "--") {
			if path {
				return out, fmt.Errorf(T("unknown argument %q"), a)
			}
			// Only its form counts here, as for status.
			if _, err := names.CheckPath(a, names.Ext4); err != nil {
				return out, fmt.Errorf("%w", err)
			}
			out.under, path = a, true
			continue
		}
		name, v, inline := strings.Cut(a, "=")
		if name != "--disk" && name != "--mode" {
			return out, fmt.Errorf(T("unknown argument %q"), a)
		}
		if !inline {
			if i+1 == len(args) {
				return out, fmt.Errorf(T("%s needs a value"), name)
			}
			i++
			v = args[i]
		}
		if given[name] {
			return out, fmt.Errorf(T("%s comes twice"), name)
		}
		given[name] = true
		if name == "--disk" {
			if !layout.IsIdentifier(v) {
				return out, fmt.Errorf(T("--disk: %q is no disk id"), v)
			}
			out.disk = v
			continue
		}
		if v != jobs.RepairChunk && v != jobs.RepairFile {
			return out, fmt.Errorf(T("--mode: %q is neither chunk nor file"), v)
		}
		out.mode = v
	}
	if !path {
		return out, errors.New(T("a path is needed"))
	}
	if out.dryRun && out.yes {
		return out, errors.New(T("--dry-run and --yes do not go together"))
	}
	return out, nil
}

func repairText(out *repairOutput) string {
	var b bytes.Buffer
	if len(out.Copies) == 0 {
		b.WriteString(T("no corrupt copy there\n"))
	}
	for _, c := range out.Copies {
		fmt.Fprintf(&b, T("%s on %s: %s, %d bad chunks, by %s\n"), pathText(c.pathItem), c.Disk, repairOutcomeText(c.Outcome), c.Chunks, c.Mode)
	}
	return b.String()
}

func repairOutcomeText(outcome string) string {
	switch outcome {
	case trash.OutcomeRepaired:
		return T("repaired")
	case jobs.OutcomeToRepair:
		return T("to repair with --yes")
	case jobs.OutcomeVerified:
		return T("reads right after all")
	case jobs.OutcomeNoHealthyCopy:
		return T("no healthy copy to repair it from")
	default:
		return outcome
	}
}
