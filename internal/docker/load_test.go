// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// archive is an image archive, as docker save writes one, naming tags.
func archive(t *testing.T, tags ...string) *bytes.Buffer {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","config":{}}`)
	manifest, err := json.Marshal([]map[string]any{{"Config": "c.json", "RepoTags": tags, "Layers": []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range map[string][]byte{"c.json": config, "manifest.json": manifest} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

// What Docker loaded is said by name, so a build can check it is what it named.
func TestLoadImageSaysWhatItLoaded(t *testing.T) {
	e := dockertest.New(t)
	got, err := e.Client().LoadImage(t.Context(), archive(t, "dockwell.invalid/svc_web:abc-def", "nginx:1.27"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"dockwell.invalid/svc_web:abc-def", "nginx:1.27"}) {
		t.Errorf("LoadImage says it loaded %q", got)
	}
}
