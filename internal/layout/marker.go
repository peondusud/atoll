package layout

import (
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/pelletier/go-toml/v2"
)

// MarkerName is the marker's name in a disk's work folder (specs.md §10).
const MarkerName = "disk.toml"

// MarkerFormat is the format_version a marker is written with, and the only one read.
const MarkerFormat = 1

// Marker is what Atoll/disk.toml says of a disk: the pool and the disk it was added as, and its filesystem.
type Marker struct {
	PoolID, DiskID, FSUUID string
}

// identifier is the grammar of a disk's id and of a pool's (specs.md §29.5), as the configuration has them.
var (
	identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	uuid       = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9A-F]{16})$`)
)

// IsIdentifier says whether s is an identifier: a disk's id, a domain or a tag.
func IsIdentifier(s string) bool {
	return identifier.MatchString(s)
}

// MarkerText writes a marker: four keys, in this order, each on its line. Every value is ASCII with no quote, so
// a basic string holds it as it is.
func MarkerText(m Marker) (string, error) {
	if !identifier.MatchString(m.PoolID) || !identifier.MatchString(m.DiskID) || !uuid.MatchString(m.FSUUID) {
		return "", fmt.Errorf("a marker of %q, %q, %q", m.PoolID, m.DiskID, m.FSUUID)
	}
	return fmt.Sprintf("format_version = %d\npool_id = %q\ndisk_id = %q\nfs_uuid = %q\n", MarkerFormat, m.PoolID, m.DiskID, m.FSUUID), nil
}

// ErrNotAMarker says that a disk.toml is not one this version reads.
var ErrNotAMarker = errors.New("not a marker this version reads")

// ParseMarker reads a marker: exactly the four keys, format_version 1, each value of its grammar.
func ParseMarker(data []byte) (Marker, error) {
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return Marker{}, fmt.Errorf("%w: %w", ErrNotAMarker, err)
	}
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"disk_id", "format_version", "fs_uuid", "pool_id"}) {
		return Marker{}, fmt.Errorf("%w: its keys are %q", ErrNotAMarker, keys)
	}
	if v, ok := tree["format_version"].(int64); !ok || v != MarkerFormat {
		return Marker{}, fmt.Errorf("%w: format_version %v", ErrNotAMarker, tree["format_version"])
	}
	m := Marker{}
	m.PoolID, _ = tree["pool_id"].(string)
	m.DiskID, _ = tree["disk_id"].(string)
	m.FSUUID, _ = tree["fs_uuid"].(string)
	if !identifier.MatchString(m.PoolID) || !identifier.MatchString(m.DiskID) || !uuid.MatchString(m.FSUUID) {
		return Marker{}, fmt.Errorf("%w: a value outside its grammar", ErrNotAMarker)
	}
	return m, nil
}
