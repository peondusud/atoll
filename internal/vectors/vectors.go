// Package vectors loads the conformance vectors of conformance/vectors/, and
// the name corpus of conformance/corpus/, for unit tests (specs.md §29.4). It
// decodes strictly: an unknown field, another format_version, a file that is
// not valid UTF-8, a surrogate escape or a flag that is not true fails the test,
// so a typo cannot silently disable a check. The file format is described in
// conformance/vectors/README.md.
package vectors

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// FormatVersion is the only vector format version this loader reads.
const FormatVersion = 1

type file[C any] struct {
	FormatVersion int    `json:"format_version"`
	Spec          string `json:"spec"`
	Description   string `json:"description"`
	Cases         []C    `json:"cases"`
}

// Load reads conformance/vectors/<name> and returns its cases, or fails t.
func Load[C any](t testing.TB, name string) []C {
	t.Helper()
	cases, err := load[C]("vectors", name)
	if err != nil {
		t.Fatalf("vectors %s: %v", name, err)
	}
	return cases
}

// LoadCorpus reads conformance/corpus/<name>, such as "names/names.json", and
// returns its cases, or fails t. The corpus uses the vector format.
func LoadCorpus[C any](t testing.TB, name string) []C {
	t.Helper()
	cases, err := load[C]("corpus", name)
	if err != nil {
		t.Fatalf("corpus %s: %v", name, err)
	}
	return cases
}

func load[C any](folder, name string) ([]C, error) {
	root, err := conformanceDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, folder, name))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return parse[C](data)
}

// parse checks and decodes the content of a vector file.
func parse[C any](data []byte) ([]C, error) {
	if err := checkText(data); err != nil {
		return nil, err
	}
	if err := checkTokens(data); err != nil {
		return nil, err
	}
	if err := checkFlags(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file[C]
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode: trailing data after the top-level object")
	}
	if f.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("format_version %d, want %d", f.FormatVersion, FormatVersion)
	}
	if len(f.Cases) == 0 {
		return nil, errors.New("no cases")
	}
	return f.Cases, nil
}

// checkText rejects a file that is not valid UTF-8 or holds an unpaired
// \uD800–\uDFFF escape: Go's decoder would turn either into U+FFFD without an
// error, while Rust's fails, so the two versions would test different strings.
// A high surrogate escape followed by a low one is a single character.
func checkText(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("not valid UTF-8: write such names in a *_b64 field")
	}
	// surrogate returns the value of the \uXXXX escape at data[i:] when it is
	// a surrogate, or 0.
	surrogate := func(i int) int {
		if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
			return 0
		}
		v, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
		if err != nil || v < 0xD800 || v > 0xDFFF {
			return 0
		}
		return int(v)
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		switch v := surrogate(i); {
		case v == 0:
			i++ // skip the escaped character, so \\ud800 is a backslash then text
		case v <= 0xDBFF && surrogate(i+6) >= 0xDC00:
			i += 11 // a pair: one character
		default:
			return fmt.Errorf("an unpaired surrogate escape at byte %d: write such names in a *_b64 field", i)
		}
	}
	return nil
}

// checkTokens rejects what Go's decoder would accept but Rust's rejects or
// reads differently: a key that is not lowercase ASCII (Go matches Name or
// NAME to name, even through Unicode folding), a key repeated in one object
// (Go keeps the last), and a number that is not a plain integer, such as -0,
// 1.0 or 1e3.
func checkTokens(data []byte) error {
	type frame struct {
		object, wantKey bool
		keys            map[string]bool
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []*frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		top := (*frame)(nil)
		if n := len(stack); n > 0 {
			top = stack[n-1]
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, wantKey: true, keys: map[string]bool{}})
			case '[':
				stack = append(stack, &frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if top != nil && top.object && top.wantKey {
				if !lowercaseKey(t) {
					return fmt.Errorf("key %q: keys are lowercase ASCII", t)
				}
				if top.keys[t] {
					return fmt.Errorf("key %q appears twice in one object", t)
				}
				top.keys[t] = true
				top.wantKey = false
				continue
			}
			valueDone()
		case json.Number:
			if !plainInteger(string(t)) {
				return fmt.Errorf("number %s: write a plain integer, without a fraction, an exponent or -0", t)
			}
			valueDone()
		default:
			valueDone()
		}
	}
}

// checkFlags rejects a case whose "error", "no_key", "absent" or "complete" is anything but true. A
// flag is true or absent, so each case has one spelling. Go's decoder would read
// null as false where Rust's fails, and a test written only to look for the key
// would read "error": false as an error case. Checking here covers every test,
// including those of files still to come.
func checkFlags(data []byte) error {
	var f struct {
		Cases []map[string]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	for i, c := range f.Cases {
		for _, flag := range []string{"error", "no_key", "absent", "complete"} {
			if v, ok := c[flag]; ok && string(v) != "true" {
				return fmt.Errorf("case %d: %q is %s: write true, or leave the field out", i, flag, v)
			}
		}
	}
	return nil
}

func lowercaseKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		lower, digit := 'a' <= c && c <= 'z', '0' <= c && c <= '9'
		if !lower && !digit && c != '_' {
			return false
		}
	}
	return true
}

func plainInteger(n string) bool {
	digits := strings.TrimPrefix(n, "-")
	if digits == "" || digits == "0" && n != "0" || len(digits) > 1 && digits[0] == '0' {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}

// RepoFile reads a file of the repository, by its path from the root with /
// separators, or fails t. The pure core has no os, so its tests read through here.
func RepoFile(t testing.TB, name string) []byte {
	t.Helper()
	root, err := conformanceDir()
	if err != nil {
		t.Fatalf("repository file %s: %v", name, err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(root), filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("repository file %s: %v", name, err)
	}
	return data
}

// conformanceDir finds conformance/ from the working directory of the test,
// which is the folder of the package under test.
func conformanceDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "conformance"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Path returns the raw bytes of a case path given as path or path_b64
// (specs.md §29.3). Exactly one of the two must be set; a path given as
// path_b64 must not be valid UTF-8, so each path has a single spelling.
func Path(path, pathB64 *string) (string, error) {
	return text("path", path, pathB64)
}

// Name returns the raw bytes of a corpus name given as name or name_b64, with
// the rules of Path.
func Name(name, nameB64 *string) (string, error) {
	return text("name", name, nameB64)
}

// Bytes returns the raw bytes of any other text a case gives as <field> or
// <field>_b64, with the rules of Path.
func Bytes(field string, plain, b64 *string) (string, error) {
	return text(field, plain, b64)
}

func text(field string, plain, b64 *string) (string, error) {
	switch {
	case plain != nil && b64 != nil:
		return "", fmt.Errorf("both %s and %s_b64", field, field)
	case plain != nil:
		return *plain, nil
	case b64 != nil:
		b, err := base64.StdEncoding.Strict().DecodeString(*b64)
		if err != nil {
			return "", fmt.Errorf("%s_b64: %w", field, err)
		}
		// Go's decoder skips line breaks even in strict mode; Rust's refuses
		// them. Only the standard encoder's own spelling is accepted.
		if base64.StdEncoding.EncodeToString(b) != *b64 {
			return "", fmt.Errorf("%s_b64 %q: write it as the standard encoder does", field, *b64)
		}
		if utf8.Valid(b) {
			return "", fmt.Errorf("%s_b64 %q is valid UTF-8: use %s", field, *b64, field)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("neither %s nor %s_b64", field, field)
	}
}
