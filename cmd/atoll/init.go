package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

type initArgs struct {
	poolID, viewMount, catalog *string
	asJSON                     bool
}

// parseInit reads the arguments of init (specs.md §25).
func parseInit(args []string) (initArgs, error) {
	var out initArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		var target **string
		switch name {
		case "--pool-id":
			target = &out.poolID
		case "--view-mount":
			target = &out.viewMount
		case "--catalog":
			target = &out.catalog
		default:
			return out, fmt.Errorf(T("unknown argument %q"), a)
		}
		if !hasValue {
			i++
			if i == len(args) {
				return out, fmt.Errorf(T("%s needs a value"), name)
			}
			value = args[i]
		}
		if *target != nil {
			return out, fmt.Errorf(T("%s comes twice"), name)
		}
		*target = &value
	}
	if (out.poolID == nil) != (out.viewMount == nil) {
		return out, errors.New(T("--pool-id and --view-mount come together"))
	}
	if out.catalog != nil && out.poolID == nil {
		return out, errors.New(T("--catalog needs --pool-id and --view-mount"))
	}
	return out, nil
}

// configText is the configuration init writes. It is checked before anything is written, so that
// what the configuration would refuse is a usage error.
func (a *initArgs) configText() (string, error) {
	var text strings.Builder
	fmt.Fprintf(&text, "pool_id = %s\n", tomlString(*a.poolID))
	if a.catalog != nil {
		fmt.Fprintf(&text, "catalog = %s\n", tomlString(*a.catalog))
	}
	fmt.Fprintf(&text, "\n[view]\nmount = %s\n", tomlString(*a.viewMount))
	if _, err := config.Parse([]byte(text.String())); err != nil {
		return "", fmt.Errorf("%w", err)
	}
	return text.String(), nil
}

// tomlString writes a basic string of TOML, whose escapes are not those of Go's %q.
func tomlString(s string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, c := range []byte(s) {
		switch {
		case c == '"' || c == '\\':
			out.WriteByte('\\')
			out.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&out, `\u%04X`, c)
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// initOutput is the output of api/schemas/init.schema.json.
type initOutput struct {
	Configuration string `json:"configuration"`
	Catalog       string `json:"catalog"`
}

func runInit(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseInit(args)
	if err != nil {
		return usage(stderr, "init: "+err.Error())
	}
	text := ""
	if parsed.poolID != nil {
		if text, err = parsed.configText(); err != nil {
			return usage(stderr, "init: "+err.Error())
		}
	}
	out, err := initialize(configPath, text)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, fmt.Sprintf(T("configuration: %s\ncatalog: %s\n"), out.Configuration, out.Catalog))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// initialize creates what is missing. text is the configuration to write, or empty.
func initialize(configPath, text string) (initOutput, error) {
	out := initOutput{Configuration: "kept", Catalog: "kept"}
	exists, err := fsx.Exists(configPath)
	if err != nil {
		return out, fmt.Errorf("configuration: %w", err)
	}
	switch {
	case exists && text != "":
		return out, fmt.Errorf(T("the configuration %q already exists: --pool-id, --view-mount and --catalog are for a new one"), configPath)
	case !exists && text == "":
		return out, fmt.Errorf(T("no configuration at %q: give --pool-id and --view-mount to write one"), configPath)
	case !exists:
		if err := fsx.MkdirAll(path.Dir(configPath)); err != nil {
			return out, fmt.Errorf("configuration: %w", err)
		}
		if err := fsx.WriteNew(configPath, []byte(text)); err != nil {
			return out, fmt.Errorf("configuration: %w", err)
		}
		out.Configuration = "created"
	}
	c, err := load(configPath)
	if err != nil {
		return out, err
	}
	created, err := catalog.Create(context.Background(), catalogPath(c), c.PoolID)
	if err != nil {
		return out, fmt.Errorf("%w", err)
	}
	if created {
		out.Catalog = "created"
	}
	return out, nil
}

func catalogPath(c *config.Config) string {
	file, _ := c.Effective()["catalog"].(string)
	return file
}
