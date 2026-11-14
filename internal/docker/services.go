// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Service is a Swarm service, as much of it as an import reads (ADR 0089): what
// it's called, what it runs and what it's labelled, how many replicas it runs, and
// its version, which an update names.
type Service struct {
	ID      string         `json:"ID"`
	Version ServiceVersion `json:"Version"`
	Spec    ServiceSpec    `json:"Spec"`
}

// ServiceVersion is a Swarm object's version: an update names the one it read, so
// a change made meanwhile isn't written over.
type ServiceVersion struct {
	Index uint64 `json:"Index"`
}

// ServiceSpec is what a Swarm service is asked to run.
type ServiceSpec struct {
	Name         string            `json:"Name"`
	Labels       map[string]string `json:"Labels"`
	TaskTemplate struct {
		ContainerSpec struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"ContainerSpec"`
		// Networks are the networks its tasks join, as Docker has them, kept raw
		// so they go back exactly.
		Networks json.RawMessage `json:"Networks,omitempty"`
		// RestartPolicy is when Swarm starts a task again: Condition "any", the
		// default, "on-failure" or "none".
		RestartPolicy struct {
			Condition string `json:"Condition,omitempty"`
		} `json:"RestartPolicy"`
	} `json:"TaskTemplate"`
	Mode         ServiceMode `json:"Mode"`
	EndpointSpec struct {
		Ports []ServicePort `json:"Ports"`
	} `json:"EndpointSpec"`
}

// ServicePort is a port a Swarm service publishes: on every node through Swarm's
// routing mesh, or on the node a task runs on.
type ServicePort struct {
	Protocol      string `json:"Protocol"`
	TargetPort    uint32 `json:"TargetPort"`
	PublishedPort uint32 `json:"PublishedPort"`
	PublishMode   string `json:"PublishMode"`
}

// ServiceMode is how a Swarm service places its tasks: a number of replicas, or one
// on each node.
type ServiceMode struct {
	Replicated *struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	} `json:"Replicated,omitempty"`
	Global *struct{} `json:"Global,omitempty"`
}

// Replicas is how many replicas a service runs, and false for one that runs on each
// node, which has no number to scale.
func (m ServiceMode) Replicas() (uint64, bool) {
	if m.Replicated == nil {
		return 0, false
	}
	if m.Replicated.Replicas == nil {
		return 1, true // Docker's default
	}
	return *m.Replicated.Replicas, true
}

// ListServices lists the Swarm services the engine runs, read-only. An engine
// that isn't a swarm's manager runs none, and says so.
func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	var out []Service
	err := c.do(ctx, http.MethodGet, "/services", nil, nil, &out)
	if status(err) == http.StatusServiceUnavailable {
		return nil, nil
	}
	return out, err
}

// InspectService reads a Swarm service by its ID or name.
func (c *Client) InspectService(ctx context.Context, id string) (*Service, error) {
	var out Service
	if err := c.do(ctx, http.MethodGet, "/services/"+esc(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ScaleService has a Swarm service run n replicas, as docker service scale does,
// and changes nothing else of it.
func (c *Client) ScaleService(ctx context.Context, id string, n uint64) error {
	return c.rewriteService(ctx, id, func(spec map[string]any) error { return replicasOf(spec, n) })
}

// PlaceService has a Swarm service run n replicas, publish ports and join
// networks, as Docker has them raw, and changes nothing else of it. None of
// either takes each off: Swarm's routing mesh holds a published port on every
// node for as long as the service publishes it, and a service on a network
// answers there by its name, with or without a task.
func (c *Client) PlaceService(ctx context.Context, id string, n uint64, ports []ServicePort, networks json.RawMessage) error {
	return c.rewriteService(ctx, id, func(spec map[string]any) error {
		if err := replicasOf(spec, n); err != nil {
			return err
		}
		endpoint, _ := spec["EndpointSpec"].(map[string]any)
		if endpoint == nil {
			endpoint = map[string]any{}
			spec["EndpointSpec"] = endpoint
		}
		list := make([]any, 0, len(ports))
		for _, p := range ports {
			list = append(list, map[string]any{"Protocol": p.Protocol, "TargetPort": p.TargetPort, "PublishedPort": p.PublishedPort, "PublishMode": p.PublishMode})
		}
		endpoint["Ports"] = list
		template, _ := spec["TaskTemplate"].(map[string]any)
		if template == nil {
			template = map[string]any{}
			spec["TaskTemplate"] = template
		}
		joins := []any{}
		if len(networks) > 0 {
			dec := json.NewDecoder(bytes.NewReader(networks))
			dec.UseNumber()
			if err := dec.Decode(&joins); err != nil {
				return fmt.Errorf("can't read the networks to put back: %w", err)
			}
		}
		template["Networks"] = joins
		// The old field for the same, which Docker still reads where it's set.
		delete(spec, "Networks")
		return nil
	})
}

// replicasOf sets the replicas in a service's spec.
func replicasOf(spec map[string]any, n uint64) error {
	mode, _ := spec["Mode"].(map[string]any)
	replicated, ok := mode["Replicated"].(map[string]any)
	if !ok {
		return errors.New("it runs one task on each node, so it has no number of replicas to change")
	}
	replicated["Replicas"] = n
	return nil
}

// rewriteService changes a Swarm service's spec as change says. Swarm takes a
// service's whole spec on an update, so it's read as Docker has it, every field
// kept as it was, numbers exactly, and written back with what change changed
// alone, naming the version it read: one changed meanwhile is refused, not
// written over.
func (c *Client) rewriteService(ctx context.Context, id string, change func(map[string]any) error) error {
	var read struct {
		Version ServiceVersion  `json:"Version"`
		Spec    json.RawMessage `json:"Spec"`
	}
	if err := c.do(ctx, http.MethodGet, "/services/"+esc(id), nil, nil, &read); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(read.Spec))
	dec.UseNumber()
	var spec map[string]any
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("can't read the service %s's spec: %w", id, err)
	}
	if err := change(spec); err != nil {
		return err
	}
	query := url.Values{"version": {strconv.FormatUint(read.Version.Index, 10)}}
	return c.do(ctx, http.MethodPost, "/services/"+esc(id)+"/update", query, spec, &struct{}{})
}
