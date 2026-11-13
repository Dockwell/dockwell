// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"net/http"
)

// Service is a Swarm service, as much of it as an import reads (ADR 0089): what
// it's called, what it runs and what it's labelled.
type Service struct {
	ID   string      `json:"ID"`
	Spec ServiceSpec `json:"Spec"`
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
	} `json:"TaskTemplate"`
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
