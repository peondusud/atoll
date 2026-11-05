// Package config loads the configuration from its TOML text (specs.md §24). It is
// pure: no I/O, no clock and no global state. Its test cases come from
// conformance/vectors/config.json.
//
// schema/config.schema.json is the one list of keys, types, allowed values and
// defaults: this package reads it rather than repeating it, so the two cannot drift.
package config
