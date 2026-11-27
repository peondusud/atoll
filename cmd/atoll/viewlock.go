package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// A catalog rebuild and Atoll's own view exclude each other (specs.md §9, §21.2, §25): each atoll mount holds the
// catalog's folder locked shared for as long as it runs, and atoll catalog rebuild --yes holds it exclusive from its
// start to its end, so that neither starts while the other runs. A view's catalog is the one it opened as it started,
// which a rebuild would set aside, its new catalog knowing nothing of what the view holds open: the SSD cache's
// transfer then moved a file under the program writing it through the view (flaw 35 of conformance/models/README.md,
// the maintainer's remedy rebuild-view, 2026-11-27). The kernel lets go of either lock when its process ends, however
// it ends. The lock is on the folder, not on the catalog, whose own lock every command and pass of the daemon takes.

// catalogFolder is the folder that holds the catalog.
func catalogFolder(c *config.Config) string {
	return path.Dir(catalogPath(c))
}

// lockForView takes the view's side of the lock: shared, refused while a rebuild holds it.
func lockForView(c *config.Config) (func(), error) {
	release, err := fsx.LockShared(catalogFolder(c))
	switch {
	case errors.Is(err, fsx.ErrBusy):
		return nil, errors.New(T("a catalog rebuild runs: Atoll's own view starts once it is over"))
	case err != nil:
		return nil, fmt.Errorf("%w", err)
	}
	return release, nil
}

// lockForRebuild takes a rebuild's side of the lock: exclusive, refused while a view, or another rebuild, holds it. The
// catalog's folder is made first when it is not there, as the rebuild makes its catalog in it.
func lockForRebuild(c *config.Config) (func(), error) {
	if err := fsx.MkdirAllClosed(catalogFolder(c)); err != nil {
		return nil, fmt.Errorf("catalog rebuild: %w", err)
	}
	release, err := fsx.LockFile(catalogFolder(c))
	switch {
	case errors.Is(err, fsx.ErrBusy):
		return nil, errors.New(T("catalog rebuild: Atoll's own view runs (atoll mount), or another rebuild does: stop atoll mount for the rebuild, and start it again once it is over: nothing is changed"))
	case err != nil:
		return nil, fmt.Errorf("catalog rebuild: %w", err)
	}
	return release, nil
}

// openView opens the catalog as Atoll's own view does, once, under the view's side of the lock, which release lets go
// of once the view is over.
func openView(ctx context.Context, c *config.Config) (db *sql.DB, release func(), err error) {
	release, err = lockForView(c)
	if err != nil {
		return nil, nil, err
	}
	db, err = catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("%w", err)
	}
	return db, release, nil
}
