package watch

import "testing"

// A process of Atoll's own executable that runs atoll mount serves Atoll's own view: what it does is a user's change,
// as what mergerfs does is (specs.md §15.1, §21.2); any other is Atoll's own.
func TestIsMountCommand(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"atoll\x00mount\x00": true,
		"/usr/local/bin/atoll\x00mount\x00/srv/pool-ro\x00": true,
		"atoll\x00--config\x00/etc/a.toml\x00mount\x00":     true,
		"atoll\x00--config=/etc/a.toml\x00mount\x00":        true,
		"atoll\x00daemon\x00":                               false,
		"atoll\x00--config\x00mount\x00daemon\x00":          false,
		"atoll\x00": false,
		"":          false,
	} {
		if got := IsMountCommand([]byte(cmdline)); got != want {
			t.Errorf("%q: %v, want %v", cmdline, got, want)
		}
	}
}
