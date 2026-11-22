package rules

import (
	"maps"
	"reflect"
	"testing"
	"time"
)

// The vectors of policy.json start from the text of a configuration, so they run in internal/config, which imports
// this package. These tests build the rules and the pool themselves, to check Effective and Sources where they live
// (specs.md §11.3).

func ptr[T any](v T) *T { return &v }

// testPool has a value for each pool-wide property unlike any a rule of these tests sets.
func testPool() Pool {
	return Pool{
		Replicas:    1,
		Mode:        "deferred",
		Settle:      10 * time.Minute,
		Priority:    0,
		TagsMode:    "prefer",
		Spread:      "domain",
		SpreadMode:  "prefer",
		Colocate:    "none",
		Overflow:    "allow",
		CacheMinAge: 7 * 24 * time.Hour,
	}
}

// poolPolicy is the policy of a file no rule matches: the pool's values, managed (specs.md §11.3).
func poolPolicy(pool Pool) Policy {
	return Policy{
		Action:      Manage,
		Replicas:    pool.Replicas,
		Mode:        pool.Mode,
		Settle:      pool.Settle,
		Priority:    pool.Priority,
		TagsMode:    pool.TagsMode,
		Spread:      pool.Spread,
		SpreadMode:  pool.SpreadMode,
		Colocate:    pool.Colocate,
		Overflow:    pool.Overflow,
		Cache:       true,
		CacheMinAge: pool.CacheMinAge,
	}
}

// everyProperty sets each property to a value other than the pool's.
func everyProperty() Properties {
	return Properties{
		Replicas:    ptr(2),
		Mode:        ptr("immediate"),
		Settle:      ptr(30 * time.Second),
		Priority:    ptr(5),
		Tags:        []string{"fast"},
		TagsMode:    ptr("require"),
		Domains:     []string{"home"},
		Spread:      ptr("disk"),
		SpreadMode:  ptr("strict"),
		Colocate:    ptr("folder"),
		Overflow:    ptr("deny"),
		Cache:       ptr(false),
		CacheMinAge: ptr(time.Hour),
	}
}

func TestEffectiveNoRule(t *testing.T) {
	pool := testPool()
	for _, list := range [][]Rule{nil, {{Path: cond(t, FieldPath, "Films/**"), Set: everyProperty()}}} {
		got := Effective(list, pool, "a/b.mkv", nbytes(1))
		if want := poolPolicy(pool); !reflect.DeepEqual(got, want) {
			t.Errorf("%d rules: Effective = %+v, want %+v", len(list), got, want)
		}
	}
}

func TestEffectiveFirstRuleWins(t *testing.T) {
	list := []Rule{
		{Label: "series", Path: cond(t, FieldPath, "Séries/**"), Set: Properties{Replicas: ptr(2)}},
		{Label: "a", Path: cond(t, FieldPath, "Films/**"), Set: Properties{Replicas: ptr(0)}},
		{Label: "b", Path: cond(t, FieldPath, "Films/**"), Set: Properties{Replicas: ptr(2)}, Action: Ignore},
	}
	got := Effective(list, testPool(), "Films/x.mkv", nil)
	if got.Rule != &list[1] {
		t.Fatalf("Effective chose %+v, want the rule a, the first that matches", got.Rule)
	}
	if got.Action != Manage || got.Replicas != 0 {
		t.Errorf("Effective = %+v, want rule a's replicas 0, managed", got)
	}
}

