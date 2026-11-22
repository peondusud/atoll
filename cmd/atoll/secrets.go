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

// tokenWarnings gives a warning when api.token_file holds a token under apiMinToken bytes, its last line break left
// out, as the API compares it (specs.md §24.4): one that short is easier to guess through api.listen's 401s; and when
// it holds none, which refuses every request over TCP. A warning, not a refusal: version 1 takes such a token. The
// token is never quoted, and a file that cannot be read says nothing here: what reads it says why.
func tokenWarnings(c *config.Config) []config.Problem {
	file, _ := c.Effective()["api.token_file"].(string)
	if file == "" {
		return nil
	}
	token, _, err := readToken(file)
	switch {
	case err != nil:
		return nil
	case token == "":
		return []config.Problem{{Key: "api.token_file", Message: "this file holds no token, so the API refuses every request over TCP: " +
			"write one of 32 bytes or more, as openssl rand -hex 32 makes"}}
	case len(token) < apiMinToken:
		return []config.Problem{{Key: "api.token_file", Message: fmt.Sprintf("its token is %d bytes long, under 32, and so easier to guess "+
			"over TCP: write one of 32 bytes or more, as openssl rand -hex 32 makes", len(token))}}
	}
	return nil
}

// configWarnings gives every warning of a configuration that loaded, those its text gives, those of the files of a
// secret it names and that of the API's token, sorted by key in byte order (specs.md §24.4).
func configWarnings(c *config.Config) []config.Problem {
	out := slices.Concat(c.Warnings, secretFileWarnings(c), tokenWarnings(c))
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
