// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"archive/tar"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

func (e *Engine) saveImage(w http.ResponseWriter, r *http.Request) {
	_, name, _ := strings.Cut(r.URL.Path, "/images/")
	ref := strings.TrimSuffix(name, "/get")
	e.mu.Lock()
	image := e.image(ref)
	e.mu.Unlock()
	if image == nil || len(image.archiveConfig) == 0 || len(image.archiveLayers) > 1 {
		fail(w, http.StatusNotFound, "no saved archive fixture for that image")
		return
	}
	config := strings.TrimPrefix(image.id, "sha256:") + ".json"
	var layers []string
	var file *os.File
	if len(image.archiveLayers) > 0 {
		var err error
		file, err = os.Open(e.ArchiveLayer)
		if err != nil {
			fail(w, http.StatusNotFound, "no layer fixture")
			return
		}
		defer file.Close()
		layers = []string{"layer.tar"}
	}
	data, err := json.Marshal([]map[string]any{{"Config": config, "Layers": layers}})
	if err != nil {
		fail(w, http.StatusInternalServerError, "cannot encode manifest")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	for _, entry := range []struct {
		name string
		data []byte
	}{{config, image.archiveConfig}, {"manifest.json", data}} {
		if tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.data))}) != nil {
			return
		}
		if _, err := tw.Write(entry.data); err != nil {
			return
		}
	}
	if file != nil {
		st, err := file.Stat()
		if err != nil || tw.WriteHeader(&tar.Header{Name: "layer.tar", Mode: 0o600, Size: st.Size()}) != nil {
			return
		}
		if _, err := io.Copy(tw, file); err != nil {
			return
		}
	}
	_ = tw.Close()
}
