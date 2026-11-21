package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzEncodePath: a path is bytes, which anyone who names a file on a disk chooses (specs.md §29.3). Whatever they
// are, a --json output gives it as path when it is UTF-8 and as path_b64 otherwise, never both, and a script that
// reads either gets the same bytes back; and a line of human output shows it as it is or in base64, with no control
// character and nothing that is not UTF-8, and its bytes can be told from what it shows.
func FuzzEncodePath(f *testing.F) {
	for _, p := range []string{"Films/Amélie (2001).mkv", "a\xffb", "a\nb\x1b[2J", " ", "(base64) YQ==", "\xed\xa0\x80", ""} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		type item struct {
			Path    string `json:"path,omitempty"`
			PathB64 string `json:"path_b64,omitempty"`
		}
		plain, b64 := encodePath(p)
		if p != "" && (plain == "") == (b64 == "") || plain != "" && (plain != p || !utf8.ValidString(plain)) {
			t.Fatalf("encodePath(%q) = %q, %q", p, plain, b64)
		}
		raw, err := json.Marshal(item{Path: plain, PathB64: b64})
		if err != nil {
			t.Fatal(err)
		}
		var back item
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		got := back.Path
		if back.PathB64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(back.PathB64)
			if err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			got = string(decoded)
		}
		if got != p {
			t.Fatalf("encodePath(%q) gives %s, which reads %q", p, raw, got)
		}
		shown := shownPath(p)
		if !utf8.ValidString(shown) || strings.ContainsFunc(shown, unicode.IsControl) {
			t.Fatalf("shownPath(%q) = %q", p, shown)
		}
		if shown != p && shown != "(base64) "+base64.StdEncoding.EncodeToString([]byte(p)) {
			t.Fatalf("shownPath(%q) = %q, neither the path nor its base64", p, shown)
		}
	})
}
