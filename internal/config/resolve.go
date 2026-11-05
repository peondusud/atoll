package config

// table returns the table at name, created if needed. The schema has already
// checked that a value there is a table.
func table(tree map[string]any, name string) map[string]any {
	t, ok := tree[name].(map[string]any)
	if !ok {
		t = map[string]any{}
		tree[name] = t
	}
	return t
}

func setIfAbsent(t map[string]any, key string, v any) {
	if _, ok := t[key]; !ok {
		t[key] = v
	}
}

// applyPreset sets what the strategy preset sets, where the file does not (specs.md
// §12.2). It comes before the defaults of the schema, so the file wins over the
// preset and the preset over the default.
func applyPreset(tree map[string]any) {
	strategy, _ := tree["strategy"].(string)
	switch strategy {
	case "safety":
		setIfAbsent(table(tree, "defaults"), "replicas", int64(1))
		setIfAbsent(table(tree, "placement"), "spread_mode", "strict")
		setIfAbsent(table(tree, "defaults"), "mode", "immediate")
		setIfAbsent(table(tree, "balance"), "schedule", "immediate")
	case "energy":
		setIfAbsent(table(tree, "defaults"), "replicas", int64(1))
		setIfAbsent(table(tree, "placement"), "new_files", "ordered")
		setIfAbsent(table(tree, "placement"), "colocate", "season")
		setIfAbsent(table(tree, "defaults"), "mode", "scheduled")
		if hasCacheDisk(tree) {
			setIfAbsent(table(tree, "cache"), "enabled", true)
		}
	case "capacity":
		setIfAbsent(table(tree, "defaults"), "replicas", int64(0))
		setIfAbsent(table(tree, "placement"), "new_files", "most-free")
	}
}

func hasCacheDisk(tree map[string]any) bool {
	disks, _ := tree["disk"].([]any)
	for _, d := range disks {
		if t, ok := d.(map[string]any); ok && t["role"] == "cache" {
			return true
		}
	}
	return false
}

// followMaintenanceWindow gives maintenance.window to the keys that follow it, where
// neither the file nor the preset sets them (specs.md §24.2). The window has no
// default of its own here: it is read from the schema with the others, so it is the
// schema's default that this needs when the file does not set one.
func followMaintenanceWindow(tree map[string]any) {
	window, ok := table(tree, "maintenance")["window"]
	if !ok {
		window = defaultWindow
	}
	setIfAbsent(table(tree, "replication"), "window", window)
	setIfAbsent(table(tree, "integrity"), "window", window)
	setIfAbsent(table(tree, "balance"), "schedule", window)
}

// defaultWindow is the default of maintenance.window in the schema, which
// TestDefaultWindow compares.
const defaultWindow = "02:00-06:00"
