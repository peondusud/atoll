package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
)

// secretFileWarnings gives a warning for each file of a secret the configuration names that other accounts may read
// or write, or that belongs to an account that is none of Atoll's, root's, the one this process runs as or the
// catalog's owner, which is the daemon's when root runs the command: that account may change it whatever its mode
// (specs.md §24.4). A warning, not a refusal: version 1 takes such a configuration. A file that cannot be looked at
// says nothing here: what reads it says why it cannot.
func secretFileWarnings(c *config.Config) []config.Problem {
	trusted := ops.TrustedOwners(catalogPath(c))
	var out []config.Problem
	for _, s := range c.SecretFiles() {
		owner, err := fsx.StatSecretFile(s.Path)
		if err != nil {
			continue
		}
		var why []string
		perm := owner.Mode.Perm()
		if perm&0o004 != 0 {
			why = append(why, "other accounts may read it (chmod o-r)")
		}
		if perm&0o002 != 0 {
			why = append(why, "other accounts may write it (chmod o-w)")
		}
		if !slices.Contains(trusted, owner.UID) {
			why = append(why, fmt.Sprintf("it belongs to the account %d, none of Atoll's, which may change it (chown root)", owner.UID))
		}
		if len(why) > 0 {
			out = append(out, config.Problem{Key: s.Key, Message: "this file holds a secret, and " + strings.Join(why, ", and ")})
		}
	}
	return out
}

// configWarnings gives every warning of a configuration that loaded, those its text gives and those of the files of a
// secret it names, sorted by key in byte order (specs.md §24.4).
func configWarnings(c *config.Config) []config.Problem {
	out := slices.Concat(c.Warnings, secretFileWarnings(c))
	slices.SortStableFunc(out, func(a, b config.Problem) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// logWarnings writes each warning of the configuration the daemon runs with to its log, at its start and at each
// reload, as nobody may run atoll rules check (specs.md §24.4).
func logWarnings(log *slog.Logger, c *config.Config) {
	for _, w := range configWarnings(c) {
		log.Warn("the configuration has a warning", "key", w.Key, "warning", w.Message, "later_phase", w.LaterPhase)
	}
}
