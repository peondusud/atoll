package rules

import "time"

// Action says what a rule does with a file (specs.md §11.2).
type Action string

// The actions.
const (
	Manage Action = "manage"
	Ignore Action = "ignore"
)

// Source says where a value of a policy comes from (specs.md §11.3).
type Source string

// The sources.
const (
	FromRule    Source = "rule"    // the rule that matches
	FromFile    Source = "file"    // a section of the configuration
	FromPreset  Source = "preset"  // the strategy preset
	FromDefault Source = "default" // the default of the schema
)

// Properties holds what a rule sets (specs.md §11.2). A nil field is not set. An empty
// list that is not nil is set: to nothing.
type Properties struct {
	Replicas    *int
	Mode        *string
	Settle      *time.Duration
	Priority    *int
	Tags        []string
	TagsMode    *string
	Domains     []string
	Spread      *string
	SpreadMode  *string
	Colocate    *string
	Overflow    *string
	Cache       *bool
	CacheMinAge *time.Duration
}

// Rule is one rule, with its conditions compiled. A nil condition does not apply.
type Rule struct {
	Label    string
	Path     *Condition
	Filename *Condition
	Ext      *Condition
	SizeMin  *int64
	SizeMax  *int64
	Action   Action // Manage when the rule leaves it out
	// ActionSet says the rule wrote its action, which only rules explain cares about.
	ActionSet bool
	Set       Properties
}

// Pool holds the pool-wide values of rule properties (specs.md §24.2), after the
// preset and the defaults.
type Pool struct {
	Replicas    int
	Mode        string
	Settle      time.Duration
	Priority    int
	TagsMode    string
	Spread      string
	SpreadMode  string
	Colocate    string
	Overflow    string
	CacheMinAge time.Duration
	// Sources says where each comes from, by the name of the rule property, such as
	// "replicas" or "cache_min_age".
	Sources map[string]Source
}

// Policy is the effective policy of a file. It holds what the configuration asks for:
// the fallbacks of a phase not yet implemented (specs.md §12.2) are for the code that
// acts on it. With Action = Ignore, nothing else is set.
type Policy struct {
	Rule        *Rule // the rule that matches, or nil. It belongs to the list given.
	Action      Action
	Replicas    int
	Mode        string
	Settle      time.Duration
	Priority    int
	Tags        []string
	TagsMode    string
	Domains     []string
	Spread      string
	SpreadMode  string
	Colocate    string
	Overflow    string
	Cache       bool
	CacheMinAge time.Duration
}

// Effective returns the policy of a file: the first rule that matches, and what it
// leaves out from the pool (specs.md §11.3). A nil size means it is not known yet, so
// a rule with a size condition does not match. The path may hold any bytes.
func Effective(list []Rule, pool Pool, path string, size *int64) Policy {
	subject := NewSubject(path)
	for i := range list {
		if r := &list[i]; r.matches(subject, size) {
			return r.policy(pool)
		}
	}
	return (&Rule{}).values(pool)
}

func (r *Rule) matches(s Subject, size *int64) bool {
	if r.SizeMin != nil || r.SizeMax != nil {
		if size == nil {
			return false
		}
		if r.SizeMin != nil && *size < *r.SizeMin || r.SizeMax != nil && *size > *r.SizeMax {
			return false
		}
	}
	return r.conditionsHold(s)
}

// conditionsHold says whether every condition of the rule but its size holds for a file.
func (r *Rule) conditionsHold(s Subject) bool {
	for _, c := range []*Condition{r.Path, r.Filename, r.Ext} {
		if c != nil && !c.Match(s) {
			return false
		}
	}
	return true
}

func (r *Rule) policy(pool Pool) Policy {
	if r.Action == Ignore {
		return Policy{Rule: r, Action: Ignore}
	}
	p := r.values(pool)
	p.Rule = r
	return p
}

// values applies what the rule sets over the pool's values, for a file it manages.
func (r *Rule) values(pool Pool) Policy {
	return Policy{
		Action:      Manage,
		Replicas:    pick(r.Set.Replicas, pool.Replicas),
		Mode:        pick(r.Set.Mode, pool.Mode),
		Settle:      pick(r.Set.Settle, pool.Settle),
		Priority:    pick(r.Set.Priority, pool.Priority),
		Tags:        r.Set.Tags,
		TagsMode:    pick(r.Set.TagsMode, pool.TagsMode),
		Domains:     r.Set.Domains,
		Spread:      pick(r.Set.Spread, pool.Spread),
		SpreadMode:  pick(r.Set.SpreadMode, pool.SpreadMode),
		Colocate:    pick(r.Set.Colocate, pool.Colocate),
		Overflow:    pick(r.Set.Overflow, pool.Overflow),
		Cache:       pick(r.Set.Cache, true),
		CacheMinAge: pick(r.Set.CacheMinAge, pool.CacheMinAge),
	}
}

func pick[T any](set *T, pool T) T {
	if set != nil {
		return *set
	}
	return pool
}

// Sources says where each value of the policy comes from, by the name of the rule
// property, for rules explain. It needs the pool the policy was computed with.
func (p Policy) Sources(pool Pool) map[string]Source {
	if p.Action == Ignore {
		return map[string]Source{"action": FromRule}
	}
	var set Properties
	action := FromDefault
	if p.Rule != nil {
		set = p.Rule.Set
		if p.Rule.ActionSet {
			action = FromRule
		}
	}
	// A property with no pool-wide value comes from the rule or from a default.
	fixed := func(isSet bool) Source {
		if isSet {
			return FromRule
		}
		return FromDefault
	}
	// One with a pool-wide value comes from the rule or from wherever that came from.
	shared := func(name string, isSet bool) Source {
		if isSet {
			return FromRule
		}
		if s, ok := pool.Sources[name]; ok {
			return s
		}
		return FromDefault
	}
	return map[string]Source{
		"action":        action,
		"replicas":      shared("replicas", set.Replicas != nil),
		"mode":          shared("mode", set.Mode != nil),
		"settle":        shared("settle", set.Settle != nil),
		"priority":      shared("priority", set.Priority != nil),
		"tags":          fixed(set.Tags != nil),
		"tags_mode":     shared("tags_mode", set.TagsMode != nil),
		"domains":       fixed(set.Domains != nil),
		"spread":        shared("spread", set.Spread != nil),
		"spread_mode":   shared("spread_mode", set.SpreadMode != nil),
		"colocate":      shared("colocate", set.Colocate != nil),
		"overflow":      shared("overflow", set.Overflow != nil),
		"cache":         fixed(set.Cache != nil),
		"cache_min_age": shared("cache_min_age", set.CacheMinAge != nil),
	}
}
