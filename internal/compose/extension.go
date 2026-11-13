// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"errors"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// extensionKey is Dockwell's own extension on a service, for what a service runs
// with that Compose has no field for: a check the agent makes, how long to let
// what is in flight finish, and whom a volume or a server path it mounts is made
// for (make_owner, by the mount's target, ADR 0085). docker compose reads past any x- key, so a file
// means the same to it with this as without, and importing an export gives the
// same specs.
const extensionKey = "x-dockwell"

var (
	extensionKeys = keys("healthcheck", "drain", "make_owner")
	agentKeys     = keys("http", "tcp", "interval", "timeout", "start_period", "retries")
	httpKeys      = keys("port", "path", "host")
	tcpKeys       = keys("port")
)

// extension is what a service's x-dockwell says.
type extension struct {
	health *resourcev1.Healthcheck
	drain  string
	// owners is whom each mount is made for, by its target.
	owners map[string]string
}

// takeExtensions reads each service's x-dockwell out of a file, once its
// variables are resolved, and leaves the rest as Compose has it. Unlike any
// other x- key, it means something here, so what it holds is checked as the
// rest of the file is, and what it can't run is refused by name.
func takeExtensions(dict map[string]any) (map[string]*extension, error) {
	services, _ := dict["services"].(map[string]any)
	out := map[string]*extension{}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc, _ := services[name].(map[string]any)
		raw, ok := svc[extensionKey]
		if !ok {
			continue
		}
		delete(svc, extensionKey)
		what := "the service " + name + "'s " + extensionKey
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid("%s should be a mapping, such as %s: {drain: 1m}", what, extensionKey)
		}
		if err := check(what, m, extensionKeys); err != nil {
			return nil, err
		}
		e := &extension{}
		var err error
		if e.drain, err = extString(what, "drain", m["drain"]); err != nil {
			return nil, err
		}
		if h, ok := m["healthcheck"]; ok {
			if e.health, err = readAgentCheck(what+"'s healthcheck", h); err != nil {
				return nil, err
			}
		}
		if o, ok := m["make_owner"]; ok {
			if e.owners, err = readOwners(what+"'s make_owner", o); err != nil {
				return nil, err
			}
		}
		out[name] = e
	}
	return out, nil
}

// readOwners reads whom each mount is made for, by its target, such as
// make_owner: {/data: "1000:1000"}.
func readOwners(what string, raw any) (map[string]string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("%s should be a mapping of each mount's target to its owner, such as make_owner: {/data: \"1000:1000\"}", what)
	}
	out := make(map[string]string, len(m))
	for _, target := range slices.Sorted(maps.Keys(m)) {
		owner, err := extString(what, target, m[target])
		if err != nil {
			return nil, err
		}
		uid, gid, err := resource.ParseOwner(owner)
		if err != nil {
			return nil, invalid("%s for %s: %v", what, target, err)
		}
		out[path.Clean(target)] = resource.FormatOwner(uid, gid)
	}
	return out, nil
}

func readAgentCheck(what string, raw any) (*resourcev1.Healthcheck, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("%s should be a mapping, such as healthcheck: {http: {port: 8080, path: /healthz}}", what)
	}
	if err := check(what, m, agentKeys); err != nil {
		return nil, err
	}
	hc := &resourcev1.Healthcheck{}
	var err error
	if hc.Interval, err = extString(what, "interval", m["interval"]); err != nil {
		return nil, err
	}
	if hc.Timeout, err = extString(what, "timeout", m["timeout"]); err != nil {
		return nil, err
	}
	if hc.StartPeriod, err = extString(what, "start_period", m["start_period"]); err != nil {
		return nil, err
	}
	if v, ok := m["retries"]; ok {
		n, err := extNumber(what, "retries", v)
		if err != nil {
			return nil, err
		}
		hc.Retries = n
	}
	if v, ok := m["http"]; ok {
		h, ok := v.(map[string]any)
		if !ok {
			return nil, invalid("%s's http should be a mapping, such as http: {port: 8080, path: /healthz}", what)
		}
		if err := check(what+"'s http", h, httpKeys); err != nil {
			return nil, err
		}
		c := &resourcev1.HttpCheck{}
		if c.Port, err = extNumber(what+"'s http", "port", h["port"]); err != nil {
			return nil, err
		}
		if c.Path, err = extString(what+"'s http", "path", h["path"]); err != nil {
			return nil, err
		}
		if c.Host, err = extString(what+"'s http", "host", h["host"]); err != nil {
			return nil, err
		}
		hc.Http = c
	}
	if v, ok := m["tcp"]; ok {
		t, ok := v.(map[string]any)
		if !ok {
			return nil, invalid("%s's tcp should be a mapping, such as tcp: {port: 5432}", what)
		}
		if err := check(what+"'s tcp", t, tcpKeys); err != nil {
			return nil, err
		}
		c := &resourcev1.TcpCheck{}
		if c.Port, err = extNumber(what+"'s tcp", "port", t["port"]); err != nil {
			return nil, err
		}
		hc.Tcp = c
	}
	if hc.Http == nil && hc.Tcp == nil {
		return nil, invalid("%s needs http or tcp, such as http: {port: 8080, path: /healthz}. A command goes in the service's own healthcheck", what)
	}
	return hc, nil
}

func extString(what, key string, v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}
	return "", invalid("%s's %s should be a string, not %v", what, key, v)
}

// extNumber reads a number as YAML gives it, or a string as a variable leaves it.
func extNumber(what, key string, v any) (uint32, error) {
	var n uint64
	var err error
	switch v := v.(type) {
	case nil:
		return 0, nil
	case int:
		n = uint64(max(v, 0))
	case int64:
		n = uint64(max(v, 0))
	case uint64:
		n = v
	case string:
		n, err = strconv.ParseUint(strings.TrimSpace(v), 10, 32)
	default:
		err = errors.New("not a number")
	}
	if err != nil || n > 1<<31 {
		return 0, invalid("%s's %s should be a number, not %v", what, key, v)
	}
	return uint32(n), nil
}

// apply puts what a service's x-dockwell says in its spec. A check the agent
// makes stands in for a command, so a file with both has to say which.
func (e *extension) apply(what string, c *resourcev1.Container) error {
	if e == nil {
		return nil
	}
	if e.health != nil {
		if hc := c.GetHealthcheck(); hc != nil && !slices.Equal(hc.GetTest(), []string{"NONE"}) {
			return invalid("%s has a healthcheck and %s has one too, and a service has one check. Remove one, or write healthcheck: {disable: true} beside %s", what, extensionKey, extensionKey)
		}
		c.Healthcheck = e.health
	}
	if e.drain != "" {
		if c.Rollout == nil {
			c.Rollout = &resourcev1.Rollout{}
		}
		c.Rollout.Drain = e.drain
	}
	for _, target := range slices.Sorted(maps.Keys(e.owners)) {
		i := slices.IndexFunc(c.GetMounts(), func(m *resourcev1.Mount) bool { return path.Clean(m.GetTarget()) == target })
		if i < 0 || c.GetMounts()[i].GetFile() != nil {
			return invalid("%s's make_owner names %s, which no volume or server path of it mounts. Name a mount's target, such as /data", what, target)
		}
		c.Mounts[i].MakeOwner = e.owners[target]
	}
	return nil
}
