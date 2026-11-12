// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
)

// MediaRoute is the pattern the control plane serves sources' images at, as
// MediaPath writes their paths.
const MediaRoute = "GET " + MediaPrefix + "{key}/{commit}/{image}"

var (
	mediaKey    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	mediaCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Media serves an image of a source's commit from where the control plane keeps the
// commits of the sources applied, never from a plan's, at MediaPath. An image is a
// stranger's file, so it's served as what its first bytes say it is, a WebP, PNG or
// JPEG image alone, which no browser may take for anything else, and as nothing a
// browser would run if it's opened on its own. authorize refuses whoever may not read
// the catalog, as a request reaches the control plane with a session's cookie.
func (s *Sources) Media(authorize func(*http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, commit, image := r.PathValue("key"), r.PathValue("commit"), r.PathValue("image")
		if !mediaKey.MatchString(key) || !mediaCommit.MatchString(commit) || !folder.ImageName(image) {
			http.NotFound(w, r)
			return
		}
		if authorize != nil {
			if err := authorize(r); err != nil {
				http.Error(w, "Sign in to see the catalog's images.", http.StatusUnauthorized)
				return
			}
		}
		data, at, err := s.readImage(key, commit, image)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		kind, err := folder.Image(image, data)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", kind)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		// The path names the commit, whose files never change.
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeContent(w, r, "", at, bytes.NewReader(data))
	})
}

// readImage reads an image a source's commit keeps, through os.Root, so no name
// reaches out of the control plane's folder of sources, or for Dockwell/catalog's
// commit this build holds, from the snapshot.
func (s *Sources) readImage(key, commit, image string) ([]byte, time.Time, error) {
	if own := snapshotSpec(); key == keyOf(own) && commit == own.GetCommit() {
		data, err := fs.ReadFile(snapshot.Entries(), folder.Media+"/"+image)
		return data, time.Time{}, err
	}
	if s.Dir == "" {
		return nil, time.Time{}, os.ErrNotExist
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer root.Close()
	f, err := root.Open("kept/" + key + "/" + commit + "/" + folder.Media + "/" + image)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, time.Time{}, os.ErrNotExist
	}
	data, err := io.ReadAll(io.LimitReader(f, folder.MediaLimits.File+1))
	if err != nil || int64(len(data)) > folder.MediaLimits.File {
		return nil, time.Time{}, os.ErrNotExist
	}
	return data, info.ModTime(), nil
}
