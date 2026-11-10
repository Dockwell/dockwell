// SPDX-License-Identifier: AGPL-3.0-only

package apidoc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v4"
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

// The routes that stream a file's bytes say so, for any client: an upload takes
// bytes, and a download answers them, and neither is JSON.
func TestTheStreamingRoutesSayTheyStream(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Stream      string `yaml:"x-dockwell-stream"`
			RequestBody struct {
				Content map[string]struct {
					Schema struct{ Type, Format string } `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"requestBody"`
			Responses map[string]struct {
				Headers map[string]any `yaml:"headers"`
				Content map[string]struct {
					Schema struct{ Type, Format string } `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(Document(), &doc); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			switch op.OperationID {
			case "FileService_UploadFile":
				found[op.OperationID] = true
				body := op.RequestBody.Content["application/octet-stream"].Schema
				if method != "put" || op.Stream != "request" || body.Format != "binary" || len(op.RequestBody.Content) != 1 {
					t.Errorf("%s %s says %+v", method, path, op)
				}
			case "FileService_DownloadFile":
				found[op.OperationID] = true
				ok := op.Responses["200"]
				if method != "get" || op.Stream != "response" || ok.Content["application/octet-stream"].Schema.Format != "binary" ||
					ok.Content["application/zip"].Schema.Format != "binary" || ok.Headers["Content-Disposition"] == nil {
					t.Errorf("%s %s says %+v", method, path, op)
				}
			}
		}
	}
	if len(found) != 2 {
		t.Errorf("found %v, want both streaming routes", found)
	}
}
