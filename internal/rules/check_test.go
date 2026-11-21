package rules

import (
	"reflect"
	"slices"
	"testing"
)

// The vectors of rules_check.json start from the text of a configuration, so they run in internal/config, which
// imports this package. These tests build the rules themselves, to check Hidden and Reach where they live
// (specs.md §11.3, §25).

// cond compiles a path or filename condition, or one ext entry, with ignore_case on, the default (specs.md §11.1).
func cond(t testing.TB, field Field, value string) *Condition {
	t.Helper()
	return condCase(t, field, value, true)
}

// condCase compiles a condition with the ignore_case given.
func condCase(t testing.TB, field Field, value string, ignoreCase bool) *Condition {
	t.Helper()
	c, err := Compile(field, value, ignoreCase)
	if err != nil {
		t.Fatalf("Compile(%s, %q, %v): %v", field, value, ignoreCase, err)
	}
	return c
}

// extOf compiles an ext list.
func extOf(t testing.TB, ignoreCase bool, entries ...string) *Condition {
	t.Helper()
	c, err := CompileExt(entries, ignoreCase)
	if err != nil {
		t.Fatalf("CompileExt(%q, %v): %v", entries, ignoreCase, err)
	}
	return c
}

// nbytes returns a size in bytes, for a size condition or a file of known size.
func nbytes(n int64) *int64 { return &n }

