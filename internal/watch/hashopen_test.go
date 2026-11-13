package watch

import (
	"errors"
	"maps"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
)

// refusing makes the open of one file fail on one disk.
func refusing(disk, name string, with error) func(string, fsx.Dir) fsx.Dir {
	return func(id string, d fsx.Dir) fsx.Dir {
		if id != disk {
			return d
		}
		return fsx.Fault(d, func(call, n string) error {
			if call == "openfile" && n == name {
				return with
			}
			return nil
		})
	}
}

// A file that cannot be opened for its hash, for a reason that says nothing of its disk, is reported unreadable, with
// the names that refuse the account, and the hashes of its disk go on (specs.md §15.10). It stopped them, as an I/O
// error must: the file stays without a hash, so each slice of the daemon stopped at it again, and the files after it
// in the order of the paths were never hashed on that disk, with nothing to say so.
func TestAFileThatCannotBeOpenedDoesNotStopTheHashes(t *testing.T) {
	for _, refusal := range []error{syscall.EACCES, syscall.ENOKEY, syscall.EPERM, syscall.EINVAL} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.write("t1", "b", "y")
		b.write("t1", "c", "z")
		b.wrap = refusing("t1", "b", refusal)
		r := b.scan(Options{Full: true}).Disks[0]
		if !slices.Equal(r.Unreadable, []string{"b"}) || !slices.Equal(r.Refused, []string{"b"}) || r.Hashed != 2 || r.Verdict.State != disks.Online {
			t.Errorf("%v: %+v", refusal, r)
		}
		if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 2 {
			t.Errorf("%v: %d hashes, want a and c", refusal, got)
		}
		if got := b.count("SELECT count(*) FROM disks WHERE id = 't1' AND state = 'missing'"); got != 0 {
			t.Errorf("%v: the disk is recorded as missing", refusal)
		}
	}
}

// An I/O error alone stops the disk, at the open as at the read (invariant 4).
func TestAnIOErrorAtTheOpenStopsTheDisk(t *testing.T) {
	for _, failure := range []error{syscall.EIO, syscall.ENODEV, syscall.ESTALE} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.write("t1", "b", "y")
		b.write("t1", "c", "z")
		b.wrap = refusing("t1", "b", failure)
		r := b.scan(Options{Full: true}).Disks[0]
		if r.Verdict.State != disks.Missing || r.Verdict.Reason != disks.IOError || r.Hashed != 1 || len(r.Refused) != 0 {
			t.Errorf("%v: %+v", failure, r)
		}
	}
}

// A process out of descriptors can open no file of any disk: the reading stops there, with an error, and no disk is
// the worse for it. What was hashed before stays.
func TestOutOfDescriptorsStopsTheReadingAndNoDisk(t *testing.T) {
	for _, failure := range []error{syscall.EMFILE, syscall.ENFILE} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.write("t1", "b", "y")
		b.write("t1", "c", "z")
		b.wrap = refusing("t1", "b", failure)
		if _, err := Scan(t.Context(), b.db, b.config, b, Options{Full: true}); !errors.Is(err, failure) {
			t.Fatalf("%v: the scan ends with %v", failure, err)
		}
		if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 1 {
			t.Errorf("%v: %d hashes, want a", failure, got)
		}
		if got := b.count("SELECT count(*) FROM disks WHERE id = 't1' AND state = 'missing'"); got != 0 {
			t.Errorf("%v: the disk is recorded as missing", failure)
		}
	}
}

// The slices of the daemon go on past such a file, on its disk and on the others, and say which files they could not
// open, by disk: the alert no-access counts them (specs.md §15.13, §23). A file one copy of which can be read gets
// its hash from it.
func TestHashNewGoesOnPastAFileItCannotOpen(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c", "d"} {
		b.write("t1", name, "x"+name)
	}
	b.write("t2", "c", "xc")
	b.scan(Options{})
	b.wrap = func(id string, d fsx.Dir) fsx.Dir {
		if id != "t1" {
			return d
		}
		return fsx.Fault(d, func(call, n string) error {
			if call == "openfile" && (n == "b" || n == "c") {
				return syscall.EACCES
			}
			return nil
		})
	}
	out, err := HashNew(t.Context(), b.db, b.config, b, b.now+3_600_000_000_000)
	if err != nil || out.Left {
		t.Fatalf("%+v, %v", out, err)
	}
	if want := map[string][]string{"t1": {"b", "c"}}; !maps.EqualFunc(out.Refused, want, slices.Equal[[]string]) {
		t.Errorf("refused %v, want %v", out.Refused, want)
	}
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT), ',') FROM (SELECT lpath FROM files WHERE blake3 IS NOT NULL ORDER BY lpath)"); got != "a,c,d" {
		t.Errorf("hashed %s, want a, c from its other copy, and d", got)
	}
	if got := b.count("SELECT count(*) FROM disks WHERE state = 'missing'"); got != 0 {
		t.Errorf("%d disks recorded as missing", got)
	}
}
