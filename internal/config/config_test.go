package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/vectors"
)

type configVector struct {
	TOML      *string     `json:"toml"`
	TOMLB64   *string     `json:"toml_b64"`
	Error     bool        `json:"error"`
	Errors    []string    `json:"errors"`
	Effective []valuePair `json:"effective"`
	Complete  bool        `json:"complete"`
	NotSet    []string    `json:"not_set"`
	Warnings  *[]string   `json:"warnings"`
	Note      string      `json:"note"`
}

type valuePair struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func canonical(t testing.TB, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestConfigVectors(t *testing.T) {
	for i, v := range vectors.Load[configVector](t, "config.json") {
		checkVector(t, i, v)
	}
}

func checkVector(t testing.TB, i int, v configVector) {
	t.Helper()
	text, err := vectors.Bytes("toml", v.TOML, v.TOMLB64)
	if err != nil {
		t.Errorf("vector %d (%s): %v", i, v.Note, err)
		return
	}
	loads := len(v.Effective) > 0 || v.Warnings != nil || v.NotSet != nil
	if v.Error && (loads || v.Errors != nil) || v.Errors != nil && (loads || len(v.Errors) == 0) || !v.Error && v.Errors == nil && !loads {
		t.Errorf("vector %d (%s): want error, errors, or what it loads to", i, v.Note)
		return
	}
	c, err := Parse([]byte(text))
	var e *Error
	if err != nil && !errors.As(err, &e) {
		t.Errorf("vector %d (%s): an error that is not an *Error: %v", i, v.Note, err)
		return
	}
	switch {
	case v.Error:
		if err == nil || len(e.Problems) != 1 || e.Problems[0].Key != "" {
			t.Errorf("vector %d (%s): got %v, want a syntax error", i, v.Note, err)
		}
	case v.Errors != nil:
		if err == nil {
			t.Errorf("vector %d (%s): loaded, want %q", i, v.Note, v.Errors)
			return
		}
		got := make([]string, 0, len(e.Problems))
		for _, p := range e.Problems {
			got = append(got, p.Key)
		}
		if !slices.Equal(got, v.Errors) {
			t.Errorf("vector %d (%s): problems at %q, want %q\n%v", i, v.Note, got, v.Errors, err)
		}
	default:
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			return
		}
		checkEffective(t, i, v, c)
	}
}

func checkEffective(t testing.TB, i int, v configVector, c *Config) {
	t.Helper()
	got := c.Effective()
	listed := map[string]bool{}
	for _, want := range v.Effective {
		listed[want.Key] = true
		value, ok := got[want.Key]
		if !ok {
			t.Errorf("vector %d (%s): no value for %s", i, v.Note, want.Key)
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if a, b := canonical(t, raw), canonical(t, want.Value); a != b {
			t.Errorf("vector %d (%s): %s = %s, want %s", i, v.Note, want.Key, a, b)
		}
	}
	if v.Complete {
		for key := range got {
			if !listed[key] {
				t.Errorf("vector %d (%s): %s = %v, which the case does not list", i, v.Note, key, got[key])
			}
		}
	}
	for _, key := range v.NotSet {
		if value, ok := got[key]; ok {
			t.Errorf("vector %d (%s): %s = %v, want no value", i, v.Note, key, value)
		}
	}
	if v.Warnings != nil {
		var keys []string
		for _, w := range c.Warnings {
			if !w.LaterPhase {
				keys = append(keys, w.Key)
			}
		}
		if !slices.Equal(keys, *v.Warnings) {
			t.Errorf("vector %d (%s): warnings at %q, want %q", i, v.Note, keys, *v.Warnings)
		}
	}
}

// The schema's keywords are the ones schema.go reads, so a keyword added to the schema
// cannot silently check nothing.
func TestSchemaKeywords(t *testing.T) {
	s, err := loadSchema()
	if err != nil {
		t.Fatal(err)
	}
	known := []string{
		"$ref", "additionalProperties", "default", "description", "enum", "format", "items",
		"maximum", "minItems", "minLength", "minimum", "pattern", "properties", "propertyNames",
		"required", "type",
	}
	annotations := []string{"$ref", "default", "description"}
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		_, ref := node["$ref"]
		for key, v := range node {
			if !slices.Contains(known, key) {
				t.Errorf("%s: a keyword schema.go does not read: %s", path, key)
			}
			if ref && !slices.Contains(annotations, key) {
				t.Errorf("%s: %s next to a $ref, which resolve would lose", path, key)
			}
			switch key {
			case "properties":
				children, _ := v.(map[string]any)
				for name, child := range children {
					sub, _ := child.(map[string]any)
					walk(path+"."+name, sub)
				}
			case "items", "propertyNames", "additionalProperties":
				if sub, ok := v.(map[string]any); ok {
					walk(path+"/"+key, sub)
				}
			case "format":
				format, _ := v.(string)
				if err := checkFormat(format, "x"); err != nil && strings.Contains(err.Error(), "unknown format") {
					t.Errorf("%s: %v", path, err)
				}
			case "pattern":
				pattern, _ := v.(string)
				if _, err := regexp.Compile(pattern); err != nil {
					t.Errorf("%s: %v", path, err)
				}
			}
		}
		if ref {
			if target := s.resolve(node); target["type"] == nil && target["enum"] == nil {
				t.Errorf("%s: a $ref that resolves to nothing checkable", path)
			}
		}
		if _, open := node["additionalProperties"]; node["type"] == "object" && !open {
			t.Errorf("%s: a table that does not say what it does with other keys", path)
		}
	}
	root := maps.Clone(s.root)
	for _, top := range []string{"$schema", "$id", "title", "$defs"} {
		delete(root, top)
	}
	walk("schema", root)
	defs, _ := s.root["$defs"].(map[string]any)
	for name, def := range defs {
		sub, _ := def.(map[string]any)
		walk("$defs."+name, sub)
		if _, ok := sub["default"]; ok {
			t.Errorf("$defs.%s: a default that applyDefaults does not read: put it next to the $ref", name)
		}
	}
}

