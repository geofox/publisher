// Package apidocs embeds the /v1 API's documents: the OpenAPI description
// (openapi.yaml, the source of truth) and the error catalogue (errors.md).
package apidocs

import (
	_ "embed"
	"sync"

	"github.com/goccy/go-yaml"
)

// OpenAPIYAML is the OpenAPI 3.1 description as written.
//
//go:embed openapi.yaml
var OpenAPIYAML []byte

// ErrorsMarkdown is the error catalogue that problem type URIs point into.
//
//go:embed errors.md
var ErrorsMarkdown []byte

var (
	jsonOnce sync.Once
	jsonDoc  []byte
	jsonErr  error
)

// OpenAPIJSON returns the description converted to JSON, as served at
// GET /v1/openapi.json.
func OpenAPIJSON() ([]byte, error) {
	jsonOnce.Do(func() { jsonDoc, jsonErr = yaml.YAMLToJSON(OpenAPIYAML) })
	return jsonDoc, jsonErr
}
