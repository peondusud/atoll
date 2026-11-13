package ops

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/layout"
)

// The disk's side of a chunk repair: the copy opened for writing in place, each bad chunk saved to the trash before
// any is written, each written and read back, then the time set back (specs.md §16.1).

// ErrNotRepaired says that a chunk written in place did not read back as its hash says.
var ErrNotRepaired = errors.New("a chunk written in place does not read back as its hash says")

// OpenRepair opens a corrupt copy for reading and writing in place, after a look that its name still holds what
// its row says (specs.md §16.1). ErrGone means it no longer does.
func (d *Disk) OpenRepair(row *catalog.KnownCopy) (fsx.WritableFile, fsx.Stat, error) {
	return d.openWritable(row.Path, untouched(row))
}

// openWritable opens the file at a logical path for reading and writing, after a look that its name holds what is
// says. ErrGone means it does not.
func (d *Disk) openWritable(logical string, is func(*fsx.Stat) bool) (fsx.WritableFile, fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return nil, fsx.Stat{}, err
	}
	path := d.rootPlace(logical)
	dir, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	defer dir.close()
	name := path[len(path)-1]
	st, err := holds(dir.Dir, name, is)
	if err != nil {
		return nil, fsx.Stat{}, err
	}
	f, err := dir.OpenWritable(name)
	switch {
	case fsx.IsUnreadableName(err), errors.Is(err, fsx.ErrNotARegularFile):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	opened, err := f.Info()
	if err != nil {
		_ = f.Close()
		return nil, fsx.Stat{}, classify(err)
	}
	if keyOf(&opened) != keyOf(&st) {
		_ = f.Close()
		return nil, fsx.Stat{}, ErrGone
	}
	return f, st, nil
}

// chunkPart is the name in <workdir>/tmp/ of a chunk a repair is saving (specs.md §16.1, step 3).
func chunkPart(opID, index int64) string {
	return strconv.FormatInt(opID, 10) + "." + strconv.FormatInt(index, 10) + ".part"
}

// SaveChunk saves the bytes of a bad chunk to the trash, before anything overwrites them (specs.md §16.1, step 3):
// written to <workdir>/tmp/<op-id>.<index>.part with the copy's permission bits and owner, synced, then moved without
// replacing anything to the operation's folder of chunks. Its folders take the mode and owner of those they stand
// for, and the one named like the file is its owner's alone: the bytes of a private file stay private. copy is the
// copy as its look found it.
func (d *Disk) SaveChunk(op *Op, index int64, data []byte, copyStat *fsx.Stat) error {
	if err := d.Verify(); err != nil {
		return err
	}
	chunk, err := layout.ChunkName(index)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	part := d.tmpPlace(chunkPart(op.ID, index))
	tmp, err := d.makeFolders(part[:len(part)-1], nil)
	if err != nil {
		return err
	}
	defer tmp.close()
	name := part[len(part)-1]
	partStat, err := d.writePart(tmp.Dir, name, data, copyStat)
	if err != nil {
		return err
	}
	src := d.rootPlace(op.Path)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	}
	defer source.close()
	dst := d.trashPlace(op.Path2)
	like, err := d.likeSource(source, src, dst)
	if err != nil {
		return err
	}
	like[len(dst)-1] = fsx.Stat{Mode: 0o700, Uid: copyStat.Uid, Gid: copyStat.Gid}
	target, err := d.makeFolders(dst, like)
	if err != nil {
		return err
	}
	defer target.close()
	if err := d.moveNoReplace(tmp.Dir, name, target.Dir, chunk, byKey(keyOf(&partStat))); err != nil {
		return err
	}
	if err := syncEntry(target.Dir, chunk); err != nil {
		return err
	}
	if err := target.Sync(); err != nil {
		return lost(err)
	}
	if err := tmp.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("chunk-saved")
	return nil
}

