//go:build !(linux && cgo && fuse3)

package fusefs

import (
	"errors"

	"github.com/peondusud/atoll/internal/view"
)

// Built says whether this build holds the view: it does only with cgo and the fuse3 tag, on Linux.
const Built = false

// ErrNotBuilt says that this build holds no FUSE view.
var ErrNotBuilt = errors.New("this build of Atoll holds no FUSE view: build it with cgo and -tags fuse3")

// Mounted is a view being served.
type Mounted struct{}

// Mount says that this build holds no view.
func Mount(*view.Tree, string) (*Mounted, error) { return nil, ErrNotBuilt }

// Unmount does nothing.
func (*Mounted) Unmount() error { return nil }
