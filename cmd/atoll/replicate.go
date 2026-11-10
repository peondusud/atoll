package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// replicateCopy is one line of api/schemas/replicate.schema.json.
type replicateCopy struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Disk    string `json:"disk"`
	Source  string `json:"source"`
	Size    int64  `json:"size"`
	Outcome string `json:"outcome"`
}

// replicateEnd is the rest of api/schemas/replicate.schema.json.
type replicateEnd struct {
	Applied bool  `json:"applied"`
	Bytes   int64 `json:"bytes"`
}

type replicateArgs struct {
	asJSON, dryRun, now, force bool
	path                       string
}

func parseReplicate(args []string) (replicateArgs, error) {
	var out replicateArgs
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch a {
		case "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case "--json":
			err = setFlag(&out.asJSON, a)
		case "--dry-run":
			err = setFlag(&out.dryRun, a)
		case "--now":
			err = setFlag(&out.now, a)
		case "--force":
			err = setFlag(&out.force, a)
		default:
			if strings.HasPrefix(a, "-") {
				return out, fmt.Errorf("unknown argument %q", a)
			}
			paths = append(paths, a)
		}
		if err != nil {
			return out, err
		}
	}
	if len(paths) != 1 {
		return out, fmt.Errorf("want one path, got %d", len(paths))
	}
	out.path = paths[0]
	if _, err := names.CheckPath(out.path, names.Ext4); err != nil {
		return out, fmt.Errorf("%w", err)
	}
	if out.now && out.force {
		return out, errors.New("--now and --force go alone")
	}
	return out, nil
}

// runReplicate queues the copies a path is missing, and with --now or --force makes them (specs.md §14.3).
func runReplicate(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseReplicate(args)
	if err != nil {
		return usage(stderr, "replicate: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	buffered := bufio.NewWriter(stdout)
	list := newJSONList(buffered, "copies")
	opts := jobs.ReplicateOptions{Path: parsed.path, Now: parsed.now, Force: parsed.force, DryRun: parsed.dryRun}
	summary, err := replicate(c, opts, func(item *jobs.ReplicateItem) error {
		if !parsed.asJSON {
			return writeText(buffered, fmt.Sprintf("%s (%s, from %s): %s\n", shownPath(item.Path), item.Disk, item.Source, item.Outcome))
		}
		path, b64 := encodePath(item.Path)
		return list.item(replicateCopy{Path: path, PathB64: b64, Disk: item.Disk, Source: item.Source, Size: item.Size, Outcome: item.Outcome})
	})
	if err == nil && summary.Disabled {
		err = writeText(stderr, "replicate: replication.enabled is false: nothing was queued or copied\n")
	}
	if err == nil {
		if parsed.asJSON {
			err = list.end(replicateEnd{Applied: summary.Applied, Bytes: summary.Bytes})
		} else {
			err = writeText(buffered, fmt.Sprintf("%d bytes\n", summary.Bytes))
		}
	}
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		return failure(stderr, err)
	}
	if summary.Applied && (summary.Failed > 0 || summary.DiskLost) {
		return exitPartial
	}
	return exitOK
}

func replicate(c *config.Config, opts jobs.ReplicateOptions, each func(*jobs.ReplicateItem) error) (summary *jobs.ReplicateSummary, err error) {
	ctx := context.Background()
	machine, err := disks.NewMachine()
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if release, err = fsx.LockFile(catalogPath(c)); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	summary, err = jobs.Replicate(ctx, db, c, machine, opts, each)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return summary, nil
}
