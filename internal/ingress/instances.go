// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// Service is a service in its Compose project, its environment.
type Service struct {
	Project, Name string
}

// Instance is one container of a service, by Compose's container number.
type Instance struct {
	Project, Service string
	Number           int
}

func (i Instance) String() string {
	return i.Project + "/" + i.Service + "/" + strconv.Itoa(i.Number)
}

// ContainerName is the container Compose would name for the instance.
func (i Instance) ContainerName() string {
	return i.Project + "-" + i.Service + "-" + strconv.Itoa(i.Number)
}

func (i Instance) service() Service { return Service{Project: i.Project, Name: i.Service} }

// Upstreams is what a rollout (#28) uses to swap a service's containers with
// no dropped requests. None of it makes a plan, a revision or drift: a route's
// spec has its service and port, not its addresses. A service with no routes
// has no address, and the rest does nothing for it, so a rollout needn't ask
// whether a service has any.
type Upstreams interface {
	// Endpoint says how a new container of an instance joins the ingress
	// network: pass network and ep to docker.Client.ConnectEndpoint before it
	// starts. It reserves the instance's address if it has none. ok is false
	// for a service with no routes.
	Endpoint(ctx context.Context, instance Instance) (network string, ep docker.EndpointSettings, ok bool, err error)
	// AddUpstream sends new requests to the instance too, on every route that
	// serves its service.
	AddUpstream(ctx context.Context, instance Instance) error
	// Drain stops sending new requests to the instance, and waits until those
	// in flight finish, or ctx is done, which isn't an error: what remains
	// ends with the container. A service's last upstream can't be drained, as
	// a route needs one.
	Drain(ctx context.Context, instance Instance) error
	// RemoveUpstream forgets the instance and frees its address.
	RemoveUpstream(ctx context.Context, instance Instance) error
}

// addressesFile is the table of instances' addresses.
const addressesFile = "addresses.json"

// table is which instance each address belongs to. It is read once, and each
// change is written before it counts, as an address that isn't on record could
// be given to another service's container, which would then get this one's
// traffic. Docker can't say: it lists a stopped container on no network.
type table struct {
	loaded bool
	// Subnet is the network's, and its first address after the gateway is
	// where giving out starts.
	Subnet    string            `json:"subnet,omitempty"`
	Addresses map[string]string `json:"addresses,omitempty"` // instance → address
}

func (t *table) load(dir string) error {
	if t.loaded {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, addressesFile)) //nolint:gosec // under the ingress's own directory
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("can't read the ingress's addresses: %w", err)
	default:
		if err := json.Unmarshal(b, t); err != nil {
			return fmt.Errorf("can't read the ingress's addresses in %s: %w. Restore the file, or delete it along with the network %s and the ingress's routes",
				filepath.Join(dir, addressesFile), err, docker.IngressNetwork)
		}
	}
	if t.Addresses == nil {
		t.Addresses = map[string]string{}
	}
	t.loaded = true
	return nil
}

func (t *table) save(dir string) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", dir, err)
	}
	tmp := filepath.Join(dir, addressesFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("can't keep the ingress's addresses: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, addressesFile)); err != nil {
		return fmt.Errorf("can't keep the ingress's addresses: %w", err)
	}
	return nil
}

// parseInstance reads an instance from its key in the table.
func parseInstance(key string) (Instance, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return Instance{}, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return Instance{}, false
	}
	return Instance{Project: parts[0], Service: parts[1], Number: n}, true
}

// instanceAt is whose an address is.
func (t *table) instanceAt(addr string) (Instance, bool) {
	for key, a := range t.Addresses {
		if a == addr {
			return parseInstance(key)
		}
	}
	return Instance{}, false
}

// instancesOf are a service's instances, by number.
func (t *table) instancesOf(s Service) []Instance {
	var out []Instance
	for key := range t.Addresses {
		if i, ok := parseInstance(key); ok && i.service() == s {
			out = append(out, i)
		}
	}
	slices.SortFunc(out, func(a, b Instance) int { return a.Number - b.Number })
	return out
}

