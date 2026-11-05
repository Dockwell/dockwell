// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"net/http"
	"net/url"
)

// Volume is a volume as the engine reports it.
type Volume struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	Options   map[string]string `json:"Options"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

// VolumeCreate is the body of a volume create.
type VolumeCreate struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	DriverOpts map[string]string `json:"DriverOpts,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// InspectVolume returns a volume by name; a missing one is an error that
// IsNotFound reports.
func (c *Client) InspectVolume(ctx context.Context, name string) (*Volume, error) {
	var out Volume
	if err := c.do(ctx, http.MethodGet, "/volumes/"+esc(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateVolume creates a volume. The engine returns the one there is when
// one of the name exists already.
func (c *Client) CreateVolume(ctx context.Context, v *VolumeCreate) (*Volume, error) {
	var out Volume
	if err := c.do(ctx, http.MethodPost, "/volumes/create", nil, v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveVolume removes a volume and its data. One a container uses is
// refused, which IsConflict reports.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/volumes/"+esc(name), url.Values{"force": {"false"}}, nil, nil)
}
