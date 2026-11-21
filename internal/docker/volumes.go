// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Volume is a volume as the engine reports it.
type Volume struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	Scope     string            `json:"Scope"`
	Options   map[string]string `json:"Options"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
	// Mountpoint is where its files are on the server, for a local volume.
	Mountpoint string `json:"Mountpoint,omitempty"`
}

// VolumeIdentity is the part of the volume inventory used for disk reads.
type VolumeIdentity struct {
	Name, Mountpoint, CreatedAt string
}

// VolumeInventory reads live volume identities without asking Docker to measure
// them or decoding driver options and labels.
func (c *Client) VolumeInventory(ctx context.Context) ([]VolumeIdentity, error) {
	var out struct {
		Volumes  []VolumeIdentity
		Warnings []string
	}
	if err := c.boundedJSON(ctx, http.MethodGet, "/volumes", nil, &out); err != nil {
		return nil, err
	}
	if len(out.Warnings) > 0 {
		return nil, errors.New("could not list all Docker volumes")
	}
	return out.Volumes, nil
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
	Scope      string
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

// InventoryVolumes reads volume metadata within the inventory byte budget.
func (c *Client) InventoryVolumes(ctx context.Context, maxRows int) ([]Volume, error) {
	var out struct {
		Volumes []Volume
	}
	if err := c.boundedJSON(ctx, http.MethodGet, "/volumes", nil, &out); err != nil {
		return nil, err
	}
	if len(out.Volumes) > maxRows {
		return nil, errors.New("there are too many Docker volumes for this inventory")
	}
	return out.Volumes, nil
}

// VolumeInventoryKey binds the name to the generation evidence Docker provides.
func VolumeInventoryKey(v *Volume) string {
	sum := sha256.Sum256([]byte(v.Name + "\x00" + v.Driver + "\x00" + v.Scope + "\x00" + v.CreatedAt + "\x00" + v.Labels[LabelStorageCreation]))
	return base64.RawURLEncoding.EncodeToString([]byte(v.Name)) + "." + hex.EncodeToString(sum[:])
}

// VolumeInventoryName validates the key and extracts the exact lookup name.
func VolumeInventoryName(key string) (string, bool) {
	if len(key) > 406 {
		return "", false
	}
	encoded, fingerprint, found := strings.Cut(key, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	name := string(decoded)
	if !found || !IsFullID(fingerprint) || err != nil || name == "" || len(name) > 255 ||
		name == "." || name == ".." || strings.ContainsFunc(name, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return false
		default:
			return true
		}
	}) {
		return "", false
	}
	return name, true
}
