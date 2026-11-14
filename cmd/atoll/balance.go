package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// balanceMove is one move of api/schemas/balance.schema.json.
type balanceMove struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Size    int64  `json:"size"`
	Outcome string `json:"outcome"`
}

// balanceEnd is the rest of api/schemas/balance.schema.json.
type balanceEnd struct {
	Applied bool  `json:"applied"`
	Bytes   int64 `json:"bytes"`
	// Originals counts the user copies that break a strict setting too, which only --move-originals moves: 0 with it.
	Originals int `json:"originals"`
}

// runBalance runs atoll balance (specs.md §13.3): it moves the Atoll copies that break a strict setting of their
// file, with --yes, and says what it would move without it. With --move-originals, the user copies too.
func runBalance(configPath string, args []string, stdout, stderr io.Writer) int {
	var flags writeFlags
	originals := false
	for _, a := range args {
		if a == "--move-originals" {
			originals = true
			continue
		}
		known, err := flags.take(a)
		if err != nil {
			return usage(stderr, "balance: "+err.Error())
		}
		if !known {
			return usage(stderr, fmt.Sprintf(T("balance: unknown argument %q"), a))
		}
	}
	if err := flags.check(); err != nil {
		return usage(stderr, "balance: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
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
	buffered := bufio.NewWriter(stdout)
	list := newJSONList(buffered, "moves")
	summary, users, err := balance(ctx, c, m, flags.yes, originals, func(item *jobs.BalanceItem) error {
		if !flags.asJSON {
			to := item.To
			if to == "" {
				to = "-"
			}
			return writeText(buffered, fmt.Sprintf(T("%s (%s -> %s): %s\n"), shownPath(item.Path), item.From, to, item.Outcome))
		}
		path, b64 := encodePath(item.Path)
		return list.item(balanceMove{Path: path, PathB64: b64, From: item.From, To: item.To, Size: item.Size, Outcome: item.Outcome})
	})
	if err == nil && summary.Disabled {
		err = writeText(stderr, T("balance: replication.enabled is false: nothing was copied or moved\n"))
	}
	if err == nil {
		if flags.asJSON {
			err = list.end(balanceEnd{Applied: summary.Applied, Bytes: summary.Bytes, Originals: users})
		} else {
			err = writeText(buffered, fmt.Sprintf(T("%d bytes moved, or to move, and %d copies staying\n"), summary.Bytes, summary.Stayed))
			if err == nil && users > 0 {
				err = writeText(buffered, fmt.Sprintf(T("%d user copies break a strict setting too, which no setting moves: atoll balance --move-originals would\n"), users))
			}
		}
	}
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		return failure(stderr, err)
	}
	switch {
	case flags.dryRun:
		return exitOK
	case summary.ToDo > 0:
		return exitConfirm
	case summary.Stayed > 0 || summary.DiskLost:
		return exitPartial
	default:
		return exitOK
	}
}

// balance runs the balance, and without originals counts the user copies that --move-originals would move as well,
// which a run that said nothing of them left the user to guess (specs.md §13.3).
func balance(ctx context.Context, c *config.Config, m disks.Machine, apply, originals bool, each func(*jobs.BalanceItem) error) (summary *jobs.BalanceSummary, users int, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if summary, err = jobs.Balance(ctx, db, c, m, apply, 0, originals, each); err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	if originals || summary.Disabled {
		return summary, 0, nil
	}
	if users, err = jobs.UserCopiesMisplaced(ctx, db, c, m.Now()); err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	return summary, users, nil
}
