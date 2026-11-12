package main

import "testing"

// Only atoll daemon without --until-idle runs for good, whatever --config comes first.
func TestRunsForGood(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"/usr/local/bin/atoll\x00daemon\x00":                                      true,
		"/usr/local/bin/atoll\x00daemon\x00--foreground\x00":                      true,
		"/usr/local/bin/atoll\x00--config\x00/etc/a.toml\x00daemon\x00":           true,
		"/usr/local/bin/atoll\x00--config=/etc/a.toml\x00daemon\x00--dry-run\x00": true,
		"/usr/local/bin/atoll\x00daemon\x00--until-idle\x00":                      false,
		"/usr/local/bin/atoll\x00scan\x00":                                        false,
		"/usr/local/bin/atoll\x00--config\x00daemon\x00scan\x00":                  false,
		"/usr/local/bin/atoll\x00":                                                false,
		"":                                                                        false,
	} {
		if got := runsForGood([]byte(cmdline)); got != want {
			t.Errorf("%q: %v", cmdline, got)
		}
	}
	if n, err := tellDaemon(); err != nil || n != 0 {
		t.Errorf("told %d: %v", n, err)
	}
}
