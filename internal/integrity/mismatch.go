package integrity

// Mismatch is what a scrub makes of a copy whose chunk does not hold what its hash says, or cannot be read on a disk
// that confirms, read again past the cache, its size and times unchanged (specs.md §16).
type Mismatch string

// What a mismatch makes of a copy.
const (
	// MismatchCorrupt: rot, the copy corrupt, repaired from a healthy copy or moved to the trash.
	MismatchCorrupt Mismatch = "corrupt"
	// MismatchConflict: another version of the file, which becomes a conflict, every version kept, nothing written.
	MismatchConflict Mismatch = "conflict"
)

// wholeSecond is a precision at which two changes in one second leave one change time.
const wholeSecond = 1_000_000_000

// TimesVouch says whether a disk's times tell every change of a copy, so that a copy whose size and times are still its
// row's holds what it held then (specs.md §10, §16). A disk that holds a time to a second or coarser, as an ext4 made
// with 128-byte inodes does, gives an edit in place made in the second of the copy's last change the change time it
// had; a precision no check read (0) vouches for nothing either.
func TimesVouch(precisionNS int64) bool {
	return precisionNS > 0 && precisionNS < wholeSecond
}

// ScrubMismatch says what a mismatch makes of a copy of a disk that holds a time to precisionNS: a conflict when the
// copy was never verified, as nothing says it ever held its file's content, or when the disk's times do not vouch for
// it, as an edit cannot be told from rot there; corrupt otherwise (specs.md §16).
func ScrubMismatch(verified bool, precisionNS int64) Mismatch {
	if verified && TimesVouch(precisionNS) {
		return MismatchCorrupt
	}
	return MismatchConflict
}
