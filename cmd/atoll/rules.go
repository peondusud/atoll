package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/value"
)

func runRules(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("rules: want check or explain"))
	}
	switch args[0] {
	case "check":
		return rulesCheck(configPath, args[1:], stdout, stderr)
	case "explain":
		return rulesExplain(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf(T("rules: unknown command %q"), args[0]))
	}
}

// load reads the configuration, then does what config.Parse leaves to the shell, as it
// depends on the machine: look the time zone up (specs.md §24.3).
func load(path string) (*config.Config, error) {
	_, c, err := readConfig(path)
	return c, err
}

// readConfig reads a configuration file once, and gives its text with what that text says: a command that edits the
// file edits the text it loaded.
func readConfig(path string) ([]byte, *config.Config, error) {
	data, err := fsx.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("configuration: %w", err)
	}
	c, err := config.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	zone, _ := c.Effective()["timezone"].(string)
	if err := lookUpZone(zone); err != nil {
		return nil, nil, &config.Error{Problems: []config.Problem{{Key: "timezone", Message: err.Error()}}}
	}
	return data, c, nil
}

func lookUpZone(zone string) error {
	if zone == "local" {
		return nil
	}
	// To time.LoadLocation, "Local" is the machine's own zone, with no file of that name.
	// Only "local" means that, and the Rust version must refuse this one as well.
	if zone == "Local" {
		return errors.New(`unknown time zone "Local": the machine's own is "local"`)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf(T("time zone %q: %w"), zone, err)
	}
	return nil
}

type checkProblem struct {
	Key     string `json:"key,omitempty"`
	Message string `json:"message"`
}

type checkWarning struct {
	Key        string `json:"key"`
	LaterPhase bool   `json:"later_phase"`
	Message    string `json:"message"`
}

// checkOutput is the output of api/schemas/rules-check.schema.json.
type checkOutput struct {
	Valid    bool           `json:"valid"`
	Problems []checkProblem `json:"problems"`
	Warnings []checkWarning `json:"warnings"`
	// Hidden are the rules an earlier one hides whole, in simple cases, and Unreached the rules no file of the catalog
	// reaches, nil when the catalog cannot be read (specs.md §11.3, §25).
	Hidden    []checkHidden `json:"hidden"`
	Unreached []string      `json:"unreached"`
}

// checkHidden is a rule an earlier one hides whole: their labels.
type checkHidden struct {
	Rule string `json:"rule"`
	By   string `json:"by"`
}

func rulesCheck(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("rules check: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "rules check: "+err.Error())
		}
	}
	out := check(configPath)
	var err error
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = printCheck(stdout, out)
	}
	if err != nil {
		return failure(stderr, err)
	}
	if !out.Valid {
		return exitError
	}
	return exitOK
}

func check(configPath string) checkOutput {
	out := checkOutput{Valid: true, Problems: []checkProblem{}, Warnings: []checkWarning{}, Hidden: []checkHidden{}}
	c, err := load(configPath)
	if err != nil {
		out.Valid = false
		var invalid *config.Error
		if !errors.As(err, &invalid) {
			out.Problems = append(out.Problems, checkProblem{Message: err.Error()})
			return out
		}
		for _, p := range invalid.Problems {
			out.Problems = append(out.Problems, checkProblem{Key: p.Key, Message: p.Message})
		}
		return out
	}
	for _, w := range c.Warnings {
		out.Warnings = append(out.Warnings, checkWarning{Key: w.Key, LaterPhase: w.LaterPhase, Message: w.Message})
	}
	for _, h := range rules.Hidden(c.Rules) {
		out.Hidden = append(out.Hidden, checkHidden{Rule: c.Rules[h.Rule].Label, By: c.Rules[h.By].Label})
	}
	out.Unreached = unreached(c)
	return out
}

// unreached gives the rules no file of the catalog reaches, nil when the catalog cannot be read: there is none yet,
// or it is another pool's or of another version (specs.md §25). It reads the catalog and nothing else, and takes no
// lock: what a command writes meanwhile is a file more or less, which the next check counts.
func unreached(c *config.Config) []string {
	ctx := context.Background()
	file := catalogPath(c)
	if there, err := fsx.Exists(file); err != nil || !there {
		return nil
	}
	db, err := catalog.OpenPool(ctx, file, c.PoolID)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	reach := rules.NewReach(c.Rules)
	if err := catalog.RuleSubjects(ctx, db, func(path string, size *int64) { reach.Add(path, size) }); err != nil {
		return nil
	}
	out := []string{}
	for _, at := range reach.Unreached() {
		out = append(out, c.Rules[at].Label)
	}
	return out
}

