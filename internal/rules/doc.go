// Package rules parses and matches the ordered rules and derives the effective
// policy of a file (specs.md §11). It is pure: no I/O and no global state. Its
// test cases come from conformance/vectors/.
package rules
