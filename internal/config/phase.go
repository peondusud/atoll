package config

import (
	"fmt"
	"regexp"
)

// laterPhaseErrors holds the one setting a later phase needs that cannot fall back
// (specs.md §24.4). It goes when phase 2a brings Atoll's own view.
func laterPhaseErrors(tree map[string]any) []Problem {
	if text(table(tree, "view"), "engine") == "atoll" {
		return []Problem{{Key: "view.engine", Message: "Atoll's own view arrives in phase 2a; there would be no merged view"}}
	}
	return nil
}

// laterPhaseWarnings warns about what the file or the preset set and this build cannot
// do yet (specs.md §24.4, §28). Delete a line when its phase lands.
func laterPhaseWarnings(set map[string]any) []Problem {
	index := regexp.MustCompile(`\[[0-9]+\]`)
	var out []Problem
	for key, v := range set {
		var phase, effect string
		switch index.ReplaceAllString(key, "") {
		case "placement.new_files":
			if v != "most-free" {
				phase, effect = "2b", "works as most-free"
			}
		case "placement.colocate", "rule.colocate":
			if v != "none" {
				phase = "2b"
			}
		case "trash.link_guard":
			if v == true {
				phase = "1b"
			}
		case "balance.equalize":
			if v != "off" {
				phase = "2b"
			}
		case "read.policy":
			if v != "oldest" {
				phase = "2a"
			}
		case "read.classes":
			phase = "2a"
		case "cache.enabled":
			if v == true {
				phase = "2b"
			}
		case "disk.role":
			if v == "cache" {
				phase = "2b"
			}
		case "rule.cache", "rule.cache_min_age":
			phase = "2b"
		case "ups.enabled":
			if v == true {
				phase = "2b"
			}
		case "health.evacuate_on", "health.avoid_hotter_than":
			phase = "4"
		}
		if phase != "" {
			if effect == "" {
				effect = "no effect yet"
			}
			out = append(out, Problem{Key: key, Message: fmt.Sprintf("arrives in phase %s: %s", phase, effect), LaterPhase: true})
		}
	}
	return out
}
