// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"bytes"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/variable"
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
		Image string `yaml:"image"`
		// Entrypoint is a list, or an empty one for none, as "entrypoint: []"
		// runs no entrypoint, not even the image's.
		Entrypoint  any                `yaml:"entrypoint,omitempty"`
		Command     []string           `yaml:"command,omitempty"`
		User        string             `yaml:"user,omitempty"`
		WorkingDir  string             `yaml:"working_dir,omitempty"`
		Environment map[string]*string `yaml:"environment,omitempty"`
		Ports       []quoted           `yaml:"ports,omitempty"`
		Volumes     []string           `yaml:"volumes,omitempty"`
		// Networks is a list of names, or a mapping that gives each its aliases.
		Networks    any               `yaml:"networks,omitempty"`
		NetworkMode string            `yaml:"network_mode,omitempty"`
		Restart     string            `yaml:"restart"`
		Healthcheck *healthEntry      `yaml:"healthcheck,omitempty"`
		Labels      map[string]string `yaml:"labels,omitempty"`
		StopGrace   string            `yaml:"stop_grace_period,omitempty"`
		ShmSize     string            `yaml:"shm_size,omitempty"`
		Tmpfs       []string          `yaml:"tmpfs,omitempty"`
		Expose      []string          `yaml:"expose,omitempty"`
		Deploy      *deployEntry      `yaml:"deploy,omitempty"`
		CapAdd      []string          `yaml:"cap_add,omitempty"`
		CapDrop     []string          `yaml:"cap_drop,omitempty"`
		GroupAdd    []string          `yaml:"group_add,omitempty"`
		Privileged  bool              `yaml:"privileged,omitempty"`
		// Logging keeps the rotation Dockwell sets on what docker compose makes
		// from the file, which it would otherwise leave without a limit, except on
		// a server whose Docker logs elsewhere by default, which keeps its choice.
		Logging *loggingEntry `yaml:"logging,omitempty"`
		// Dockwell is what Compose has no field for: x-dockwell.
		Dockwell *dockwellEntry `yaml:"x-dockwell,omitempty"`
	}
	joinEntry struct {
		Aliases []string `yaml:"aliases,omitempty"`
	}
	deployEntry struct {
		Resources    *limitsEntry `yaml:"resources,omitempty"`
		UpdateConfig *updateEntry `yaml:"update_config,omitempty"`
	}
	limitsEntry struct {
		Limits       *amountEntry `yaml:"limits,omitempty"`
		Reservations *amountEntry `yaml:"reservations,omitempty"`
	}
	amountEntry struct {
		CPUs   string `yaml:"cpus,omitempty"`
		Memory string `yaml:"memory,omitempty"`
	}
	loggingEntry struct {
		Driver  string            `yaml:"driver"`
		Options map[string]string `yaml:"options"`
	}
	updateEntry struct {
		Order string `yaml:"order"`
	}
	dockwellEntry struct {
		Healthcheck *agentCheckEntry `yaml:"healthcheck,omitempty"`
		Drain       string           `yaml:"drain,omitempty"`
	}
	agentCheckEntry struct {
		HTTP        *httpCheckEntry `yaml:"http,omitempty"`
		TCP         *tcpCheckEntry  `yaml:"tcp,omitempty"`
		Interval    string          `yaml:"interval,omitempty"`
		Timeout     string          `yaml:"timeout,omitempty"`
		StartPeriod string          `yaml:"start_period,omitempty"`
		Retries     uint32          `yaml:"retries,omitempty"`
	}
	httpCheckEntry struct {
		Port uint32 `yaml:"port"`
		Path string `yaml:"path,omitempty"`
		Host string `yaml:"host,omitempty"`
	}
	tcpCheckEntry struct {
		Port uint32 `yaml:"port"`
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
		External   bool              `yaml:"external,omitempty"`
	}
	volumeEntry struct {
		Driver     string            `yaml:"driver,omitempty"`
		DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
		Labels     map[string]string `yaml:"labels,omitempty"`
		External   bool              `yaml:"external,omitempty"`
	}
)

