package integrity

import "fmt"

// Holds says what a chunk of a copy holds now, against what a repair knows of it (specs.md §16.1).
type Holds string

// What a chunk can hold.
const (
	// HoldsGood is what its hash says.
	HoldsGood Holds = "good"
	// HoldsSaved is what the repair saved of it to the trash.
	HoldsSaved Holds = "saved"
	// HoldsOther is neither.
	HoldsOther Holds = "other"
)

// RepairChunk is one chunk of a copy a repair left planned: whether the repair found it bad, whether its bytes
// were saved to the trash, and what it holds now.
type RepairChunk struct {
	Bad   bool
	Saved bool
	Holds Holds
}

// RepairAction is what the reprise of a repair does (specs.md §16.1).
type RepairAction string

// What a reprise of a repair can do.
const (
	// RepairFinish: the writes were all done; the time is set back and the operation ends done.
	RepairFinish RepairAction = "finish"
	// RepairKeep: the writes began; the time is set back, the row takes the change time, and the copy stays
	// corrupt, the operation undone.
	RepairKeep RepairAction = "keep"
	// RepairUntouched: no write reached the copy; the operation is undone, and nothing is written.
	RepairUntouched RepairAction = "untouched"
	// RepairForeign: someone else changed the copy; the operation is undone, nothing is written, and the next
	// pass finds the change.
	RepairForeign RepairAction = "foreign"
)

// RepairReprise says what the reprise of a repair does with a copy at its name, of its inode and its row's size,
// from what each chunk holds, by index, and whether its change time is still its row's (specs.md §16.1). Every bad
// chunk is saved before any is written, and they are written in order: the repair explains what it finds when every
// chunk it did not find bad holds what its hash says, and its bad chunks hold what their hashes say, then at most
// one chunk that holds neither that nor what was saved of it, then what was saved of them. A chunk saved that was
// not found bad, or a word it does not know, is an error.
func RepairReprise(chunks []RepairChunk, ctimeSame bool) (RepairAction, error) {
	allSaved := true
	for i := range chunks {
		c := &chunks[i]
		switch c.Holds {
		case HoldsGood, HoldsOther:
		case HoldsSaved:
			if !c.Saved {
				return "", fmt.Errorf("repair reprise: chunk %d holds what was saved of it, and none was", i)
			}
		default:
			return "", fmt.Errorf("repair reprise: chunk %d holds %q", i, c.Holds)
		}
		if c.Saved && !c.Bad {
			return "", fmt.Errorf("repair reprise: chunk %d was saved, and not found bad", i)
		}
		if !c.Bad && c.Holds != HoldsGood {
			return RepairForeign, nil
		}
		allSaved = allSaved && (!c.Bad || c.Saved)
	}
	if !allSaved {
		// No write begins before every bad chunk is saved: each still holds what the repair found.
		for i := range chunks {
			if chunks[i].Bad && chunks[i].Holds == HoldsGood {
				return RepairForeign, nil
			}
		}
		if ctimeSame {
			return RepairUntouched, nil
		}
		return RepairForeign, nil
	}
	// Written in order: what holds its hash, then at most one chunk torn by the crash, then what was saved.
	const (
		written = iota
		torn
		left
	)
	stage, good, saved := written, 0, 0
	bad := 0
	for i := range chunks {
		c := &chunks[i]
		if !c.Bad {
			continue
		}
		bad++
		switch {
		case c.Holds == HoldsGood && stage == written:
			good++
		case c.Holds == HoldsOther && stage == written:
			stage = torn
		case c.Holds == HoldsSaved:
			stage = left
			saved++
		default:
			return RepairForeign, nil
		}
	}
	switch {
	case good == bad:
		return RepairFinish, nil
	case saved == bad && ctimeSame:
		return RepairUntouched, nil
	case saved == bad:
		// Nothing the repair wrote, and the change time moved: someone else's.
		return RepairForeign, nil
	default:
		return RepairKeep, nil
	}
}
