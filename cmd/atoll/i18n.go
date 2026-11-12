package main

import "strings"

// french says that what the CLI writes for people is in French (specs.md §25). main sets it once, from the
// environment, before anything is written; a test that runs a command in-process stays in English unless it sets it.
var french bool

// wantsFrench says whether an environment asks for French: the first of LC_ALL, LC_MESSAGES and LANG that is set and
// not empty starts with "fr" (specs.md §25).
func wantsFrench(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(name); v != "" {
			return strings.HasPrefix(v, "fr")
		}
	}
	return false
}

// T translates a message the CLI writes for people, its English text being the key. The --json outputs, the words
// they share with it, names, paths and configuration keys stay in English (specs.md §25). A message with no
// translation stays in English: a test checks that every one has one.
func T(english string) string {
	if french {
		if text, ok := frenchMessages[english]; ok {
			return text
		}
	}
	return english
}
