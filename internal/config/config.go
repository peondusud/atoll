package config

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/schema"
)

// Problem is one thing wrong with a configuration, or one warning about it.
type Problem struct {
	// Key names the key, such as "disk[0].uuid" (specs.md §24.3). A syntax error
	// has none.
	Key string
	// Message explains it. Its wording is not part of the contract.
	Message string
	// LaterPhase marks a warning about a feature that does not exist yet (specs.md
	// §24.4). The list of these shrinks with each phase, so vectors never pin it.
	LaterPhase bool
}

// Error is a configuration that did not load: every problem of the step that stopped
// it, at most one per key, sorted by key in byte order.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	lines := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		if p.Key == "" {
			lines = append(lines, p.Message)
		} else {
			lines = append(lines, p.Key+": "+p.Message)
		}
	}
	return "configuration: " + strings.Join(lines, "; ")
}

// The states a disk's configuration may set (specs.md §18). Left out, the disk is in service.
const (
	DiskDraining = "draining"
	DiskFailed   = "failed"
)

// Disk is one [[disk]], with root and workdir resolved (specs.md §24.2).
type Disk struct {
	ID      string
	UUID    string
	Mount   string
	FS      names.Filesystem
	Role    string
	Domain  string // empty when the disk forms its own domain
	Tags    []string
	Root    string
	Workdir string
	// State is DiskDraining, DiskFailed, or "" for a disk in service.
	State string
}

// Config is a configuration that loaded. Typed fields appear as the code that reads
// them does; Effective gives every value in the meantime.
type Config struct {
	PoolID   string
	Strategy string // empty without a preset
	Disks    []Disk
	Rules    []rules.Rule
	Pool     rules.Pool
	// Warnings are sorted by key in byte order (specs.md §24.4).
	Warnings []Problem
	// MediaServers are the tables of [notify] whose url is set, Plex, then Jellyfin, then Emby (specs.md §21.5).
	MediaServers []MediaServer

	effective map[string]any
}

// The media servers [notify] can tell of a change (specs.md §21.5).
const (
	Plex     = "plex"
	Jellyfin = "jellyfin"
	Emby     = "emby"
)

// MediaServer is one table of [notify] whose url is set.
type MediaServer struct {
	// Kind is Plex, Jellyfin or Emby.
	Kind string
	URL  string
	// SecretFile is Plex's token_file, or the api_key_file of the others: empty when not set.
	SecretFile string
	// PathMap is path_map: a host path, to the path the server sees there.
	PathMap map[string]string
}

// Effective returns every value of the configuration after the rules of specs.md
// §24.2, by key: a string, an int64, a bool, a []any of these or an empty map. A key
// with no value is not there. The caller must not change it.
func (c *Config) Effective() map[string]any {
	return c.effective
}

// MinFree returns placement.min_free: what to leave free on each disk (specs.md §13.1).
func (c *Config) MinFree() (value.MinFree, error) {
	raw, _ := c.effective["placement.min_free"].([]any)
	list := make([]string, 0, len(raw))
	for _, v := range raw {
		text, _ := v.(string)
		list = append(list, text)
	}
	minFree, err := value.ParseMinFree(list)
	if err != nil {
		return value.MinFree{}, fmt.Errorf("placement.min_free: %w", err)
	}
	return minFree, nil
}

// Policy returns the effective policy of a file (specs.md §11.3). A nil size means it
// is not known yet.
func (c *Config) Policy(path string, size *int64) rules.Policy {
	return rules.Effective(c.Rules, c.Pool, path, size)
}

