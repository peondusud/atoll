// Package fusefs serves Atoll's own view through FUSE with go-fuse, which speaks the kernel's protocol itself, as the
// Rust version's fuser will: no cgo and no libfuse (specs.md §21.2, docs/research/fuse-library.md). Every entry and
// statfs come from the catalog, and the data of a file from the copy it presents.
package fusefs
