package ops

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
)

// A chunk a repair saves keeps the permission bits of the copy it comes from, as a copy's part does, whatever the
// umask: it holds the copy's own bytes (specs.md §16.1).
func TestASavedChunkHasTheModeOfItsCopy(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	if err := os.Chmod(filepath.Join(mount, "media", "Films", "a"), 0o640); err != nil {
		t.Fatal(err)
	}
	copyStat := statAt(t, mount, "media/Films/a")
	op := &Op{ID: 9, Disk: "t1", Kind: catalog.OpRepair, Path: "Films/a", Path2: "2026-01-01/9.chunks/Films/a"}
	d := openAt(t, mount, nil)
	if err := d.SaveChunk(op, 0, []byte("film"), &copyStat); err != nil {
		t.Fatal(err)
	}
	if got := statAt(t, mount, "Atoll/trash/2026-01-01/9.chunks/Films/a/00000000").Mode & 0o777; got != 0o640 {
		t.Errorf("the chunk is %o", got)
	}
	// The folder of the chunks synced once they are there: a sync that fails is the disk lost.
	sync := &failingOn{call: "sync", name: "a"}
	if err := openAt(t, mount, sync.fail).SyncChunks(op); !errors.Is(err, ErrDiskLost) || sync.calls != 1 {
		t.Errorf("%d syncs: %v", sync.calls, err)
	}
}

// A chunk is written in place at its own offset, past the first, and read back from there (specs.md §16.1, step 5).
func TestAChunkIsWrittenAtItsOffset(t *testing.T) {
	mount := newMount(t)
	content := append(bytes.Repeat([]byte("a"), integrity.ChunkSize), "xyz"...)
	full := filepath.Join(mount, "media", "Films", "a")
	put(t, mount, "media/Films/a", string(content))
	row := rowAt(t, mount, "Films/a")
	d := openAt(t, mount, nil)
	f, _, err := d.OpenRepair(row)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	good := integrity.ChunkSum([]byte("abc"))
	if err := d.WriteChunk(f, 1, []byte("abc"), good[:]); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:integrity.ChunkSize], content[:integrity.ChunkSize]) || string(got[integrity.ChunkSize:]) != "abc" {
		t.Error("the chunk landed elsewhere")
	}
}

// shortBack reads back fewer bytes than a chunk holds, with the error a read gives.
type shortBack struct {
	fsx.WritableFile
	err error
}

func (s *shortBack) ReadAt(p []byte, off int64) (int, error) {
	n, _ := s.WritableFile.ReadAt(p[:len(p)-1], off)
	return n, s.err
}

// A chunk that reads back short, the end of the file reached or no error said, is not repaired; one whose read back
// fails says so (specs.md §16.1).
func TestAChunkThatReadsBackShortIsNotRepaired(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "fi1m")
	row := rowAt(t, mount, "Films/a")
	d := openAt(t, mount, nil)
	good := integrity.ChunkSum([]byte("film"))
	for _, tc := range []struct {
		err  error
		want error
	}{{nil, ErrNotRepaired}, {io.EOF, ErrNotRepaired}, {syscall.EIO, ErrReadBack}} {
		f, _, err := d.OpenRepair(row)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.WriteChunk(&shortBack{WritableFile: f, err: tc.err}, 0, []byte("film"), good[:]); !errors.Is(err, tc.want) {
			t.Errorf("read back short with %v: %v", tc.err, err)
		}
		_ = f.Close()
		row = rowAt(t, mount, "Films/a")
	}
}

// A purge of chunks removes what is named as a repair names a chunk, and leaves anything else, a number written
// otherwise included, with the folder that holds it, and says so; a folder it empties goes without a word.
func TestAPurgeOfChunksLeavesWhatIsNoChunk(t *testing.T) {
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(old)
	mount := newMount(t)
	const folder = "Atoll/trash/2026-01-01/9.chunks/Films/a"
	put(t, mount, folder+"/00000000", "c0")
	put(t, mount, folder+"/7", "not a chunk's name")
	op := &Op{ID: 10, Disk: "t1", Kind: catalog.OpUnlink, Path: "Films/a", Path2: "2026-01-01/9.chunks/Films/a"}
	d := openAt(t, mount, nil)
	if err := d.PurgeChunks(op); err != nil {
		t.Fatal(err)
	}
	if !absent(mount, folder+"/00000000") || !holding(mount, folder+"/7", "not a chunk's name") {
		t.Error("a chunk stays, or what is none went")
	}
	if !strings.Contains(logged.String(), "something not a chunk stays") || !strings.Contains(logged.String(), "a folder of chunks stays") {
		t.Errorf("what stays is not said: %s", logged.String())
	}
	if err := os.Remove(filepath.Join(mount, filepath.FromSlash(folder), "7")); err != nil {
		t.Fatal(err)
	}
	put(t, mount, folder+"/00000001", "c1")
	logged.Reset()
	if err := d.PurgeChunks(op); err != nil {
		t.Fatal(err)
	}
	if !absent(mount, folder) || logged.Len() != 0 {
		t.Errorf("the folder emptied: %v, said %q", absent(mount, folder), logged.String())
	}
}
