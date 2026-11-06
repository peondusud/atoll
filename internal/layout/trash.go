package layout

import (
	"fmt"
	"strconv"
	"time"

	"github.com/peondusud/atoll/internal/names"
)

// TrashFolder is the trash, below the work folder of a disk (specs.md §6).
const TrashFolder = "trash"

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