func printCheck(w io.Writer, out checkOutput) error {
	var text strings.Builder
	for _, p := range out.Problems {
		if p.Key == "" {
			fmt.Fprintf(&text, T("problem: %s\n"), p.Message)
		} else {
			fmt.Fprintf(&text, T("problem: %s: %s\n"), p.Key, p.Message)
		}
	}
	for _, warning := range out.Warnings {
		fmt.Fprintf(&text, T("warning: %s: %s\n"), warning.Key, warning.Message)
	}
	for _, h := range out.Hidden {
		fmt.Fprintf(&text, T("warning: rule %s: rule %s, before it, matches every file it does: it never applies\n"), h.Rule, h.By)
	}
	for _, label := range out.Unreached {
		fmt.Fprintf(&text, T("note: rule %s: no file of the catalog reaches it\n"), label)
	}
	if out.Valid {
		text.WriteString(T("the configuration is valid\n"))
	}
	return writeText(w, text.String())
}

type explainArgs struct {
	path   string
	size   *int64
	asJSON bool
}

// parseExplain reads the arguments of rules explain (specs.md §25).
func parseExplain(args []string) (explainArgs, error) {
	var out explainArgs
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case a == "--json":
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
		case a == "--size":
			i++
			if i == len(args) {
				return out, errors.New(T("--size needs a size"))
			}
			if err := out.setSize(args[i]); err != nil {
				return out, err
			}
		case strings.HasPrefix(a, "--size="):
			if err := out.setSize(strings.TrimPrefix(a, "--size=")); err != nil {
				return out, err
			}
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf(T("unknown argument %q"), a)
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) != 1 {
		return out, fmt.Errorf(T("want one path, got %d"), len(paths))
	}
	out.path = paths[0]
	// Only its form counts here: a name a filesystem refuses still has a policy.
	if _, err := names.CheckPath(out.path, names.Ext4); err != nil {
		return out, fmt.Errorf("%w", err)
	}
	return out, nil
}

func (e *explainArgs) setSize(text string) error {
	if e.size != nil {
		return errors.New(T("--size comes twice"))
	}
	size, err := value.ParseSize(text)
	if err != nil {
		return fmt.Errorf("--size: %w", err)
	}
	e.size = &size
	return nil
}

// explainOutput is the output of api/schemas/rules-explain.schema.json.
type explainOutput struct {
	Path    string                  `json:"path,omitempty"`
	PathB64 string                  `json:"path_b64,omitempty"`
	Rule    *string                 `json:"rule"`
	Size    *int64                  `json:"size"`
	Policy  map[string]any          `json:"policy"`
	Sources map[string]rules.Source `json:"sources"`
}

func rulesExplain(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseExplain(args)
	if err != nil {
		return usage(stderr, "rules explain: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	policy := c.Policy(parsed.path, parsed.size)
	out := explainOutput{Size: parsed.size, Policy: policyValues(policy), Sources: policy.Sources(c.Pool)}
	if utf8.ValidString(parsed.path) {
		out.Path = parsed.path
	} else {
		out.PathB64 = base64.StdEncoding.EncodeToString([]byte(parsed.path))
	}
	if policy.Rule != nil {
		out.Rule = &policy.Rule.Label
	}
	if parsed.asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = printExplain(stdout, out)
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// policyValues gives a policy the names of the schema. Those of a time say their unit, and
// a file that is ignored has no other property.
func policyValues(p rules.Policy) map[string]any {
	if p.Action == rules.Ignore {
		return map[string]any{"action": string(p.Action)}
	}
	return map[string]any{
		"action":           string(p.Action),
		"replicas":         p.Replicas,
		"mode":             p.Mode,
		"settle_ns":        p.Settle.Nanoseconds(),
		"priority":         p.Priority,
		"tags":             listOrEmpty(p.Tags),
		"tags_mode":        p.TagsMode,
		"domains":          listOrEmpty(p.Domains),
		"spread":           p.Spread,
		"spread_mode":      p.SpreadMode,
		"colocate":         p.Colocate,
		"overflow":         p.Overflow,
		"cache":            p.Cache,
		"cache_min_age_ns": p.CacheMinAge.Nanoseconds(),
	}
}

func listOrEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func printExplain(w io.Writer, out explainOutput) error {
	var text strings.Builder
	if out.Rule != nil {
		fmt.Fprintf(&text, T("rule: %s\n"), *out.Rule)
	} else {
		text.WriteString(T("no rule matches\n"))
	}
	for _, key := range slices.Sorted(maps.Keys(out.Policy)) {
		name, isTime := strings.CutSuffix(key, "_ns")
		shown := out.Policy[key]
		// A time is written as the configuration writes it, not in nanoseconds (specs.md §25).
		if ns, ok := shown.(int64); ok && isTime {
			shown = value.FormatDuration(time.Duration(ns))
		}
		fmt.Fprintf(&text, "%s = %v (%s)\n", name, shown, out.Sources[name])
	}
	return writeText(w, text.String())
}

func writeText(w io.Writer, text string) error {
	if _, err := io.WriteString(w, text); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return nil
}
