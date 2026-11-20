//go:build !race

package jobs

// raceBuild says that the tests run with the race detector: here they do not, and a sweep fails every call.
const raceBuild = false
