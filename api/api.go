// Package api holds the language-neutral contracts of what the daemon answers (specs.md §23.1, §25.1, §29.3). The
// files are the contract; this package only embeds the one the binary reads, as Go can embed a file from its own
// folder and below.
package api

import _ "embed" // the contract is read as it is written

//go:embed metrics.json
var metrics []byte

// Metrics returns api/metrics.json: the metrics /metrics gives, in their order. The caller must not change it.
func Metrics() []byte {
	return metrics
}
