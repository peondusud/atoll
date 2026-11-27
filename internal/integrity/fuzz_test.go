package integrity

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// The reprise of a repair decides from what each chunk of a copy holds after a crash (specs.md §16.1), and a copy is
// what anyone who can write its disk made of it. The fuzz targets check the decision on any chunks, and against a
// repair stopped at each of its steps.

// chunksOf reads bytes as chunks: of each byte, bit 0 says that the repair found the chunk bad, bit 1 that it saved
// it, and the next two what it holds, a word the reprise does not know among them.
func chunksOf(data []byte) []RepairChunk {
	words := []Holds{HoldsGood, HoldsSaved, HoldsOther, "torn"}
	chunks := make([]RepairChunk, 0, len(data))
	for _, b := range data {
		chunks = append(chunks, RepairChunk{Bad: b&1 != 0, Saved: b&2 != 0, Holds: words[b>>2&3]})
	}
	return chunks
}

// FuzzRepairReprise: on any chunks, an error for a word it does not know or a chunk saved that was not found bad or
// holding what was never saved, and no error otherwise; finish only over a copy whose every chunk holds its hash;
// untouched only when no chunk was written and the change time is the row's; keep only once every bad chunk was saved
// and a write began; and never a time written over a chunk the repair never touched.
func FuzzRepairReprise(f *testing.F) {
	for _, v := range vectors.Load[repriseVector](f, "repair_reprise.json") {
		data := make([]byte, 0, len(v.Chunks))
		for _, c := range v.Chunks {
			var b byte
			if c.Bad != nil && *c.Bad {
				b |= 1
			}
			if c.Saved != nil && *c.Saved {
				b |= 2
			}
			if c.Holds != nil {
				switch Holds(*c.Holds) {
				case HoldsGood:
				case HoldsSaved:
					b |= 1 << 2
				case HoldsOther:
					b |= 2 << 2
				default:
					b |= 3 << 2
				}
			}
			data = append(data, b)
		}
		f.Add(data, v.CtimeSame != nil && *v.CtimeSame)
	}
	f.Fuzz(func(t *testing.T, data []byte, ctimeSame bool) {
		chunks := chunksOf(data)
		got, err := RepairReprise(chunks, ctimeSame)
		valid, untouched, written, allSaved, allGood := true, true, false, true, true
		for _, c := range chunks {
			known := c.Holds == HoldsGood || c.Holds == HoldsSaved || c.Holds == HoldsOther
			valid = valid && known && (!c.Saved || c.Bad) && (c.Holds != HoldsSaved || c.Saved)
			allGood = allGood && c.Holds == HoldsGood
			untouched = untouched && (c.Bad || c.Holds == HoldsGood)
			if c.Bad {
				allSaved = allSaved && c.Saved
				written = written || c.Holds != HoldsSaved
			}
		}
		switch {
		case err != nil:
			if valid {
				t.Fatalf("RepairReprise(%+v, %v): %v, on chunks it knows", chunks, ctimeSame, err)
			}
			return
		case !valid && got != RepairForeign:
			// The first chunk someone else changed decides before a later one it does not know.
			t.Fatalf("RepairReprise(%+v, %v) = %s, on chunks it does not know", chunks, ctimeSame, got)
		}
		switch got {
		case RepairFinish:
			if !allGood || !allSaved {
				t.Fatalf("RepairReprise(%+v, %v): finish over a copy that does not hold its hashes", chunks, ctimeSame)
			}
		case RepairUntouched:
			if !ctimeSame || !untouched || slicesAny(chunks, func(c RepairChunk) bool { return c.Bad && c.Holds == HoldsGood }) {
				t.Fatalf("RepairReprise(%+v, %v): untouched over a copy the repair or someone else wrote", chunks, ctimeSame)
			}
		case RepairKeep:
			if !untouched || !allSaved || !written || allGood {
				t.Fatalf("RepairReprise(%+v, %v): keep over a copy its repair does not explain", chunks, ctimeSame)
			}
		case RepairForeign:
		default:
			t.Fatalf("RepairReprise(%+v, %v) = %q", chunks, ctimeSame, got)
		}
	})
}

func slicesAny(chunks []RepairChunk, f func(RepairChunk) bool) bool {
	for _, c := range chunks {
		if f(c) {
			return true
		}
	}
	return false
}

// FuzzRepairStopped: a repair saves every bad chunk, then writes them in order, and a crash can stop it at any step,
// a chunk torn halfway (specs.md §16.1). Whatever the step, the reprise says what that step left: untouched before the
// first write, unless the change time moved, which is then someone else's; finish once every chunk is written; keep
// in between; and a chunk torn halfway someone else's, as a user's write there is never told from it (flaw 33).
func FuzzRepairStopped(f *testing.F) {
	f.Add([]byte{1, 0, 1, 1}, uint8(3), uint8(1), true, true)
	f.Add([]byte{1}, uint8(0), uint8(0), false, false)
	f.Add([]byte{0, 0}, uint8(0), uint8(0), false, true)
	f.Fuzz(func(t *testing.T, layout []byte, saved, written uint8, torn, ctimeSame bool) {
		chunks := make([]RepairChunk, len(layout))
		var bad []int
		for i, b := range layout {
			chunks[i] = RepairChunk{Holds: HoldsGood}
			if b&1 != 0 {
				bad = append(bad, i)
				chunks[i] = RepairChunk{Bad: true, Holds: HoldsOther}
			}
		}
		var want RepairAction
		if s := int(saved) % (len(bad) + 1); s < len(bad) {
			// Saving: some saved, none written.
			for _, i := range bad[:s] {
				chunks[i].Saved, chunks[i].Holds = true, HoldsSaved
			}
			want = RepairUntouched
			if !ctimeSame {
				want = RepairForeign
			}
		} else {
			w := int(written) % (len(bad) + 1)
			for k, i := range bad {
				chunks[i].Saved = true
				switch {
				case k < w:
					chunks[i].Holds = HoldsGood
				case k == w && torn:
					chunks[i].Holds = HoldsOther
				default:
					chunks[i].Holds = HoldsSaved
				}
			}
			switch {
			case w == len(bad):
				want = RepairFinish
			case torn:
				want = RepairForeign
			case w == 0 && ctimeSame:
				want = RepairUntouched
			case w == 0:
				want = RepairForeign
			default:
				want = RepairKeep
			}
		}
		if got, err := RepairReprise(chunks, ctimeSame); err != nil || got != want {
			t.Fatalf("RepairReprise(%+v, %v) = %s, %v, want %s", chunks, ctimeSame, got, err, want)
		}
	})
}
