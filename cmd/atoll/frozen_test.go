package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/ops"
)

// The contracts at format version 1 (specs.md §29.3), as conformance/frozen/v1.json pins them: what the migrations
// were, each key of the configuration, each field of each --json output, each path of the local API, the exit codes,
// and the names of the on-disk layout. ATOLL_FREEZE=1 writes it from what is there, which was done once, on
// 2026-11-16, and is never to be done again for version 1.
const frozenFile = "../../conformance/frozen/v1.json"

// shape is what a contract says of one place of a JSON document: the types it may hold, the values of a closed list,
// its format, and whether it is always there.
type shape struct {
	Types    []string `json:"types"`
	Enum     []string `json:"enum,omitempty"`
	Format   string   `json:"format,omitempty"`
	Required bool     `json:"required,omitempty"`
}

type frozen struct {
	FormatVersion int                         `json:"format_version"`
	Frozen        string                      `json:"frozen"`
	Migrations    map[string]string           `json:"migrations"`
	Configuration map[string]shape            `json:"configuration"`
	Outputs       map[string]map[string]shape `json:"outputs"`
	API           []string                    `json:"api"`
	ExitCodes     map[string]int              `json:"exit_codes"`
	Layout        map[string]string           `json:"layout"`
}

// shapesOf gives the shape of each place a JSON schema describes: an object's properties by name, joined by a dot, and
// what an array holds as [].
func shapesOf(doc map[string]any) map[string]shape {
	defs, _ := doc["$defs"].(map[string]any)
	out := map[string]shape{}
	var walk func(path string, node map[string]any, required bool, depth int)
	walk = func(path string, node map[string]any, required bool, depth int) {
		if depth > 32 {
			return
		}
		if ref, ok := node["$ref"].(string); ok {
			if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any); ok {
				merged := map[string]any{}
				for k, v := range def {
					merged[k] = v
				}
				for k, v := range node {
					if k != "$ref" {
						merged[k] = v
					}
				}
				node = merged
			}
		}
		s := out[path]
		s.Required = s.Required || required
		switch t := node["type"].(type) {
		case string:
			s.Types = appendNew(s.Types, t)
		case []any:
			for _, x := range t {
				if name, ok := x.(string); ok {
					s.Types = appendNew(s.Types, name)
				}
			}
		}
		if f, ok := node["format"].(string); ok {
			s.Format = f
		}
		if enum, ok := node["enum"].([]any); ok {
			for _, v := range enum {
				text, _ := json.Marshal(v)
				s.Enum = appendNew(s.Enum, string(text))
			}
		}
		if c, ok := node["const"]; ok {
			text, _ := json.Marshal(c)
			s.Enum = appendNew(s.Enum, string(text))
		}
		out[path] = s
		for _, key := range []string{"anyOf", "oneOf", "allOf"} {
			if alts, ok := node[key].([]any); ok {
				for _, alt := range alts {
					if m, ok := alt.(map[string]any); ok {
						walk(path, m, false, depth+1)
					}
				}
			}
		}
		req := map[string]bool{}
		if list, ok := node["required"].([]any); ok {
			for _, r := range list {
				if name, ok := r.(string); ok {
					req[name] = true
				}
			}
		}
		if props, ok := node["properties"].(map[string]any); ok {
			for name, p := range props {
				if m, ok := p.(map[string]any); ok {
					walk(join(path, name), m, req[name], depth+1)
				}
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(path+"[]", items, false, depth+1)
		}
	}
	walk("", doc, true, 0)
	for k, s := range out {
		sort.Strings(s.Types)
		sort.Strings(s.Enum)
		out[k] = s
	}
	return out
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func appendNew(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

func readSchema(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// apiOf gives each method and path of the local API, as openapi.yaml writes them: a path at two spaces under paths,
// each method at four below it.
func apiOf(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pathLine := regexp.MustCompile(`^  (/[^:]*):\s*$`)
	methodLine := regexp.MustCompile(`^    (get|put|post|delete|patch):`)
	var out []string
	in, current := false, ""
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "paths:":
			in = true
		case in && line != "" && !strings.HasPrefix(line, " "):
			in = false
		case in && pathLine.MatchString(line):
			current = pathLine.FindStringSubmatch(line)[1]
		case in && current != "" && methodLine.MatchString(line):
			out = append(out, strings.ToUpper(methodLine.FindStringSubmatch(line)[1])+" "+current)
		}
	}
	sort.Strings(out)
	return out
}

// current reads every contract as it is now.
func current(t *testing.T) *frozen {
	t.Helper()
	f := &frozen{FormatVersion: 1, Frozen: "2026-11-16", Migrations: map[string]string{}, Outputs: map[string]map[string]shape{}}
	migrations, err := filepath.Glob("../../schema/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		f.Migrations[filepath.Base(m)] = hex.EncodeToString(sum[:])
	}
	f.Configuration = shapesOf(readSchema(t, "../../schema/config.schema.json"))
	outputs, err := filepath.Glob("../../api/schemas/*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outputs {
		f.Outputs[filepath.Base(o)] = shapesOf(readSchema(t, o))
	}
	f.API = apiOf(t)
	f.ExitCodes = map[string]int{"success": exitOK, "error": exitError, "usage": exitUsage, "confirm": exitConfirm, "partial": exitPartial}
	at := int64(1_763_251_200_000_000_000) // 2025-11-16T00:00:00Z
	trash, err := layout.TrashPath(at, 7, "Films/a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := layout.ChunksPath(at, 7, "Films/a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	link, err := layout.LinkName(5)
	if err != nil {
		t.Fatal(err)
	}
	f.Layout = map[string]string{
		"marker": layout.MarkerName, "marker_format": fmt.Sprint(layout.MarkerFormat), "trash": layout.TrashFolder, "tmp": layout.TmpFolder,
		"links": layout.LinksFolder, "catalog": ops.CatalogFolder, "trash_path": trash, "chunks_path": chunks,
		"part": layout.PartName(7), "chunk_part": layout.ChunkPartName(7, 3), "link": link,
	}
	return f
}

// TestContractsKeepFormatVersion1: what conformance/frozen/v1.json pins of version 1 still holds (specs.md §29.3).
func TestContractsKeepFormatVersion1(t *testing.T) {
	now := current(t)
	if os.Getenv("ATOLL_FREEZE") == "1" {
		data, err := json.MarshalIndent(now, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(frozenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(frozenFile, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("written")
	}
	data, err := os.ReadFile(frozenFile)
	if err != nil {
		t.Fatal(err)
	}
	var was frozen
	if err := json.Unmarshal(data, &was); err != nil {
		t.Fatal(err)
	}
	for name, sum := range was.Migrations {
		if now.Migrations[name] != sum {
			t.Errorf("migration %s was edited, or went: a frozen migration is never edited (specs.md §29.3)", name)
		}
	}
	keeps(t, "configuration", was.Configuration, now.Configuration, true)
	for name, places := range was.Outputs {
		keeps(t, name, places, now.Outputs[name], false)
	}
	for _, route := range was.API {
		if !slices.Contains(now.API, route) {
			t.Errorf("the local API no longer has %s", route)
		}
	}
	for name, code := range was.ExitCodes {
		if now.ExitCodes[name] != code {
			t.Errorf("exit code %s is %d, was %d", name, now.ExitCodes[name], code)
		}
	}
	for name, v := range was.Layout {
		if now.Layout[name] != v {
			t.Errorf("layout %s is %q, was %q", name, now.Layout[name], v)
		}
	}
}

// keeps says what a place of version 1 must still be: there; for an input, the configuration, taking at least what it
// took, and not required where it was not; for an output, of the same types, and always there where it was. A closed
// list may gain values, never lose one, and a format stays.
func keeps(t *testing.T, doc string, was, now map[string]shape, input bool) {
	t.Helper()
	for place, w := range was {
		n, ok := now[place]
		switch {
		case !ok:
			t.Errorf("%s: %q went", doc, place)
			continue
		case input && !subset(w.Types, n.Types), !input && !slices.Equal(w.Types, n.Types):
			t.Errorf("%s: %q holds %v, held %v", doc, place, n.Types, w.Types)
		case len(w.Enum) > 0 && len(n.Enum) > 0 && !subset(w.Enum, n.Enum):
			t.Errorf("%s: %q lost values: %v, had %v", doc, place, n.Enum, w.Enum)
		case w.Format != n.Format:
			t.Errorf("%s: %q has format %q, had %q", doc, place, n.Format, w.Format)
		case input && !w.Required && n.Required:
			t.Errorf("%s: %q is required now", doc, place)
		case !input && w.Required && !n.Required:
			t.Errorf("%s: %q is no longer always there", doc, place)
		}
	}
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}
