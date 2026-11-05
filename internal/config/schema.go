package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/value"
)

// schemaWalker reads the configuration schema: the part of JSON Schema it uses, which
// TestSchemaKeywords keeps to what is written here.
type schemaWalker struct {
	root map[string]any
}

// resolve follows a $ref. Next to a $ref the schema only holds annotations
// (TestSchemaKeywords), so nothing is lost.
func (w *schemaWalker) resolve(s map[string]any) map[string]any {
	ref, ok := s["$ref"].(string)
	if !ok {
		return s
	}
	name, ok := strings.CutPrefix(ref, "#/$defs/")
	if !ok {
		return s
	}
	defs, _ := w.root["$defs"].(map[string]any)
	target, ok := defs[name].(map[string]any)
	if !ok {
		return s
	}
	return w.resolve(target)
}

// check validates a value, and reports at most one problem for it, the first one
// found. A table or list goes on to what it holds.
func (w *schemaWalker) check(key string, s map[string]any, v any, found *problems) {
	s = w.resolve(s)
	if err := checkScalar(s, v); err != nil {
		found.add(key, "%v", err)
		return
	}
	switch x := v.(type) {
	case map[string]any:
		w.checkTable(key, s, x, found)
	case []any:
		w.checkList(key, s, x, found)
	}
}

func (w *schemaWalker) checkTable(key string, s, table map[string]any, found *problems) {
	required, _ := s["required"].([]any)
	for _, v := range required {
		name, _ := v.(string)
		if _, ok := table[name]; !ok {
			found.add(joinKey(key, name), "required")
		}
	}
	properties, _ := s["properties"].(map[string]any)
	names, _ := s["propertyNames"].(map[string]any)
	for name, v := range table {
		child := joinKey(key, name)
		if names != nil {
			if err := checkFormat(stringOf(names, "format"), name); err != nil {
				found.add(child, "%v", err)
				continue
			}
		}
		if sub, ok := properties[name].(map[string]any); ok {
			w.check(child, sub, v, found)
			continue
		}
		extra, open := s["additionalProperties"].(map[string]any)
		if !open {
			found.add(child, "unknown key")
			continue
		}
		w.check(child, extra, v, found)
	}
}

func (w *schemaWalker) checkList(key string, s map[string]any, list []any, found *problems) {
	items, _ := s["items"].(map[string]any)
	before := len(found.byKey)
	for i, v := range list {
		w.check(indexKey(key, i), items, v, found)
	}
	// A format on a list reads all of it, so it needs every item right.
	if len(found.byKey) == before {
		if err := checkFormat(stringOf(s, "format"), list); err != nil {
			found.add(key, "%v", err)
		}
	}
}

func stringOf(s map[string]any, key string) string {
	v, _ := s[key].(string)
	return v
}

// checkScalar checks everything about a value but what it holds. On a list, format
// waits for the items, so checkList applies it.
func checkScalar(s map[string]any, v any) error {
	if want := stringOf(s, "type"); want != "" && typeOf(v) != want {
		return fmt.Errorf("want a %s, not a %s", want, typeOf(v))
	}
	if allowed, ok := s["enum"].([]any); ok && !slices.Contains(allowed, v) {
		return fmt.Errorf("want one of %v", allowed)
	}
	if n, ok := v.(int64); ok {
		if err := checkRange(s, n); err != nil {
			return err
		}
	}
	if list, ok := v.([]any); ok {
		if least, ok := s["minItems"].(json.Number); ok {
			if want, err := least.Int64(); err == nil && int64(len(list)) < want {
				return fmt.Errorf("want at least %d items", want)
			}
		}
		return nil
	}
	text, ok := v.(string)
	if !ok {
		return nil
	}
	if least, ok := s["minLength"].(json.Number); ok {
		if want, err := least.Int64(); err == nil && int64(len([]rune(text))) < want {
			return fmt.Errorf("want at least %d characters", want)
		}
	}
	if pattern := stringOf(s, "pattern"); pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("configuration schema: %w", err)
		}
		if !re.MatchString(text) {
			return fmt.Errorf("%q does not have the right form", text)
		}
	}
	return checkFormat(stringOf(s, "format"), text)
}

