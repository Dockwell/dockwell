// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// Config is a container's configuration as the engine takes and reports
// it: the fields Dockwell uses. The engine fills in what a container
// leaves out from its image, such as Cmd and Env.
type Config struct {
	Image string   `json:"Image,omitempty"`
	Cmd   []string `json:"Cmd,omitempty"`
	// Entrypoint nil takes the image's, and empty runs none.
	Entrypoint   []string            `json:"Entrypoint"`
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
	// CapDrop and CapAdd change the capabilities a container has; "ALL"
	// drops every one.
	CapDrop []string `json:"CapDrop,omitempty"`
	CapAdd  []string `json:"CapAdd,omitempty"`
	// ReadonlyRootfs makes its root filesystem read-only, and Tmpfs mounts
	// a tmpfs at each path, with its options.
	ReadonlyRootfs bool              `json:"ReadonlyRootfs,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	// LogConfig says how the engine keeps what it prints.
	LogConfig *LogConfig `json:"LogConfig,omitempty"`
}

// LogConfig is a container's logging driver, with its options.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
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
	// IPAMConfig asks for a static address, on a network with a configured
	// subnet. The engine gives the container that address each time it
	// starts, or fails to start it.
	IPAMConfig *EndpointIPAM `json:"IPAMConfig,omitempty"`
}

// EndpointIPAM is the address a container asks for on a network.
type EndpointIPAM struct {
	IPv4Address string `json:"IPv4Address,omitempty"`
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

// InspectContainer returns a container by its full ID; a missing one is
// an error that IsNotFound reports. The engine takes any reference here,
// so to find a container by name, use ContainerNamed.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Container, error) {
	var out Container
	if err := c.do(ctx, http.MethodGet, "/containers/"+esc(id)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ContainerNamed returns the container of exactly this name; none is an
// error that IsNotFound reports. Act on its ID from then on.
//
// The engine takes a reference as a full ID, then an exact name, then a
// prefix of one ID alone. So asking for a name made of hex digits, such as
// db, can answer with a container whose ID starts db when none is named
// that. An answer by the name itself is the container, and none at all
// means there is none; any other answer, another container or a prefix
// several IDs share, is settled by looking for the name in the list.
func (c *Client) ContainerNamed(ctx context.Context, name string) (*Container, error) {
	ctr, err := c.InspectContainer(ctx, name)
	switch {
	case err == nil && ctr.Name == "/"+name:
		return ctr, nil
	case err == nil:
		// Another container's, by its ID's prefix: none has the name, or
		// the engine would have answered with it.
		return nil, noContainer(name)
	case IsNotFound(err) || status(err) == 0:
		return nil, err
	}
	list, err := c.ListContainers(ctx, map[string][]string{"name": {"^/" + regexp.QuoteMeta(name) + "$"}})
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if !slices.Contains(s.Names, "/"+name) {
			continue
		}
		ctr, err := c.InspectContainer(ctx, s.ID)
		switch {
		case IsNotFound(err) || err == nil && ctr.Name != "/"+name:
			return nil, noContainer(name) // removed or renamed since
		case err != nil:
			return nil, err
		}
		return ctr, nil
	}
	return nil, noContainer(name)
}

func noContainer(name string) error {
	return &Error{Status: http.StatusNotFound, Message: "No such container: " + name}
}

// ContainerSummary is a container as the engine lists it.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"` // with a leading slash
	Labels map[string]string `json:"Labels"`
	// Created is when it was made, in seconds since 1970.
	Created int64 `json:"Created"`
	// Ports are the ports it publishes, and those it only exposes.
	Ports []SummaryPort `json:"Ports"`
}

// SummaryPort is a container's port as the engine lists it. PublicPort is
// zero for one that isn't published.
type SummaryPort struct {
	PublicPort uint16 `json:"PublicPort"`
	Type       string `json:"Type"` // tcp or udp
}

// ListContainers lists the containers, running or not, that pass
// filters, as the engine takes them: such as name, a regular expression
// on the name with its leading slash; label, a key or key=value; and
// volume, a volume's name.
func (c *Client) ListContainers(ctx context.Context, filters map[string][]string) ([]ContainerSummary, error) {
	q := url.Values{"all": {"1"}}
	if len(filters) > 0 {
		// The form every engine takes: {"name": {"^/db$": true}}.
		f := make(map[string]map[string]bool, len(filters))
		for k, vs := range filters {
			f[k] = map[string]bool{}
			for _, v := range vs {
				f[k][v] = true
			}
		}
		b, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(b))
	}
	var out []ContainerSummary
	if err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
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
	return c.ConnectEndpoint(ctx, network, container, EndpointSettings{Aliases: aliases})
}

// ConnectEndpoint puts a container on a network as ep says. It works on a
// running container, which keeps running.
func (c *Client) ConnectEndpoint(ctx context.Context, network, container string, ep EndpointSettings) error {
	return c.do(ctx, http.MethodPost, "/networks/"+esc(network)+"/connect", nil, struct {
		Container      string           `json:"Container"`
		EndpointConfig EndpointSettings `json:"EndpointConfig"`
	}{container, ep}, nil)
}

// DisconnectNetwork takes a container off a network; it keeps running.
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	return c.do(ctx, http.MethodPost, "/networks/"+esc(network)+"/disconnect", nil, struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}{container, true}, nil)
}
