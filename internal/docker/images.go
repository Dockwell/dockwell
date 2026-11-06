// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Image is an image as the engine reports it.
type Image struct {
	ID     string  `json:"Id"`
	Config *Config `json:"Config"`
	// RepoDigests are what its registries call it, such as nginx@sha256:ab12….
	// An image built here has none.
	RepoDigests []string `json:"RepoDigests"`
	// Architecture is what it's built for, such as amd64.
	Architecture string `json:"Architecture"`
}

// Digest says what a registry calls an image a reference names, or nothing for
// one none does: the reference's own digest, or its image's for that repository.
func (i *Image) Digest(ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return digest
	}
	want := repository(ref)
	for _, known := range i.RepoDigests {
		if name, digest, ok := strings.Cut(known, "@"); ok && repository(name) == want {
			return digest
		}
	}
	return ""
}

// repository is a reference's name, without its tag, or the docker.io/library/
// that a name from Docker Hub can be written with.
func repository(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return strings.TrimPrefix(strings.TrimPrefix(name, "docker.io/"), "library/")
}

// InspectImage returns an image by reference or ID; a missing one is an
// error that IsNotFound reports.
func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	var out Image
	if err := c.do(ctx, http.MethodGet, "/images/"+escRef(ref)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Config == nil {
		out.Config = &Config{}
	}
	return &out, nil
}

// CheckPullable asks the image's registry for it, through the engine, without
// pulling it or sending any credentials, as a pull doesn't either. It is an
// error that NotPullable reports when the registry won't give it to a pull.
func (c *Client) CheckPullable(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodGet, "/distribution/"+escRef(WithTag(ref))+"/json", nil, nil, &struct{}{})
}

// NotPullable reports what CheckPullable returned for an image a pull can't
// get: its registry doesn't have it, or wants credentials for it, which the
// agent doesn't send, and a registry won't say which. The engine reports some of
// these as a server error, so its words count too. A registry that is down, slow
// or limiting how often it's asked says nothing about the image, so none of
// those count.
func NotPullable(err error) bool {
	if err == nil || IsUnavailable(err) {
		return false
	}
	switch status(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	text := strings.ToLower(err.Error())
	for _, says := range []string{"manifest unknown", "repository does not exist", "pull access denied", "requested access to the resource is denied", "authentication required"} {
		if strings.Contains(text, says) {
			return true
		}
	}
	return false
}

// escRef escapes an image reference for a path, part by part: the engine
// takes the slashes between a reference's parts as they are.
func escRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = esc(p)
	}
	return strings.Join(parts, "/")
}

// WithTag returns ref with ":latest" when it names neither a tag nor a
// digest, as the engine pulls every tag of an image named without one.
func WithTag(ref string) string {
	last := ref[strings.LastIndex(ref, "/")+1:]
	if strings.Contains(last, ":") || strings.Contains(ref, "@") {
		return ref
	}
	return ref + ":latest"
}

// pullEvent is one line of a pull's progress.
type pullEvent struct {
	Status         string `json:"status"`
	ID             string `json:"id"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// progressEvery is how often a pull reports how far it is.
const progressEvery = time.Second

// PullImage pulls an image, and tells progress how far it is, at most
// once a second, such as "12 of 48 MB".
func (c *Client) PullImage(ctx context.Context, ref string, progress func(string)) error {
	ref = WithTag(ref)
	path, err := c.versioned(ctx, "/images/create")
	if err != nil {
		return err
	}
	res, err := c.send(ctx, http.MethodPost, path, url.Values{"fromImage": {ref}}, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	defer discard(res.Body)
	layers := map[string][2]int64{} // id → current, total
	last := time.Time{}
	dec := json.NewDecoder(res.Body)
	for {
		var ev pullEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("can't read the progress of pulling %s: %w", ref, err)
		}
		if ev.Error != "" {
			msg := ev.ErrorDetail.Message
			if msg == "" {
				msg = ev.Error
			}
			return &Error{Status: http.StatusInternalServerError, Message: msg}
		}
		if ev.ID == "" {
			continue
		}
		cur := layers[ev.ID]
		switch ev.Status {
		case "Downloading":
			if ev.ProgressDetail.Total > 0 {
				cur = [2]int64{ev.ProgressDetail.Current, ev.ProgressDetail.Total}
			}
		case "Download complete", "Pull complete", "Already exists":
			cur[0] = cur[1]
		}
		layers[ev.ID] = cur
		if progress != nil && time.Since(last) >= progressEvery {
			var done, total int64
			for _, l := range layers {
				done, total = done+l[0], total+l[1]
			}
			if total > 0 {
				progress(sizeOf(done, total))
				last = time.Now()
			}
		}
	}
}

// sizeOf words how much of a size is done, in the size's unit, such as
// "12 of 48 MB" or "1.2 of 4 GB", 1024-based with one decimal under 10.
func sizeOf(done, total int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(total)
	i, div := 0, 1.0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		div *= 1024
		i++
	}
	num := func(v float64) string {
		if v < 10 && i > 0 {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
		}
		return fmt.Sprintf("%.0f", v)
	}
	return num(float64(done)/div) + " of " + num(f) + " " + units[i]
}