// writePart writes the bytes of a chunk to a part of tmp/, never over anything, with the copy's permission bits and
// owner where the driver allows it, and syncs it.
func (d *Disk) writePart(dir fsx.Dir, name string, data []byte, copyStat *fsx.Stat) (fsx.Stat, error) {
	f, err := dir.CreateFile(name, copyStat.Mode&0o777)
	switch {
	case fsx.IsExisting(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.SetOwner(copyStat.Uid, copyStat.Gid); err != nil {
		if fsx.IsDiskLost(err) {
			return fsx.Stat{}, lost(err)
		}
		slog.Warn("a saved chunk keeps the owner it was made with", "name", name, "error", err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return fsx.Stat{}, classify(err)
	}
	if err := f.Sync(); err != nil {
		return fsx.Stat{}, lost(err)
	}
	st, err := f.Info()
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	if err := dir.Sync(); err != nil {
		return fsx.Stat{}, lost(err)
	}
	return st, nil
}

// SyncChunks makes the folder of the chunks a repair saved durable, once they are all there (specs.md §16.1).
func (d *Disk) SyncChunks(op *Op) error {
	if err := d.Verify(); err != nil {
		return err
	}
	dir, err := d.open(d.trashPlace(op.Path2))
	if err != nil {
		return classify(err)
	}
	defer dir.close()
	if err := dir.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("chunks-saved")
	return nil
}

// WriteChunk writes a chunk in place, at its offset, syncs the file, then reads it back past the page cache and
// checks it against its hash (specs.md §16.1, step 5). ErrNotRepaired means that it read back otherwise.
func (d *Disk) WriteChunk(f fsx.WritableFile, index int64, data, want []byte) error {
	if _, err := f.WriteAt(data, index*integrity.ChunkSize); err != nil {
		return classify(err)
	}
	if err := f.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("chunk-written")
	if err := f.DropCache(); err != nil {
		return classify(err)
	}
	back := make([]byte, len(data))
	if n, err := f.ReadAt(back, index*integrity.ChunkSize); n < len(back) {
		if err == nil || errors.Is(err, io.EOF) {
			return ErrNotRepaired
		}
		return classify(err)
	}
	sum := integrity.ChunkSum(back)
	if !bytes.Equal(sum[:], want) {
		return ErrNotRepaired
	}
	return nil
}

// SetTimeBack sets a repaired copy's modification time back to its row's, leaving its access time as it is, and
// syncs the file (specs.md §16.1, step 6). It gives what the file is after.
func (d *Disk) SetTimeBack(f fsx.WritableFile, mtime fsx.Time) (fsx.Stat, error) {
	if err := f.SetTimes(fsx.OmitTime, mtime); err != nil {
		return fsx.Stat{}, classify(err)
	}
	if err := f.Sync(); err != nil {
		return fsx.Stat{}, lost(err)
	}
	d.machine.Checkpoint("times-set")
	st, err := f.Info()
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return st, nil
}

// SavedChunks gives the chunks a repair saved, by index, with their bytes: what its folder of chunks holds. A folder
// never made holds none.
func (d *Disk) SavedChunks(op *Op) (map[int64][]byte, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	dir, err := d.open(d.trashPlace(op.Path2))
	switch {
	case fsx.IsAbsent(err):
		return map[int64][]byte{}, nil
	case err != nil:
		return nil, classify(err)
	}
	defer dir.close()
	listed, err := dir.Names()
	if err != nil {
		return nil, classify(err)
	}
	saved := map[int64][]byte{}
	for _, name := range listed {
		index, err := strconv.ParseInt(name, 10, 64)
		if err != nil || index < 0 {
			continue
		}
		if want, _ := layout.ChunkName(index); want != name {
			continue
		}
		data, err := readAll(dir.Dir, name)
		if err != nil {
			return nil, err
		}
		saved[index] = data
	}
	return saved, nil
}

// PurgeChunks deletes an entry of chunks a repair left in the trash (specs.md §17): each chunk of its folder, then
// the folder, its own; the folders above are for CleanTrash. A folder already gone holds none, and what is not a
// chunk stays, with its folder.
func (d *Disk) PurgeChunks(op *Op) error {
	if err := d.Verify(); err != nil {
		return err
	}
	path := d.trashPlace(op.Path2)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil
	case err != nil:
		return classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	dir, err := parent.OpenDir(name)
	switch {
	case fsx.IsAbsent(err):
		return nil
	case err != nil:
		return classify(err)
	}
	defer func() { _ = dir.Close() }()
	listed, err := dir.Names()
	if err != nil {
		return classify(err)
	}
	for _, chunk := range listed {
		index, err := strconv.ParseInt(chunk, 10, 64)
		if want, _ := layout.ChunkName(index); err != nil || want != chunk {
			slog.Warn("something not a chunk stays in the trash", "disk", d.config.ID, "entry", op.Path2, "name", chunk)
			continue
		}
		if st, err := dir.Stat(chunk); err != nil || st.Kind != fsx.KindFile {
			continue
		}
		if err := dir.Unlink(chunk); err != nil && !fsx.IsAbsent(err) {
			return classify(err)
		}
	}
	d.machine.Checkpoint("moved")
	if err := dir.Sync(); err != nil {
		return lost(err)
	}
	if err := parent.Rmdir(name); err != nil && !fsx.IsAbsent(err) {
		slog.Warn("a folder of chunks stays in the trash", "disk", d.config.ID, "entry", op.Path2, "error", err)
	}
	if err := parent.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return nil
}

// readAll reads a file of a folder whole.
func readAll(dir fsx.Dir, name string) ([]byte, error) {
	f, err := dir.OpenFile(name)
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Info()
	if err != nil {
		return nil, classify(err)
	}
	data := make([]byte, st.Size)
	if n, err := f.ReadAt(data, 0); int64(n) < st.Size {
		return nil, classify(fmt.Errorf("read %q: %w", name, err))
	}
	return data, nil
}

// Why a repair stopped, in its event, when a reprise stops it (specs.md §16.1).
const (
	stoppedByReprise = "reprise"
	stoppedByChange  = "changed"
)

// recoverRepair resolves a repair left planned by reading the copy whole, and the chunks its folder in the trash
// holds, as specs.md §16.1 says: the writes all done, it finishes; begun, the time is set back and the copy stays
// corrupt; not begun, or the copy someone else's since, nothing is written. A repair that does not finish ends done
// when a chunk reached the trash, and undone when none did.
func (d *Disk) recoverRepair(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	_, err := d.ResolveRepair(ctx, db, op, nowNS, "")
	return err
}

// ResolveRepair resolves a repair at once, as its reprise would, after it stopped for why, which its event says when
// it does not finish; a reprise gives none. It says whether the repair finished after all.
func (d *Disk) ResolveRepair(ctx context.Context, db *sql.DB, op *Op, nowNS int64, why string) (bool, error) {
	finished, err := d.resolveRepair(ctx, db, op, nowNS, why)
	return finished, err
}

func (d *Disk) resolveRepair(ctx context.Context, db *sql.DB, op *Op, nowNS int64, why string) (bool, error) {
	saved, err := d.SavedChunks(op)
	if err != nil {
		return false, err
	}
	var savedBytes int64
	for _, data := range saved {
		savedBytes += int64(len(data))
	}
	stop := func(ctime sql.NullInt64, reason string) (bool, error) {
		if why != "" {
			reason = why
		}
		if err := catalog.StopRepair(ctx, db, op, savedBytes, len(saved) > 0, ctime, reason, nowNS); err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		return false, nil
	}
	bad, mtimeNS, known, err := catalog.RepairOf(ctx, db, op.ID)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	row, found, err := catalog.RepairCopyOf(ctx, db, op)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if !known || !found {
		return stop(sql.NullInt64{}, stoppedByChange)
	}
	f, st, err := d.openWritable(op.Path, byKey(op.FileKey))
	switch {
	case errors.Is(err, ErrGone):
		return stop(sql.NullInt64{}, stoppedByChange)
	case err != nil:
		return false, err
	}
	defer func() { _ = f.Close() }()
	chunks := (row.Size + integrity.ChunkSize - 1) / integrity.ChunkSize
	if st.Size != row.Size || int64(len(row.Chunks)) != chunks*integrity.HashSize {
		return stop(sql.NullInt64{}, stoppedByChange)
	}
	seen, err := chunksNow(f, &row, bad, saved)
	if err != nil {
		return false, err
	}
	ctime := TimeOf(st.Ctime)
	action, err := integrity.RepairReprise(seen, ctime.Valid && ctime.Int64 == row.CtimeNS)
	if err != nil {
		// What the trash holds is not what this repair saves: someone else's, as the copy is.
		slog.Warn("a repair's chunks are not what it saves", "disk", d.config.ID, "op", op.ID, "error", err)
		action = integrity.RepairForeign
	}
	switch action {
	case integrity.RepairFinish:
		after, err := d.SetTimeBack(f, fsx.TimeOf(mtimeNS))
		if err != nil {
			return false, err
		}
		if err := catalog.FinishRepair(ctx, db, op, TimeOf(after.Ctime).Int64, nowNS); err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		return true, nil
	case integrity.RepairKeep:
		after, err := d.SetTimeBack(f, fsx.TimeOf(mtimeNS))
		if err != nil {
			return false, err
		}
		return stop(TimeOf(after.Ctime), stoppedByReprise)
	case integrity.RepairUntouched:
		return stop(sql.NullInt64{}, stoppedByReprise)
	case integrity.RepairForeign:
		return stop(sql.NullInt64{}, stoppedByChange)
	default:
		return false, fmt.Errorf("reprise: operation %d: no action %q", op.ID, action)
	}
}

// chunksNow reads a copy whole, and says of each chunk whether the repair found it bad, whether it saved it, and
// what it holds now.
func chunksNow(f fsx.WritableFile, row *catalog.RepairCopy, bad []int64, saved map[int64][]byte) ([]integrity.RepairChunk, error) {
	n := (row.Size + integrity.ChunkSize - 1) / integrity.ChunkSize
	found := make(map[int64]bool, len(bad))
	for _, i := range bad {
		found[i] = true
	}
	buf := make([]byte, integrity.ChunkSize)
	out := make([]integrity.RepairChunk, n)
	for i := range n {
		length := min(integrity.ChunkSize, row.Size-i*integrity.ChunkSize)
		if read, err := f.ReadAt(buf[:length], i*integrity.ChunkSize); int64(read) < length {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, classify(err)
		}
		sum := integrity.ChunkSum(buf[:length])
		data, isSaved := saved[i]
		holds := integrity.HoldsOther
		switch {
		case bytes.Equal(sum[:], row.Chunks[i*integrity.HashSize:(i+1)*integrity.HashSize]):
			holds = integrity.HoldsGood
		case isSaved && bytes.Equal(data, buf[:length]):
			holds = integrity.HoldsSaved
		}
		out[i] = integrity.RepairChunk{Bad: found[i], Saved: isSaved, Holds: holds}
	}
	return out, nil
}
