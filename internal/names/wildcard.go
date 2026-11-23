package names

import "strings"

// Wildcards are the five wildcard characters of Windows, which the name collation of the type ntfs, the NTFS driver of
// Linux 7.1 and later, calls invalid: it cannot take such a name out of its folder, to move it or to remove it,
// without setting its whole volume read-only (measured, docs/research/ntfs-driver.md).
const Wildcards = "\"*<>?"

// HoldsWildcard says whether a component of a logical path, a folder's included, holds one of the Wildcards. Atoll
// never writes such a name to NTFS (forbidden-character), and leaves a copy at such a path on a disk of the type ntfs
// as it is (specs.md §15.8, §22, conformance/vectors/wildcard_name.json). The path is read by its bytes, so a name that
// is not valid UTF-8 is read too, and none of the characters is ever a byte of another's UTF-8 encoding.
func HoldsWildcard(path string) bool {
	return strings.ContainsAny(path, Wildcards)
}
