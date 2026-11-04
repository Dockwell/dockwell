// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"bytes"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v4"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
)

// The file's shape, in the order docker compose config writes it.
type (
	file struct {
		Name     string                  `yaml:"name"`
		Services map[string]serviceEntry `yaml:"services"`
		Networks map[string]networkEntry `yaml:"networks,omitempty"`
		Volumes  map[string]volumeEntry  `yaml:"volumes,omitempty"`
	}
	serviceEntry struct {
		Image       string             `yaml:"image"`
		Entrypoint  []string           `yaml:"entrypoint,omitempty"`
		Command     []string           `yaml:"command,omitempty"`
		User        string             `yaml:"user,omitempty"`
		WorkingDir  string             `yaml:"working_dir,omitempty"`
		Environment map[string]*string `yaml:"environment,omitempty"`
		Ports       []quoted           `yaml:"ports,omitempty"`
		Volumes     []string           `yaml:"volumes,omitempty"`
		Networks    []string           `yaml:"networks,omitempty"`
		NetworkMode string             `yaml:"network_mode,omitempty"`
		Restart     string             `yaml:"restart"`
		Healthcheck *healthEntry       `yaml:"healthcheck,omitempty"`
		Labels      map[string]string  `yaml:"labels,omitempty"`
	}
	healthEntry struct {
		Test        []string `yaml:"test,omitempty"`
		Interval    string   `yaml:"interval,omitempty"`
		Timeout     string   `yaml:"timeout,omitempty"`
		StartPeriod string   `yaml:"start_period,omitempty"`
		Retries     uint32   `yaml:"retries,omitempty"`
		Disable     bool     `yaml:"disable,omitempty"`
	}
	networkEntry struct {
		Driver     string            `yaml:"driver,omitempty"`
		DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
		Internal   bool              `yaml:"internal,omitempty"`
		Labels     map[string]string `yaml:"labels,omitempty"`
	}
	volumeEntry struct {
		Driver     string            `yaml:"driver,omitempty"`
		DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
		Labels     map[string]string `yaml:"labels,omitempty"`
	}
)

// Exported is a Compose file, and the environment variables it leaves
// out, as service.NAME.
type Exported struct {
	File    []byte
	Omitted []string
}

// Export writes services, volumes and networks as a Compose file for the
// Compose project project, which docker compose then runs as Dockwell
// does: the same names, and the labels Dockwell gives them, so docker
// compose finds what Dockwell runs. Specs must be valid.
//
// It never writes a secret. An environment variable whose value may hold
// one, as its field's sensitive option says, is listed by name alone, for
// docker compose to take from its shell. That is every variable until
// environments separate plain values from secrets (#29); plain ones are
// written whole from then on.
func Export(project string, stack *Stack) (*Exported, error) {
	f := file{Name: project, Services: map[string]serviceEntry{}}
	var omitted []string
	for _, s := range stack.Services {
		c := s.Spec.GetApp()
		if c == nil {
			continue
		}
		entry := serviceEntry{
			Image: c.GetImage(), Entrypoint: c.GetEntrypoint(), Command: c.GetCommand(),
			User: c.GetUser(), WorkingDir: c.GetWorkingDir(), Networks: c.GetNetworks(),
		}
		var left []string
		entry.Environment, left = environment(c)
		for _, k := range left {
			omitted = append(omitted, s.Name+"."+k)
		}
		for _, p := range c.GetPorts() {
			entry.Ports = append(entry.Ports, quoted(portString(p)))
		}
		for _, m := range c.GetMounts() {
			src := m.GetVolume()
			if src == "" {
				src = m.GetHostPath()
			}
			v := src + ":" + m.GetTarget()
			if m.GetReadOnly() {
				v += ":ro"
			}
			entry.Volumes = append(entry.Volumes, v)
		}
		if len(entry.Networks) == 0 {
			// Not Compose's default network, which it would make: Docker's
			// bridge, which Dockwell ran it on.
			entry.NetworkMode = "bridge"
		}
		for name, policy := range restarts {
			if name != "" && policy == c.GetRestart() {
				entry.Restart = name
			}
		}
		if hc := c.GetHealthcheck(); hc != nil {
			if slices.Equal(hc.GetTest(), []string{"NONE"}) {
				entry.Healthcheck = &healthEntry{Disable: true}
			} else {
				entry.Healthcheck = &healthEntry{
					Test: hc.GetTest(), Interval: hc.GetInterval(), Timeout: hc.GetTimeout(),
					StartPeriod: hc.GetStartPeriod(), Retries: hc.GetRetries(),
				}
			}
		}
		entry.Labels = maps.Clone(c.GetLabels())
		if own := container.OwnLabels(c); len(own) > 0 {
			if entry.Labels == nil {
				entry.Labels = map[string]string{}
			}
			maps.Copy(entry.Labels, own)
		}
		f.Services[s.Name] = entry
	}
	for _, n := range stack.Networks {
		if f.Networks == nil {
			f.Networks = map[string]networkEntry{}
		}
		labels := maps.Clone(n.Spec.GetLabels())
		if own := network.OwnLabels(n.Spec); len(own) > 0 {
			if labels == nil {
				labels = map[string]string{}
			}
			maps.Copy(labels, own)
		}
		f.Networks[n.Name] = networkEntry{Driver: n.Spec.GetDriver(), DriverOpts: n.Spec.GetOptions(), Internal: n.Spec.GetInternal(), Labels: labels}
	}
	for _, v := range stack.Volumes {
		if f.Volumes == nil {
			f.Volumes = map[string]volumeEntry{}
		}
		f.Volumes[v.Name] = volumeEntry{Driver: v.Spec.GetDriver(), DriverOpts: v.Spec.GetOptions(), Labels: v.Spec.GetLabels()}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Written by Dockwell. Its project, %s, is the\n# environment's, so docker compose sees what Dockwell runs.\n", project)
	if len(omitted) > 0 {
		buf.WriteString("# Variables listed without a value may hold secrets, so they are left out: set\n" +
			"# them in your shell, or in an .env file beside this one, before docker compose up.\n")
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, fmt.Errorf("can't write the Compose file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	slices.Sort(omitted)
	return &Exported{File: buf.Bytes(), Omitted: omitted}, nil
}

// environment is a container's environment for a Compose file, and the
// variables left out as they may hold secrets. It reads the env field's
// sensitive option, so plain values are written once #29 makes them so.
func environment(c *resourcev1.Container) (map[string]*string, []string) {
	env := c.GetEnv()
	if len(env) == 0 {
		return nil, nil
	}
	fd := c.ProtoReflect().Descriptor().Fields().ByName("env")
	sensitive := resource.IsSensitive(fd)
	out := make(map[string]*string, len(env))
	var left []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if sensitive {
			out[k] = nil
			left = append(left, k)
			continue
		}
		v := env[k]
		out[k] = &v
	}
	return out, left
}

// quoted is a string written in double quotes, as a port such as
// "8080:80" must be, or a YAML 1.1 reader takes it for a number in base 60.
type quoted string

func (q quoted) MarshalYAML() (any, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: string(q)}, nil
}

// portString writes a port in Compose's short form, such as
// "127.0.0.1:8080:80/udp".
func portString(p *resourcev1.PortMapping) string {
	s := strconv.Itoa(int(p.GetHostPort())) + ":" + strconv.Itoa(int(p.GetContainerPort()))
	if ip := p.GetHostIp(); ip != "" {
		if addr, err := netip.ParseAddr(ip); err == nil && addr.Is6() {
			ip = "[" + ip + "]"
		}
		s = ip + ":" + s
	}
	if p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
		s += "/udp"
	}
	return s
}
