//go:build loopdev && cgo && fuse3 && !gofuse

package loopdev

import (
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/fusefs"
)

type unmounter interface{ Unmount() error }

// mountOwn mounts Atoll's own view through cgofuse.
func mountOwn(tree *view.Tree, at string) (unmounter, error) {
	return fusefs.Mount(tree, at)
}
