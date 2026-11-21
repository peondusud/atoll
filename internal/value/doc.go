// Package value parses the value grammars of the configuration (specs.md
// §29.5): durations, sizes, percentages, time windows, and where a UPS is read
// through NUT (§20). Globs and regexes
// live in internal/glob and internal/rules.
//
// It is pure: no I/O, no clock and no global state. Its test cases come from
// conformance/vectors/. Numbers are computed exactly, never through floats, so
// the Go and Rust versions agree on every input.
package value
