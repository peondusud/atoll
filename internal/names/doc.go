// Package names handles exact paths, collision keys and per-filesystem name
// compatibility (specs.md §15.8), and how a person is shown a name (§23, §25).
// It is pure: no I/O and no global state. Its test cases come from
// conformance/vectors/ and conformance/corpus/names/.
package names
