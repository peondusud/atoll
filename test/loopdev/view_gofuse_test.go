//go:build loopdev && gofuse

package loopdev

import (
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/gofusefs"
)

type unmounter interface{ Unmount() error }

// mountOwn mounts Atoll's own view through go-fuse.
func mountOwn(tree *view.Tree, at string) (unmounter, error) {
	return gofusefs.Mount(tree, at)
}
