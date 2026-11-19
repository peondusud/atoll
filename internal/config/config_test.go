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
		"required", "type", "writeOnly",
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

// The warnings of this build: every later-phase key of the example, and no default. Its cache disk, with mergerfs, has
// no effect, a warning of no later phase (specs.md §19, §24.4).
func TestLaterPhaseWarnings(t *testing.T) {
	c, err := Parse(specExample(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range c.Warnings {
		got = append(got, w.Key)
		switch {
		case w.Key == "disk[3].role":
			if w.LaterPhase || !strings.Contains(w.Message, "mergerfs") {
				t.Errorf("%v", w)
			}
		case !w.LaterPhase:
			t.Errorf("the example names something no disk carries: %v", w)
		case !strings.Contains(w.Message, "phase"):
			t.Errorf("%v does not name the phase", w)
		}
	}
	want := []string{
		"disk[3].role",
		"health.avoid_hotter_than",
		"health.evacuate_on",
	}
	if !slices.Equal(got, want) {
		t.Errorf("warnings at\n%q, want\n%q", got, want)
	}
}

// read.policy and read.classes came with phase 2b, which knows which disk spins. read.classes still warns, as a
// setting of no effect and of no later phase, where the file sets it and read.policy is "oldest", which reads the
// original alone (specs.md §7, §24.4).
func TestReadClassesWarnWithOldest(t *testing.T) {
	for _, tc := range []struct {
		name, read string
		warn       bool
	}{
		{"classes with oldest, the default", `classes = ["sata"]` + "\n", true},
		{"an empty list, as the file wrote it", "classes = []\n", true},
		{"classes with oldest said", `policy = "oldest"` + "\n" + `classes = ["sata"]` + "\n", true},
		{"classes with prefer-spinning", `policy = "prefer-spinning"` + "\n" + `classes = ["sata"]` + "\n", false},
		{"prefer-spinning alone", `policy = "prefer-spinning"` + "\n", false},
	} {
		c, err := Parse([]byte(`pool_id = "p"
[view]
mount = "/srv/pool"
[[disk]]
id = "t1"
uuid = "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"
mount = "/mnt/t1"
fs = "ext4"
tags = ["sata"]
[read]
` + tc.read))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []Problem
		for _, w := range c.Warnings {
			if w.Key == "read.classes" || w.Key == "read.policy" {
				got = append(got, w)
			}
		}
		switch {
		case !tc.warn && len(got) != 0:
			t.Errorf("%s: %v", tc.name, got)
		case tc.warn && (len(got) != 1 || got[0].Key != "read.classes" || got[0].LaterPhase || !strings.Contains(got[0].Message, "oldest")):
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// placement.new_files and colocate came with phase 2b: neither warns any more (specs.md §12.4, §24.4).
func TestStrategiesNoLongerWarn(t *testing.T) {
	c, err := Parse([]byte(`pool_id = "p"
[view]
mount = "/srv/pool"
[placement]
new_files = "ordered"
colocate = "season"
[integrity]
auto_repair = false
[ups]
enabled = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings: %v", c.Warnings)
	}
}

// Effective promises these types, defaults included, where the vectors compare JSON.
func TestEffectiveTypes(t *testing.T) {
	c, err := Parse(specExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Effective()["replication.max_parallel_per_ssd"]; got != int64(1) {
		t.Errorf("max_parallel_per_ssd from the file: %T %v", got, got)
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

// view.engine = "atoll" is Atoll's own view at view.mount, in place of mergerfs, since phase 2a (specs.md §21.2, §24.4).
func TestViewEngineAtoll(t *testing.T) {
	c, err := Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\nengine = \"atoll\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if engine, _ := c.Effective()["view.engine"].(string); engine != "atoll" {
		t.Errorf("view.engine %q", engine)
	}
}

// The copies at once a disk takes (specs.md §14, the maintainer, 2026-11-19): max_parallel_per_disk, set, for every disk
// alike, as in format version 1; left out, the per-kind keys, one copy at a time on every disk by default, as copies at once are
// experimental (specs.md §28).
func TestCopyLimits(t *testing.T) {
	for text, want := range map[string]CopyLimits{
		"": {PerDisk: 0, HDD: 1, SSD: 1},
		"[replication]\nmax_parallel_per_disk = 1\n":                {PerDisk: 1, HDD: 1, SSD: 1},
		"[replication]\nmax_parallel_per_disk = 3\n":                {PerDisk: 3, HDD: 1, SSD: 1},
		"[replication]\nmax_parallel_per_hdd = 2\n":                 {PerDisk: 0, HDD: 2, SSD: 1},
		"[replication]\nmax_parallel_per_ssd = 16\n":                {PerDisk: 0, HDD: 1, SSD: 16},
		"[replication]\nmax_parallel_per_ssd = 1\nenabled = true\n": {PerDisk: 0, HDD: 1, SSD: 1},
	} {
		c, err := Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n" + text))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.CopyLimits(); got != want {
			t.Errorf("%q: %+v, want %+v", text, got, want)
		}
	}
}

// A point given to atoll mount on its command line follows the relations of view.readonly_mount (specs.md §21.2,
// §24.3), which the vectors pin for the key itself.
func TestCheckReadonlyMount(t *testing.T) {
	c, err := Parse([]byte("pool_id = \"p\"\ncatalog = \"/var/lib/atoll/catalog.db\"\n[view]\nmount = \"/srv/pool\"\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = \"/mnt/t1\"\nfs = \"ext4\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for point, key := range map[string]string{
		"/srv/pool-ro": "", "/srv/pool": "view.readonly_mount", "/srv": "view.readonly_mount", "/srv/pool/ro": "view.readonly_mount",
		"/mnt/t1/x": "view.readonly_mount", "/mnt": "view.readonly_mount", "srv/ro": "view.readonly_mount", "/srv/ro/": "view.readonly_mount",
		"/srv/./ro": "view.readonly_mount", "/var/lib/atoll": "catalog",
	} {
		err := CheckReadonlyMount(c, point)
		var e *Error
		switch {
		case key == "" && err != nil:
			t.Errorf("%s: %v", point, err)
		case key != "" && (!errors.As(err, &e) || len(e.Problems) != 1 || e.Problems[0].Key != key):
			t.Errorf("%s: got %v, want a problem at %s", point, err, key)
		}
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

// A value that may hold a secret is never written in an error: the URL of a ntfy topic, or one typed with a user and
// a password, reached the log with the message that refused it (specs.md §23, §24.3). A value that holds none is
// still shown, as that is what tells a typo.
func TestAnErrorShowsNoURL(t *testing.T) {
	const head = "pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n"
	for _, text := range []string{
		head + "[alerts]\nntfy = \"htps://ntfy.sh/s3cret-topic\"\n",
		head + "[alerts]\nntfy = \"https://user:s3cret@ntfy.example/t opic\"\n",
		head + "[alerts]\ngotify = \"ftp://s3cret@host\"\n",
	} {
		_, err := Parse([]byte(text))
		if err == nil {
			t.Fatalf("loaded: %q", text)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error shows the value: %v", err)
		}
		if !strings.Contains(err.Error(), "alerts.") {
			t.Errorf("the error does not name the key: %v", err)
		}
	}
	_, err := Parse([]byte("pool_id = \"Not An Id\"\n[view]\nmount = \"/srv/pool\"\n"))
	if err == nil || !strings.Contains(err.Error(), "Not An Id") {
		t.Errorf("a value that holds no secret is not shown: %v", err)
	}
}