// A rule with a size condition matches only a file whose size is known and within its bounds, inclusive (specs.md
// §11.3); the next rule may match the others.
func TestEffectiveSize(t *testing.T) {
	cases := []struct {
		min, max *int64
		size     *int64
		match    bool
	}{
		{nil, nil, nil, true},
		{nil, nil, nbytes(0), true},
		{nbytes(100), nil, nil, false},
		{nbytes(100), nil, nbytes(99), false},
		{nbytes(100), nil, nbytes(100), true},
		{nbytes(100), nil, nbytes(1 << 40), true},
		{nil, nbytes(200), nil, false},
		{nil, nbytes(200), nbytes(0), true},
		{nil, nbytes(200), nbytes(200), true},
		{nil, nbytes(200), nbytes(201), false},
		{nbytes(100), nbytes(200), nil, false},
		{nbytes(100), nbytes(200), nbytes(99), false},
		{nbytes(100), nbytes(200), nbytes(100), true},
		{nbytes(100), nbytes(200), nbytes(150), true},
		{nbytes(100), nbytes(200), nbytes(200), true},
		{nbytes(100), nbytes(200), nbytes(201), false},
		{nbytes(0), nbytes(0), nbytes(0), true},
		{nbytes(0), nbytes(0), nbytes(1), false},
	}
	show := func(n *int64) any {
		if n == nil {
			return "none"
		}
		return *n
	}
	for _, c := range cases {
		list := []Rule{{Label: "sized", SizeMin: c.min, SizeMax: c.max}, {Label: "rest"}}
		got := Effective(list, testPool(), "a.mkv", c.size)
		if matched := got.Rule == &list[0]; matched != c.match || got.Rule == nil {
			t.Errorf("size_min %v, size_max %v, size %v: rule %+v, want the sized rule %v", show(c.min), show(c.max), show(c.size), got.Rule, c.match)
		}
	}
}

// Every condition of a rule must hold: path, filename and ext, combined with AND (specs.md §11.1).
func TestEffectiveConditionsAnd(t *testing.T) {
	list := []Rule{{
		Path:     cond(t, FieldPath, "Films/**"),
		Filename: cond(t, FieldFilename, "*1080p*"),
		Ext:      extOf(t, true, "mkv"),
		SizeMin:  nbytes(10),
	}}
	cases := []struct {
		path  string
		match bool
	}{
		{"Films/a.1080p.mkv", true},
		{"films/A.1080P.MKV", true},
		{"Films/a.720p.mkv", false},
		{"Séries/a.1080p.mkv", false},
		{"Films/a.1080p.srt", false},
		{"Films/1080p/a.mkv", false},
	}
	for _, c := range cases {
		got := Effective(list, testPool(), c.path, nbytes(10))
		if matched := got.Rule == &list[0]; matched != c.match {
			t.Errorf("%q: matched %v, want %v", c.path, matched, c.match)
		}
	}
	if got := Effective(list, testPool(), "Films/a.1080p.mkv", nbytes(9)); got.Rule != nil {
		t.Errorf("a size below size_min: rule %+v, want none", got.Rule)
	}
}

// A rule's values override the pool's, each one it sets; the others are the pool's (specs.md §11.3). cache has no
// pool-wide value: true unless a rule sets it.
func TestEffectiveValues(t *testing.T) {
	pool := testPool()
	set := everyProperty()
	list := []Rule{{Label: "all", Path: cond(t, FieldPath, "a/**"), Set: set}, {Label: "none", Path: cond(t, FieldPath, "b/**")}}

	got := Effective(list, pool, "a/x", nil)
	want := Policy{
		Rule:        &list[0],
		Action:      Manage,
		Replicas:    *set.Replicas,
		Mode:        *set.Mode,
		Settle:      *set.Settle,
		Priority:    *set.Priority,
		Tags:        set.Tags,
		TagsMode:    *set.TagsMode,
		Domains:     set.Domains,
		Spread:      *set.Spread,
		SpreadMode:  *set.SpreadMode,
		Colocate:    *set.Colocate,
		Overflow:    *set.Overflow,
		Cache:       *set.Cache,
		CacheMinAge: *set.CacheMinAge,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a rule that sets every property:\n got %+v\nwant %+v", got, want)
	}

	got = Effective(list, pool, "b/x", nil)
	want = poolPolicy(pool)
	want.Rule = &list[1]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a rule that sets none:\n got %+v\nwant %+v", got, want)
	}

	// An empty list that is not nil is set: to nothing.
	list[1].Set.Tags, list[1].Set.Domains = []string{}, []string{}
	got = Effective(list, pool, "b/x", nil)
	if got.Tags == nil || len(got.Tags) != 0 || got.Domains == nil || len(got.Domains) != 0 {
		t.Errorf("tags and domains set to nothing: %q and %q, want both empty and set", got.Tags, got.Domains)
	}
}