func checkRange(s map[string]any, n int64) error {
	if least, ok := s["minimum"].(json.Number); ok {
		if want, err := least.Int64(); err == nil && n < want {
			return fmt.Errorf("%d is below %d", n, want)
		}
	}
	if most, ok := s["maximum"].(json.Number); ok {
		if want, err := most.Int64(); err == nil && n > want {
			return fmt.Errorf("%d is above %d", n, want)
		}
	}
	return nil
}

// typeOf names the JSON type of a value the TOML parser gave. "date" is no type the
// schema allows: no key takes a date or a time.
func typeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case int64:
		return "integer"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "date"
	}
}

// checkFormat applies the grammar that a format starting with atoll- names (specs.md
// §29.5). Any other value of v is a mistake in the schema, which a test catches.
func checkFormat(format string, v any) error {
	if format == "" {
		return nil
	}
	if list, ok := v.([]any); ok && format == "atoll-min-free" {
		values := make([]string, 0, len(list))
		for _, item := range list {
			text, _ := item.(string)
			values = append(values, text)
		}
		_, err := value.ParseMinFree(values)
		return err
	}
	text, ok := v.(string)
	if !ok {
		return fmt.Errorf("configuration schema: format %s on a %s", format, typeOf(v))
	}
	var err error
	switch format {
	case "atoll-duration":
		_, err = value.ParseDuration(text)
	case "atoll-size":
		_, err = value.ParseSize(text)
	case "atoll-percent":
		_, err = value.ParsePercent(text)
	case "atoll-window":
		_, err = value.ParseWindow(text)
	case "atoll-schedule":
		if text != "never" && text != "immediate" {
			_, err = value.ParseWindow(text)
		}
	case "atoll-min-free":
		err = errors.New("want a list")
	case "atoll-absolute-path":
		err = checkAbsolutePath(text)
	case "atoll-optional-path":
		if text != "" {
			err = checkAbsolutePath(text)
		}
	default:
		err = fmt.Errorf("configuration schema: unknown format %s", format)
	}
	return err
}

// checkAbsolutePath applies the rule of specs.md §29.5. The Windows form comes with
// phase 4.
func checkAbsolutePath(path string) error {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return fmt.Errorf("path %q: want an absolute path", path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("path %q: holds a NUL", path)
	}
	for _, c := range strings.Split(rest, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("path %q: an empty, . or .. component", path)
		}
	}
	return nil
}

// applyDefaults sets each default of the schema where the tree has no value, creating
// the tables that takes.
func (w *schemaWalker) applyDefaults(s, table map[string]any) {
	s = w.resolve(s)
	properties, _ := s["properties"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(properties)) {
		raw, _ := properties[name].(map[string]any)
		sub := w.resolve(raw)
		v, present := table[name]
		if !present {
			if d, ok := raw["default"]; ok {
				table[name] = fromJSON(d)
			} else if sub["properties"] != nil {
				child := map[string]any{}
				w.applyDefaults(sub, child)
				table[name] = child
			}
			continue
		}
		switch x := v.(type) {
		case map[string]any:
			w.applyDefaults(sub, x)
		case []any:
			items, _ := sub["items"].(map[string]any)
			for _, item := range x {
				if t, ok := item.(map[string]any); ok && items != nil {
					w.applyDefaults(items, t)
				}
			}
		}
	}
}

// fromJSON turns a default read from the schema into what the TOML parser would give.
func fromJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil { // the vector of every default would fail
			return x.String()
		}
		return n
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, fromJSON(e))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = fromJSON(e)
		}
		return out
	default:
		return v
	}
}
