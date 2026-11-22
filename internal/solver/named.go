package solver

// NamedVerdict is what a look limited to the names the watcher gave makes of the verdict a whole walk would give a copy
// (specs.md §15.13). Such a look sees nothing but those names, so a copy it no longer finds may have gone anywhere it did
// not look: it is gone only when an event said that its name, or a folder above it, was deleted, or when its row says it
// went already, as a look found before. Otherwise nothing is concluded of it, and whole says that the next pass walks
// the disk whole. Any other verdict is what the look found: a copy found at a name it read is there, whatever an event
// said.
func NamedVerdict(v CopyVerdict, deleted bool, rowKind string) (next CopyVerdict, whole bool) {
	if v != VerdictGone || deleted || rowKind == string(VerdictGone) {
		return v, false
	}
	return VerdictUnknown, true
}
