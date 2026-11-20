//go:build race

package jobs

// raceBuild says that the tests run with the race detector, which makes a sweep of every call of a pass too slow to
// run whole: a sweep then fails one call in sweepStride, each error at other calls.
const raceBuild = true
