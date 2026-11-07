// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Tty is whether it has a terminal, which makes its log plain lines.
	Tty bool `json:"Tty,omitempty"`
	// OpenStdin keeps its stdin open for what attaches, and StdinOnce closes it
	// when that hangs up, which is how it hears the end of what it's given.
	OpenStdin bool `json:"OpenStdin,omitempty"`
	StdinOnce bool `json:"StdinOnce,omitempty"`
	// Volumes are the paths its image declares with VOLUME, as an image's reads.
	// Docker makes an empty volume at each that nothing else is mounted at.
	Volumes map[string]struct{} `json:"Volumes,omitempty"`
	// StopTimeout is how many seconds the container has to stop before the
	// engine kills it, when asked to stop it without saying. Nil takes the
	// engine's 10.
	StopTimeout *int `json:"StopTimeout,omitempty"`
	// StopSignal is what it is sent to stop, which no spec says, read from a
	// container made outside Dockwell, and from its image, to tell them apart.
	StopSignal string `json:"StopSignal,omitempty"`
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
	Privileged    bool                     `json:"Privileged,omitempty"`
	GroupAdd      []string                 `json:"GroupAdd,omitempty"`
	// NanoCPUs limits it to this many billionths of a CPU, and
	// MemoryReservation is the memory the engine tries to leave it when the server
	// runs short. Zero has no limit.
	NanoCPUs          int64 `json:"NanoCpus,omitempty"`
	MemoryReservation int64 `json:"MemoryReservation,omitempty"`
	// CapDrop and CapAdd change the capabilities a container has; "ALL"
	// drops every one.
	CapDrop []string `json:"CapDrop,omitempty"`
	CapAdd  []string `json:"CapAdd,omitempty"`
	// ReadonlyRootfs makes its root filesystem read-only, and Tmpfs mounts
	// a tmpfs at each path, with its options.
	ReadonlyRootfs bool              `json:"ReadonlyRootfs,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	// ShmSize is the size of its /dev/shm, in bytes. Zero takes the engine's
	// default, and the engine reports what it took.
	ShmSize int64 `json:"ShmSize,omitempty"`
	// LogConfig says how the engine keeps what it prints.
	LogConfig *LogConfig `json:"LogConfig,omitempty"`
	// Memory limits its memory, in bytes, and MemorySwap its memory and swap
	// together: the same number gives it no swap. PidsLimit limits its
	// processes. Zero leaves each as the engine has it.
	Memory     int64 `json:"Memory,omitempty"`
	MemorySwap int64 `json:"MemorySwap,omitempty"`
	PidsLimit  int64 `json:"PidsLimit,omitempty"`
	// UsernsMode "host" keeps it in the server's user namespace on an engine
	// with userns-remap, which refuses the host network otherwise. Elsewhere it
	// changes nothing.
	UsernsMode string `json:"UsernsMode,omitempty"`
	// SecurityOpt sets options such as no-new-privileges.
	SecurityOpt []string `json:"SecurityOpt,omitempty"`
}

// Mount is a volume, bind or tmpfs mount, as HostConfig.Mounts has it.
type Mount struct {
	Type     string `json:"Type"` // "volume", "bind" or "tmpfs"
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
	// TmpfsOptions says more of a tmpfs. Dockwell makes none this way, and docker
	// compose does, from a volume of type tmpfs.
	TmpfsOptions *TmpfsOptions `json:"TmpfsOptions,omitempty"`
}

// TmpfsOptions are a tmpfs mount's options.
type TmpfsOptions struct {
	SizeBytes int64 `json:"SizeBytes,omitempty"`
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
	// NetworkID is the network it is on, as the engine reports it. A stopped
	// container keeps the ID of a network that has gone, and fails to start
	// until something takes it off that.
	NetworkID string `json:"NetworkID,omitempty"`
	// IPAMConfig asks for a static address, on a network with a configured
	// subnet. The engine gives the container that address each time it
	// starts, or fails to start it.
	IPAMConfig *EndpointIPAM `json:"IPAMConfig,omitempty"`
	// IPAddress is the address it has there while it runs, as the engine
	// reports it. The engine takes no such thing in a request.
	IPAddress string `json:"IPAddress,omitempty"`
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
	Created         time.Time  `json:"Created"`
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
	// LogPath is where json-file keeps its log, on the server.
	LogPath string `json:"LogPath"`
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
	Paused     bool    `json:"Paused"`
	ExitCode   int     `json:"ExitCode"`
	Error      string  `json:"Error"`
	Health     *Health `json:"Health,omitempty"`
	// StartedAt and FinishedAt are zero until it first starts and stops.
	StartedAt  time.Time `json:"StartedAt,omitzero"`
	FinishedAt time.Time `json:"FinishedAt,omitzero"`
	// OOMKilled is true when the kernel killed it for going past its memory
	// limit, the last time it stopped. Docker clears it as a restart policy starts
	// it again, so a container that's running needs WentPastMemory.
	OOMKilled bool `json:"OOMKilled"`
}

// Health is a container's health check status: "starting", "healthy" or
// "unhealthy". Its log holds what the last few checks printed, which may
// hold secrets, so nothing passes it on without internal/health's Clean.
type Health struct {
	Status        string      `json:"Status"`
	FailingStreak int         `json:"FailingStreak"`
	Log           []HealthRun `json:"Log,omitempty"`
}

// HealthRun is one run of a container's health check, as the engine keeps
// its last five.
type HealthRun struct {
	End      time.Time `json:"End"`
	ExitCode int       `json:"ExitCode"`
	Output   string    `json:"Output"`
}

// InspectContainer returns a container by its full ID; a missing one is
// an error that IsNotFound reports. The engine takes any reference here,
// so to find a container by name, use ContainerNamed.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Container, error) {
	var out Container
	if err := c.do(ctx, http.MethodGet, "/containers/"+esc(id)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	if t, ok := c.Adopted.snapshot()[out.ID]; ok {
		out.Config.Labels = asTaken(out.Config.Labels, t, out.Name)
	}
	return &out, nil
}

// TakenView is c as it will read once taken on as t.
func TakenView(c *Container, t Taken) *Container {
	view := *c
	view.Config.Labels = asTaken(c.Config.Labels, t, c.Name)
	return &view
}

// Options is how a container runs, key by key as Docker says it, for what reads a
// container that wasn't made from a spec. Fields would leave out whatever they don't
// name, and what Dockwell doesn't know of is what such a reader is looking for.
type Options struct {
	Name            string                     `json:"Name"` // with a leading slash
	Config          map[string]json.RawMessage `json:"Config"`
	HostConfig      map[string]json.RawMessage `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