// What followMaintenanceWindow falls back on is the default in the schema.
func TestDefaultWindow(t *testing.T) {
	c, err := Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Effective()["maintenance.window"]; got != defaultWindow {
		t.Errorf("the schema says %v, defaultWindow says %v", got, defaultWindow)
	}
}

// specExample returns the example of specs.md §24.1.
func specExample(t *testing.T) []byte {
	t.Helper()
	spec := vectors.RepoFile(t, "specs.md")
	_, rest, ok := bytes.Cut(spec, []byte("### 24.1 Example"))
	if !ok {
		t.Fatal("specs.md has no §24.1")
	}
	_, rest, ok = bytes.Cut(rest, []byte("```toml\n"))
	if !ok {
		t.Fatal("§24.1 has no TOML block")
	}
	block, _, ok := bytes.Cut(rest, []byte("```\n"))
	if !ok {
		t.Fatal("§24.1 has an unterminated TOML block")
	}
	return block
}

// The example of the spec loads, and is the sample that deploy/ ships.
func TestSpecExample(t *testing.T) {
	example := specExample(t)
	if sample := vectors.RepoFile(t, "deploy/atoll.toml"); !bytes.Equal(example, sample) {
		t.Error("deploy/atoll.toml differs from the example of specs.md §24.1")
	}
	c, err := Parse(example)
	if err != nil {
		t.Fatal(err)
	}
	if c.PoolID != "home-media" || c.Strategy != "energy" || len(c.Disks) != 4 || len(c.Rules) != 10 {
		t.Errorf("got %q, %q, %d disks, %d rules", c.PoolID, c.Strategy, len(c.Disks), len(c.Rules))
	}
	u1 := c.Disks[2]
	if u1.ID != "u1" || u1.FS != "ntfs" || u1.Domain != "d4-320" || u1.Root != "media" || u1.Workdir != "Atoll" || !slices.Equal(u1.Tags, []string{"usb"}) {
		t.Errorf("disk u1: %+v", u1)
	}
	if u1.UUID != "01D9F3A2B4C5E6F7" || u1.Mount != "/mnt/u1" || u1.Role != "data" || c.Disks[3].Role != "cache" {
		t.Errorf("disks: %+v", c.Disks)
	}
}

