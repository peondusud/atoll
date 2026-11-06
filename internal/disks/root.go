package disks

import (
	"path"

	"github.com/peondusud/atoll/internal/fsx"
)

// lookAtRoot answers question 10 on the real folder. Both builds use it: only what a mount is
// differs between them.
func lookAtRoot(mount, root string) RootSeen {
	top, err := fsx.StatPath(mount)
	if err != nil {
		return RootSeen{Folder: FolderIOError, Note: err.Error()}
	}
	folder, err := fsx.StatPath(path.Join(mount, root))
	switch {
	case fsx.IsAbsent(err):
		return RootSeen{Folder: FolderAbsent}
	case err != nil:
		return RootSeen{Folder: FolderIOError, Note: err.Error()}
	case !folder.IsDir:
		return RootSeen{Folder: FolderNotAFolder}
	case folder.MountID != top.MountID:
		return RootSeen{Folder: FolderOtherMount}
	}
	return RootSeen{Folder: FolderOK, OtherCase: answersOtherCase(mount, root, folder)}
}

// answersOtherCase asks whether the root also answers with each ASCII letter in the other case.
// Nothing else shows lowntfs-3g ignore_case. A real second folder has another inode, and a name
// with no ASCII letter cannot be asked.
func answersOtherCase(mount, root string, folder fsx.PathInfo) bool {
	other := swapCase(root)
	if other == root {
		return false
	}
	seen, err := fsx.StatPath(path.Join(mount, other))
	return err == nil && seen.Ino == folder.Ino && seen.MountID == folder.MountID
}

// swapCase works on bytes: a name is bytes, and only ASCII letters change.
func swapCase(name string) string {
	out := []byte(name)
	for i, c := range out {
		switch {
		case c >= 'a' && c <= 'z':
			out[i] = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z':
			out[i] = c - 'A' + 'a'
		}
	}
	return string(out)
}
