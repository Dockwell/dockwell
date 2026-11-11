// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
)

// Volume is a volume as the engine reports it.
type Volume struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	Options   map[string]string `json:"Options"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
	// Mountpoint is where its files are on the server, for a local volume.
	Mountpoint string `json:"Mountpoint,omitempty"`
}

// ListVolumes lists the volumes that have a label, whatever its value.
func (c *Client) ListVolumes(ctx context.Context, label string) ([]Volume, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	var out struct {
		Volumes []Volume `json:"Volumes"`
	}
	if err := c.do(ctx, http.MethodGet, "/volumes", url.Values{"filters": {string(filters)}}, nil, &out); err != nil {
		return nil, err
	}
	// An engine that reads no filters lists every volume.
	return slices.DeleteFunc(out.Volumes, func(v Volume) bool {
		_, ok := v.Labels[label]
		return !ok
	}), nil
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