// give records an address for an instance, the lowest free one past the
// gateway, and returns it.
func (t *table) give(dir string, i Instance, gateway string) (string, error) {
	if a, ok := t.Addresses[i.String()]; ok {
		return a, nil
	}
	prefix, err := netip.ParsePrefix(t.Subnet)
	if err != nil {
		return "", fmt.Errorf("the ingress's subnet %q isn't one: %w", t.Subnet, err)
	}
	used := map[string]bool{gateway: true}
	for _, a := range t.Addresses {
		used[a] = true
	}
	// The network's own address isn't a host's, nor is the one Docker keeps
	// for its gateway when it has none on record.
	for a := prefix.Addr().Next().Next(); prefix.Contains(a); a = a.Next() {
		if !used[a.String()] && prefix.Contains(a.Next()) {
			t.Addresses[i.String()] = a.String()
			if err := t.save(dir); err != nil {
				delete(t.Addresses, i.String())
				return "", err
			}
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("the ingress network's subnet %s has no address left, so %s can't join it. Remove some routes", t.Subnet, i)
}

// free takes an instance's address off the table.
func (t *table) free(dir string, i Instance) error {
	if _, ok := t.Addresses[i.String()]; !ok {
		return nil
	}
	a := t.Addresses[i.String()]
	delete(t.Addresses, i.String())
	if err := t.save(dir); err != nil {
		t.Addresses[i.String()] = a
		return err
	}
	return nil
}

// networkOptions keep the network to itself: a service on it reaches no other
// through it, and gets no way out, and only the server, which is where the
// proxy is, reaches a service on it (ADR 0024).
var networkOptions = map[string]string{"com.docker.network.bridge.enable_icc": "false"}

// networkLabels mark the network as the ingress's.
var networkLabels = map[string]string{docker.OwnPrefix + "ingress": "network"}

// ensureNetwork makes the ingress network, with a subnet that is free on the
// server. Docker gives a static address only on a network with a configured
// subnet, and doesn't check a configured subnet against the server's routes,
// though it does check one it picks. So a throwaway network asks it to pick.
func (m *Manager) ensureNetwork(ctx context.Context) error {
	n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
	switch {
	case err == nil:
		if n.Labels[docker.OwnPrefix+"ingress"] == "" {
			return fmt.Errorf("a network named %[1]s is on this server, and Dockwell didn't make it, so the ingress can't use it. Rename or remove it with docker network rm %[1]s", docker.IngressNetwork)
		}
		if len(n.IPAM.Config) > 0 && m.table.Subnet != n.IPAM.Config[0].Subnet {
			m.table.Subnet = n.IPAM.Config[0].Subnet
			return m.table.save(m.Dir)
		}
		return nil
	case !docker.IsNotFound(err):
		return err
	}
	const probe = docker.IngressNetwork + "_probe"
	if _, err := m.Client.NetworkNamed(ctx, probe); err == nil {
		_ = m.Client.RemoveNetwork(ctx, probe)
	}
	if _, err := m.Client.CreateNetwork(ctx, &docker.NetworkCreate{Name: probe, Driver: "bridge", Internal: true, Labels: networkLabels}); err != nil {
		return fmt.Errorf("docker can't make a network for the ingress: %w. It may have no address space left: remove a network you don't need with docker network prune", err)
	}
	made, err := m.Client.NetworkNamed(ctx, probe)
	if err != nil {
		return err
	}
	if err := m.Client.RemoveNetwork(ctx, made.ID); err != nil {
		return fmt.Errorf("can't remove the network %s that found the ingress's subnet: %w", probe, err)
	}
	if len(made.IPAM.Config) == 0 || made.IPAM.Config[0].Subnet == "" {
		return errors.New("docker made a network with no IPv4 subnet, so the ingress can't have static addresses. Check the default-address-pools in /etc/docker/daemon.json")
	}
	subnet := made.IPAM.Config[0].Subnet
	if _, err := m.Client.CreateNetwork(ctx, &docker.NetworkCreate{
		Name: docker.IngressNetwork, Driver: "bridge", Internal: true, Options: networkOptions, Labels: networkLabels,
		IPAM: &docker.IPAM{Config: []docker.IPAMConfig{{Subnet: subnet}}},
	}); err != nil {
		return fmt.Errorf("docker can't make the network %s on %s: %w", docker.IngressNetwork, subnet, err)
	}
	m.table.Subnet = subnet
	m.table.Addresses = map[string]string{}
	return m.table.save(m.Dir)
}

// removeNetwork removes the ingress network, once nothing is on it.
func (m *Manager) removeNetwork(ctx context.Context) error {
	n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
	switch {
	case docker.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if err := m.Client.RemoveNetwork(ctx, n.ID); err != nil && !docker.IsNotFound(err) {
		return err
	}
	m.table.Subnet = ""
	m.table.Addresses = map[string]string{}
	return m.table.save(m.Dir)
}

// reserveAll gives a service's instances their addresses, its first if it has
// none, and puts each on the network.
func (m *Manager) reserveAll(ctx context.Context, s Service) ([]Instance, error) {
	instances := m.table.instancesOf(s)
	if len(instances) == 0 {
		instances = []Instance{{Project: s.Project, Service: s.Name, Number: 1}}
	}
	for _, i := range instances {
		if _, err := m.table.give(m.Dir, i, m.gateway(ctx)); err != nil {
			return nil, err
		}
		if err := m.attach(ctx, i); err != nil {
			return nil, err
		}
	}
	return instances, nil
}

// gateway is the network's gateway, which is no one's address. An engine may
// pick any in the subnet.
func (m *Manager) gateway(ctx context.Context) string {
	n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
	if err != nil || len(n.IPAM.Config) == 0 {
		return ""
	}
	return n.IPAM.Config[0].Gateway
}

// attach puts an instance's container on the network at its address, if there
// is one and it isn't on it. It works on a running container, which keeps
// running, so adding a route has no downtime.
func (m *Manager) attach(ctx context.Context, i Instance) error {
	c, err := m.Client.ContainerNamed(ctx, i.ContainerName())
	switch {
	case docker.IsNotFound(err):
		return nil // the container driver connects it when it makes one
	case err != nil:
		return err
	}
	addr := m.table.Addresses[i.String()]
	if ep, on := c.NetworkSettings.Networks[docker.IngressNetwork]; on {
		if ep != nil && ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address == addr {
			return nil
		}
		if err := m.Client.DisconnectNetwork(ctx, docker.IngressNetwork, c.ID); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("can't take %s off the network %s, as it has the wrong address: %w", i.ContainerName(), docker.IngressNetwork, err)
		}
	}
	if err := m.Client.ConnectEndpoint(ctx, docker.IngressNetwork, c.ID, endpoint(addr)); err != nil {
		return fmt.Errorf("can't put %s on the network %s at %s: %w", i.ContainerName(), docker.IngressNetwork, addr, err)
	}
	return nil
}

func endpoint(addr string) docker.EndpointSettings {
	return docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAM{IPv4Address: addr}}
}

// attachAll puts each instance on the table on the network, as something else
// may have made its container again, such as docker compose up.
func (m *Manager) attachAll(ctx context.Context) error {
	if m.table.Subnet == "" {
		return nil
	}
	if err := m.ensureNetwork(ctx); err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(m.table.Addresses)) {
		if i, ok := parseInstance(key); ok {
			if err := m.attach(ctx, i); err != nil {
				return err
			}
		}
	}
	return nil
}

