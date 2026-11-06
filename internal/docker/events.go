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
	"strconv"
	"time"
)

// WentPastMemory reports whether the kernel killed something in a container for
// going past its memory limit between two times. A container's own state can't say
// that of one a restart policy brought back, as Docker clears OOMKilled as it starts
// it. Its events keep it, for as long as the few hundred Docker holds reach back.
func (c *Client) WentPastMemory(ctx context.Context, id string, from, to time.Time) (bool, error) {
	filters, err := json.Marshal(map[string][]string{"type": {"container"}, "container": {id}, "event": {"oom"}})
	if err != nil {
		return false, err
	}
	path, err := c.versioned(ctx, "/events")
	if err != nil {
		return false, err
	}
	// With an end in the past or now, Docker sends what it has and closes.
	query := url.Values{
		"since":   {strconv.FormatInt(from.Unix(), 10)},
		"until":   {strconv.FormatInt(to.Unix(), 10)},
		"filters": {string(filters)},
	}
	res, err := c.send(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	defer discard(res.Body)
	var event struct {
		Action string `json:"Action"`
	}
	switch err := json.NewDecoder(res.Body).Decode(&event); {
	case errors.Is(err, io.EOF):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("can't read Docker's events: %w", err)
	}
	return event.Action == "oom", nil
}
