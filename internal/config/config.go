package config

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

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

// SecretFile is a file of a secret the configuration names: its key, and its path.
type SecretFile struct {
	Key, Path string
}

// SecretFiles gives the files of a secret the configuration names, by key in byte order (specs.md §24.4): the token or
// key of each media server, the tokens and the password of the alert channels, the local API's token and the OTLP
// headers file. A key left empty names none.
func (c *Config) SecretFiles() []SecretFile {
	var out []SecretFile
	for _, key := range []string{
		"alerts.gotify_token_file", "alerts.ntfy_token_file", "alerts.smtp_password_file", "api.token_file",
		"notify.emby.api_key_file", "notify.jellyfin.api_key_file", "notify.plex.token_file", "telemetry.otlp_headers_file",
	} {
		if path, _ := c.effective[key].(string); path != "" {
			out = append(out, SecretFile{Key: key, Path: path})
		}
	}
	return out
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

// Read returns [read]: whether read.policy is "prefer-spinning" rather than "oldest", and read.classes (specs.md §7).
func (c *Config) Read() (preferSpinning bool, classes []string) {
	policy, _ := c.effective["read.policy"].(string)
	raw, _ := c.effective["read.classes"].([]any)
	for _, v := range raw {
		if text, ok := v.(string); ok {
			classes = append(classes, text)
		}
	}
	return policy == "prefer-spinning", classes
}

// The values of walk.long_paths (specs.md §15.8, §24).
const (
	// WalkSkip: a walk neither records nor goes below a name whose path through its disk's mount is past the limit
	// of a mergerfs branch.
	WalkSkip = "skip"
	// WalkAll: a walk goes below every name, holding at most walk.max_open folders open at once.
	WalkAll = "walk"
)

// Walk returns [walk]: whether a walk goes past the limit of a mergerfs branch, walk.long_paths = "walk", and how
// many folders it then holds open at most, walk.max_open (specs.md §15.8).
func (c *Config) Walk() (all bool, maxOpen int) {
	long, _ := c.effective["walk.long_paths"].(string)
	open, _ := c.effective["walk.max_open"].(int64)
	return long == WalkAll, int(open)
}

// Cache is what the SSD cache's settings say (specs.md §19).
type Cache struct {
	// InView says that view.engine is "atoll", whose view presents the cache disks and lands new files there: with
	// mergerfs the cache does nothing, and its disks count as any other.
	InView bool
	// Enabled is cache.enabled: new files land on the cache, and stay there their residency.
	Enabled bool
	// MaxFill is cache.max_fill: above it, the oldest files of a cache disk leave at once.
	MaxFill value.Percent
	// Mirror and Early are cache.mirror and cache.replicate_early.
	Mirror, Early bool
}

// Cache returns the SSD cache's settings (specs.md §19). A file's cache and cache_min_age are its policy's.
func (c *Config) Cache() (Cache, error) {
	engine, _ := c.effective["view.engine"].(string)
	out := Cache{InView: engine == "atoll"}
	out.Enabled, _ = c.effective["cache.enabled"].(bool)
	out.Mirror, _ = c.effective["cache.mirror"].(bool)
	out.Early, _ = c.effective["cache.replicate_early"].(bool)
	text, _ := c.effective["cache.max_fill"].(string)
	maxFill, err := value.ParsePercent(text)
	if err != nil {
		return Cache{}, fmt.Errorf("cache.max_fill: %w", err)
	}
	out.MaxFill = maxFill
	return out, nil
}

// The formats a process may write its log in (specs.md §23.2).
const (
	LogJSON = "json"
	LogText = "text"
)

// Telemetry is what [telemetry] says (specs.md §23.2): how each process writes its log, and what goes to an OTLP/HTTP
// collector.
type Telemetry struct {
	// LogFormat is LogJSON or LogText.
	LogFormat string
	// Endpoint is otlp_endpoint, empty for none; Metrics and Logs say which signals go there.
	Endpoint      string
	Metrics, Logs bool
	// Interval is how often the metrics go, and Timeout what a request may take.
	Interval, Timeout time.Duration
	// HeadersFile holds the headers each request carries, empty for none.
	HeadersFile string
}

// Telemetry returns [telemetry] (specs.md §23.2).
func (c *Config) Telemetry() (Telemetry, error) {
	e := c.effective
	t := Telemetry{}
	t.LogFormat, _ = e["telemetry.log_format"].(string)
	t.Endpoint, _ = e["telemetry.otlp_endpoint"].(string)
	t.Metrics, _ = e["telemetry.otlp_metrics"].(bool)
	t.Logs, _ = e["telemetry.otlp_logs"].(bool)
	t.HeadersFile, _ = e["telemetry.otlp_headers_file"].(string)
	var err error
	interval, _ := e["telemetry.otlp_interval"].(string)
	if t.Interval, err = value.ParseDuration(interval); err != nil {
		return Telemetry{}, fmt.Errorf("telemetry.otlp_interval: %w", err)
	}
	timeout, _ := e["telemetry.otlp_timeout"].(string)
	if t.Timeout, err = value.ParseDuration(timeout); err != nil {
		return Telemetry{}, fmt.Errorf("telemetry.otlp_timeout: %w", err)
	}
	return t, nil
}

// Schedule returns replication.window, maintenance.window when it is left out, and the time zone both are read in,
// timezone, local when left out (specs.md §12.3): where the copies of a file in scheduled mode run, and when files
// leave the SSD cache (§19).
func (c *Config) Schedule() (value.Window, *time.Location, error) {
	text, _ := c.effective["replication.window"].(string)
	w, err := value.ParseWindow(text)
	if err != nil {
		return value.Window{}, nil, fmt.Errorf("replication.window: %w", err)
	}
	location := time.Local
	if zone, _ := c.effective["timezone"].(string); zone != "" && zone != "local" {
		if location, err = time.LoadLocation(zone); err != nil {
			return value.Window{}, nil, fmt.Errorf("timezone: %w", err)
		}
	}
	return w, location, nil
}

// CopyLimits is how many copies may read or write a disk at once (specs.md §14): PerDisk, replication.max_parallel_per_disk,
// for every disk alike, 0 when it is left out; otherwise HDD for a disk with a spindle or whose kind is unknown, and SSD
// for one with none, replication.max_parallel_per_hdd and max_parallel_per_ssd.
type CopyLimits struct {
	PerDisk, HDD, SSD int
}

// CopyLimits returns the keys of [replication] that bound the copies at once (specs.md §14).
func (c *Config) CopyLimits() CopyLimits {
	perDisk, _ := c.effective["replication.max_parallel_per_disk"].(int64)
	hdd, _ := c.effective["replication.max_parallel_per_hdd"].(int64)
	ssd, _ := c.effective["replication.max_parallel_per_ssd"].(int64)
	return CopyLimits{PerDisk: int(perDisk), HDD: int(max(hdd, 1)), SSD: int(max(ssd, 1))}
}

// Policy returns the effective policy of a file (specs.md §11.3). A nil size means it
// is not known yet.
func (c *Config) Policy(path string, size *int64) rules.Policy {
	return rules.Effective(c.Rules, c.Pool, path, size)
}

// StrictPlacement says whether the pool or a rule sets what can leave a copy misplaced (specs.md §13.3): tags_mode or
// spread_mode "strict", or overflow "deny". Without any, no copy ever is, and nothing need plan the pool to say so.
func (c *Config) StrictPlacement() bool {
	strict := func(tagsMode, spreadMode, overflow string) bool {
		return tagsMode == "strict" || spreadMode == "strict" || overflow == "deny"
	}
	if strict(c.Pool.TagsMode, c.Pool.SpreadMode, c.Pool.Overflow) {
		return true
	}
	for i := range c.Rules {
		set := &c.Rules[i].Set
		if strict(deref(set.TagsMode), deref(set.SpreadMode), deref(set.Overflow)) {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
	c.Warnings = slices.Concat(unknownLabels(tree), noEffect(tree, set), unreadableUPS(tree), inClear(tree), laterPhaseWarnings(set))
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
