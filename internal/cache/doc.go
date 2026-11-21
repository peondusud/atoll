// Package cache decides what the SSD tier does with each file on it: whether it stays, resident, or leaves now for the
// data disks (specs.md §19). It is pure: the plan of the solver places what leaves, and a pass moves it.
package cache