// Parse loads a configuration from its text, in the three steps of specs.md §24.3.
// The error is an *Error. A timezone is only checked for its form: which names exist
// depends on the machine.
func Parse(data []byte) (*Config, error) {
	// go-toml refuses a byte order mark, where Rust's toml ignores one.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return nil, &Error{Problems: []Problem{{Message: err.Error()}}}
	}

	s, err := loadSchema()
	if err != nil {
		return nil, err
	}
	var found problems
	s.check("", s.root, tree, &found)
	if err := found.err(); err != nil {
		return nil, err
	}

	// What the file sets, then what the file or the preset sets: rules explain tells the
	// two apart, and the warnings about later phases read the second.
	user := flatten(tree)
	applyPreset(tree)
	set := flatten(tree)
	followMaintenanceWindow(tree)
	s.applyDefaults(s.root, tree)

	c := &Config{}
	c.Pool = buildPool(tree, user, set)
	checkRelations(tree, user, c, &found)
	found.addAll(laterPhaseErrors(tree))
	if err := found.err(); err != nil {
		return nil, err
	}
	c.effective = flatten(tree)
	c.PoolID, c.Strategy = text(tree, "pool_id"), text(tree, "strategy")
	c.MediaServers = mediaServers(tree)
	c.Warnings = append(unknownLabels(tree), laterPhaseWarnings(set)...)
	sortProblems(c.Warnings)
	return c, nil
}

// mediaServers reads the tables of [notify] whose url is set, in the order of specs.md §21.5.
func mediaServers(tree map[string]any) []MediaServer {
	notify, _ := tree["notify"].(map[string]any)
	var out []MediaServer
	for _, kind := range []string{Plex, Jellyfin, Emby} {
		t, _ := notify[kind].(map[string]any)
		url := text(t, "url")
		if url == "" {
			continue
		}
		secret := "api_key_file"
		if kind == Plex {
			secret = "token_file"
		}
		m := MediaServer{Kind: kind, URL: url, SecretFile: text(t, secret), PathMap: map[string]string{}}
		paths, _ := t["path_map"].(map[string]any)
		for host, seen := range paths {
			m.PathMap[host], _ = seen.(string)
		}
		out = append(out, m)
	}
	return out
}

// problems collects at most one problem per key.
type problems struct {
	byKey map[string]string
}

func (p *problems) add(key, format string, args ...any) {
	if p.byKey == nil {
		p.byKey = map[string]string{}
	}
	if _, ok := p.byKey[key]; !ok {
		p.byKey[key] = fmt.Sprintf(format, args...)
	}
}

func (p *problems) addAll(list []Problem) {
	for _, x := range list {
		p.add(x.Key, "%s", x.Message)
	}
}

// err returns the problems found, sorted, or nil.
func (p *problems) err() error {
	if len(p.byKey) == 0 {
		return nil
	}
	list := make([]Problem, 0, len(p.byKey))
	for _, key := range slices.Sorted(maps.Keys(p.byKey)) {
		list = append(list, Problem{Key: key, Message: p.byKey[key]})
	}
	return &Error{Problems: list}
}

func sortProblems(list []Problem) {
	slices.SortStableFunc(list, func(a, b Problem) int { return cmp.Compare(a.Key, b.Key) })
}

// flatten returns the leaves of a tree, by key: a table's values, and each item of a
// list of tables. An empty table or list, and a list of plain values, is a leaf.
func flatten(tree map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if len(x) == 0 {
				out[key] = x
			}
			for k, e := range x {
				walk(joinKey(key, k), e)
			}
		case []any:
			if len(x) > 0 {
				if _, tables := x[0].(map[string]any); tables {
					for i, e := range x {
						walk(indexKey(key, i), e)
					}
					return
				}
			}
			out[key] = x
		default:
			out[key] = v
		}
	}
	for k, v := range tree {
		walk(joinKey("", k), v)
	}
	return out
}

// joinKey adds a part to a key, written as specs.md §24.3 says.
func joinKey(parent, part string) string {
	if !bareKey(part) {
		part = quoteKey(part)
	}
	if parent == "" {
		return part
	}
	return parent + "." + part
}

func indexKey(parent string, i int) string {
	return fmt.Sprintf("%s[%d]", parent, i)
}

func bareKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		digit := '0' <= c && c <= '9'
		if !letter && !digit && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// quoteKey writes a key part in double quotes. It is written by hand because the
// quoting of Go and Rust differ, and the two versions must report the same key.
func quoteKey(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// loadSchema reads the embedded schema, keeping integers exact.
func loadSchema() (*schemaWalker, error) {
	dec := json.NewDecoder(bytes.NewReader(schema.Config()))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("configuration schema: %w", err)
	}
	return &schemaWalker{root: root}, nil
}
