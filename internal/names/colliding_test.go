package names

import (
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// Colliding is the rule of ntfs_collision.json for one folder: each vector whose names are all one component is a
// folder's listing and a name of it, the names that collide in byte order (specs.md §15.7).
func TestCollidingVectors(t *testing.T) {
	checked := 0
	for i, v := range vectors.Load[collisionVector](t, "ntfs_collision.json") {
		if v.Existing == nil || v.With == nil || len(v.ExistingB64) > 0 {
			continue
		}
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil || strings.Contains(path, "/") || slices.ContainsFunc(*v.Existing, func(e string) bool { return strings.Contains(e, "/") }) {
			continue
		}
		got := Colliding(path, *v.Existing, "")
		if !slices.Equal(got, *v.With) && (len(got) != 0 || len(*v.With) != 0) {
			t.Errorf("vector %d: Colliding(%+q, %q) = %q, want %q", i, path, *v.Existing, got, *v.With)
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("only %d vectors of one folder", checked)
	}
}

// Two names of lpath_key.json collide exactly when their keys are one and their bytes are not, the ASCII ones compared
// without their keys too; but leaves one out.
func TestCollidingAgainstTheKeys(t *testing.T) {
	var list []string
	for _, v := range vectors.Load[keyVector](t, "lpath_key.json") {
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, strings.Split(path, "/")...)
	}
	list = append(list, "film.mkv", "Film.mkv", "FILM.MKV", "film.mkV", "film", "K", "k", "K", "ſ", "s", "S", "ı", "i", "I")
	for _, a := range list {
		for _, b := range list {
			ka, oka := CollisionKey(a)
			kb, okb := CollisionKey(b)
			want := oka && okb && ka == kb && a != b
			if got := len(Colliding(a, []string{b}, "")) == 1; got != want {
				t.Errorf("Colliding(%+q, %+q) = %v, want %v", a, b, got, want)
			}
			if got := Colliding(a, []string{b}, b); len(got) != 0 {
				t.Errorf("Colliding(%+q, %+q, but itself) = %q", a, b, got)
			}
		}
	}
}

// An ASCII name's key is the name with a to z in capitals, which the comparison without keys relies on.
func TestASCIIKeys(t *testing.T) {
	for c := range 128 {
		s := string(rune(c))
		key, ok := CollisionKey(s)
		want := s
		if c >= 'a' && c <= 'z' {
			want = string(rune(c - 'a' + 'A'))
		}
		if !ok || key != want {
			t.Errorf("CollisionKey(%+q) = %+q, want %+q", s, key, want)
		}
	}
	got := Colliding("Films", []string{"films", "FILMS", "Films", "Film", "Films", "Séries", "fiKlms"}, "")
	if want := []string{"FILMS", "films"}; !slices.Equal(got, want) {
		t.Errorf("Colliding = %q, want %q", got, want)
	}
}
