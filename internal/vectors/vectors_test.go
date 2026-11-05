package vectors

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPath(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name          string
		path, pathB64 *string
		want          string
		wantErr       bool
	}{
		{name: "path", path: str("Films/a.mkv"), want: "Films/a.mkv"},
		{name: "path_b64", pathB64: str("Yf9i"), want: "a\xffb"},
		{name: "neither", wantErr: true},
		{name: "both", path: str("a"), pathB64: str("Yf9i"), wantErr: true},
		{name: "path_b64 of valid UTF-8", pathB64: str("YWI="), wantErr: true},
		{name: "invalid base64", pathB64: str("Yf9"), wantErr: true},
		{name: "base64 with a line break", pathB64: str("Yf9i\n"), wantErr: true},
		{name: "base64 split by a line break", pathB64: str("Y\r\nf9i"), wantErr: true},
	} {
		got, err := Path(tc.path, tc.pathB64)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("%s: Path = %q, want an error", tc.name, got)
		case !tc.wantErr && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case got != tc.want:
			t.Errorf("%s: Path = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckText(t *testing.T) {
	for _, tc := range []struct {
		data string
		ok   bool
	}{
		{`{"a": "caf\u00e9"}`, true},
		{`{"a": "\u4e00\uffff"}`, true},
		{`{"a": "\ud83d\ude00"}`, true},
		{`{"a": "\uD83D\uDE00"}`, true},
		{`{"a": "\ud83d"}`, false},
		{`{"a": "\ud83dx\ude00"}`, false},
		{`{"a": "\uDC00"}`, false},
		{`{"a": "\ude00\ud83d"}`, false},
		{`{"a": "\\ud800"}`, true},
		{`{"a": "\\\ud800"}`, false},
		{"{\"a\": \"\xff\"}", false},
		{"{\"a\": \"\xed\xa0\x80\"}", false},
	} {
		if err := checkText([]byte(tc.data)); (err == nil) != tc.ok {
			t.Errorf("checkText(%q) = %v, want ok %v", tc.data, err, tc.ok)
		}
	}
}

// Every vector file and the corpus follow the text, token and flag rules, even
// a file that no test loads yet.
func TestFilesFollowTextRules(t *testing.T) {
	root, err := conformanceDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"vectors/*.json", "corpus/*/*.json"} {
		files, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no file matches %s", pattern)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkText(data); err != nil {
				t.Errorf("%s: %v", f, err)
			}
			if err := checkTokens(data); err != nil {
				t.Errorf("%s: %v", f, err)
			}
			if err := checkFlags(data); err != nil {
				t.Errorf("%s: %v", f, err)
			}
		}
	}
}

func TestCheckFlags(t *testing.T) {
	for _, tc := range []struct {
		data string
		ok   bool
	}{
		{`{"cases": [{"input": "a", "n": 1}]}`, true},
		{`{"cases": [{"input": "a", "error": true}, {"input": "b", "n": 1}]}`, true},
		{`{"cases": [{"path": "a", "no_key": true}, {"path": "b", "key": "B"}]}`, true},
		{`{"cases": [{"match": false, "inside": false}]}`, true},
		{`{"cases": [{"input": "a", "n": 1, "error": false}]}`, false},
		{`{"cases": [{"input": "a", "error": true}, {"input": "b", "error": false}]}`, false},
		{`{"cases": [{"input": "a", "error": null}]}`, false},
		{`{"cases": [{"input": "a", "error": 1}]}`, false},
		{`{"cases": [{"input": "a", "error": "true"}]}`, false},
		{`{"cases": [{"path": "a", "key": "A", "no_key": false}]}`, false},
		{`{"cases": [{"path": "a", "no_key": null}]}`, false},
	} {
		if err := checkFlags([]byte(tc.data)); (err == nil) != tc.ok {
			t.Errorf("checkFlags(%s) = %v, want ok %v", tc.data, err, tc.ok)
		}
	}
}

// parse applies every rule, whatever the type of the case: the vector tests
// declare error as a plain bool, which would read false and null as absent.
func TestParse(t *testing.T) {
	type vector struct {
		Input string `json:"input"`
		N     *int64 `json:"n"`
		Error bool   `json:"error"`
	}
	file := func(version, cases string) string {
		return `{"format_version": ` + version + `, "spec": "s", "description": "d", "cases": [` + cases + `]}`
	}
	for _, tc := range []struct {
		name, data string
		want       int
	}{
		{"valid", file("1", `{"input": "a", "n": 1}, {"input": "b", "error": true}`), 2},
		{"flag false", file("1", `{"input": "a", "n": 1, "error": false}`), -1},
		{"flag null", file("1", `{"input": "a", "n": 1, "error": null}`), -1},
		{"another version", file("2", `{"input": "a", "n": 1}`), -1},
		{"no cases", file("1", ``), -1},
		{"unknown field", file("1", `{"input": "a", "m": 1}`), -1},
		{"uppercase key", file("1", `{"INPUT": "a"}`), -1},
		{"not a plain integer", file("1", `{"input": "a", "n": 1.0}`), -1},
		{"invalid UTF-8", file("1", "{\"input\": \"\xff\"}"), -1},
		{"trailing data", file("1", `{"input": "a"}`) + `{}`, -1},
	} {
		got, err := parse[vector]([]byte(tc.data))
		switch {
		case tc.want < 0 && err == nil:
			t.Errorf("%s: parse = %v, want an error", tc.name, got)
		case tc.want >= 0 && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want >= 0 && len(got) != tc.want:
			t.Errorf("%s: %d cases, want %d", tc.name, len(got), tc.want)
		}
	}
}

func TestCheckTokens(t *testing.T) {
	for _, tc := range []struct {
		data string
		ok   bool
	}{
		{`{"cases": [{"name": "a", "n": 0}, {"n": -12}]}`, true},
		{`{"cases": [{"name": "a"}], "description": "x"}`, true},
		{`{"cases": [{"name": [{"a": 1}], "b": {"c": "NAME"}}]}`, true},
		{`{"NAME": "a"}`, false},
		{`{"Ntfs": []}`, false},
		{"{\"ntf\u017f\": []}", false},
		{`{"name": "a", "name": "b"}`, false},
		{`{"cases": [{"ntfs": [], "ntfs": []}]}`, false},
		{`{"a": {"b": 1}, "b": 2}`, true},
		{`{"n": -0}`, false},
		{`{"n": 1.0}`, false},
		{`{"n": 1e3}`, false},
		{`{"n": 0}`, true},
	} {
		if err := checkTokens([]byte(tc.data)); (err == nil) != tc.ok {
			t.Errorf("checkTokens(%s) = %v, want ok %v", tc.data, err, tc.ok)
		}
	}
}
