package vectors

import "testing"

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