func TestHidden(t *testing.T) {
	cases := []struct {
		note  string
		rules func(t *testing.T) []Rule
		want  []Hiding
	}{
		{"no rule", func(*testing.T) []Rule { return nil }, nil},
		{"one rule", func(t *testing.T) []Rule { return []Rule{{Path: cond(t, FieldPath, "**")}} }, nil},
		{"a path ** hides every later rule, whatever its conditions and sizes", func(t *testing.T) []Rule {
			return []Rule{
				{Path: cond(t, FieldPath, "**")},
				{Path: cond(t, FieldPath, "Films/**")},
				{Filename: cond(t, FieldFilename, "*.mkv")},
				{Ext: extOf(t, true, "mkv")},
				{SizeMax: nbytes(1024)},
				{Path: cond(t, FieldPath, "re:^Films/")},
				{Path: condCase(t, FieldPath, "Films/**", false), SizeMin: nbytes(1), SizeMax: nbytes(2)},
			}
		}, []Hiding{{1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0}, {6, 0}}},
		{"a path ** without ignore_case holds for every file too", func(t *testing.T) []Rule {
			return []Rule{{Path: condCase(t, FieldPath, "**", false)}, {Path: cond(t, FieldPath, "Films/**")}}
		}, []Hiding{{1, 0}}},
		{"a filename * holds for every file", func(t *testing.T) []Rule {
			return []Rule{{Filename: cond(t, FieldFilename, "*")}, {Path: cond(t, FieldPath, "a/**")}}
		}, []Hiding{{1, 0}}},
		{"a rule with no condition holds for every file", func(t *testing.T) []Rule {
			return []Rule{{Action: Ignore}, {Path: cond(t, FieldPath, "a/**"), Ext: extOf(t, true, "mkv")}}
		}, []Hiding{{1, 0}}},
		{"a path * is only the files at the top: no simple case", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "*")}, {Path: cond(t, FieldPath, "Films/**")}}
		}, nil},
		{"a path other than ** holds for some files only", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "Films/**")}, {Path: cond(t, FieldPath, "Séries/**")}}
		}, nil},
		{"a filename other than * holds for some files only", func(t *testing.T) []Rule {
			return []Rule{{Filename: cond(t, FieldFilename, "a*")}, {Filename: cond(t, FieldFilename, "b*")}}
		}, nil},
		{"a regex that holds for every file is no simple case", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "re:.*")}, {Path: cond(t, FieldPath, "Films/**")}}
		}, nil},
		{"the same glob with the same ignore_case: hidden", func(t *testing.T) []Rule {
			return []Rule{
				{Path: cond(t, FieldPath, "Films/**")},
				{Path: cond(t, FieldPath, "Films/**"), Ext: extOf(t, true, "mkv")},
				{Path: condCase(t, FieldPath, "Films/**", false)},
				{Path: condCase(t, FieldPath, "Films/**", false), Filename: cond(t, FieldFilename, "*1080p*")},
			}
		}, []Hiding{{1, 0}, {3, 2}}},
		{"the same glob with another ignore_case is another condition", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "films/**")}, {Path: condCase(t, FieldPath, "films/**", false)}}
		}, nil},
		{"the same filename glob: hidden", func(t *testing.T) []Rule {
			return []Rule{{Filename: cond(t, FieldFilename, "*.tmp"), Action: Ignore}, {Filename: cond(t, FieldFilename, "*.tmp"), Path: cond(t, FieldPath, "x/**")}}
		}, []Hiding{{1, 0}}},
		{"a glob the later rule has as another condition: not hidden", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "a*")}, {Filename: cond(t, FieldFilename, "a*")}}
		}, nil},
		{"a condition of the earlier rule the later one lacks: not hidden", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "Films/**"), Ext: extOf(t, true, "mkv")}, {Path: cond(t, FieldPath, "Films/**")}}
		}, nil},
		{"one condition alike and another not: not hidden", func(t *testing.T) []Rule {
			return []Rule{
				{Path: cond(t, FieldPath, "Films/**"), Ext: extOf(t, true, "mkv")},
				{Path: cond(t, FieldPath, "Séries/**"), Ext: extOf(t, true, "mkv")},
				{Path: cond(t, FieldPath, "Films/**"), Ext: extOf(t, true, "srt")},
			}
		}, nil},
		{"every extension of the later rule among the earlier's: hidden", func(t *testing.T) []Rule {
			return []Rule{
				{Ext: extOf(t, true, "mkv", "mp4")},
				{Ext: extOf(t, true, "mp4")},
				{Ext: extOf(t, true, "mp4", "mkv")},
			}
		}, []Hiding{{1, 0}, {2, 0}}},
		{"an extension the earlier rule does not list: not hidden", func(t *testing.T) []Rule {
			return []Rule{{Ext: extOf(t, true, "mkv")}, {Ext: extOf(t, true, "mkv", "mp4")}}
		}, nil},
		{"the same extension with another ignore_case: not hidden", func(t *testing.T) []Rule {
			return []Rule{{Ext: extOf(t, true, "mkv")}, {Ext: extOf(t, false, "mkv")}}
		}, nil},
		{"the same regex, written the same: hidden", func(t *testing.T) []Rule {
			return []Rule{
				{Filename: cond(t, FieldFilename, "re:^x$")},
				{Filename: cond(t, FieldFilename, "re:^x$"), Ext: extOf(t, true, "mkv")},
				{Filename: condCase(t, FieldFilename, "re:^y$", false)},
				{Filename: condCase(t, FieldFilename, "re:^y$", false)},
			}
		}, []Hiding{{1, 0}, {3, 2}}},
		{"a regex is compared as written: another text is not the same", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "re:^Films/")}, {Path: cond(t, FieldPath, "re:^(?:Films)/")}}
		}, nil},
		{"the same regex with another ignore_case: not hidden", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "re:^a")}, {Path: condCase(t, FieldPath, "re:^a", false)}}
		}, nil},
		{"a glob does not hide a regex of the same text", func(t *testing.T) []Rule {
			return []Rule{{Path: condCase(t, FieldPath, "Films/a", false)}, {Path: condCase(t, FieldPath, "re:Films/a", false)}}
		}, nil},
		{"a regex does not hide a glob of the same text", func(t *testing.T) []Rule {
			return []Rule{{Path: condCase(t, FieldPath, "re:Films/a", false)}, {Path: condCase(t, FieldPath, "Films/a", false)}}
		}, nil},
		{"size_min: an earlier bound at or below the later one hides it", func(t *testing.T) []Rule {
			return []Rule{
				{SizeMin: nbytes(100)},
				{SizeMin: nbytes(100)},
				{SizeMin: nbytes(200), SizeMax: nbytes(300)},
			}
		}, []Hiding{{1, 0}, {2, 0}}},
		{"size_min: an earlier bound above the later one does not", func(t *testing.T) []Rule {
			return []Rule{{SizeMin: nbytes(200)}, {SizeMin: nbytes(199)}}
		}, nil},
		{"size_max: an earlier bound at or above the later one hides it", func(t *testing.T) []Rule {
			return []Rule{
				{SizeMax: nbytes(200)},
				{SizeMax: nbytes(200)},
				{SizeMin: nbytes(10), SizeMax: nbytes(100)},
			}
		}, []Hiding{{1, 0}, {2, 0}}},
		{"size_max: an earlier bound below the later one does not", func(t *testing.T) []Rule {
			return []Rule{{SizeMax: nbytes(200)}, {SizeMax: nbytes(201)}}
		}, nil},
		{"a range that holds the later one hides it, one it does not hold does not", func(t *testing.T) []Rule {
			return []Rule{
				{SizeMin: nbytes(10), SizeMax: nbytes(100)},
				{SizeMin: nbytes(10), SizeMax: nbytes(100)},
				{SizeMin: nbytes(20), SizeMax: nbytes(50)},
				{SizeMin: nbytes(5), SizeMax: nbytes(50)},
				{SizeMin: nbytes(20), SizeMax: nbytes(200)},
			}
		}, []Hiding{{1, 0}, {2, 0}}},
		{"a bound the later rule lacks: it reaches sizes the earlier does not, and files of no size known", func(t *testing.T) []Rule {
			return []Rule{
				{SizeMin: nbytes(0)},
				{SizeMax: nbytes(100)},
				{Path: cond(t, FieldPath, "**")},
			}
		}, nil},
		{"a rule with no size condition hides one with one", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "Films/**")}, {Path: cond(t, FieldPath, "Films/**"), SizeMin: nbytes(1)}}
		}, []Hiding{{1, 0}}},
		{"each rule hidden is named with the first rule that hides it", func(t *testing.T) []Rule {
			return []Rule{
				{Path: cond(t, FieldPath, "Films/**")},
				{Path: cond(t, FieldPath, "**")},
				{Path: cond(t, FieldPath, "Films/**")},
				{Path: cond(t, FieldPath, "Séries/**")},
			}
		}, []Hiding{{2, 0}, {3, 1}}},
	}
	for _, c := range cases {
		got := Hidden(c.rules(t))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: Hidden = %v, want %v", c.note, got, c.want)
		}
	}
}

