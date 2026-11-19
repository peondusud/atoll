package solver

import (
	"bytes"
	"slices"
	"strings"
)

// xattrNamespace is the namespace of the extended attributes Atoll keeps (specs.md §14, §21.2).
const xattrNamespace = "user."

// KeptXattr says whether Atoll keeps an extended attribute: one of the user. namespace, a name past user., compared byte
// for byte. Those of security., trusted. and system., ACLs among them, are never read, given or copied
// (conformance/vectors/xattrs.json).
func KeptXattr(name string) bool {
	return len(name) > len(xattrNamespace) && strings.HasPrefix(name, xattrNamespace)
}

// XattrPlan is what a copy must be given to hold the attributes of another: the names to set, and those to remove, each
// in byte order.
type XattrPlan struct {
	Set    []string
	Remove []string
}

// Empty says that a copy holds what it must already.
func (p XattrPlan) Empty() bool {
	return len(p.Set) == 0 && len(p.Remove) == 0
}

// PlanXattrs says what a copy holding have must be given to hold the kept attributes of want: those it lacks or holds
// with another value, and those want does not hold. What Atoll does not keep is never set nor removed.
func PlanXattrs(want, have map[string][]byte) XattrPlan {
	plan := XattrPlan{Set: []string{}, Remove: []string{}}
	for name, value := range want {
		if old, ok := have[name]; KeptXattr(name) && (!ok || !bytes.Equal(old, value)) {
			plan.Set = append(plan.Set, name)
		}
	}
	for name := range have {
		if _, ok := want[name]; KeptXattr(name) && !ok {
			plan.Remove = append(plan.Remove, name)
		}
	}
	slices.Sort(plan.Set)
	slices.Sort(plan.Remove)
	return plan
}
