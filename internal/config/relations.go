package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/value"
)

// list returns the tables of a list of tables. The schema has already checked them.
func list(tree map[string]any, key string) []map[string]any {
	raw, _ := tree[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		t, _ := v.(map[string]any)
		out = append(out, t)
	}
	return out
}

func text(t map[string]any, key string) string {
	s, _ := t[key].(string)
	return s
}

func stringList(t map[string]any, key string) []string {
	raw, _ := t[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// checkRelations applies step 3 of specs.md §24.3, on values the schema has checked,
// and fills the typed fields of c.
// checkAPI refuses HTTP over TCP with no token to ask for (specs.md §25.1).
func checkAPI(tree map[string]any, found *problems) {
	api := table(tree, "api")
	if text(api, "listen") != "" && text(api, "token_file") == "" {
		found.add("api.token_file", "api.listen needs a token file: HTTP over TCP asks every request for its token")
	}
}

func checkRelations(tree, user map[string]any, c *Config, found *problems) {
	checkDisks(tree, c, found)
	checkSpread("placement.spread", table(tree, "placement"), table(tree, "placement"), found)
	if ups := table(tree, "ups"); ups["enabled"] == true && text(ups, "nut") == "" {
		found.add("ups.nut", "ups.enabled needs a NUT address")
	}
	checkAPI(tree, found)
	checkRules(tree, user, c, found)
}

func checkDisks(tree map[string]any, c *Config, found *problems) {
	layout := table(tree, "layout")
	if text(layout, "root") == text(layout, "workdir") {
		found.add("layout.workdir", "the same name as layout.root")
	}
	disks := list(tree, "disk")
	seen := map[string]bool{}
	for i, d := range disks {
		key := indexKey("disk", i)
		for _, field := range []string{"id", "uuid", "mount"} {
			mark := field + "=" + text(d, field)
			if seen[mark] {
				found.add(key+"."+field, "another disk has %s", text(d, field))
			}
			seen[mark] = true
		}
		fs := names.Filesystem(text(d, "fs"))
		if !uuidFits(text(d, "uuid"), fs) {
			found.add(key+".uuid", "the wrong form for %s: a UUID in lowercase, or 16 uppercase hexadecimal digits on ntfs", fs)
		}
		root, workdir := checkFolders(key, d, layout, fs, found)
		c.Disks = append(c.Disks, Disk{
			ID: text(d, "id"), UUID: text(d, "uuid"), Mount: text(d, "mount"), FS: fs,
			Role: text(d, "role"), Domain: text(d, "domain"), Tags: stringList(d, "tags"),
			Root: root, Workdir: workdir, State: text(d, "state"),
		})
	}
	checkMounts(tree, disks, found)
	// Only now: checkFolders needs to know which names the disk set itself.
	for i, d := range disks {
		d["root"], d["workdir"] = c.Disks[i].Root, c.Disks[i].Workdir
	}
}

// uuidFits checks the form of an identifier for its filesystem. The schema has
// checked that it is one of the two forms.
func uuidFits(uuid string, fs names.Filesystem) bool {
	return (len(uuid) == 16) == (fs == names.NTFS)
}

// checkFolders checks a disk's root and workdir, and returns them.
func checkFolders(key string, d, layout map[string]any, fs names.Filesystem, found *problems) (string, string) {
	root, rootKey := text(layout, "root"), "layout.root"
	workdir, workdirKey := text(layout, "workdir"), "layout.workdir"
	ownRoot, ownWorkdir := false, false
	if v, ok := d["root"].(string); ok {
		root, rootKey, ownRoot = v, key+".root", true
	}
	if v, ok := d["workdir"].(string); ok {
		workdir, workdirKey, ownWorkdir = v, key+".workdir", true
	}

	for _, f := range []struct{ name, key string }{{root, rootKey}, {workdir, workdirKey}} {
		reasons, err := names.ComponentReasons(f.name, fs)
		switch {
		case err != nil:
			found.add(f.key, "%v", err)
		case len(reasons) > 0:
			found.add(f.key, "%s refuses this name: %v", fs, reasons)
		}
	}
	same := root == workdir
	if fs == names.NTFS {
		a, okA := names.CollisionKey(root)
		b, okB := names.CollisionKey(workdir)
		same = same || okA && okB && a == b
	}
	if same {
		at := workdirKey
		if ownRoot && !ownWorkdir {
			at = rootKey
		}
		found.add(at, "root and workdir are the same name on this disk")
	}
	return root, workdir
}

// checkMounts checks that no mount, view.mount, view.readonly_mount or catalog sits inside another one.
func checkMounts(tree map[string]any, disks []map[string]any, found *problems) {
	for i, d := range disks {
		mount := text(d, "mount")
		for j, other := range disks {
			if i != j && inside(mount, text(other, "mount")) {
				found.add(indexKey("disk", i)+".mount", "inside the mount of disk %s", text(other, "id"))
			}
		}
	}
	view := text(table(tree, "view"), "mount")
	readonly := text(table(tree, "view"), "readonly_mount")
	catalog := text(tree, "catalog")
	if inside(catalog, view) || catalog == view {
		found.add("catalog", "inside view.mount")
	}
	mounts := make([]diskMount, 0, len(disks))
	for _, d := range disks {
		mount := text(d, "mount")
		mounts = append(mounts, diskMount{id: text(d, "id"), mount: mount})
		if overlaps(view, mount) {
			found.add("view.mount", "on, inside or around the mount of disk %s", text(d, "id"))
		}
		if catalog == mount || inside(catalog, mount) {
			found.add("catalog", "inside the mount of disk %s", text(d, "id"))
		}
	}
	if readonly != "" {
		checkReadonlyMount(readonly, view, catalog, mounts, found)
	}
}

// diskMount is a disk's id and mount.
type diskMount struct{ id, mount string }

// checkReadonlyMount checks view.readonly_mount against view.mount, the disks' mounts and the catalog (specs.md
// §24.3).
func checkReadonlyMount(readonly, view, catalog string, mounts []diskMount, found *problems) {
	if inside(catalog, readonly) || catalog == readonly {
		found.add("catalog", "inside view.readonly_mount")
	}
	if overlaps(readonly, view) {
		found.add("view.readonly_mount", "on, inside or around view.mount")
	}
	for _, d := range mounts {
		if overlaps(readonly, d.mount) {
			found.add("view.readonly_mount", "on, inside or around the mount of disk %s", d.id)
		}
	}
}

// CheckReadonlyMount checks a point given to atoll mount on its command line as view.readonly_mount would be: an
// absolute path, apart from view.mount, from the disks' mounts and from the catalog (specs.md §21.2, §24.3).
func CheckReadonlyMount(c *Config, point string) error {
	found := &problems{}
	if err := checkAbsolutePath(point); err != nil {
		found.add("view.readonly_mount", "%s", err)
		return found.err()
	}
	view, _ := c.effective["view.mount"].(string)
	catalog, _ := c.effective["catalog"].(string)
	mounts := make([]diskMount, 0, len(c.Disks))
	for _, d := range c.Disks {
		mounts = append(mounts, diskMount{id: d.ID, mount: d.Mount})
	}
	checkReadonlyMount(point, view, catalog, mounts, found)
	return found.err()
}

// overlaps reports whether two paths are the same, or one is inside the other.
func overlaps(a, b string) bool {
	return a == b || inside(a, b) || inside(b, a)
}

// inside reports whether path is strictly below parent, by whole components.
func inside(path, parent string) bool {
	return strings.HasPrefix(path, parent+"/")
}

// checkSpread refuses spread = "none" with spread_mode = "strict", where a rule (or
// [placement]) reads its values from t and then from pool.
func checkSpread(spreadKey string, t, pool map[string]any, found *problems) {
	read := func(key string) string {
		if v, ok := t[key].(string); ok {
			return v
		}
		return text(pool, key)
	}
	if read("spread") == "none" && read("spread_mode") == "strict" {
		found.add(spreadKey, `spread = "none" cannot go with spread_mode = "strict"`)
	}
}

func checkRules(tree, user map[string]any, c *Config, found *problems) {
	placement := table(tree, "placement")
	seen := map[string]bool{}
	for i, r := range list(tree, "rule") {
		key := indexKey("rule", i)
		label := text(r, "label")
		if seen[label] {
			found.add(key+".label", "another rule has this label")
		}
		seen[label] = true

		_, setSpread := r["spread"]
		_, setMode := r["spread_mode"]
		switch {
		case setSpread:
			checkSpread(key+".spread", r, placement, found)
		case setMode:
			checkSpread(key+".spread_mode", r, placement, found)
		}
		if r["action"] == "ignore" {
			for _, p := range ruleProperties() {
				if _, ok := r[p]; ok {
					found.add(key+"."+p, `action = "ignore" takes no other property`)
				}
			}
		}
		checkReadBeforeSize(key, r, found)
		if r["tags_mode"] == "strict" && len(stringList(r, "tags")) == 0 && len(stringList(r, "domains")) == 0 {
			found.add(key+".tags_mode", `"strict" with neither tags nor domains limits nothing`)
		}
		compiled := compileRule(key, label, r, found)
		_, compiled.ActionSet = user[key+".action"]
		c.Rules = append(c.Rules, compiled)
	}
}

// checkReadBeforeSize refuses what is read before a file's size is known on a rule that
// only matches once it is, so it could never apply (specs.md §11.3).
func checkReadBeforeSize(key string, r map[string]any, found *problems) {
	_, smallest := r["size_min"]
	_, largest := r["size_max"]
	if !smallest && !largest {
		return
	}
	for _, p := range []string{"settle", "cache"} {
		if _, ok := r[p]; ok {
			found.add(key+"."+p, "read before the size is known, so it cannot go with size_min or size_max")
		}
	}
}

// ruleProperties lists what a rule may set beyond its conditions and label, action
// aside (specs.md §11.2).
func ruleProperties() []string {
	return []string{
		"replicas", "mode", "settle", "priority", "tags", "tags_mode", "domains",
		"spread", "spread_mode", "colocate", "overflow", "cache", "cache_min_age",
	}
}

func compileRule(key, label string, r map[string]any, found *problems) rules.Rule {
	action, _ := r["action"].(string)
	out := rules.Rule{Label: label, Action: rules.Action(action), Set: ruleSet(r)}
	fold, _ := r["ignore_case"].(bool)
	pattern := func(field rules.Field, name string) *rules.Condition {
		s, ok := r[name].(string)
		if !ok {
			return nil
		}
		cond, err := rules.Compile(field, s, fold)
		if err != nil {
			found.add(key+"."+name, "%v", err)
		}
		return cond
	}
	out.Path = pattern(rules.FieldPath, "path")
	out.Filename = pattern(rules.FieldFilename, "filename")
	if _, ok := r["ext"]; ok {
		cond, err := rules.CompileExt(stringList(r, "ext"), fold)
		if err != nil {
			found.add(key+".ext", "%v", err)
		}
		out.Ext = cond
	}
	size := func(name string) *int64 {
		s, ok := r[name].(string)
		if !ok {
			return nil
		}
		n, err := value.ParseSize(s)
		if err != nil { // the schema has checked it
			found.add(key+"."+name, "%v", err)
			return nil
		}
		return &n
	}
	out.SizeMin, out.SizeMax = size("size_min"), size("size_max")
	if out.SizeMin != nil && out.SizeMax != nil && *out.SizeMin > *out.SizeMax {
		found.add(key+".size_min", "above size_max")
	}
	return out
}

// ruleSet reads what a rule sets. The schema has checked these values.
func ruleSet(r map[string]any) rules.Properties {
	return rules.Properties{
		Replicas:    intOrNil(r, "replicas"),
		Mode:        textOrNil(r, "mode"),
		Settle:      durationOrNil(r, "settle"),
		Priority:    intOrNil(r, "priority"),
		Tags:        listOrNil(r, "tags"),
		TagsMode:    textOrNil(r, "tags_mode"),
		Domains:     listOrNil(r, "domains"),
		Spread:      textOrNil(r, "spread"),
		SpreadMode:  textOrNil(r, "spread_mode"),
		Colocate:    textOrNil(r, "colocate"),
		Overflow:    textOrNil(r, "overflow"),
		Cache:       boolOrNil(r, "cache"),
		CacheMinAge: durationOrNil(r, "cache_min_age"),
	}
}

func intOrNil(t map[string]any, key string) *int {
	v, ok := t[key].(int64)
	if !ok {
		return nil
	}
	n := int(v)
	return &n
}

func textOrNil(t map[string]any, key string) *string {
	v, ok := t[key].(string)
	if !ok {
		return nil
	}
	return &v
}

func boolOrNil(t map[string]any, key string) *bool {
	v, ok := t[key].(bool)
	if !ok {
		return nil
	}
	return &v
}

func durationOrNil(t map[string]any, key string) *time.Duration {
	s, ok := t[key].(string)
	if !ok {
		return nil
	}
	d, _ := value.ParseDuration(s)
	return &d
}

func listOrNil(t map[string]any, key string) []string {
	if _, ok := t[key]; !ok {
		return nil
	}
	return stringList(t, key)
}

// buildPool reads the pool-wide values of rule properties (specs.md §24.2), from the
// tree with its defaults, and where each comes from: user holds what the file sets,
// and set what the file or the preset sets.
func buildPool(tree, user, set map[string]any) rules.Pool {
	defaults, placement, cache := table(tree, "defaults"), table(tree, "placement"), table(tree, "cache")
	sources := map[string]rules.Source{}
	for name, key := range poolKeys() {
		_, inFile := user[key]
		_, inSet := set[key]
		switch {
		case inFile:
			sources[name] = rules.FromFile
		case inSet:
			sources[name] = rules.FromPreset
		default:
			sources[name] = rules.FromDefault
		}
	}
	return rules.Pool{
		Replicas:    *intOrNil(defaults, "replicas"),
		Mode:        text(defaults, "mode"),
		Settle:      *durationOrNil(defaults, "settle"),
		Priority:    *intOrNil(defaults, "priority"),
		TagsMode:    text(placement, "tags_mode"),
		Spread:      text(placement, "spread"),
		SpreadMode:  text(placement, "spread_mode"),
		Colocate:    text(placement, "colocate"),
		Overflow:    text(placement, "overflow"),
		CacheMinAge: *durationOrNil(cache, "min_age"),
		Sources:     sources,
	}
}

// poolKeys gives the key of each rule property that has a pool-wide value (specs.md
// §24.2).
func poolKeys() map[string]string {
	return map[string]string{
		"replicas":      "defaults.replicas",
		"mode":          "defaults.mode",
		"settle":        "defaults.settle",
		"priority":      "defaults.priority",
		"tags_mode":     "placement.tags_mode",
		"spread":        "placement.spread",
		"spread_mode":   "placement.spread_mode",
		"colocate":      "placement.colocate",
		"overflow":      "placement.overflow",
		"cache_min_age": "cache.min_age",
	}
}

// noEffect warns about what the file or the preset set and what else it set makes of no effect (specs.md §24.4):
// read.classes with read.policy = "oldest", which reads the original alone (§7).
func noEffect(tree map[string]any, set map[string]any) []Problem {
	var out []Problem
	if _, classes := set["read.classes"]; classes && text(table(tree, "read"), "policy") == "oldest" {
		out = append(out, Problem{Key: "read.classes", Message: `no effect with read.policy = "oldest", which reads the original alone`})
	}
	// The SSD cache needs Atoll's own view: mergerfs cannot apply a rule when a file is created (specs.md §19).
	if text(table(tree, "view"), "engine") == "mergerfs" {
		const needsOwnView = `no effect with view.engine = "mergerfs": the SSD cache needs Atoll's own view`
		if set["cache.enabled"] == true {
			out = append(out, Problem{Key: "cache.enabled", Message: needsOwnView})
		}
		for i, d := range list(tree, "disk") {
			if key := indexKey("disk", i) + ".role"; set[key] == "cache" && text(d, "role") == "cache" {
				out = append(out, Problem{Key: key, Message: needsOwnView})
			}
		}
	}
	return out
}

// unreadableUPS warns about a UPS the daemon is to read at an address NUT cannot have, as a port past 65535: the
// schema takes it, and so does a configuration of format version 1, so it loads, and the daemon finds the UPS
// unreachable (specs.md §20, §24.4).
func unreadableUPS(tree map[string]any) []Problem {
	ups := table(tree, "ups")
	nut := text(ups, "nut")
	if ups["enabled"] != true || nut == "" {
		return nil
	}
	if _, err := value.ParseNUT(nut); err != nil {
		return []Problem{{Key: "ups.nut", Message: fmt.Sprintf("the daemon cannot read a UPS there: %v", err)}}
	}
	return nil
}

// unknownLabels warns about tags and domains no disk carries (specs.md §24.4).
func unknownLabels(tree map[string]any) []Problem {
	tags, domains := map[string]bool{}, map[string]bool{}
	for _, d := range list(tree, "disk") {
		domains[text(d, "domain")] = true
		for _, tag := range stringList(d, "tags") {
			tags[tag] = true
		}
	}
	var out []Problem
	warn := func(key string, values []string, known map[string]bool, what string) {
		for i, v := range values {
			if !known[v] {
				out = append(out, Problem{Key: indexKey(key, i), Message: fmt.Sprintf("no disk has the %s %q", what, v)})
			}
		}
	}
	warn("read.classes", stringList(table(tree, "read"), "classes"), tags, "tag")
	for i, r := range list(tree, "rule") {
		key := indexKey("rule", i)
		warn(key+".tags", stringList(r, "tags"), tags, "tag")
		warn(key+".domains", stringList(r, "domains"), domains, "domain")
	}
	return out
}