// An ignored file has no other property (specs.md §11.3).
func TestEffectiveIgnore(t *testing.T) {
	list := []Rule{{Label: "tmp", Filename: cond(t, FieldFilename, "*.tmp"), Action: Ignore, ActionSet: true, Set: everyProperty()}}
	got := Effective(list, testPool(), "a/b.TMP", nil)
	if want := (Policy{Rule: &list[0], Action: Ignore}); !reflect.DeepEqual(got, want) {
		t.Errorf("Effective = %+v, want %+v", got, want)
	}
	if got, want := got.Sources(testPool()), map[string]Source{"action": FromRule}; !maps.Equal(got, want) {
		t.Errorf("Sources = %v, want %v", got, want)
	}
}

// Where each value comes from: the rule that sets it, or, for a property with a pool-wide value, wherever that came
// from, and a default otherwise (specs.md §11.3).
func TestSources(t *testing.T) {
	shared := []string{"replicas", "mode", "settle", "priority", "tags_mode", "spread", "spread_mode", "colocate", "overflow", "cache_min_age"}
	fixed := []string{"tags", "domains", "cache"}
	every := func(names []string, s Source, into map[string]Source) {
		for _, n := range names {
			into[n] = s
		}
	}

	pool := testPool()
	pool.Sources = map[string]Source{}
	for i, n := range shared {
		pool.Sources[n] = []Source{FromFile, FromPreset}[i%2]
	}
	fromPool := map[string]Source{"action": FromDefault}
	maps.Copy(fromPool, pool.Sources)
	every(fixed, FromDefault, fromPool)

	defaults := map[string]Source{"action": FromDefault}
	every(shared, FromDefault, defaults)
	every(fixed, FromDefault, defaults)

	fromRule := map[string]Source{"action": FromRule}
	every(shared, FromRule, fromRule)
	every(fixed, FromRule, fromRule)

	list := []Rule{
		{Label: "every", Path: cond(t, FieldPath, "every/**"), Action: Manage, ActionSet: true, Set: everyProperty()},
		{Label: "none", Path: cond(t, FieldPath, "none/**")},
		{Label: "action", Path: cond(t, FieldPath, "action/**"), Action: Manage, ActionSet: true},
	}
	fromAction := maps.Clone(fromPool)
	fromAction["action"] = FromRule

	cases := []struct {
		note string
		pool Pool
		path string
		want map[string]Source
	}{
		{"a rule that sets everything", pool, "every/a", fromRule},
		{"a rule that sets nothing", pool, "none/a", fromPool},
		{"a rule that sets its action alone", pool, "action/a", fromAction},
		{"no rule", pool, "a", fromPool},
		{"no rule, and a pool of defaults", testPool(), "a", defaults},
		{"a rule that sets nothing, and a pool of defaults", testPool(), "none/a", defaults},
	}
	for _, c := range cases {
		got := Effective(list, c.pool, c.path, nil).Sources(c.pool)
		if !maps.Equal(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.note, got, c.want)
		}
	}

	// Each property set alone comes from the rule, and only it.
	set := reflect.ValueOf(everyProperty())
	names := map[string]string{
		"Replicas": "replicas", "Mode": "mode", "Settle": "settle", "Priority": "priority", "Tags": "tags",
		"TagsMode": "tags_mode", "Domains": "domains", "Spread": "spread", "SpreadMode": "spread_mode",
		"Colocate": "colocate", "Overflow": "overflow", "Cache": "cache", "CacheMinAge": "cache_min_age",
	}
	if n := set.NumField(); n != len(names) {
		t.Fatalf("Properties has %d fields, the test names %d", n, len(names))
	}
	for i := range set.NumField() {
		field := set.Type().Field(i).Name
		var one Properties
		reflect.ValueOf(&one).Elem().Field(i).Set(set.Field(i))
		rule := []Rule{{Set: one}}
		got := Effective(rule, pool, "a", nil).Sources(pool)
		want := maps.Clone(fromPool)
		want[names[field]] = FromRule
		if !maps.Equal(got, want) {
			t.Errorf("%s set alone:\n got %v\nwant %v", field, got, want)
		}
	}
}
