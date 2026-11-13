package layout

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/names"
)

// TrashFolder is the trash, below the work folder of a disk (specs.md §6).
const TrashFolder = "trash"

// TmpFolder holds copies in progress, below the work folder of a disk (specs.md §6).
const TmpFolder = "tmp"

// LinksFolder holds the link guard's links, below the work folder of a disk: one per copy, named by its file's id
// (specs.md §6, §17).
const LinksFolder = "links"

// LinkName gives the name of a file's link in LinksFolder: its id in decimal. An id that is not positive is an error.
func LinkName(fileID int64) (string, error) {
	if fileID <= 0 {
		return "", fmt.Errorf("link name: file %d", fileID)
	}
	return strconv.FormatInt(fileID, 10), nil
}

// LinkID reads a name of LinksFolder: the id it names, and whether it is one, written as LinkName writes it. Anything
// else there is not Atoll's.
func LinkID(name string) (int64, bool) {
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != name {
		return 0, false
	}
	return id, true
}

// TrashPath gives the location of a copy inside the trash: the UTC date of the operation, its id,
// then the logical path, byte for byte (specs.md §17). An error means that the id is not positive,
// or that the path is not well-formed, which could lead out of the operation's folder.
func TrashPath(startedNS, opID int64, logical string) (string, error) {
	if opID <= 0 {
		return "", fmt.Errorf("trash path: operation %d", opID)
	}
	// Only its form counts here: a name a filesystem refuses is for the move to find out.
	if _, err := names.CheckPath(logical, names.Ext4); err != nil {
		return "", fmt.Errorf("trash path: %w", err)
	}
	return time.Unix(0, startedNS).UTC().Format("2006-01-02") + "/" + strconv.FormatInt(opID, 10) + "/" + logical, nil
}

// ChunksPath gives where the chunks a repair overwrites go inside the trash: the operation's folder with the suffix
// .chunks, then the logical path, whose last component is a folder of chunks (specs.md §17). The suffix sits on the
// operation's folder, so that a name of 255 bytes still fits. Its errors are TrashPath's.
func ChunksPath(startedNS, opID int64, logical string) (string, error) {
	if opID <= 0 {
		return "", fmt.Errorf("chunks path: operation %d", opID)
	}
	if _, err := names.CheckPath(logical, names.Ext4); err != nil {
		return "", fmt.Errorf("chunks path: %w", err)
	}
	return time.Unix(0, startedNS).UTC().Format("2006-01-02") + "/" + strconv.FormatInt(opID, 10) + chunksSuffix + "/" + logical, nil
}

// IsChunks says whether a location of the trash holds the chunks a repair overwrote: its operation's folder has the
// suffix .chunks (specs.md §17). Such an entry is never restored.
func IsChunks(trashPath string) bool {
	parts := strings.SplitN(trashPath, "/", 3)
	return len(parts) == 3 && strings.HasSuffix(parts[1], chunksSuffix)
}

// ChunkName gives the name of a chunk in its folder: its index in decimal, from 0, with at least 8 digits
// (specs.md §17). A negative index is an error.
func ChunkName(index int64) (string, error) {
	if index < 0 {
		return "", fmt.Errorf("chunk name: index %d", index)
	}
	return fmt.Sprintf("%08d", index), nil
}
