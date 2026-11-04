// SPDX-License-Identifier: AGPL-3.0-only

// Package compose turns a Docker Compose file into an environment's
// services, volumes and networks, and back. It parses with compose-go, as
// docker compose does, in the control plane alone. An environment is one
// Compose project: its containers carry Compose's labels, so docker compose
// sees them, and an exported file runs the same under docker compose. See
// docs/adr/0021-the-docker-driver.md.
package compose

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/transform"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/go-viper/mapstructure/v2"
	"github.com/sirupsen/logrus"
	"go.yaml.in/yaml/v4"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// Item is one resource a Compose file makes: its name and spec.
type Item[T any] struct {
	Name string
	Spec T
}

// Stack is what a Compose file makes in an environment, in the order to
// make it: networks and volumes first, then services, each after those it
// depends on.
type Stack struct {
	Networks []Item[*resourcev1.Network]
	Volumes  []Item[*resourcev1.Volume]
	Services []Item[*resourcev1.Service]
}

// ErrInvalid is a Compose file Dockwell can't take, with why.
var ErrInvalid = errors.New("invalid Compose file")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Message is an error's text for people, without ErrInvalid's prefix.
func Message(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// What Dockwell runs of a Compose file. Anything else is refused by name,
// rather than dropped, so nothing in a file is quietly ignored, apart from
// x- extensions and what changes nothing on one server, such as expose.
var (
	topKeys     = keys("name", "version", "services", "volumes", "networks")
	serviceKeys = keys("image", "command", "entrypoint", "environment", "ports", "restart", "labels", "volumes",
		"networks", "network_mode", "user", "working_dir", "healthcheck", "depends_on", "expose")
	volumeKeys  = keys("driver", "driver_opts", "labels")
	networkKeys = keys("driver", "driver_opts", "internal", "labels")
	healthKeys  = keys("test", "interval", "timeout", "start_period", "retries", "disable")
	portKeys    = keys("target", "published", "host_ip", "protocol", "mode")
	mountKeys   = keys("type", "source", "target", "read_only")
	dependsKeys = keys("condition", "required", "restart")

	// why says why a key Dockwell can't run is refused, and what to do,
	// where that is more than removing it.
	why = map[string]string{ //nolint:gosec // reasons for refusing keys such as secrets, not credentials
		"build":          "builds an image, which Dockwell can't do yet. Build and push the image, then name it with image",
		"env_file":       "reads env_file, a file beside the Compose file, which Dockwell doesn't have. Put the variables in environment",
		"label_file":     "reads label_file, a file beside the Compose file, which Dockwell doesn't have. Put the labels in labels",
		"container_name": "names its container, which Dockwell names as Compose does. Remove container_name",
		"extends":        "extends another service, which Dockwell can't follow yet. Write the service out in full",
		"profiles":       "is in profiles, which Dockwell doesn't select. Remove profiles to run it, or the service to leave it out",
		"secrets":        "uses Compose secrets, which Dockwell can't run yet. Pass the values as environment variables",
		"configs":        "uses Compose configs, which Dockwell can't run yet. Mount the files from the server instead",
		"include":        "includes other files, which Dockwell doesn't have. Put everything in one file",
		"external":       "is external, made outside the file, which Dockwell can't refer to yet. Let the file make it",
		"name":           "names itself in Docker, which Dockwell names as Compose does, <project>_<name>. Remove name",
	}
)

func keys(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// check refuses what a section of the file has beyond allowed, naming it
// as what, such as "the service web".
func check(what string, m map[string]any, allowed map[string]bool) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if allowed[k] || strings.HasPrefix(k, "x-") {
			continue
		}
		if reason, ok := why[k]; ok {
			return invalid("%s %s", what, reason)
		}
		return invalid("%s uses %s, which Dockwell can't run yet. Remove it from the Compose file to import it", what, k)
	}
	return nil
}

