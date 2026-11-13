package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/value"
)

// dupeGroupOutput is one group of api/schemas/dupes.schema.json.
type dupeGroupOutput struct {
	Size   int64      `json:"size"`
	Blake3 string     `json:"blake3"`
	Wasted int64      `json:"wasted"`
	Paths  []pathItem `json:"paths"`
}

// dupesOutput is the output of api/schemas/dupes.schema.json.
type dupesOutput struct {
	Groups   []dupeGroupOutput `json:"groups"`
	Files    int               `json:"files"`
	Wasted   int64             `json:"wasted"`
	Unhashed int64             `json:"unhashed"`
}

type dupesArgs struct {
	under   string
	minSize int64
	asJSON  bool
}

// runDupes runs atoll dupes (specs.md §16, §25): the files that hold the same content, from the catalog alone.
func runDupes(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseDupes(args)
	if err != nil {
		return usage(stderr, "dupes: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out, err := dupes(context.Background(), c, &parsed)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, dupesText(out))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func dupes(ctx context.Context, c *config.Config, p *dupesArgs) (out *dupesOutput, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	groups, err := catalog.Dupes(ctx, db, p.minSize)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	out = &dupesOutput{Groups: []dupeGroupOutput{}}
	if out.Unhashed, err = catalog.Unhashed(ctx, db, p.under); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	for _, g := range groups {
		if !slices.ContainsFunc(g.Paths, func(path string) bool { return atOrBelow(path, p.under) }) {
			continue
		}
		wasted := g.Size * int64(len(g.Paths)-1)
		out.Groups = append(out.Groups, dupeGroupOutput{Size: g.Size, Blake3: hex.EncodeToString(g.Hash), Wasted: wasted, Paths: pathItems(g.Paths)})
		out.Files += len(g.Paths)
		out.Wasted += wasted
	}
	// The bytes a group holds more than once, the most first, then by hash (specs.md §16).
	slices.SortFunc(out.Groups, func(a, b dupeGroupOutput) int {
		return cmp.Or(cmp.Compare(b.Wasted, a.Wasted), strings.Compare(a.Blake3, b.Blake3))
	})
	return out, nil
}

// atOrBelow says whether a path is under, or below it by whole components; any is under "".
func atOrBelow(path, under string) bool {
	return under == "" || path == under || strings.HasPrefix(path, under+"/")
}

// parseDupes reads the arguments of dupes: --path and --min-size with a value, and --json, each once (specs.md §25).
func parseDupes(args []string) (dupesArgs, error) {
	out := dupesArgs{minSize: 1}
	given := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
			continue
		}
		name, v, inline := strings.Cut(a, "=")
		if name != "--path" && name != "--min-size" {
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
		if name == "--path" {
			// Only its form counts here, as for status.
			if _, err := names.CheckPath(v, names.Ext4); err != nil {
				return out, fmt.Errorf("--path: %w", err)
			}
			out.under = v
			continue
		}
		size, err := value.ParseSize(v)
		if err != nil {
			return out, fmt.Errorf(T("--min-size: %q is no size"), v)
		}
		out.minSize = size
	}
	return out, nil
}

func dupesText(out *dupesOutput) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, T("%d groups, %d files, %d bytes held more than once\n"), len(out.Groups), out.Files, out.Wasted)
	for _, g := range out.Groups {
		fmt.Fprintf(&b, T("%d bytes, %d times, blake3 %s:\n"), g.Size, len(g.Paths), g.Blake3)
		for _, p := range g.Paths {
			fmt.Fprintf(&b, "  %s\n", pathText(p))
		}
	}
	if out.Unhashed > 0 {
		fmt.Fprintf(&b, T("%d files not hashed yet, which a full scan reads\n"), out.Unhashed)
	}
	return b.String()
}