// dialsOf are a route's upstreams: its service's instances' addresses and its
// port.
func (m *Manager) dialsOf(instances []Instance, port uint32) []string {
	out := make([]string, 0, len(instances))
	for _, i := range instances {
		out = append(out, m.table.Addresses[i.String()]+":"+strconv.Itoa(int(port)))
	}
	return out
}

// instanceOf is whose an upstream is, if it is anyone's the table knows.
func (m *Manager) instanceOf(up string) (Instance, bool) {
	host, _, err := splitUpstream(up)
	if err != nil {
		return Instance{}, false
	}
	return m.table.instanceAt(host)
}

// releaseUnused takes each service with no route left off the network, and
// frees its addresses. It goes by service, not by upstream: a rollout's new
// instance has an address before it has any traffic, and a drained one after.
func (m *Manager) releaseUnused(ctx context.Context) error {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	used := map[Service]bool{}
	for _, r := range routes {
		// A route edited by hand may be about any service, until someone
		// reverts or adopts it. Taking a service off the network now would
		// leave it nothing to go back to.
		if r.Edited {
			return nil
		}
		for _, up := range r.Upstreams {
			i, known := m.instanceOf(up)
			if !known {
				return nil
			}
			used[i.service()] = true
		}
	}
	var failed error
	for _, key := range slices.Sorted(maps.Keys(m.table.Addresses)) {
		i, ok := parseInstance(key)
		if !ok || used[i.service()] {
			continue
		}
		c, err := m.Client.ContainerNamed(ctx, i.ContainerName())
		if err == nil {
			if _, on := c.NetworkSettings.Networks[docker.IngressNetwork]; on {
				err = m.Client.DisconnectNetwork(ctx, docker.IngressNetwork, c.ID)
			}
		}
		if err != nil && !docker.IsNotFound(err) {
			failed = errors.Join(failed, err)
			continue
		}
		if err := m.table.free(m.Dir, i); err != nil {
			failed = errors.Join(failed, err)
		}
	}
	return failed
}

