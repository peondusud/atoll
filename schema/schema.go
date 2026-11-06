// Package schema holds the language-neutral schemas of the configuration and the catalog
// (specs.md §9, §24, §29.3). The files are the contract; this package only embeds them, as Go
// can embed a file from its own folder and below.
package schema

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
)

//go:embed config.schema.json
var configSchema []byte

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Config returns the JSON Schema of the configuration. The caller must not change it.
func Config() []byte {
	return configSchema
}

// Migration is one numbered SQL file of the catalog schema.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the catalog migrations in order. An error means that a file breaks the
// rules of schema/README.md: a name that is not NNNN_name.sql, or numbers that are not
// consecutive from 1.
func Migrations() ([]Migration, error) {
	return readMigrations(migrationFiles)
}

func readMigrations(files fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("catalog migrations: %w", err)
	}
	named := regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)
	list := make([]Migration, 0, len(entries))
	for _, e := range entries {
		match := named.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("catalog migration %q: want NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("catalog migration %q: %w", e.Name(), err)
		}
		text, err := fs.ReadFile(files, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("catalog migration %q: %w", e.Name(), err)
		}
		list = append(list, Migration{Version: version, Name: e.Name(), SQL: string(text)})
	}
	for i, m := range list {
		if m.Version != i+1 {
			return nil, fmt.Errorf("catalog migration %q: number %d, want %d", m.Name, m.Version, i+1)
		}
	}
	return list, nil
}
