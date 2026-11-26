package jobs

import (
	"slices"
	"testing"
)

// On a disk that holds a time to a second or coarser, as an ext4 made with 128-byte inodes does, or whose precision no
// check read, an edit in place made in the second of a copy's last change, keeping its size and modification time,
// leaves every time its row has (flaw 31 of the TLA+ models): a mismatch there is a conflict, every version kept,
// nothing written, with integrity.auto_repair or without; on a disk whose times tell every change, in the same run,
// rot is corrupt and repaired or moved to the trash as before (specs.md §10, §16).
func TestAMismatchOnADiskWhoseTimesDoNotVouchIsAConflict(t *testing.T) {
	for _, config := range []string{"", "[integrity]\nauto_repair = false\n"} {
		for _, precision := range []int64{1_000_000_000, 0} {
			b := newBenchWith(t, config, "t1", "t2")
			b.precision["t2"] = precision
			for _, id := range []string{"t1", "t2"} {
				b.write(id, "a.mkv", "a film")
				b.write(id, "b.mkv", "another film")
			}
			b.scan(true)
			b.now += int64(month)
			// An edit on the coarse disk, and rot on the other, each as its look leaves it.
			b.rot("t2", "a.mkv", "a FILM")
			b.rot("t1", "b.mkv", "another filM")
			out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
			t1, t2 := diskOf(out, "t1"), diskOf(out, "t2")
			if !slices.Equal(t2.Conflict, []string{"a.mkv"}) || len(t2.Corrupt) != 0 || len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 || len(t2.Kept) != 0 {
				t.Errorf("%q, %d ns: t2 %+v", config, precision, *t2)
			}
			if got := b.read("t2", "a.mkv"); got != "a FILM" {
				t.Errorf("%q, %d ns: the edit on t2 now holds %q", config, precision, got)
			}
			if got := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv'"); got != "conflict" {
				t.Errorf("%q, %d ns: a.mkv is %s", config, precision, got)
			}
			if n := b.count("SELECT count(*) FROM trash WHERE disk_id = 't2'"); n != 0 {
				t.Errorf("%q, %d ns: %d entries in t2's trash", config, precision, n)
			}
			mended := t1.Repaired
			if config != "" {
				mended = t1.Quarantined
			}
			if !slices.Equal(t1.Corrupt, []string{"b.mkv"}) || !slices.Equal(mended, []string{"b.mkv"}) || len(t1.Conflict) != 0 {
				t.Errorf("%q, %d ns: t1 %+v", config, precision, *t1)
			}
			if config == "" && b.read("t1", "b.mkv") != "another film" {
				t.Errorf("%q, %d ns: the rot on t1 is not repaired: %q", config, precision, b.read("t1", "b.mkv"))
			}
		}
	}
}

// A copy a scrub found corrupt on a disk whose times tell every change, and left in the root, as a disk that may only
// be read leaves it, is taken again by a later scrub on a disk known since to hold its times to the second, as one
// Atoll found corrupt before it knew so: an edit made since in the second of its last change cannot be told from the
// rot, so it is read as it is, and its file is a conflict, nothing written, never repaired (specs.md §16, §16.1).
func TestACorruptCopyTakenAgainOnACoarseDiskIsAConflict(t *testing.T) {
	for _, config := range []string{"", "[integrity]\nauto_repair = false\n"} {
		b := newBenchWith(t, config, "t1", "t2")
		b.write("t1", "a.mkv", "a film")
		b.write("t2", "a.mkv", "a film")
		b.scan(true)
		b.now += int64(month)
		b.rot("t2", "a.mkv", "a FILM")
		b.ro[b.dirs["t2"]] = true
		if out := b.scrub(ScrubOptions{Apply: true, Quarantine: true}); !slices.Equal(diskOf(out, "t2").Kept, []string{"a.mkv"}) {
			t.Fatalf("%q: %+v", config, *diskOf(out, "t2"))
		}
		delete(b.ro, b.dirs["t2"])
		b.precision["t2"] = 1_000_000_000
		b.rot("t2", "a.mkv", "A FILM")
		// Read as a version once the edit is settle old, as a copy still being written is none yet (specs.md §15.4).
		if at := b.ctimeOf("t2", "a.mkv") + settleNS; at > b.now {
			b.now = at
		}
		out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
		t2 := diskOf(out, "t2")
		if !slices.Equal(t2.Again, []string{"a.mkv"}) || !slices.Equal(t2.Conflict, []string{"a.mkv"}) || len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 {
			t.Errorf("%q: %+v", config, *t2)
		}
		if got := b.read("t2", "a.mkv"); got != "A FILM" {
			t.Errorf("%q: the edit on t2 now holds %q", config, got)
		}
		if got := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv'"); got != "conflict" {
			t.Errorf("%q: a.mkv is %s", config, got)
		}
		if n := b.count("SELECT count(*) FROM trash"); n != 0 {
			t.Errorf("%q: %d entries in the trash", config, n)
		}
		// One that holds its file's content after all is verified, as on any disk.
		b2 := newBenchWith(t, config, "t1", "t2")
		b2.write("t1", "a.mkv", "a film")
		b2.write("t2", "a.mkv", "a film")
		b2.scan(true)
		b2.now += int64(month)
		b2.rot("t2", "a.mkv", "a FILM")
		b2.ro[b2.dirs["t2"]] = true
		b2.scrub(ScrubOptions{Apply: true, Quarantine: true})
		delete(b2.ro, b2.dirs["t2"])
		b2.precision["t2"] = 1_000_000_000
		b2.rot("t2", "a.mkv", "a film")
		b2.scrub(ScrubOptions{Apply: true, Quarantine: true})
		if got := b2.text("SELECT state || ' ' || (verified_ns IS NOT NULL) FROM copies WHERE disk_id = 't2'"); got != "ok 1" {
			t.Errorf("%q: put right, the copy on t2 is %s", config, got)
		}
	}
}
