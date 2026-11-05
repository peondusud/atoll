// Package fsx abstracts disk I/O, with a real implementation and one with
// fault injection. All disk I/O goes through it: business logic never calls
// os.* directly.
package fsx