// InspectOptions reads what InspectContainer leaves out as nothing else uses it.
func (c *Client) InspectOptions(ctx context.Context, id string) (*Options, error) {
	var out Options
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
	for i := range list {
		s := &list[i]
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
	// Image is what it was made from, as it was asked for, or the image's ID once
	// that name means another image, or none.
	Image string `json:"Image"`
	// Ports are the ports it publishes, and those it only exposes. Only a port
	// it has bound on the server now has a PublicPort, so a stopped one holds none.
	Ports []SummaryPort `json:"Ports"`
	// Mounts are what is mounted in it, the image's anonymous volumes too.
	Mounts []MountPoint `json:"Mounts"`
	// State is created, running, restarting, exited, paused or dead.
	State string `json:"State"`
	// Status says it for people, with its health check's state, such as "Up
	// 3 minutes (healthy)".
	Status string `json:"Status"`
	// NetworkSettings says where it is on each network, as of this list.
	NetworkSettings SummaryNetworks `json:"NetworkSettings"`
}

// SummaryNetworks is a container's networks as the engine lists them.
type SummaryNetworks struct {
	Networks map[string]*EndpointSettings `json:"Networks"`
}

// SummaryPort is a container's port as the engine lists it. PublicPort is
// zero for one that isn't published.
type SummaryPort struct {
	IP          string `json:"IP"` // the server address, "0.0.0.0" or "::" for every one
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort"`
	Type        string `json:"Type"` // tcp or udp
}

// ListContainers lists the containers, running or not, that pass
// filters, as the engine takes them: such as name, a regular expression
// on the name with its leading slash; label, a key or key=value; and
// volume, a volume's name.
func (c *Client) ListContainers(ctx context.Context, filters map[string][]string) ([]ContainerSummary, error) {
	var out []ContainerSummary
	if err := c.eachListed(ctx, filters, func(s *ContainerSummary) error {
		out = append(out, *s)
		return nil
	}); err != nil {
		return nil, err
	}
	return c.listTaken(out, filters, func(more map[string][]string, each func(*ContainerSummary) error) error {
		return c.eachListed(ctx, more, each)
	})
}

// EachContainer is ListContainers one container at a time, as Docker sends them,
// so a server of hundreds costs what its largest container costs, not all of them
// at once. The summary is each's alone, and an error from each ends the list. It
// takes no label filter, as Docker can't apply one to what was taken on.
func (c *Client) EachContainer(ctx context.Context, filters map[string][]string, each func(*ContainerSummary) error) error {
	taken := c.Adopted.snapshot()
	return c.eachListed(ctx, filters, func(s *ContainerSummary) error {
		if t, ok := taken[s.ID]; ok {
			s.Labels = asTaken(s.Labels, t, plainSummaryName(s.Names))
		}
		return each(s)
	})
}

// eachListed reads Docker's list as it comes.
func (c *Client) eachListed(ctx context.Context, filters map[string][]string, each func(*ContainerSummary) error) error {
	q, err := listQuery(filters)
	if err != nil {
		return err
	}
	path, err := c.versioned(ctx, "/containers/json")
	if err != nil {
		return err
	}
	res, err := c.send(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	defer discard(res.Body)
	read := func(err error) error { return fmt.Errorf("can't read Docker's list of containers: %w", err) }
	dec := json.NewDecoder(res.Body)
	if _, err := dec.Token(); err != nil {
		return read(err)
	}
	for dec.More() {
		var s ContainerSummary
		if err := dec.Decode(&s); err != nil {
			return read(err)
		}
		if err := each(&s); err != nil {
			return err
		}
	}
	return nil
}

// listQuery is the query of a list of containers, stopped ones included.
func listQuery(filters map[string][]string) (url.Values, error) {
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
	return q, nil
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
	err := c.do(ctx, http.MethodDelete, "/containers/"+esc(id), q, nil, nil)
	if err == nil || IsNotFound(err) {
		if c.Adopted != nil {
			if ferr := c.Adopted.Forget(id); ferr != nil {
				return ferr
			}
		}
	}
	return err
}

// RenameContainer gives a container a new name.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// UpdateRestartPolicy changes a container's restart policy in place.
func (c *Client) UpdateRestartPolicy(ctx context.Context, id string, p RestartPolicy) error {
	return c.UpdateContainer(ctx, id, ContainerUpdate{RestartPolicy: p})
}

// UpdateContainer changes what the engine can change on a running container.
func (c *Client) UpdateContainer(ctx context.Context, id string, u ContainerUpdate) error {
	// Docker refuses a memory limit past the swap it has for a container unless
	// it is told a new one, and has none for a container with no limit yet. Twice
	// the limit is what it gives a container made with one.
	return c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/update", nil, struct {
		ContainerUpdate
		MemorySwap int64 `json:"MemorySwap,omitempty"`
	}{u, 2 * u.Memory}, &struct{}{})
}

// ContainerUpdate is what the engine changes on a running container. It reads
// a limit of zero as leaving it as it is, so it can't lift one.
type ContainerUpdate struct {
	RestartPolicy     RestartPolicy `json:"RestartPolicy"`
	NanoCPUs          int64         `json:"NanoCpus,omitempty"`
	Memory            int64         `json:"Memory,omitempty"`
	MemoryReservation int64         `json:"MemoryReservation,omitempty"`
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

// Number is a container's number among its service's, as Compose's label has
// it, or 0 for one with no such label.
func (c *ContainerSummary) Number() int {
	n, err := strconv.Atoi(c.Labels[LabelNumber])
	if err != nil {
		return 0
	}
	return n
}

// DisconnectNetwork takes a container off a network; it keeps running.
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	return c.do(ctx, http.MethodPost, "/networks/"+esc(network)+"/disconnect", nil, struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}{container, true}, nil)
}