// checkRaw reads the file's YAML, and refuses anything Dockwell doesn't
// run, and anything that would read files on the control plane, such as
// env_file and include.
func checkRaw(file []byte) (map[string]any, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(file, &raw); err != nil {
		return nil, invalid("the Compose file isn't valid YAML: %v", err)
	}
	if len(raw) == 0 {
		return nil, invalid("the Compose file is empty. Give it services, such as web with image: nginx:1.27")
	}
	if err := check("the Compose file", raw, topKeys); err != nil {
		return nil, err
	}
	if err := conform(place{owner: "the Compose file"}, raw, fileShape); err != nil {
		return nil, err
	}
	services, _ := raw["services"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(services)) {
		what := "the service " + name
		svc, ok := services[name].(map[string]any)
		if !ok {
			return nil, invalid("%s has no image. Name one, such as image: nginx:1.27", what)
		}
		if err := check(what, svc, serviceKeys); err != nil {
			return nil, err
		}
		if hc, ok := svc["healthcheck"].(map[string]any); ok {
			if err := check(what+"'s healthcheck", hc, healthKeys); err != nil {
				return nil, err
			}
		}
		for _, p := range list(svc["ports"]) {
			if m, ok := p.(map[string]any); ok {
				if err := check(what+"'s port", m, portKeys); err != nil {
					return nil, err
				}
			}
		}
		for _, v := range list(svc["volumes"]) {
			if m, ok := v.(map[string]any); ok {
				if err := check(what+"'s volume", m, mountKeys); err != nil {
					return nil, err
				}
			}
		}
		if nets, ok := svc["networks"].(map[string]any); ok {
			for _, n := range slices.Sorted(maps.Keys(nets)) {
				if m, ok := nets[n].(map[string]any); ok && len(m) > 0 {
					return nil, invalid("%s sets options for the network %s, such as aliases, which Dockwell can't run yet. "+
						"Others on the network reach it by its name; remove the options to import the file", what, n)
				}
			}
		}
		if deps, ok := svc["depends_on"].(map[string]any); ok {
			for _, d := range slices.Sorted(maps.Keys(deps)) {
				if m, ok := deps[d].(map[string]any); ok {
					if err := check(what+"'s dependency on "+d, m, dependsKeys); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	for section, allowed := range map[string]map[string]bool{"volumes": volumeKeys, "networks": networkKeys} {
		entries, _ := raw[section].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			m, _ := entries[name].(map[string]any)
			if err := check(fmt.Sprintf("the %s %s", strings.TrimSuffix(section, "s"), name), m, allowed); err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

var quiet sync.Once

// model is the part of a Compose file Dockwell runs, in compose-go's
// types.
type model struct {
	Services types.Services `yaml:"services"`
	Volumes  types.Volumes  `yaml:"volumes"`
	Networks types.Networks `yaml:"networks"`
}

// Parse reads a Compose file into what it makes in an environment.
// Variables such as ${TAG} take their values from variables, or the
// defaults the file gives; one with neither is refused, rather than left
// blank, at every level (interpolate).
//
// It reads the file as docker compose does, with compose-go's canonical
// forms of the short syntaxes and its types, but not its loader. The loader validates against the Compose
// schema with a JSON Schema library that allocates 1.7 MB as every
// process starts, agents included (ADR 0021); checkRaw allows far less
// than the schema does, and the steps here do the rest of what the loader
// would: resolving variables, the default network, and references between
// services, volumes and networks.
func Parse(_ context.Context, file []byte, variables map[string]string) (*Stack, error) {
	raw, err := checkRaw(file)
	if err != nil {
		return nil, err
	}
	// compose-go warns through logrus, which Dockwell doesn't use: what
	// matters comes back as an error.
	quiet.Do(func() { logrus.SetOutput(io.Discard) })

	dict, err := interpolate(raw, variables)
	if err != nil {
		return nil, err
	}
	if dict, err = transform.Canonical(dict, false); err != nil {
		return nil, invalid("the Compose file isn't valid: %v", err)
	}
	if dict, err = transform.SetDefaultValues(dict); err != nil {
		return nil, invalid("the Compose file isn't valid: %v", err)
	}
	var m model
	if err := decode(dict, &m); err != nil {
		return nil, invalid("the Compose file isn't valid: %v", err)
	}
	if err := m.resolve(variables); err != nil {
		return nil, err
	}
	return stack(&m)
}

// decode reads a canonical Compose model into compose-go's types, as its
// loader does: each type that reads its own short forms, such as a
// command given as one string, does, and strings become the numbers and
// booleans they stand for.
func decode(dict map[string]any, out *model) error {
	hook := func(from, to reflect.Value) (any, error) {
		type decoder interface{ DecodeMapstructure(any) error }
		u, ok := to.Interface().(decoder)
		if !ok && to.CanAddr() {
			u, ok = to.Addr().Interface().(decoder)
		}
		if !ok {
			return from.Interface(), nil
		}
		if to.Kind() == reflect.Pointer && to.IsNil() {
			to.Set(reflect.New(to.Type().Elem()))
			u = to.Interface().(decoder)
		}
		if err := u.DecodeMapstructure(from.Interface()); err != nil {
			return to.Interface(), err
		}
		return to.Interface(), nil
	}
	d, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       hook,
		Result:           out,
		TagName:          "yaml",
		Squash:           true,
		WeaklyTypedInput: true,
	})
	if err != nil {
		return err
	}
	return d.Decode(dict)
}

// resolve does what compose-go's loader does after reading a file: a
// variable named without a value takes it from variables, a service that
// names no network joins the project's default one, and every volume,
// network and service a service names must be in the file.
func (m *model) resolve(variables map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(m.Services)) {
		s := m.Services[name]
		what := "the service " + name
		for k, v := range s.Environment {
			if val, ok := variables[k]; v == nil && ok {
				s.Environment[k] = &val
			}
		}
		if len(s.Networks) == 0 && s.NetworkMode == "" {
			s.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}
			if _, ok := m.Networks["default"]; !ok {
				if m.Networks == nil {
					m.Networks = types.Networks{}
				}
				m.Networks["default"] = types.NetworkConfig{}
			}
		}
		for n := range s.Networks {
			if _, ok := m.Networks[n]; !ok {
				return invalid("%s joins the network %s, which the file doesn't list under networks. Add it there", what, n)
			}
		}
		for _, v := range s.Volumes {
			if _, ok := m.Volumes[v.Source]; v.Type == types.VolumeTypeVolume && v.Source != "" && !ok {
				return invalid("%s mounts the volume %s, which the file doesn't list under volumes. Add it there", what, v.Source)
			}
		}
		for dep := range s.DependsOn {
			if _, ok := m.Services[dep]; !ok {
				return invalid("%s depends on %s, which isn't a service in the file", what, dep)
			}
		}
		m.Services[name] = s
	}
	return nil
}

// stack turns a read file into specs.
func stack(p *model) (*Stack, error) {
	out := &Stack{}
	for _, name := range slices.Sorted(maps.Keys(p.Networks)) {
		n := p.Networks[name]
		out.Networks = append(out.Networks, Item[*resourcev1.Network]{Name: name, Spec: &resourcev1.Network{
			Driver: n.Driver, Options: nonEmpty(n.DriverOpts), Internal: n.Internal, Labels: userLabels(n.Labels),
		}})
	}
	for _, name := range slices.Sorted(maps.Keys(p.Volumes)) {
		v := p.Volumes[name]
		out.Volumes = append(out.Volumes, Item[*resourcev1.Volume]{Name: name, Spec: &resourcev1.Volume{
			Driver: v.Driver, Options: nonEmpty(v.DriverOpts), Labels: userLabels(v.Labels),
		}})
	}
	if len(p.Services) == 0 {
		return nil, invalid("the Compose file has no services. Give it one, such as web with image: nginx:1.27")
	}
	specs := map[string]*resourcev1.Service{}
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		s := p.Services[name]
		c, err := app(name, &s)
		if err != nil {
			return nil, err
		}
		specs[name] = &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
	}
	order, err := ordered(p.Services)
	if err != nil {
		return nil, err
	}
	for _, name := range order {
		out.Services = append(out.Services, Item[*resourcev1.Service]{Name: name, Spec: specs[name]})
	}
	return out, nil
}

// ordered lists services so each comes after those it depends on, by name
// where that leaves a choice, and refuses a cycle.
func ordered(services types.Services) ([]string, error) {
	var out []string
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return invalid("the services depend on each other in a circle, %s. Break it, as nothing could start first",
				strings.Join(append(path, name), " → "))
		}
		state[name] = visiting
		for _, dep := range slices.Sorted(maps.Keys(services[name].DependsOn)) {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		out = append(out, name)
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nonEmpty[M ~map[string]string](m M) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(map[string]string(m))
}

// userLabels are a file's labels without Dockwell's and Compose's own,
// which an exported file carries and Dockwell sets again.
func userLabels(l types.Labels) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		if !docker.IsOwnLabel(k) {
			out[k] = v
		}
	}
	return nonEmpty(out)
}

