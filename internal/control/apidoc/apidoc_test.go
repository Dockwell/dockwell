// SPDX-License-Identifier: AGPL-3.0-only

package apidoc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The embedded copy must be the document make generate wrote; check-generated
// catches the same in CI, and this catches it in go test.
func TestDocumentIsTheGeneratedOne(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "gen", "openapi", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Document(), want) {
		t.Error("internal/control/apidoc/openapi.yaml differs from gen/openapi/openapi.yaml. Run make generate, then commit the result.")
	}
	if !bytes.HasPrefix(bytes.TrimLeft(bytes.SplitN(Document(), []byte("\nopenapi: "), 2)[1], " "), []byte("3.")) {
		t.Error("the document isn't OpenAPI 3")
	}
}
