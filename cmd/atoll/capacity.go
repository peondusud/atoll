package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/watch"
)

// capacityLevel is one level of api/schemas/capacity.schema.json.
type capacityLevel struct {
	Copies      int   `json:"copies"`
	Theoretical int64 `json:"theoretical"`
	Reachable   int64 `json:"reachable"`
	Stored      int64 `json:"stored"`
	Short       int64 `json:"short"`
}

// capacityOutput is the output of api/schemas/capacity.schema.json.
type capacityOutput struct {
	SpreadMode string          `json:"spread_mode"`
	Levels     []capacityLevel `json:"levels"`
}

// runCapacity runs atoll capacity: what the pool can hold with 1, 2 and 3 copies of every file, in theory and
// without moving a user copy, from the catalog alone (specs.md §13.1, §13.3).
func runCapacity(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("capacity: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "capacity: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out, err := capacity(context.Background(), c)
	if err != nil {
		return failure(stderr, err)
	}
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, capacityText(out))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func capacity(ctx context.Context, c *config.Config) (out *capacityOutput, err error) {
	m, err := disks.NewMachine()
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	levels, err := watch.Capacity(ctx, db, c, m.Now())
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	out = &capacityOutput{SpreadMode: c.Pool.SpreadMode, Levels: make([]capacityLevel, 0, len(levels))}
	for _, l := range levels {
		out.Levels = append(out.Levels, capacityLevel{Copies: l.Copies, Theoretical: l.Theoretical, Reachable: l.Reachable, Stored: l.Stored, Short: l.Short})
	}
	return out, nil
}

func capacityText(out *capacityOutput) string {
	var b bytes.Buffer
	for _, l := range out.Levels {
		fmt.Fprintf(&b, T("%d copies: %d bytes in theory, %d reachable without moving a copy; %d bytes stored, %d files short\n"),
			l.Copies, l.Theoretical, l.Reachable, l.Stored, l.Short)
	}
	if out.SpreadMode == "strict" {
		b.WriteString(T("spread_mode is strict: each copy of a file in another failure domain, as far as they go\n"))
	}
	return b.String()
}
