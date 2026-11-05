// Package schema holds the language-neutral schema of the configuration (specs.md
// §24, §29.3). The JSON file is the contract; this package only embeds it, as Go can
// embed a file from its own folder and below.
package schema

import _ "embed" // for the go:embed below

//go:embed config.schema.json
var configSchema []byte

// Config returns the JSON Schema of the configuration. The caller must not change it.
func Config() []byte {
	return configSchema
}
