// Package integrity holds the BLAKE3 whole-file and 4 MiB chunk hashes, scrub,
// repair, quarantine and duplicates (specs.md §16). Hashing is pure and loads
// its test cases from conformance/vectors/; the rest does I/O.
package integrity
