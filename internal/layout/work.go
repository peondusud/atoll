package layout

import (
	"strconv"
	"strings"
)

// The names of a disk's work folder that hold an operation's id (specs.md §9): in TmpFolder, the copy an operation
// makes and each chunk a repair saves on its way to the trash; below a date of TrashFolder, an operation's folder and
// its folder of chunks. What reads one back reads it only as it is written here: any other name is not Atoll's, and is
// neither removed from tmp/ nor counted when a new catalog starts its operations past those the disks name.
const (
	partSuffix   = ".part"
	chunksSuffix = ".chunks"
)

// PartName gives the name in TmpFolder of the copy an operation makes: <op-id>.part (specs.md §14.2).
func PartName(opID int64) string {
	return strconv.FormatInt(opID, 10) + partSuffix
}

// ChunkPartName gives the name in TmpFolder of a chunk a repair saves, on its way to the trash:
// <op-id>.<index>.part, the index written as it is, not as the chunk's name in the trash has it (specs.md §16.1).
func ChunkPartName(opID, index int64) string {
	return strconv.FormatInt(opID, 10) + "." + strconv.FormatInt(index, 10) + partSuffix
}

// PartOp reads a name of TmpFolder: the operation that names it and, for a chunk a repair saves, its index. ok is
// false for any name that PartName and ChunkPartName do not write.
func PartOp(name string) (opID, index int64, chunk, ok bool) {
	stem, found := strings.CutSuffix(name, partSuffix)
	if !found {
		return 0, 0, false, false
	}
	id, rest, chunk := strings.Cut(stem, ".")
	if opID, ok = decimal(id, 1); !ok {
		return 0, 0, false, false
	}
	if !chunk {
		return opID, 0, false, true
	}
	if index, ok = decimal(rest, 0); !ok {
		return 0, 0, false, false
	}
	return opID, index, true, true
}

// TrashOp reads the name of a folder below a date of TrashFolder: the operation whose folder it is, and whether it
// holds the chunks a repair overwrote. ok is false for any name that TrashPath and ChunksPath do not write.
func TrashOp(name string) (opID int64, chunks, ok bool) {
	id, chunks := strings.CutSuffix(name, chunksSuffix)
	if opID, ok = decimal(id, 1); !ok {
		return 0, false, false
	}
	return opID, chunks, true
}

// decimal reads a number written as strconv.FormatInt writes it, from a least value on: the digits 0 to 9, with no
// sign and no leading zero.
func decimal(text string, least int64) (int64, bool) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < least || strconv.FormatInt(n, 10) != text {
		return 0, false
	}
	return n, true
}
