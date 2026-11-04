// Package vectors loads the conformance vectors of conformance/vectors/ for unit
// tests (specs.md §29.4). It decodes strictly: an unknown field or another
// format_version fails the test, so a typo cannot silently disable a check.
// The file format is described in conformance/vectors/README.md.
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
	cases, err := load[C](name)
	if err != nil {
		t.Fatalf("vectors %s: %v", name, err)
	}
	return cases
}

func load[C any](name string) ([]C, error) {
	dir, err := vectorDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
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

// vectorDir finds conformance/vectors/ from the working directory of the test,
// which is the folder of the package under test.
func vectorDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "conformance", "vectors"), nil
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
	switch {
	case path != nil && pathB64 != nil:
		return "", errors.New("both path and path_b64")
	case path != nil:
		return *path, nil
	case pathB64 != nil:
		b, err := base64.StdEncoding.Strict().DecodeString(*pathB64)
		if err != nil {
			return "", fmt.Errorf("path_b64: %w", err)
		}
		if utf8.Valid(b) {
			return "", fmt.Errorf("path_b64 %q is valid UTF-8: use path", *pathB64)
		}
		return string(b), nil
	default:
		return "", errors.New("neither path nor path_b64")
	}
}
