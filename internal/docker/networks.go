// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"net/http"
)

// Network is a network as the engine reports it.
type Network struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Internal bool              `json:"Internal"`
	Options  map[string]string `json:"Options"`
	Labels   map[string]string `json:"Labels"`
	// Containers on the network, by ID.
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
}

// NetworkCreate is the body of a network create.
type NetworkCreate struct {
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver,omitempty"`
	Internal bool              `json:"Internal,omitempty"`
	Options  map[string]string `json:"Options,omitempty"`
	Labels   map[string]string `json:"Labels,omitempty"`
	// CheckDuplicate refuses a name in use, which engines before API 1.44
	// would otherwise allow.
	CheckDuplicate bool `json:"CheckDuplicate,omitempty"`
}

// InspectNetwork returns a network by its ID; a missing one is an error
// that IsNotFound reports. The engine takes any reference here, so to find
// a network by name, use NetworkNamed.
func (c *Client) InspectNetwork(ctx context.Context, id string) (*Network, error) {
	var out Network
	if err := c.do(ctx, http.MethodGet, "/networks/"+esc(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NetworkNamed returns the network of exactly this name; none is an error
// that IsNotFound reports. Act on its ID from then on. The engine takes a
// reference as a full ID, then an exact name, then a prefix of one ID, so
// a network answering to another name means none has this one.
func (c *Client) NetworkNamed(ctx context.Context, name string) (*Network, error) {
	n, err := c.InspectNetwork(ctx, name)
	if err == nil && n.Name != name {
		return nil, &Error{Status: http.StatusNotFound, Message: "network " + name + " not found"}
	}
	return n, err
}

// CreateNetwork creates a network, and returns its ID.
func (c *Client) CreateNetwork(ctx context.Context, n *NetworkCreate) (string, error) {
	n.CheckDuplicate = !c.AtLeast("1.44")
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/networks/create", nil, n, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// RemoveNetwork removes a network. One a container is on is refused,
// which IsConflict or a 403 from older engines reports.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/networks/"+esc(name), nil, nil, nil)
}
