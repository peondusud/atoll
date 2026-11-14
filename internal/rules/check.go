package rules

import "slices"

// Hiding says that a rule is hidden whole by an earlier one: their places in the list.
type Hiding struct {
	Rule, By int
}

// Hidden gives the rules an earlier one hides whole, each with the first that does, in the list's order (specs.md
// §11.3, §25). Only simple cases are told: each condition of the earlier rule holds for every file, as a path ** or a
// filename * does, or is one the later rule has too, written the same with the same ignore_case, or for ext lists
// every extension the later lists; and its size range holds the later's. A rule with no size condition reaches a file
// whose size is not known, which a rule with one never does, so it is never hidden by one. Patterns that only cover
// each other, as Films/** does Films/Action/**, are no simple case: the catalog tells of those (Reach).
func Hidden(list []Rule) []Hiding {
	var out []Hiding
	for j := range list {
		for i := range j {
			if hides(&list[i], &list[j]) {
				out = append(out, Hiding{Rule: j, By: i})
				break
			}
		}
	}
	return out
}

func hides(earlier, later *Rule) bool {
	if earlier.SizeMin != nil && (later.SizeMin == nil || *later.SizeMin < *earlier.SizeMin) {
		return false
	}
	if earlier.SizeMax != nil && (later.SizeMax == nil || *later.SizeMax > *earlier.SizeMax) {
		return false
	}
	return covers(earlier.Path, later.Path) && covers(earlier.Filename, later.Filename) && covers(earlier.Ext, later.Ext)
}

// covers says whether an earlier rule's condition holds for every file a later rule's does, in the simple cases
// Hidden tells of. A condition left out holds for every file.
func covers(earlier, later *Condition) bool {
	switch {
	case earlier == nil || earlier.always():
		return true
	case later == nil || earlier.field != later.field || earlier.fold != later.fold:
		return false
	case earlier.field == FieldExt:
		return !slices.ContainsFunc(later.exts, func(ext []rune) bool {
			return !slices.ContainsFunc(earlier.exts, func(have []rune) bool { return slices.Equal(have, ext) })
		})
	case earlier.re != nil || later.re != nil:
		return earlier.re != nil && later.re != nil && earlier.re.String() == later.re.String()
	default:
		return earlier.glob.String() == later.glob.String()
	}
}

// always says whether a condition holds for every file: a path ** or a filename *.
func (c *Condition) always() bool {
	if c.re != nil || c.glob == nil {
		return false
	}
	text := c.glob.String()
	return c.field == FieldPath && text == "**" || c.field == FieldFilename && text == "*"
}

// Reach counts the rules the files of a pool reach (specs.md §11.3, §25): a file reaches the first rule that matches
// it, as Effective says. With no size known, as for a file a rule ignores, which the catalog holds without one, it
// reaches each rule that may match it, up to and including the first that has no size condition, so that no rule
// that a file may reach is told unreached.
type Reach struct {
	list    []Rule
	reached []bool
}

// NewReach counts the rules of a list, none reached yet.
func NewReach(list []Rule) *Reach {
	return &Reach{list: list, reached: make([]bool, len(list))}
}

// Add counts one file.
func (r *Reach) Add(path string, size *int64) {
	s := NewSubject(path)
	for i := range r.list {
		rule := &r.list[i]
		if size != nil {
			if rule.matches(s, size) {
				r.reached[i] = true
				return
			}
			continue
		}
		if !rule.conditionsHold(s) {
			continue
		}
		r.reached[i] = true
		if rule.SizeMin == nil && rule.SizeMax == nil {
			return
		}
	}
}

// Unreached gives the places of the rules no file reached, in the list's order.
func (r *Reach) Unreached() []int {
	var out []int
	for i, reached := range r.reached {
		if !reached {
			out = append(out, i)
		}
	}
	return out
}
