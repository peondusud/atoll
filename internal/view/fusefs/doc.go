// Package fusefs serves Atoll's own view through FUSE, with cgofuse and libfuse3 (specs.md §21.2): every entry and
// statfs from the catalog, the data of a file from the copy it presents. It is built with cgo and the fuse3 tag only:
// cgofuse loads libfuse3.so.3 when the view mounts, and any other build has a stub, which says the view is not in it.
package fusefs