var restarts = map[string]resourcev1.RestartPolicy{
	"":               resourcev1.RestartPolicy_RESTART_POLICY_NO,
	"no":             resourcev1.RestartPolicy_RESTART_POLICY_NO,
	"always":         resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
	"unless-stopped": resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
	"on-failure":     resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
}

// app turns one Compose service into the container it runs.
func app(name string, s *types.ServiceConfig) (*resourcev1.Container, error) {
	if err := resource.CheckLabel("service", name, "api-v2"); err != nil {
		return nil, invalid("%v. Rename the service in the Compose file", err)
	}
	what := "the service " + name
	if s.Image == "" {
		return nil, invalid("%s has no image. Name one, such as image: nginx:1.27", what)
	}
	c := &resourcev1.Container{
		Image: s.Image, Command: s.Command, Entrypoint: s.Entrypoint,
		User: s.User, WorkingDir: s.WorkingDir, Labels: userLabels(s.Labels),
	}
	restart, ok := restarts[s.Restart]
	if !ok {
		return nil, invalid("%s restarts %q, which Dockwell can't run yet. Use no, always, unless-stopped or on-failure, without a count", what, s.Restart)
	}
	c.Restart = restart
	for _, k := range slices.Sorted(maps.Keys(s.Environment)) {
		v := s.Environment[k]
		if v == nil {
			return nil, invalid("%s's variable %s has no value. Give it one in the file, such as %s: value, or pass it in variables", what, k, k)
		}
		if c.Env == nil {
			c.Env = map[string]string{}
		}
		c.Env[k] = *v
	}
	for _, p := range s.Ports {
		m, err := port(what, p)
		if err != nil {
			return nil, err
		}
		c.Ports = append(c.Ports, m)
	}
	for _, v := range s.Volumes {
		m := &resourcev1.Mount{Target: v.Target, ReadOnly: v.ReadOnly}
		switch {
		case v.Type == types.VolumeTypeVolume && v.Source == "":
			return nil, invalid("%s mounts an unnamed volume at %s. Name it, and list it under volumes, so Dockwell can keep its data", what, v.Target)
		case v.Type == types.VolumeTypeVolume:
			m.Source = &resourcev1.Mount_Volume{Volume: v.Source}
		case v.Type == types.VolumeTypeBind && !path.IsAbs(v.Source):
			return nil, invalid("%s mounts %s, a path relative to the Compose file, which means nothing on a server. Use an absolute path, such as /srv/%s/data, or a named volume",
				what, v.Source, name)
		case v.Type == types.VolumeTypeBind:
			m.Source = &resourcev1.Mount_HostPath{HostPath: v.Source}
		default:
			return nil, invalid("%s mounts a %s at %s, which Dockwell can't run yet. Use a named volume or a server path", what, v.Type, v.Target)
		}
		c.Mounts = append(c.Mounts, m)
	}
	switch s.NetworkMode {
	case "", "bridge":
	default:
		return nil, invalid("%s has network_mode %s, which Dockwell can't run yet. Put it on networks instead", what, s.NetworkMode)
	}
	c.Networks = slices.Sorted(maps.Keys(s.Networks))
	if hc := s.HealthCheck; hc != nil {
		if hc.Disable {
			c.Healthcheck = &resourcev1.Healthcheck{Test: []string{"NONE"}}
		} else {
			c.Healthcheck = &resourcev1.Healthcheck{Test: hc.Test}
			dur := func(d *types.Duration) string {
				if d == nil || *d == 0 {
					return ""
				}
				return time.Duration(*d).String()
			}
			c.Healthcheck.Interval, c.Healthcheck.Timeout, c.Healthcheck.StartPeriod = dur(hc.Interval), dur(hc.Timeout), dur(hc.StartPeriod)
			if hc.Retries != nil {
				c.Healthcheck.Retries = uint32(min(*hc.Retries, 1<<31))
			}
		}
	}
	return c, nil
}

// port turns a Compose port into a published one. Ports must name the
// server's port: one Docker picks would move each time the service is
// recreated.
func port(what string, p types.ServicePortConfig) (*resourcev1.PortMapping, error) {
	if p.Published == "" {
		return nil, invalid("%s publishes port %d on a server port Docker would pick. Name the server's port, such as \"8080:%d\"", what, p.Target, p.Target)
	}
	hp, err := strconv.ParseUint(p.Published, 10, 16)
	if err != nil {
		return nil, invalid("%s publishes port %d on %s, a range Dockwell can't run yet. Name one server port, such as \"8080:%d\"", what, p.Target, p.Published, p.Target)
	}
	m := &resourcev1.PortMapping{HostPort: uint32(hp), ContainerPort: p.Target, HostIp: p.HostIP}
	switch cmp.Or(p.Protocol, "tcp") {
	case "tcp":
		m.Protocol = resourcev1.Protocol_PROTOCOL_TCP
	case "udp":
		m.Protocol = resourcev1.Protocol_PROTOCOL_UDP
	default:
		return nil, invalid("%s publishes port %d over %s, which Dockwell can't run yet. Use tcp or udp", what, p.Target, p.Protocol)
	}
	return m, nil
}