// Exported is a Compose file, and what it leaves out as it may hold
// secrets: environment variables, as service.NAME, and a volume's driver
// options, as volumes.NAME.driver_opts.
type Exported struct {
	File    []byte
	Omitted []string
}

// Export writes services, volumes and networks as a Compose file for the
// Compose project project, which docker compose then runs as Dockwell
// does: the same names, and the labels Dockwell gives its own, so docker compose
// finds what Dockwell runs and what compose makes from it reads back the same.
// Specs must be valid.
//
// It never writes a secret. An environment variable whose value may hold
// one, as its field's sensitive option says, is listed by name alone, for
// docker compose to take from its shell: a container's own environment,
// and the secrets a service uses. The variables a service uses aren't
// secret, so their values are written whole. The label that says what a
// service uses goes in too, names and revisions alone, so a container
// docker compose makes reads back as the service, and importing the file
// keeps its references (usesOf).
func Export(project string, stack *Stack) (*Exported, error) {
	f := file{Name: project, Services: map[string]serviceEntry{}}
	var omitted []string
	volumesLeft, ownLabel, namesValues := false, false, false
	for _, s := range stack.Services {
		c := s.Spec.GetApp()
		if c == nil {
			continue
		}
		rotation := docker.Rotation()
		entry := serviceEntry{
			Image: escape(c.GetImage()), Command: exportWords(c, binding.FieldCommand),
			User: escape(c.GetUser()), WorkingDir: escape(c.GetWorkingDir()),
			CapAdd: c.GetCapAdd(), CapDrop: c.GetCapDrop(), GroupAdd: c.GetGroupAdd(), Privileged: c.GetPrivileged(),
			Networks: exportNetworks(c), Deploy: exportResources(c.GetResources()),
		}
		if stack.LogDrivers[s.Name] == "" {
			entry.Logging = &loggingEntry{Driver: rotation.Type, Options: rotation.Config}
		}
		// A network the server has is listed as external, by its name in Docker, so
		// docker compose joins it and never makes it.
		for _, n := range c.GetExternalNetworks() {
			if f.Networks == nil {
				f.Networks = map[string]networkEntry{}
			}
			f.Networks[n] = networkEntry{External: true}
		}
		switch {
		case c.GetNoEntrypoint():
			entry.Entrypoint = []string{}
		case len(c.GetEntrypoint()) > 0:
			entry.Entrypoint = exportWords(c, binding.FieldEntrypoint)
		}
		var left []string
		entry.Environment, left = environment(c)
		named := binding.ReferencedNames(c)
		namesValues = namesValues || len(named) > 0
		for _, u := range binding.Uses(s.Spec) {
			if entry.Environment == nil {
				entry.Environment = map[string]*string{}
			}
			if v, ok := stack.Variables[u.Name]; ok && u.Kind == variable.VariableKind {
				v = escape(v)
				entry.Environment[u.As] = &v
				// A file's ${NAME} reads from its shell, and not from its environment.
				if slices.Contains(named, u.As) {
					left = append(left, u.As)
				}
				continue
			}
			entry.Environment[u.As] = nil
			left = append(left, u.As)
		}
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
			var options []string
			if m.GetReadOnly() {
				options = append(options, "ro")
			}
			switch m.GetRelabel() {
			case resourcev1.Relabel_RELABEL_SHARED:
				options = append(options, "z")
			case resourcev1.Relabel_RELABEL_PRIVATE:
				options = append(options, "Z")
			case resourcev1.Relabel_RELABEL_UNSPECIFIED:
			}
			v := src + ":" + m.GetTarget()
			if len(options) > 0 {
				v += ":" + strings.Join(options, ",")
			}
			entry.Volumes = append(entry.Volumes, escape(v))
			// A volume the server has, as a network it has is listed.
			if m.GetExternal() {
				if f.Volumes == nil {
					f.Volumes = map[string]volumeEntry{}
				}
				f.Volumes[m.GetVolume()] = volumeEntry{External: true}
			}
		}
		switch {
		case c.GetHostNetwork():
			entry.NetworkMode = "host"
		case entry.Networks == nil:
			// Not Compose's default network, which it would make: Docker's
			// bridge, which Dockwell ran it on.
			entry.NetworkMode = "bridge"
		}
		for name, policy := range restarts {
			if name != "" && policy == c.GetRestart() {
				entry.Restart = name
			}
		}
		if n := c.GetRestartRetries(); n > 0 {
			entry.Restart += ":" + strconv.FormatUint(uint64(n), 10)
		}
		entry.StopGrace, entry.ShmSize, entry.Expose = c.GetStopGracePeriod(), c.GetShmSize(), c.GetExpose()
		for _, t := range c.GetTmpfs() {
			v := t.GetTarget()
			if t.GetSize() != "" {
				v += ":size=" + t.GetSize()
			}
			entry.Tmpfs = append(entry.Tmpfs, escape(v))
		}
		var order string
		switch c.GetRollout().GetOrder() {
		case resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST:
			order = "start-first"
		case resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST:
			order = "stop-first"
		case resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED:
		}
		if order != "" {
			if entry.Deploy == nil {
				entry.Deploy = &deployEntry{}
			}
			entry.Deploy.UpdateConfig = &updateEntry{Order: order}
		}
		if d := c.GetRollout().GetDrain(); d != "" {
			entry.Dockwell = &dockwellEntry{Drain: d}
		}
		if hc := c.GetHealthcheck(); hc != nil {
			switch {
			case container.AgentCheck(hc) != nil:
				// Compose has no form for it, so it has no check of its own to
				// run, and Dockwell's goes beside it.
				entry.Healthcheck = &healthEntry{Disable: true}
				if entry.Dockwell == nil {
					entry.Dockwell = &dockwellEntry{}
				}
				check := &agentCheckEntry{Interval: hc.GetInterval(), Timeout: hc.GetTimeout(), StartPeriod: hc.GetStartPeriod(), Retries: hc.GetRetries()}
				if h := hc.GetHttp(); h != nil {
					check.HTTP = &httpCheckEntry{Port: h.GetPort(), Path: escape(h.GetPath()), Host: escape(h.GetHost())}
				} else {
					check.TCP = &tcpCheckEntry{Port: hc.GetTcp().GetPort()}
				}
				entry.Dockwell.Healthcheck = check
			case slices.Equal(hc.GetTest(), []string{"NONE"}):
				entry.Healthcheck = &healthEntry{Disable: true}
			default:
				entry.Healthcheck = &healthEntry{
					Test: exportWords(c, binding.FieldTest), Interval: hc.GetInterval(), Timeout: hc.GetTimeout(),
					StartPeriod: hc.GetStartPeriod(), Retries: hc.GetRetries(),
				}
			}
		}
		// The labels of the container the service runs as, what it uses
		// included, with its environment's names alone.
		names := maps.Clone(c.GetEnv())
		if names == nil && len(binding.Uses(s.Spec)) > 0 {
			names = map[string]string{}
		}
		for _, u := range binding.Uses(s.Spec) {
			names[u.As] = ""
		}
		run := binding.App(s.Spec, names)
		entry.Labels = maps.Clone(run.GetLabels())
		if own := container.OwnLabels(run); len(own) > 0 {
			if entry.Labels == nil {
				entry.Labels = map[string]string{}
			}
			maps.Copy(entry.Labels, own)
			ownLabel = true
		}
		entry.Labels = escapeValues(entry.Labels)
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
			ownLabel = true
		}
		f.Networks[n.Name] = networkEntry{
			Driver: escape(n.Spec.GetDriver()), DriverOpts: escapeValues(n.Spec.GetOptions()), Internal: n.Spec.GetInternal(), Labels: escapeValues(labels),
		}
	}
	for _, v := range stack.Volumes {
		if f.Volumes == nil {
			f.Volumes = map[string]volumeEntry{}
		}
		// Options may hold a password, as a CIFS mount's goes in o, and
		// have no form for a shell to fill in, so they are left out whole.
		entry := volumeEntry{Driver: escape(v.Spec.GetDriver()), Labels: escapeValues(v.Spec.GetLabels())}
		if len(v.Spec.GetOptions()) > 0 {
			fd := v.Spec.ProtoReflect().Descriptor().Fields().ByName("options")
			if resource.IsSensitive(fd) {
				omitted = append(omitted, "volumes."+v.Name+".driver_opts")
				volumesLeft = true
			} else {
				entry.DriverOpts = escapeValues(v.Spec.GetOptions())
			}
		}
		f.Volumes[v.Name] = entry
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by Dockwell. Its name is the Compose project Dockwell runs it in, so\n" +
		"# docker compose sees what Dockwell runs.\n")
	if ownLabel {
		buf.WriteString("# A " + container.LabelSet + " label is there on purpose: it says what the spec set, so what compose makes reads back the same.\n")
	}
	if len(omitted) > 0 {
		buf.WriteString("# A variable written as NAME: null is there on purpose. Its value may be a secret,\n" +
			"# so it is left out: set it in your shell, or in an .env file beside this one,\n" +
			"# before docker compose up.\n")
	}
	if namesValues {
		buf.WriteString("# A ${NAME} in a command, an entrypoint or a health check is there on purpose too.\n" +
			"# Dockwell keeps the value out of what it shows, and docker compose reads it from\n" +
			"# your shell, or an .env file beside this one, so set it before docker compose up.\n")
	}
	if volumesLeft {
		buf.WriteString("# A volume's driver options may hold secrets, so they are left out too. Docker\n" +
			"# keeps them with a volume that is on the server already; write them in for a new one.\n")
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

// exportNetworks are the networks a service's entry lists, as a list, or as a
// mapping when it answers to other names on them, as Compose has them for each
// network. Nil is none.
func exportNetworks(c *resourcev1.Container) any {
	names := slices.Concat(c.GetNetworks(), c.GetExternalNetworks())
	if len(names) == 0 {
		return nil
	}
	if len(c.GetAliases()) == 0 {
		return names
	}
	out := make(map[string]joinEntry, len(names))
	for _, n := range names {
		out[n] = joinEntry{Aliases: c.GetAliases()}
	}
	return out
}

// exportResources are a service's limits as Compose has them, or nil for none.
func exportResources(r *resourcev1.Resources) *deployEntry {
	if r == nil {
		return nil
	}
	var out limitsEntry
	if r.GetCpus() != "" || r.GetMemory() != "" {
		out.Limits = &amountEntry{CPUs: r.GetCpus(), Memory: r.GetMemory()}
	}
	if r.GetMemoryReservation() != "" {
		out.Reservations = &amountEntry{Memory: r.GetMemoryReservation()}
	}
	return &deployEntry{Resources: &out}
}

// environment is a container's own environment for a Compose file, and
// the variables left out as they may hold secrets. It reads the env field's
// sensitive option: a workload's environment stays configuration that may
// hold one, and what is plain lives in the environment's variables.
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
		v := escape(env[k])
		out[k] = &v
	}
	return out, left
}

// exportWords is a field of a container that may name values, as a Compose file
// writes it: one that does is written that way already.
func exportWords(c *resourcev1.Container, field string) []string {
	if slices.Contains(c.GetReferences(), field) {
		return binding.WordsOf(c, field)
	}
	return escapeAll(binding.WordsOf(c, field))
}

// escape writes a $ as $$, which Compose reads as a $ of its own, not the
// start of a variable. Compose resolves variables in every value of a
// file, never in its keys, so values alone are escaped.
func escape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func escapeAll(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = escape(s)
	}
	return out
}

func escapeValues(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = escape(v)
	}
	return out
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