// routesOf are the routes that send to a service, as the proxy has them, with
// the port each sends to.
func (m *Manager) routesOf(ctx context.Context, s Service) ([]Route, uint32, error) {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []Route
	var port uint32
	for _, r := range routes {
		if r.Edited {
			continue
		}
		for _, up := range r.Upstreams {
			host, p, err := splitUpstream(up)
			if err != nil {
				continue
			}
			if i, known := m.table.instanceAt(host); known && i.service() == s {
				out = append(out, r)
				port = p
				break
			}
		}
	}
	return out, port, nil
}

// Endpoint says how a new container of an instance joins the ingress network.
func (m *Manager) Endpoint(ctx context.Context, i Instance) (string, docker.EndpointSettings, bool, error) {
	if err := m.mu.lock(ctx); err != nil {
		return "", docker.EndpointSettings{}, false, err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return "", docker.EndpointSettings{}, false, err
	}
	if a, ok := m.table.Addresses[i.String()]; ok {
		return docker.IngressNetwork, endpoint(a), true, nil
	}
	if len(m.table.instancesOf(i.service())) == 0 {
		return "", docker.EndpointSettings{}, false, nil
	}
	a, err := m.table.give(m.Dir, i, m.gateway(ctx))
	if err != nil {
		return "", docker.EndpointSettings{}, false, err
	}
	return docker.IngressNetwork, endpoint(a), true, nil
}

// AddUpstream sends new requests to the instance too, on every route that
// serves its service.
func (m *Manager) AddUpstream(ctx context.Context, i Instance) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	if len(m.table.instancesOf(i.service())) == 0 {
		return nil
	}
	a, err := m.table.give(m.Dir, i, m.gateway(ctx))
	if err != nil {
		return err
	}
	return m.editUpstreams(ctx, i.service(), func(r *Route, port uint32) {
		up := a + ":" + strconv.Itoa(int(port))
		if !slices.Contains(r.Upstreams, up) {
			r.Upstreams = append(r.Upstreams, up)
		}
	})
}

// editUpstreams changes each route of a service, as one reload for each.
func (m *Manager) editUpstreams(ctx context.Context, s Service, edit func(r *Route, port uint32)) error {
	routes, _, err := m.routesOf(ctx, s)
	if err != nil {
		return err
	}
	for k := range routes {
		r := &routes[k]
		_, port, err := splitUpstream(r.Upstreams[0])
		if err != nil {
			return err
		}
		edit(r, port)
		if err := m.Proxy.PutRoute(ctx, *r); err != nil {
			return fmt.Errorf("%s refused the route for %s: %w", m.Proxy.Name(), s.Name, err)
		}
	}
	return nil
}

// drainPoll is how often Drain looks at what is in flight.
const drainPoll = 250 * time.Millisecond

// Drain stops sending new requests to the instance, and waits until those in
// flight finish.
func (m *Manager) Drain(ctx context.Context, i Instance) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	if err := m.table.load(m.Dir); err != nil {
		m.mu.unlock()
		return err
	}
	a, ok := m.table.Addresses[i.String()]
	if !ok {
		m.mu.unlock()
		return nil
	}
	var ups []string
	err := m.editUpstreams(ctx, i.service(), func(r *Route, port uint32) {
		up := a + ":" + strconv.Itoa(int(port))
		ups = append(ups, up)
		if len(r.Upstreams) == 1 && r.Upstreams[0] == up {
			return
		}
		r.Upstreams = slices.DeleteFunc(r.Upstreams, func(u string) bool { return u == up })
	})
	m.mu.unlock()
	if err != nil {
		return err
	}
	t := time.NewTicker(drainPoll)
	defer t.Stop()
	for {
		total := 0
		for _, up := range ups {
			n, err := m.Proxy.InFlight(ctx, up)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			total += n
		}
		if total == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			m.logger().InfoContext(ctx, "stopped waiting for requests in flight to an instance going away", slog.String("instance", i.String()), slog.Int("in_flight", total))
			return nil
		case <-t.C:
		}
	}
}

// RemoveUpstream forgets the instance and frees its address. Its service keeps
// at least one: a route needs an upstream.
func (m *Manager) RemoveUpstream(ctx context.Context, i Instance) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	a, ok := m.table.Addresses[i.String()]
	if !ok || len(m.table.instancesOf(i.service())) < 2 {
		return nil
	}
	if err := m.editUpstreams(ctx, i.service(), func(r *Route, port uint32) {
		up := a + ":" + strconv.Itoa(int(port))
		r.Upstreams = slices.DeleteFunc(r.Upstreams, func(u string) bool { return u == up })
	}); err != nil {
		return err
	}
	return m.table.free(m.Dir, i)
}
