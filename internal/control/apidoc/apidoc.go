// SPDX-License-Identifier: AGPL-3.0-only

// Package apidoc serves the OpenAPI document for the API's REST paths
// (docs/adr/0015-rest-and-openapi-from-the-schema.md).
//
// openapi.yaml is a copy of gen/openapi/openapi.yaml, which make generate
// writes from proto/ and then copies here, as go:embed can't reach outside
// this directory. Like the schema it comes from, the document is licensed
// under Apache-2.0 (docs/adr/0012-apache-for-api-schema-and-clients.md).
package apidoc

import (
	"bytes"
	_ "embed"
	"net/http"
	"time"
)

//go:embed openapi.yaml
var document []byte

// Path is where the document is served. It needs no session: the schema is
// public, and the same for every install of a version.
const Path = "/v1/openapi.yaml"

// Document returns the OpenAPI document, as YAML.
func Document() []byte { return bytes.Clone(document) }

// Handler serves the document.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "openapi.yaml", time.Time{}, bytes.NewReader(document))
	})
}