func TestReach(t *testing.T) {
	type file struct {
		path string
		size *int64
	}
	cases := []struct {
		note  string
		rules func(t *testing.T) []Rule
		files []file
		want  []int
	}{
		{"no rule, no file", func(*testing.T) []Rule { return nil }, nil, nil},
		{"no file: every rule unreached", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "Films/**")}, {}}
		}, nil, []int{0, 1}},
		{"a file reaches the first rule that matches it, and no later one", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "Films/**")}, {Path: cond(t, FieldPath, "**")}, {}}
		}, []file{{"Films/a.mkv", nbytes(1)}}, []int{1, 2}},
		{"a file the first rule leaves reaches the next that matches it", func(t *testing.T) []Rule {
			return []Rule{{Path: cond(t, FieldPath, "Films/**")}, {Path: cond(t, FieldPath, "Music/**")}, {}}
		}, []file{{"Films/a.mkv", nbytes(1)}, {"b.mkv", nbytes(1)}}, []int{1}},
		{"a known size: a rule whose size does not hold is not reached", func(t *testing.T) []Rule {
			return []Rule{{SizeMin: nbytes(100)}, {SizeMax: nbytes(10)}, {}}
		}, []file{{"a", nbytes(50)}}, []int{0, 1}},
		{"a known size within the bounds reaches the rule", func(t *testing.T) []Rule {
			return []Rule{{SizeMin: nbytes(100)}, {SizeMax: nbytes(10)}, {}}
		}, []file{{"a", nbytes(100)}, {"b", nbytes(10)}}, []int{2}},
		{"no size known: each rule that may match, up to and including the first with no size condition", func(t *testing.T) []Rule {
			return []Rule{
				{SizeMin: nbytes(100)},
				{SizeMax: nbytes(10), Action: Ignore},
				{SizeMin: nbytes(1), SizeMax: nbytes(5)},
				{Path: cond(t, FieldPath, "Films/**")},
				{},
			}
		}, []file{{"Films/a.nfo", nil}}, []int{4}},
		{"no size known: a rule whose other conditions do not hold is not reached", func(t *testing.T) []Rule {
			return []Rule{
				{Path: cond(t, FieldPath, "Films/**"), SizeMin: nbytes(100)},
				{Path: cond(t, FieldPath, "Films/**")},
				{SizeMax: nbytes(10)},
				{},
				{},
			}
		}, []file{{"b.nfo", nil}}, []int{0, 1, 4}},
	}
	for _, c := range cases {
		reach := NewReach(c.rules(t))
		for _, f := range c.files {
			reach.Add(f.path, f.size)
		}
		if got := reach.Unreached(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Unreached = %v, want %v", c.note, got, c.want)
		}
	}
}

// Reach counts a file as Effective chooses its rule, when its size is known (specs.md §11.3).
func TestReachAgreesWithEffective(t *testing.T) {
	list := []Rule{
		{Path: cond(t, FieldPath, "Films/**"), SizeMax: nbytes(1000)},
		{Filename: cond(t, FieldFilename, "*.srt")},
		{Ext: extOf(t, true, "mkv"), SizeMin: nbytes(10)},
		{Path: cond(t, FieldPath, "re:^Music/")},
	}
	paths := []string{"Films/a.mkv", "Films/a.srt", "a.mkv", "a.srt", "Music/a.mkv", "Music/a.flac", "b"}
	sizes := []int64{0, 9, 10, 1000, 1001}
	for _, p := range paths {
		for _, s := range sizes {
			reach := NewReach(list)
			reach.Add(p, &s)
			var want []int
			policy := Effective(list, Pool{}, p, &s)
			for i := range list {
				if policy.Rule != &list[i] {
					want = append(want, i)
				}
			}
			if got := reach.Unreached(); !slices.Equal(got, want) {
				t.Errorf("%q of %d bytes: Unreached = %v, Effective says %v", p, s, got, want)
			}
		}
	}
}
