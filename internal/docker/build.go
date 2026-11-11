// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BuiltRegistry starts the name of every image Dockwell builds. It is a name that
// never resolves (RFC 2606), so nothing pulls one: a name with no host, such as
// dockwell/web, is Docker Hub's, and a pull of it would run whatever someone
// published there (ADR 0046).
const BuiltRegistry = "dockwell.invalid"

// LabelBuilder marks a container that runs a build, so one a crash left can be
// found and removed.
const LabelBuilder = OwnPrefix + "builder"

// IsBuilt reports an image Dockwell builds, by its name.
func IsBuilt(ref string) bool { return strings.HasPrefix(ref, BuiltRegistry+"/") }

// BuiltImage is the image a build makes for a service: the service's ID, then a tag
// of the commit's first 12 digits and the change's ID, so no two builds share one,
// as ADR 0021 needs of a build on the server.
func BuiltImage(serviceID, commit, changeID string) string {
	short := commit
	if len(short) > 12 {
		short = short[:12]
	}
	return BuiltRegistry + "/" + strings.ToLower(serviceID) + ":" + strings.ToLower(short) + "-" + strings.ToLower(strings.TrimPrefix(changeID, "chg_"))
}

// BuiltRepository is the repository of a service's built images.
func BuiltRepository(serviceID string) string {
	return BuiltRegistry + "/" + strings.ToLower(serviceID)
}

// LoadImage takes an image archive, as docker save writes and BuildKit's docker
// exporter does, into the engine's images, as docker load does, and says what it
// loaded, by the names Docker says, or by ID for one with none. It reads archive as
// it comes, so nothing of it waits in memory here.
func (c *Client) LoadImage(ctx context.Context, archive io.Reader) ([]string, error) {
	path, err := c.versioned(ctx, "/images/load")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+path+"?quiet=1", archive)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := c.http.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("can't reach Docker at %s: %w", c.socket, err)
	}
	defer res.Body.Close()
	defer discard(res.Body)
	if res.StatusCode >= http.StatusBadRequest {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		var e struct{ Message string }
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &Error{Status: res.StatusCode, Message: e.Message}
	}
	var loaded []string
	dec := json.NewDecoder(res.Body)
	for {
		var ev struct {
			Stream      string `json:"stream"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return loaded, nil
			}
			return loaded, fmt.Errorf("can't read what Docker said as it loaded the image: %w", err)
		}
		if msg := ev.ErrorDetail.Message; msg != "" || ev.Error != "" {
			if msg == "" {
				msg = ev.Error
			}
			return loaded, &Error{Status: http.StatusInternalServerError, Message: msg}
		}
		for _, line := range strings.Split(ev.Stream, "\n") {
			if name, ok := strings.CutPrefix(line, "Loaded image: "); ok {
				loaded = append(loaded, strings.TrimSpace(name))
			} else if id, ok := strings.CutPrefix(line, "Loaded image ID: "); ok {
				loaded = append(loaded, strings.TrimSpace(id))
			}
		}
	}
}

// RemoveContainersLabelled removes every container with label, running or not,
// as what a crash left of Dockwell's own containers. It says how many it removed.
func (c *Client) RemoveContainersLabelled(ctx context.Context, label string) (int, error) {
	list, err := c.ListContainers(ctx, map[string][]string{"label": {label}})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range list {
		if err := c.RemoveContainer(ctx, list[i].ID, true); err != nil && !IsNotFound(err) {
			return n, err
		}
		n++
	}
	return n, nil
}

// RemoveUnusedImage removes an image by reference unless a container uses it, as
// docker rmi without --force does. It says whether it removed it.
func (c *Client) RemoveUnusedImage(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodDelete, "/images/"+escRef(ref), nil, nil, nil)
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err), IsConflict(err):
		return false, nil
	}
	return false, err
}
