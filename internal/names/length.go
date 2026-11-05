package names

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxBranchPathBytes is the longest <mount>/<root>/<logical path> that mergerfs
// can reach, in bytes: PATH_MAX is 4096 with the terminating NUL. Past it,
// mergerfs lists the copy and stat gives ENOENT (specs.md §15.8).
const MaxBranchPathBytes = 4095

// MaxWindowsPathUnits is the longest path, in UTF-16 code units, that a Windows
// program without long-path support can open: MAX_PATH is 260 with the
// terminating NUL (specs.md §15.8).
const MaxWindowsPathUnits = 259

// FitsBranch reports whether mergerfs can reach the logical path on a disk with
// this mount path and root, counted on the exact bytes. A copy past the limit
// protects nothing, so the solver never writes one (specs.md §12.4, §15.8). An
// error means that path is not a well-formed logical path, or that mount or
// root is empty.
func FitsBranch(mount, root, path string) (bool, error) {
	if mount == "" {
		return false, errors.New("branch path: no mount path")
	}
	if err := checkRootAndPath(root, path); err != nil {
		return false, err
	}
	return len(mount)+1+len(root)+1+len(path) <= MaxBranchPathBytes, nil
}

// FitsWindows reports whether a Windows program without long-path support can
// open the logical path on a disk with this root, as D:\<root>\<path>, counted
// on the exact text. The limit depends on the program, so this is only reported
// (specs.md §15.8). A path that is not valid UTF-8 has no UTF-16 length, so it
// gets no verdict: FitsWindows returns true. An error means that path is not a
// well-formed logical path, or that root is empty.
func FitsWindows(root, path string) (bool, error) {
	if err := checkRootAndPath(root, path); err != nil {
		return false, err
	}
	if !utf8.ValidString(path) {
		return true, nil
	}
	return len(`D:\`)+utf16Len(root)+1+utf16Len(path) <= MaxWindowsPathUnits, nil
}

func checkRootAndPath(root, path string) error {
	if root == "" {
		return errors.New("path length: no root")
	}
	return checkLogicalPath(path)
}

// checkLogicalPath rejects what can never be a logical path.
func checkLogicalPath(path string) error {
	for _, c := range strings.Split(path, "/") {
		if err := checkComponent(c); err != nil {
			return fmt.Errorf("logical path %q: %w", path, err)
		}
	}
	return nil
}
