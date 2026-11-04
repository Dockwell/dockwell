// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Config is a container's configuration as the engine takes and reports
// it: the fields Dockwell uses. The engine fills in what a container
// leaves out from its image, such as Cmd and Env.
type Config struct {
	Image        string              `json:"Image,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	User         string              `json:"User,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Healthcheck  *Healthcheck        `json:"Healthcheck,omitempty"`
}

// Healthcheck is a container's health check. Durations are in
// nanoseconds; zero takes the engine's default.
type Healthcheck struct {
	Test        []string      `json:"Test,omitempty"`
	Interval    time.Duration `json:"Interval,omitempty"`
	Timeout     time.Duration `json:"Timeout,omitempty"`
	StartPeriod time.Duration `json:"StartPeriod,omitempty"`
	Retries     int           `json:"Retries,omitempty"`
}

// HostConfig is how a container runs on the server.
type HostConfig struct {
	Binds         []string                 `json:"Binds,omitempty"`
	Mounts        []Mount                  `json:"Mounts,omitempty"`
	PortBindings  map[string][]PortBinding `json:"PortBindings,omitempty"`
	RestartPolicy RestartPolicy            `json:"RestartPolicy"`
	NetworkMode   string                   `json:"NetworkMode,omitempty"`
}

// Mount is a volume or bind mount, as HostConfig.Mounts has it.
type Mount struct {
	Type     string `json:"Type"` // "volume" or "bind"
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

// PortBinding publishes a container port on the server.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy says when the engine restarts the container. Name is "",
// "no", "always", "unless-stopped" or "on-failure".
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

// EndpointSettings is a container's place on one network.
type EndpointSettings struct {
	Aliases []string `json:"Aliases,omitempty"`
}

// NetworkingConfig names the networks a new container joins.
type NetworkingConfig struct {
	EndpointsConfig map[string]*EndpointSettings `json:"EndpointsConfig,omitempty"`
}

// CreateRequest is the body of a container create.
type CreateRequest struct {
	Config
	HostConfig       HostConfig       `json:"HostConfig"`
	NetworkingConfig NetworkingConfig `json:"NetworkingConfig"`
}

// Container is a container as the engine reports it.
type Container struct {
	ID              string     `json:"Id"`
	Name            string     `json:"Name"`  // with a leading slash
	Image           string     `json:"Image"` // the image's ID
	Config          Config     `json:"Config"`
	HostConfig      HostConfig `json:"HostConfig"`
	State           State      `json:"State"`
	RestartCount    int        `json:"RestartCount"`
	NetworkSettings struct {
		Networks map[string]*EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
	// Mounts are what is mounted now, the image's anonymous volumes too.
	Mounts []MountPoint `json:"Mounts"`
}

// MountPoint is something mounted in a container now.
type MountPoint struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"` // a volume's
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// State is whether a container runs.
type State struct {
	Status     string  `json:"Status"` // created, running, restarting, exited…
	Running    bool    `json:"Running"`
	Restarting bool    `json:"Restarting"`
	ExitCode   int     `json:"ExitCode"`
	Error      string  `json:"Error"`
	Health     *Health `json:"Health,omitempty"`
}

// Health is a container's health check status: "starting", "healthy" or
// "unhealthy". Its log, which may echo what the check printed, is left
// out.
type Health struct {
	Status        string `json:"Status"`
	FailingStreak int    `json:"FailingStreak"`
}

// InspectContainer returns a container by name or ID; a missing one is an
// error that IsNotFound reports.
func (c *Client) InspectContainer(ctx context.Context, name string) (*Container, error) {
	var out Container
	if err := c.do(ctx, http.MethodGet, "/containers/"+esc(name)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateContainer creates a container, and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, req *CreateRequest) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// StartContainer starts a container; one already running is left be.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/start", nil, nil, nil)
}

// StopContainer stops a container, killing it after timeout; one already
// stopped is left be.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout / time.Second))}}
	return c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/stop", q, nil, nil)
}

// RemoveContainer removes a container, stopping it first if force is set.
// Its anonymous volumes stay, until a prune.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{"force": {strconv.FormatBool(force)}, "v": {"false"}}
	return c.do(ctx, http.MethodDelete, "/containers/"+esc(id), q, nil, nil)
}

// RenameContainer gives a container a new name.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// UpdateRestartPolicy changes a container's restart policy in place.
func (c *Client) UpdateRestartPolicy(ctx context.Context, id string, p RestartPolicy) error {
	return c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/update", nil, struct {
		RestartPolicy RestartPolicy `json:"RestartPolicy"`
	}{p}, &struct{}{})
}

// ConnectNetwork puts a container on a network, with aliases other
// containers there can reach it by.
func (c *Client) ConnectNetwork(ctx context.Context, network, container string, aliases []string) error {
	return c.do(ctx, http.MethodPost, "/networks/"+esc(network)+"/connect", nil, struct {
		Container      string           `json:"Container"`
		EndpointConfig EndpointSettings `json:"EndpointConfig"`
	}{container, EndpointSettings{Aliases: aliases}}, nil)
}
