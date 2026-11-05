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

// Every vector file and the corpus follow the text and token rules.
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
