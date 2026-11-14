package ops

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
)

// FoundEntry is an entry of a disk's trash as a catalog rebuild finds it again (specs.md §9, §17): an operation's
// folder, trash/<date>/<op id>, that holds one file, or a repair's, trash/<date>/<op id>.chunks, that holds the chunks
// of one file. Path is the logical path below the operation's folder, and TrashPath the entry's place in the trash.
// FileKey is the file's inode, and empty for chunks; Size is the file's, or what the chunks hold together.
type FoundEntry struct {
	Day       time.Time
	OpID      int64
	Chunks    bool
	Path      string
	TrashPath string
	Size      int64
	FileKey   string
}

// FoundTrash lists the entries of the disk's trash, by date, then by operation. What is no operation's folder below a
// date, or an operation's folder that holds anything but one regular file below its folders, or chunks that are not
// those of one file, is no entry: left tells of it, by its place in the trash, and it stays as it is. A disk with no
// trash has none.
func (d *Disk) FoundTrash(left func(place, why string)) ([]FoundEntry, error) {
	trash, err := d.open(components(d.config.Workdir, layout.TrashFolder))
	switch {
	case fsx.IsAbsent(err):
		return nil, nil
	case err != nil:
		return nil, classify(err)
	}
	defer trash.close()
	days, err := trash.Names()
	if err != nil {
		return nil, classify(fmt.Errorf("list the trash: %w", err))
	}
	slices.Sort(days)
	var out []FoundEntry
	for _, day := range days {
		when, err := time.Parse(time.DateOnly, day)
		if err != nil || when.Format(time.DateOnly) != day {
			left(day, "not a date")
			continue
		}
		below, err := trash.OpenDir(day)
		switch {
		case fsx.IsUnreadableName(err):
			left(day, "not a folder")
			continue
		case err != nil:
			return nil, classify(fmt.Errorf("list the trash: %w", err))
		}
		found, err := foundOn(below, day, when, left)
		_ = below.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// foundOn lists the entries below one date of the trash.
func foundOn(dir fsx.Dir, day string, when time.Time, left func(place, why string)) ([]FoundEntry, error) {
	names, err := dir.Names()
	if err != nil {
		return nil, classify(fmt.Errorf("list the trash: %w", err))
	}
	type op struct {
		id     int64
		chunks bool
		name   string
	}
	var list []op
	for _, name := range names {
		id, chunks, ok := layout.TrashOp(name)
		if !ok {
			left(day+"/"+name, "no operation's")
			continue
		}
		list = append(list, op{id, chunks, name})
	}
	slices.SortFunc(list, func(a, b op) int { return int(a.id - b.id) })
	var out []FoundEntry
	for _, o := range list {
		place := day + "/" + o.name
		folder, err := dir.OpenDir(o.name)
		switch {
		case fsx.IsUnreadableName(err):
			left(place, "not a folder")
			continue
		case err != nil:
			return nil, classify(fmt.Errorf("list the trash: %w", err))
		}
		var files []foundFile
		other, err := filesBelow(folder, "", &files)
		_ = folder.Close()
		if err != nil {
			return nil, err
		}
		e := FoundEntry{Day: when, OpID: o.id, Chunks: o.chunks}
		why := ""
		switch {
		case other:
			why = "holds what is no file or folder"
		case !o.chunks && len(files) != 1:
			why = fmt.Sprintf("holds %d files", len(files))
		case !o.chunks:
			e.Path, e.Size, e.FileKey = files[0].path, files[0].st.Size, keyOf(&files[0].st)
		default:
			e.Path, e.Size, why = chunksOf(files)
		}
		if why != "" {
			left(place, why)
			continue
		}
		e.TrashPath = place + "/" + e.Path
		out = append(out, e)
	}
	return out, nil
}

type foundFile struct {
	path string
	st   fsx.Stat
}

// filesBelow gathers the regular files below a folder, by their path from it, and says whether it holds anything
// else: a link, a device, what cannot be read.
func filesBelow(dir fsx.Dir, prefix string, files *[]foundFile) (other bool, err error) {
	names, err := dir.Names()
	if err != nil {
		return false, classify(fmt.Errorf("list the trash: %w", err))
	}
	slices.Sort(names)
	for _, name := range names {
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		st, err := dir.Stat(name)
		switch {
		case fsx.IsUnreadableName(err):
			other = true
			continue
		case err != nil:
			return false, classify(err)
		}
		switch st.Kind {
		case fsx.KindFile:
			*files = append(*files, foundFile{path, st})
		case fsx.KindDir:
			below, err := dir.OpenDir(name)
			if fsx.IsUnreadableName(err) {
				other = true
				continue
			}
			if err != nil {
				return false, classify(err)
			}
			more, err := filesBelow(below, path, files)
			_ = below.Close()
			if err != nil {
				return false, err
			}
			other = other || more
		case fsx.KindSymlink, fsx.KindOther:
			other = true
		}
	}
	return other, nil
}

// chunksOf reads what a repair's folder holds as one entry: the chunks of one file, each named by its index, all in
// the folder that has the file's name. It gives the file's path and what the chunks hold, or why it is none.
func chunksOf(files []foundFile) (path string, size int64, why string) {
	if len(files) == 0 {
		return "", 0, "holds no chunk"
	}
	for i := range files {
		at := strings.LastIndexByte(files[i].path, '/')
		if at < 0 {
			return "", 0, "holds a chunk with no file's folder"
		}
		name, chunk := files[i].path[:at], files[i].path[at+1:]
		if _, ok := layout.ChunkIndex(chunk); !ok {
			return "", 0, "holds what is no chunk"
		}
		if i == 0 {
			path = name
		} else if name != path {
			return "", 0, "holds the chunks of several files"
		}
		size += files[i].st.Size
	}
	return path, size, ""
}