func TestRuleConditions(t *testing.T) {
	c, err := Parse([]byte(`pool_id = "p"
[view]
mount = "/srv/pool"
[[rule]]
label = "a"
path = "Films/**"
filename = "*.MKV"
ext = ["mkv"]
size_min = "1KiB"
size_max = "2KiB"
[[rule]]
label = "b"
filename = "*.MKV"
ignore_case = false
[[rule]]
label = "c"
`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Rules[0]
	if a.Label != "a" || a.Path == nil || a.Filename == nil || a.Ext == nil || a.SizeMin == nil || a.SizeMax == nil {
		t.Fatalf("rule a: %+v", a)
	}
	if *a.SizeMin != 1024 || *a.SizeMax != 2048 {
		t.Errorf("sizes %d, %d", *a.SizeMin, *a.SizeMax)
	}
	film := rules.NewSubject("Films/x.mkv")
	if !a.Path.Match(film) || !a.Filename.Match(film) || !a.Ext.Match(film) {
		t.Error("rule a should match: ignore_case is true by default")
	}
	if c.Rules[1].Filename.Match(film) {
		t.Error("rule b should not match: ignore_case is false")
	}
	if b := c.Rules[2]; b.Path != nil || b.Filename != nil || b.Ext != nil || b.SizeMin != nil || b.SizeMax != nil {
		t.Errorf("rule c has conditions: %+v", b)
	}
}

// The warnings of this build: every later-phase key of the example, and no default.
func TestLaterPhaseWarnings(t *testing.T) {
	c, err := Parse(specExample(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range c.Warnings {
		if !w.LaterPhase {
			t.Errorf("the example names something no disk carries: %v", w)
		}
		got = append(got, w.Key)
	}
	want := []string{
		"disk[3].role",
		"health.avoid_hotter_than",
		"health.evacuate_on",
		"placement.colocate", // set by the energy preset, unlike new_files, which the file sets
		"read.classes",
		"rule[5].cache_min_age",
		"rule[6].cache_min_age",
		"rule[7].colocate",
	}
	if !slices.Equal(got, want) {
		t.Errorf("warnings at\n%q, want\n%q", got, want)
	}
	for _, w := range c.Warnings {
		if !strings.Contains(w.Message, "phase") {
			t.Errorf("%v does not name the phase", w)
		}
	}
}

func TestFallbackWarnings(t *testing.T) {
	c, err := Parse([]byte(`pool_id = "p"
[view]
mount = "/srv/pool"
[placement]
new_files = "ordered"
[integrity]
auto_repair = false
[ups]
enabled = false
`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range c.Warnings {
		got[w.Key] = w.Message
	}
	if len(got) != 1 || !strings.Contains(got["placement.new_files"], "works as most-free") {
		t.Errorf("warnings: %v", c.Warnings)
	}
}

// Effective promises these types, defaults included, where the vectors compare JSON.
func TestEffectiveTypes(t *testing.T) {
	c, err := Parse(specExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Effective()["replication.max_parallel_per_disk"]; got != int64(1) {
		t.Errorf("max_parallel_per_disk from the file: %T %v", got, got)
	}
	minimal, err := Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, values := range []map[string]any{c.Effective(), minimal.Effective()} {
		for key, v := range values {
			switch x := v.(type) {
			case string, int64, bool:
			case []any:
				for _, item := range x {
					if _, ok := item.(string); !ok {
						t.Errorf("%s: an item of type %T", key, item)
					}
				}
			case map[string]any:
				if len(x) != 0 {
					t.Errorf("%s: a table that is not empty: %v", key, x)
				}
			default:
				t.Errorf("%s: a value of type %T", key, v)
			}
		}
	}
	if got := minimal.Effective()["defaults.replicas"]; got != int64(1) {
		t.Errorf("replicas from the schema: %T %v", got, got)
	}
}

func TestViewEngineAtollWaitsForItsPhase(t *testing.T) {
	_, err := Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\nengine = \"atoll\"\n"))
	var e *Error
	// The message tells it from a value the schema does not know.
	if !errors.As(err, &e) || len(e.Problems) != 1 || e.Problems[0].Key != "view.engine" || !strings.Contains(e.Problems[0].Message, "phase 2a") {
		t.Errorf("got %v, want a problem at view.engine that names phase 2a", err)
	}
}

func TestErrorMessage(t *testing.T) {
	_, err := Parse([]byte("pool_id = 1\nfoo = 2\n[view]\nmount = \"/srv/pool\"\n"))
	if err == nil || !strings.Contains(err.Error(), "pool_id: ") || !strings.Contains(err.Error(), "foo: unknown key") {
		t.Errorf("got %v", err)
	}
	_, err = Parse([]byte("pool_id = \n"))
	if err == nil || !strings.HasPrefix(err.Error(), "configuration: ") {
		t.Errorf("got %v", err)
	}
}

// The media servers are the tables of [notify] whose url is set, in the order Plex, Jellyfin, Emby, each with its
// secret file and its path_map (specs.md §21.5).
func TestMediaServers(t *testing.T) {
	c, err := Parse([]byte(`pool_id = "p"
catalog = "/srv/catalog.db"
[view]
mount = "/srv/pool"
[notify.emby]
url = "http://127.0.0.1:8096"
api_key_file = "/etc/atoll/emby.key"
[notify.jellyfin]
url = ""
api_key_file = "/etc/atoll/jellyfin.key"
[notify.plex]
url = "http://127.0.0.1:32400"
token_file = "/etc/atoll/plex.token"
path_map = { "/srv/pool" = "/data", "/srv/pool/tv" = "/tv" }
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []MediaServer{
		{Kind: Plex, URL: "http://127.0.0.1:32400", SecretFile: "/etc/atoll/plex.token", PathMap: map[string]string{"/srv/pool": "/data", "/srv/pool/tv": "/tv"}},
		{Kind: Emby, URL: "http://127.0.0.1:8096", SecretFile: "/etc/atoll/emby.key", PathMap: map[string]string{}},
	}
	if !reflect.DeepEqual(c.MediaServers, want) {
		t.Errorf("%+v", c.MediaServers)
	}
	for _, w := range c.Warnings {
		t.Errorf("a warning: %v", w)
	}
	if c, err := Parse([]byte("pool_id = \"p\"\ncatalog = \"/srv/catalog.db\"\n[view]\nmount = \"/srv/pool\"\n")); err != nil || len(c.MediaServers) != 0 {
		t.Errorf("none set: %+v, %v", c, err)
	}
}
