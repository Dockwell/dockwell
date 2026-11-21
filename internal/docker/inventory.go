// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// InventoryEngine holds the overview facts read without walking the disk.
type InventoryEngine struct {
	Images, Networks, Volumes    uint32
	DockerRootDir, LoggingDriver string
	LiveRestoreEnabled           *bool
	RegistryConfig               struct{ Mirrors []string }
}

// InventoryInfo reads lightweight facts. Disk usage can walk local volumes.
func (c *Client) InventoryInfo(ctx context.Context) (InventoryEngine, error) {
	var info InventoryEngine
	var network []struct{}
	var volume struct{ Volumes []struct{} }
	if err := c.do(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return info, err
	}
	if err := c.do(ctx, http.MethodGet, "/networks", nil, nil, &network); err != nil {
		return info, err
	}
	if err := c.do(ctx, http.MethodGet, "/volumes", nil, nil, &volume); err != nil {
		return info, err
	}
	info.Networks = uint32(len(network))       //nolint:gosec // Engine object counts fit in uint32.
	info.Volumes = uint32(len(volume.Volumes)) //nolint:gosec // Engine object counts fit in uint32.
	return info, nil
}

// InventoryImage holds the fields in Engine's image summary. Engines may
// omit usage and platform information when shared size is not requested.
type InventoryImage struct {
	DiskUsageImage
	Size, Containers *int64
	Descriptor       *struct {
		Platform *struct{ OS, Architecture, Variant string }
	}
}

// InventoryImages reads image identities and inexpensive facts without asking
// Docker to measure shared layers or walk volumes.
func (c *Client) InventoryImages(ctx context.Context) ([]InventoryImage, error) {
	var rows []InventoryImage
	err := c.boundedJSON(ctx, http.MethodGet, "/images/json", url.Values{"all": {"1"}}, &rows)
	return rows, err
}

// InventoryEvents reports lifecycle and health changes. It decodes no attributes,
// which can hold labels and other user-supplied values.
func (c *Client) InventoryEvents(ctx context.Context, opened func(), changed func(Event)) error {
	p, err := c.versioned(ctx, "/events")
	if err != nil {
		return err
	}
	// Docker combines type and action filters independently. Check their pairing
	// as well, and take actual health transitions through the separate path.
	q := url.Values{"filters": {`{"type":["container","image","network","volume"],"event":["start","stop","die","destroy","create","rename","pause","unpause","delete","pull","tag","untag","health_status: starting","health_status: healthy","health_status: unhealthy"]}`}}
	res, err := c.send(ctx, http.MethodGet, p, q, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	opened()
	d := json.NewDecoder(res.Body)
	for {
		var event struct {
			Type, Action string
			Actor        struct{ ID string }
		}
		if err := d.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if inventoryAction(event.Type, event.Action) {
			changed(Event{Type: event.Type, Action: event.Action, ID: event.Actor.ID})
		}
	}
}

func inventoryAction(kind, action string) bool {
	switch kind {
	case "container":
		switch action {
		case "start", "stop", "die", "destroy", "create", "rename", "pause", "unpause",
			"health_status: starting", "health_status: healthy", "health_status: unhealthy":
			return true
		}
	case "image":
		return action == "create" || action == "delete" || action == "pull" || action == "tag" || action == "untag"
	case "network", "volume":
		return action == "create" || action == "destroy"
	}
	return false
}

type cpuCounters struct {
	CPUUsage struct {
		Total uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	System uint64 `json:"system_cpu_usage"`
	Online uint32 `json:"online_cpus"`
}

// ContainerCounters is the narrow projection used by a live inventory view.
type ContainerCounters struct {
	CPU     *float64
	Memory  *uint64
	Limit   *uint64
	Rx, Tx  *uint64
	Read    *uint64
	Written *uint64
}

// ContainerCounters samples one running container with Engine's non-streaming
// stats. The caller bounds concurrency and request lifetime.
func (c *Client) ContainerCounters(ctx context.Context, id string) (*ContainerCounters, error) {
	var raw struct {
		CPU    cpuCounters `json:"cpu_stats"`
		Before cpuCounters `json:"precpu_stats"`
		Memory struct {
			Usage *uint64 `json:"usage"`
			Limit *uint64 `json:"limit"`
			Stats struct {
				Inactive      *uint64 `json:"inactive_file"`
				TotalInactive *uint64 `json:"total_inactive_file"`
			} `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			Rx uint64 `json:"rx_bytes"`
			Tx uint64 `json:"tx_bytes"`
		} `json:"networks"`
		Block struct {
			Bytes []struct {
				Op    string `json:"op"`
				Value uint64 `json:"value"`
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
	}
	if err := c.do(ctx, http.MethodGet, "/containers/"+esc(id)+"/stats", url.Values{"stream": {"false"}}, nil, &raw); err != nil {
		return nil, err
	}
	out := &ContainerCounters{Memory: raw.Memory.Usage, Limit: raw.Memory.Limit}
	if total, before := raw.CPU.CPUUsage.Total, raw.Before.CPUUsage.Total; total >= before &&
		raw.Before.System > 0 && raw.CPU.System > raw.Before.System && raw.CPU.Online > 0 {
		cpu := 100 * float64(total-before) / float64(raw.CPU.System-raw.Before.System) * float64(raw.CPU.Online)
		out.CPU = &cpu
	}
	inactive := raw.Memory.Stats.Inactive
	if inactive == nil {
		inactive = raw.Memory.Stats.TotalInactive
	}
	if out.Memory != nil && inactive != nil {
		working := *out.Memory - min(*out.Memory, *inactive)
		out.Memory = &working
	}
	if len(raw.Networks) != 0 {
		var rx, tx uint64
		for _, n := range raw.Networks {
			rx += n.Rx
			tx += n.Tx
		}
		out.Rx, out.Tx = &rx, &tx
	}
	if len(raw.Block.Bytes) != 0 {
		var read, written uint64
		for _, op := range raw.Block.Bytes {
			switch op.Op {
			case "Read", "read":
				read += op.Value
			case "Write", "write":
				written += op.Value
			}
		}
		out.Read, out.Written = &read, &written
	}
	return out, nil
}
