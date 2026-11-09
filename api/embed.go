// Package api holds the OpenAPI description of the HTTP interface and embeds
// it so the binary can serve it at /openapi.yaml.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.0 document describing the service.
//
//go:embed openapi.yaml
var OpenAPI []byte
