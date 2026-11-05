// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// Move sends new requests to an instance in place of others, on every route
	// that serves its service, with no reload of the proxy: what is in flight to
	// the others goes on. Drain waits for that.
	Move(ctx context.Context, to Instance, from []Instance) error
	// RemoveUpstream forgets the instance and frees its address.
	RemoveUpstream(ctx context.Context, instance Instance) error
	// Reachable says why a new instance can't take a route's traffic yet: a
	// port a route of its service sends to that doesn't take a connection on
	// its address. It answers nil for a service with no routes.
	Reachable(ctx context.Context, instance Instance) error
	// Hold keeps Reconcile off a service while a rollout moves its instances,
	// until release is called. An address a rollout reserved has no container
	// yet, which Reconcile would otherwise take for what a crash left.
	Hold(service Service) (release func())
	// Routing says what a rollout has to know of a service's routes before it
	// decides how to replace it.
	Routing(ctx context.Context, service Service) (Routing, error)
}

// Routing is what a rollout has to know of a service's routes.
type Routing struct {
	// Routed says a route sends to the service.
	Routed bool
	// Edited names routes edited by hand that send to it, or could: Dockwell
	// leaves those as they are, so they go on dialing what they dial.
	Edited []string
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
	// Owners says which service a route to a port on the server belongs to, by the
	// route's ID. That port is all the proxy knows, and the service is part of what
	// the route says, so without this it would read as drift for as long as it ran.
	Owners map[string]string `json:"owners,omitempty"`
	// Last is the address given out last. Giving out goes on from here, round
	// the subnet, so an address isn't used again until all the others have been.
	// A config a reload left behind can still send to an address, for as long as
	// a connection stays on it, and that must find nothing rather than another
	// service's container, with this one's cookies (ADR 0027).
	Last string `json:"last,omitempty"`
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
	if err := writeDurably(filepath.Join(dir, addressesFile), b); err != nil {
		return fmt.Errorf("can't keep the ingress's addresses: %w", err)
	}
	return nil
}

// writeDurably replaces a file, whole or not at all, even across a power cut: an
// empty table would stop this server's ingress until a person mended it.
func writeDurably(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // under the ingress's own directory
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
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

// give records an address for an instance, the next free one past the last it
// gave, round the subnet, and returns it. So an address isn't given again until
// the subnet has gone round, and a config or a connection that outlives a rollout
// finds nothing there rather than another app. busy are addresses that aren't to
// be given, whatever the table says: what the proxy still dials above all, as an
// address it dials goes to whichever container has it.
func (t *table) give(dir string, i Instance, gateway string, busy []string) (string, error) {
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
	for _, a := range busy {
		used[a] = true
	}
	// The network's own address isn't a host's, nor is the one Docker keeps
	// for its gateway when it has none on record.
	first := prefix.Addr().Next().Next()
	start := first
	if last, err := netip.ParseAddr(t.Last); err == nil && prefix.Contains(last) && prefix.Contains(last.Next()) && last.Compare(first) >= 0 {
		start = last.Next()
	}
	take := func(from, to netip.Addr) (string, bool) {
		for a := from; prefix.Contains(a) && a.Compare(to) < 0; a = a.Next() {
			if !used[a.String()] && prefix.Contains(a.Next()) {
				return a.String(), true
			}
		}
		return "", false
	}
	a, ok := take(start, lastOf(prefix))
	if !ok {
		a, ok = take(first, start)
	}
	if !ok {
		return "", fmt.Errorf("the ingress network's subnet %s has no address left, so %s can't join it. Remove some routes", t.Subnet, i)
	}
	lastBefore := t.Last
	t.Addresses[i.String()], t.Last = a, a
	if err := t.save(dir); err != nil {
		delete(t.Addresses, i.String())
		t.Last = lastBefore
		return "", err
	}
	return a, nil
}

// lastOf is the top address of a subnet, its broadcast address, which take stops
// short of.
func lastOf(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	bits := p.Bits()
	for i := bits; i < 32; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	return netip.AddrFrom4(b)
}

// give gives an instance an address, one the proxy doesn't dial for anyone: the
// table can lag what the proxy has, as after a table restored by hand, and a
// proxy's dial goes to whichever container has the address. If the proxy can't
// say what it dials, none is given.
func (m *Manager) give(ctx context.Context, i Instance) (string, error) {
	if a, ok := m.table.Addresses[i.String()]; ok {
		return a, nil
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return "", fmt.Errorf("can't give %s an address, as what the proxy dials can't be read: %w", i, err)
	}
	var busy []string
	if a, ok := keeperAddress(m.table.Subnet); ok {
		busy = append(busy, a)
	}
	for k := range routes {
		r := &routes[k]
		for _, up := range r.Upstreams {
			if host, _, err := splitUpstream(up); err == nil {
				busy = append(busy, host)
			}
		}
	}
	return m.table.give(m.Dir, i, m.gateway(ctx), busy)
}

// free takes an instance's address off the table.
// own notes whose route this is, or that it's nobody's, and keeps that.
func (t *table) own(dir, route, service string) error {
	if t.Owners[route] == service {
		return nil
	}
	was, had := t.Owners[route]
	if service == "" {
		delete(t.Owners, route)
	} else {
		if t.Owners == nil {
			t.Owners = map[string]string{}
		}
		t.Owners[route] = service
	}
	if err := t.save(dir); err != nil {
		if had {
			t.Owners[route] = was
		} else {
			delete(t.Owners, route)
		}
		return err
	}
	return nil
}

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
//
// A network that has gone, as docker network prune removes one with no running
// container on it, comes back on its recorded subnet with its table kept, as the
// proxy's routes still dial those addresses (restoreNetwork).
func (m *Manager) ensureNetwork(ctx context.Context) error {
	n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
	switch {
	case err == nil:
		if n.Labels[docker.OwnPrefix+"ingress"] == "" {
			return fmt.Errorf("a network named %[1]s is on this server, and Dockwell didn't make it, so the ingress can't use it. Rename or remove it with docker network rm %[1]s", docker.IngressNetwork)
		}
		if len(n.IPAM.Config) == 0 || m.table.Subnet == n.IPAM.Config[0].Subnet {
			return nil
		}
		// A table with addresses on another subnet would have the proxy dial
		// addresses this network doesn't have, and something else may.
		if m.table.Subnet != "" {
			return m.closeRoutes(ctx, fmt.Errorf("the network %s has the subnet %s, and this server's routes dial addresses on %s, which it may no longer reach, so every route on this server answers 503 until they agree. Remove the network with docker network rm %s, and the agent makes it again on its own subnet within a minute",
				docker.IngressNetwork, n.IPAM.Config[0].Subnet, m.table.Subnet, docker.IngressNetwork))
		}
		m.table.Subnet = n.IPAM.Config[0].Subnet
		return m.table.save(m.Dir)
	case !docker.IsNotFound(err):
		return err
	}
	dials, err := m.dialing(ctx)
	if err != nil {
		return err
	}
	if m.table.Subnet != "" && (dials || len(m.table.Addresses) > 0) {
		return m.restoreNetwork(ctx)
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
	if err := m.createNetwork(ctx, subnet); err != nil {
		return fmt.Errorf("docker can't make the network %s on %s: %w", docker.IngressNetwork, subnet, err)
	}
	m.table.Subnet = subnet
	m.table.Addresses = map[string]string{}
	return m.table.save(m.Dir)
}

func (m *Manager) createNetwork(ctx context.Context, subnet string) error {
	_, err := m.Client.CreateNetwork(ctx, &docker.NetworkCreate{
		Name: docker.IngressNetwork, Driver: "bridge", Internal: true, Options: networkOptions, Labels: networkLabels,
		IPAM: &docker.IPAM{Config: []docker.IPAMConfig{{Subnet: subnet}}},
	})
	return err
}

// dialing reports a proxy with a route that dials an address, which goes to
// whichever container has it.
func (m *Manager) dialing(ctx context.Context) (bool, error) {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return false, err
	}
	return dialsContainers(routes), nil
}

// dialsContainers reports a route that dials anything but a port on the server
// itself, which needs no network, and no container can take.
func dialsContainers(routes []Route) bool {
	return slices.ContainsFunc(routes, func(r Route) bool {
		return slices.ContainsFunc(r.Upstreams, func(up string) bool {
			host, _, err := splitUpstream(up)
			return err != nil || host != ServerHost
		})
	})
}

// restoreNetwork makes the ingress network again on its recorded subnet, and
// keeps the table, as the proxy's routes still dial those addresses, and each
// instance's container still asks for its own. If it were made anew instead,
// another service could be given an address a route dials, and take its users.
//
// If another network has the subnet now, as Docker gives out the first free one,
// it isn't made anywhere else, and no address is reused. Every route answers 503
// until a person sorts it out, as what they dial could be that network's
// containers. The panel's site isn't a route, and stays up.
func (m *Manager) restoreNetwork(ctx context.Context) error {
	subnet := m.table.Subnet
	taker, err := m.networkOn(ctx, subnet)
	if err != nil {
		return err
	}
	if taker == "" {
		err = m.createNetwork(ctx, subnet)
		switch {
		case err == nil:
			m.logger().WarnContext(ctx, "the ingress network had gone, as docker network prune removes one with no running container on it, so the agent made it again on its own subnet, with each service's address as it was",
				slog.String("network", docker.IngressNetwork), slog.String("subnet", subnet))
			return nil
		case docker.IsUnavailable(err):
			return err
		}
		return m.closeRoutes(ctx, fmt.Errorf("the ingress network %s has gone, and docker can't make it again on its subnet %s: %w. The routes on this server could reach whatever now has those addresses, so each answers 503 until the network is back. Free that subnet, and the agent makes the network again within a minute; or delete this server's routes and add them again, which starts on another subnet",
			docker.IngressNetwork, subnet, err))
	}
	return m.closeRoutes(ctx, fmt.Errorf("the ingress network %s has gone, and the network %s now has its subnet, %s. The routes on this server could reach that network's containers, so each answers 503 until the network is back. Remove it with docker network rm %s, and the agent makes the ingress network again within a minute; or delete this server's routes and add them again, which starts on another subnet",
		docker.IngressNetwork, taker, subnet, taker))
}

// networkOn names a network that overlaps a subnet, or is empty.
func (m *Manager) networkOn(ctx context.Context, subnet string) (string, error) {
	want, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "", fmt.Errorf("the ingress's subnet %q isn't one: %w", subnet, err)
	}
	list, err := m.Client.ListNetworks(ctx)
	if err != nil {
		return "", err
	}
	for i := range list {
		for _, c := range list[i].IPAM.Config {
			if have, err := netip.ParsePrefix(c.Subnet); err == nil && have.Overlaps(want) {
				return list[i].Name, nil
			}
		}
	}
	return "", nil
}

// closeRoutes has every route answer 503, and returns why, for the step or the
// log. Its failure is added, as a person then needs to know both.
func (m *Manager) closeRoutes(ctx context.Context, why error) error {
	if err := m.Proxy.SetClosed(context.WithoutCancel(ctx), true); err != nil {
		return errors.Join(why, fmt.Errorf("and %s can't answer 503 for them: %w", m.Proxy.Name(), err))
	}
	return why
}

// KeeperName is the name of what keeps the ingress network from being pruned.
const KeeperName = ContainerName + "-keep"

// keeperLabel marks the keeper as the ingress's.
const keeperLabel = "keeper"

// Limits on the keeper, which only sleeps.
const (
	keeperMemory = 16 << 20
	keeperPids   = 8
)

// keeperAddress is the keeper's address on a subnet: its last host, as
// instances' come from its start. ok is false for what isn't an IPv4 subnet.
func keeperAddress(subnet string) (string, bool) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return "", false
	}
	base := prefix.Masked().Addr().As4()
	n := binary.BigEndian.Uint32(base[:]) | ((uint32(1) << (32 - prefix.Bits())) - 1)
	var last [4]byte
	binary.BigEndian.PutUint32(last[:], n-1)
	return netip.AddrFrom4(last).String(), true
}

// ensureKeeper keeps a container asleep on the ingress network. docker network
// prune, which docker system prune runs, removes a network with no running
// container on it, and every routed service may be stopped at once. Without the
// network, the proxy dials addresses that anything Docker next makes could have.
// It runs Caddy's image, which the server has already, as a user that isn't root,
// with nothing else: no capabilities, no mounts, and no way out of a network that
// has none. It is best effort, as the rest holds without it.
//
// inStep says a route's step is running, under a plan someone asked for. A keeper
// moves to a new pin only then, as the proxy does: all it does is sleep, so what
// it runs doesn't matter, and an upgrade of Dockwell moves nothing on its own.
// Nothing but a step pulls either: that can take minutes with the manager's lock
// held, and every plan on this server waits for it.
func (m *Manager) ensureKeeper(ctx context.Context, inStep bool) {
	if m.KeeperImage == "" {
		return
	}
	if err := m.keepNetwork(ctx, inStep); err != nil && ctx.Err() == nil {
		m.logger().WarnContext(ctx, "can't run the container that keeps the ingress network from being pruned, so docker network prune could remove it. If it does, the agent makes it again on its own subnet; the agent tries again within a minute",
			slog.String("container", KeeperName), slog.String("error", err.Error()))
	}
}

func (m *Manager) keepNetwork(ctx context.Context, inStep bool) error {
	addr, ok := keeperAddress(m.table.Subnet)
	if !ok {
		return nil
	}
	// What it runs next, when it needs making: found first, so that a keeper that
	// runs isn't taken away for what can't be had.
	image, err := m.keeperImage(ctx, inStep)
	if err != nil {
		return err
	}
	got, err := m.Client.ContainerNamed(ctx, KeeperName)
	switch {
	case err == nil:
		if got.Config.Labels[docker.OwnPrefix+"ingress"] == "" {
			return fmt.Errorf("a container named %[1]s is on this server, and Dockwell didn't make it. Rename or remove it with docker rename %[1]s or docker rm %[1]s", KeeperName)
		}
		replace := inStep && image != "" && got.Config.Image != image
		if !replace {
			if got.State.Running {
				return nil
			}
			ep := got.NetworkSettings.Networks[docker.IngressNetwork]
			n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
			if err != nil {
				return err
			}
			if ep != nil && (ep.NetworkID == "" || ep.NetworkID == n.ID) {
				return m.Client.StartContainer(ctx, got.ID)
			}
		}
		// On another pin in a step, or asleep on a network that has gone, which it
		// can't start on: it goes.
		if err := m.Client.RemoveContainer(ctx, got.ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	case !docker.IsNotFound(err):
		return err
	}
	if image == "" {
		return fmt.Errorf("the server doesn't have %s yet, so it waits for this server's next route change", m.KeeperImage)
	}
	id, err := m.Client.CreateContainer(ctx, KeeperName, &docker.CreateRequest{
		Config: docker.Config{
			Image: image, Cmd: []string{"sleep", "infinity"}, User: "65532:65532",
			Labels: map[string]string{docker.OwnPrefix + "ingress": keeperLabel},
		},
		HostConfig: docker.HostConfig{
			NetworkMode: docker.IngressNetwork, RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			CapDrop: []string{"ALL"}, ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges"},
			Memory: keeperMemory, MemorySwap: keeperMemory, PidsLimit: keeperPids,
			LogConfig: &docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "1"}},
		},
		NetworkingConfig: docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{docker.IngressNetwork: {IPAMConfig: &docker.EndpointIPAM{IPv4Address: addr}}}},
	})
	if err != nil {
		return err
	}
	return m.Client.StartContainer(ctx, id)
}

// keeperImage is what a keeper can run now: the pinned image if the server has it,
// or else, in a step, once that has pulled it. With neither, outside a step, it is
// what the proxy runs, which the server must have, and failing that, nothing.
func (m *Manager) keeperImage(ctx context.Context, inStep bool) (string, error) {
	_, err := m.Client.InspectImage(ctx, m.KeeperImage)
	switch {
	case err == nil:
		return m.KeeperImage, nil
	case !docker.IsNotFound(err):
		return "", err
	}
	if inStep {
		if err := m.Client.PullImage(ctx, m.KeeperImage, func(string) {}); err != nil {
			return "", err
		}
		return m.KeeperImage, nil
	}
	st, err := m.Proxy.Status(ctx)
	if err != nil || st.Image == "" {
		return "", err
	}
	if _, err := m.Client.InspectImage(ctx, st.Image); err != nil {
		if docker.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return st.Image, nil
}

// removeNetwork removes the ingress network, and what kept it, once nothing is
// on it. A network that has gone already is as good as removed, so what was
// recorded of it goes either way.
func (m *Manager) removeNetwork(ctx context.Context) error {
	if got, err := m.Client.ContainerNamed(ctx, KeeperName); err == nil && got.Config.Labels[docker.OwnPrefix+"ingress"] != "" {
		if err := m.Client.RemoveContainer(ctx, got.ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
	switch {
	case docker.IsNotFound(err):
	case err != nil:
		return err
	default:
		if err := m.Client.RemoveNetwork(ctx, n.ID); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	m.table.Subnet = ""
	m.table.Addresses = map[string]string{}
	return m.table.save(m.Dir)
}

// serving are the instances a route of a service should send to: those its
// other routes send to, or, on its first, the oldest of its containers that
// runs. A new instance of a rollout has an address before it is healthy, so
// having an address isn't enough: it joins with AddUpstream, once it is.
func (m *Manager) serving(ctx context.Context, s Service) ([]Instance, error) {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[Instance]bool{}
	var out []Instance
	for k := range routes {
		r := &routes[k]
		if r.Edited {
			continue
		}
		for _, up := range r.Upstreams {
			if i, known := m.instanceOf(up); known && i.service() == s && !seen[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
	}
	if len(out) > 0 {
		slices.SortFunc(out, func(a, b Instance) int { return a.Number - b.Number })
		return out, nil
	}
	list, err := m.Client.ListContainers(ctx, map[string][]string{
		"label": {docker.LabelProject + "=" + s.Project, docker.LabelService + "=" + s.Name},
	})
	if err != nil {
		return nil, err
	}
	number, running := 0, false
	for k := range list {
		c := &list[k]
		if c.Labels[docker.LabelOneoff] == "True" {
			continue
		}
		n, up := c.Number(), c.State == "running"
		if number == 0 || up && !running || up == running && n < number {
			number, running = n, up
		}
	}
	return []Instance{{Project: s.Project, Service: s.Name, Number: max(number, 1)}}, nil
}

// reserveAll gives the instances a route of a service sends to their addresses,
// and puts each on the network.
func (m *Manager) reserveAll(ctx context.Context, s Service) ([]Instance, error) {
	instances, err := m.serving(ctx, s)
	if err != nil {
		return nil, err
	}
	for _, i := range instances {
		if _, err := m.give(ctx, i); err != nil {
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
	if c.HostConfig.NetworkMode == hostNetworkMode {
		return serversNetwork(i.Service) // a plan refuses this, and one may be old
	}
	addr := m.table.Addresses[i.String()]
	if ep, on := c.NetworkSettings.Networks[docker.IngressNetwork]; on {
		n, err := m.Client.NetworkNamed(ctx, docker.IngressNetwork)
		if err != nil {
			return err
		}
		// A stopped container keeps the ID of a network that has gone, and
		// can't start until it comes off that one.
		current := ep != nil && (ep.NetworkID == "" || ep.NetworkID == n.ID)
		if current && ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address == addr {
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

// hostNetworkMode is how Docker says a container is on the server's own network.
const hostNetworkMode = "host"

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

// settle takes off the table, and out of the routes, each instance whose
// container is gone while another of its service's isn't, as a rollout that
// stopped between removing a container and forgetting it leaves one. A service's
// last instance stays on the table however it stands: a route needs an upstream,
// and that is where the container comes back. It leaves a service a rollout
// holds alone.
func (m *Manager) settle(ctx context.Context) error {
	there := map[Instance]bool{}
	byService := map[Service][]Instance{}
	for _, key := range slices.Sorted(maps.Keys(m.table.Addresses)) {
		i, ok := parseInstance(key)
		if !ok || m.held(i.service()) {
			continue
		}
		// By its labels, as what a rollout is stopping or swapping in has another
		// name for now, and is there all the same.
		list, err := m.Client.ListContainers(ctx, map[string][]string{"label": {
			docker.LabelProject + "=" + i.Project, docker.LabelService + "=" + i.Service, docker.LabelNumber + "=" + strconv.Itoa(i.Number),
		}})
		if err != nil {
			return err
		}
		there[i] = slices.ContainsFunc(list, func(c docker.ContainerSummary) bool { return c.Labels[docker.LabelOneoff] != "True" })
		byService[i.service()] = append(byService[i.service()], i)
	}
	var failed error
	for _, s := range slices.SortedFunc(maps.Keys(byService), func(a, b Service) int { return strings.Compare(a.Project+"/"+a.Name, b.Project+"/"+b.Name) }) {
		instances := byService[s]
		if !slices.ContainsFunc(instances, func(i Instance) bool { return there[i] }) {
			continue
		}
		for _, i := range instances {
			if there[i] {
				continue
			}
			m.logger().InfoContext(ctx, "forgetting an instance whose container is gone, as a rollout that stopped partway leaves one", slog.String("instance", i.String()))
			a := m.table.Addresses[i.String()]
			err := m.editUpstreams(ctx, s, func(r *Route, port uint32) {
				up := a + ":" + strconv.Itoa(int(port))
				r.Upstreams = slices.DeleteFunc(r.Upstreams, func(u string) bool { return u == up })
			})
			if err == nil {
				err = m.table.free(m.Dir, i)
			}
			failed = errors.Join(failed, err)
		}
	}
	return failed
}

// Hold keeps Reconcile off a service while a rollout moves its instances.
func (m *Manager) Hold(s Service) (release func()) {
	m.rolloutsMu.Lock()
	defer m.rolloutsMu.Unlock()
	if m.rollouts == nil {
		m.rollouts = map[Service]int{}
	}
	m.rollouts[s]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.rolloutsMu.Lock()
			defer m.rolloutsMu.Unlock()
			m.rollouts[s]--
			if m.rollouts[s] <= 0 {
				delete(m.rollouts, s)
			}
		})
	}
}

// Routing says what a rollout has to know of a service's routes. A route edited by
// hand isn't moved, and it may not say what it dials at all, so one in the service's
// project that doesn't read as sending elsewhere counts as this service's.
func (m *Manager) Routing(ctx context.Context, s Service) (Routing, error) {
	if err := m.mu.lock(ctx); err != nil {
		return Routing{}, err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return Routing{}, err
	}
	if len(m.table.instancesOf(s)) == 0 {
		return Routing{}, nil
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return Routing{}, err
	}
	var out Routing
	prefix := "dockwell." + s.Project + "."
	for k := range routes {
		r := &routes[k]
		ours, readable := false, false
		for _, up := range r.Upstreams {
			host, _, err := splitUpstream(up)
			if err != nil {
				continue
			}
			readable = true
			if i, known := m.table.instanceAt(host); known && i.service() == s {
				ours = true
			}
		}
		switch {
		case r.Edited && (ours || !readable && strings.HasPrefix(r.ID, prefix)):
			out.Edited = append(out.Edited, strings.TrimPrefix(r.ID, prefix))
		case ours:
			out.Routed = true
		}
	}
	slices.Sort(out.Edited)
	return out, nil
}

// held reports a rollout moving a service's instances.
func (m *Manager) held(s Service) bool {
	m.rolloutsMu.Lock()
	defer m.rolloutsMu.Unlock()
	return m.rollouts[s] > 0
}

// reachWait bounds one try at a route's port on a new instance.
const reachWait = time.Second

// Reachable says why a new instance can't take a route's traffic yet: a port a
// route of its service sends to that doesn't take a connection. A service with
// no routes has none, and neither does an instance with no address. A rollout
// asks this of a service with no health check, as otherwise "it runs" would be
// all it knew, and the first request would find an app still starting.
func (m *Manager) Reachable(ctx context.Context, i Instance) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	if err := m.table.load(m.Dir); err != nil {
		m.mu.unlock()
		return err
	}
	a, ok := m.table.Addresses[i.String()]
	var routes []Route
	var err error
	if ok {
		routes, err = m.routesOf(ctx, i.service())
	}
	m.mu.unlock()
	if err != nil {
		return err
	}
	seen := map[uint32]bool{}
	for k := range routes {
		_, port, err := splitUpstream(routes[k].Upstreams[0])
		if err != nil || seen[port] {
			continue
		}
		seen[port] = true
		dial, cancel := context.WithTimeout(ctx, reachWait)
		conn, err := (&net.Dialer{}).DialContext(dial, "tcp", net.JoinHostPort(a, strconv.Itoa(int(port))))
		cancel()
		if err != nil {
			return fmt.Errorf("nothing takes a connection on port %d, which a route for %s sends to. Check the route's port, and that the app listens on every address in its container, not only on localhost", port, i.Service)
		}
		_ = conn.Close()
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
	for k := range routes {
		r := &routes[k]
		// A route edited by hand may be about any service, until someone
		// reverts or adopts it. Taking a service off the network now would
		// leave it nothing to go back to.
		if r.Edited {
			return nil
		}
		for _, up := range r.Upstreams {
			i, known := m.instanceOf(up)
			if !known {
				if host, _, err := splitUpstream(up); err == nil && host == ServerHost {
					continue // a port on the server itself, which is no service's
				}
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

// newestOf is the container of a service with the highest number, which is what
// reads as the service, or nil for none: a rollout moves a service up its numbers.
func (m *Manager) newestOf(ctx context.Context, s Service) (*docker.Container, error) {
	list, err := m.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + s.Project, docker.LabelService + "=" + s.Name}})
	if err != nil {
		return nil, err
	}
	id, highest := "", -1
	for i := range list {
		n, err := strconv.Atoi(list[i].Labels[docker.LabelNumber])
		if err == nil && n > highest && list[i].Labels[docker.LabelOneoff] != "True" {
			id, highest = list[i].ID, n
		}
	}
	if id == "" {
		return nil, nil
	}
	c, err := m.Client.InspectContainer(ctx, id)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	return c, err
}

// routesOf are the routes that send to a service, as the proxy has them.
func (m *Manager) routesOf(ctx context.Context, s Service) ([]Route, error) {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil, err
	}
	var out []Route
	for k := range routes {
		r := &routes[k]
		if r.Edited {
			continue
		}
		for _, up := range r.Upstreams {
			host, _, err := splitUpstream(up)
			if err != nil {
				continue
			}
			if i, known := m.table.instanceAt(host); known && i.service() == s {
				out = append(out, *r)
				break
			}
		}
	}
	return out, nil
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
	if len(m.table.instancesOf(i.service())) == 0 {
		return "", docker.EndpointSettings{}, false, nil
	}
	// It may have gone, and the container needs it to start.
	if err := m.ensureNetwork(ctx); err != nil {
		return "", docker.EndpointSettings{}, false, err
	}
	a, err := m.give(ctx, i)
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
	a, err := m.give(ctx, i)
	if err != nil {
		return err
	}
	// A route may have come to the service while the instance was starting, so
	// it may not be on the network yet. It joins while it runs, with no gap.
	if err := m.attach(ctx, i); err != nil {
		return err
	}
	return m.editUpstreams(ctx, i.service(), func(r *Route, port uint32) {
		up := a + ":" + strconv.Itoa(int(port))
		if !slices.Contains(r.Upstreams, up) {
			r.Upstreams = append(r.Upstreams, up)
		}
	})
}

// Move sends new requests to an instance in place of others. A route that sends
// to one upstream dials through a file, so this reloads nothing, and no other site
// on the server pays for it, or needs the proxy to be running (ADR 0027).
func (m *Manager) Move(ctx context.Context, to Instance, from []Instance) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	if len(m.table.instancesOf(to.service())) == 0 {
		return nil
	}
	a, err := m.give(ctx, to)
	if err != nil {
		return err
	}
	// A route may have come to the service while the instance was starting, so
	// it may not be on the network yet. It joins while it runs, with no gap.
	if err := m.attach(ctx, to); err != nil {
		return err
	}
	var gone []string
	for _, i := range from {
		if old, ok := m.table.Addresses[i.String()]; ok && i != to {
			gone = append(gone, old)
		}
	}
	return m.editUpstreams(ctx, to.service(), func(r *Route, port uint32) {
		r.Upstreams = slices.DeleteFunc(r.Upstreams, func(u string) bool {
			host, _, err := splitUpstream(u)
			return err == nil && slices.Contains(gone, host)
		})
		up := a + ":" + strconv.Itoa(int(port))
		if !slices.Contains(r.Upstreams, up) {
			r.Upstreams = append(r.Upstreams, up)
		}
	})
}

// editUpstreams changes each route of a service, in one call to the proxy. One the
// edit leaves as it was isn't sent again, so a rollout that finds its work done, as
// on going on after a crash, does nothing.
func (m *Manager) editUpstreams(ctx context.Context, s Service, edit func(r *Route, port uint32)) error {
	routes, err := m.routesOf(ctx, s)
	if err != nil {
		return err
	}
	var changed []Route
	for k := range routes {
		r := &routes[k]
		_, port, err := splitUpstream(r.Upstreams[0])
		if err != nil {
			return err
		}
		before := slices.Clone(r.Upstreams)
		edit(r, port)
		if !slices.Equal(before, r.Upstreams) {
			changed = append(changed, *r)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := m.Proxy.PutRoutes(ctx, changed); err != nil {
		return fmt.Errorf("%s refused the routes for %s: %w", m.Proxy.Name(), s.Name, err)
	}
	return nil
}

// How Drain looks at what is in flight: this often, until it reads nothing this
// many times in a row.
const (
	drainPoll  = 250 * time.Millisecond
	drainClear = 2
)

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
	t := time.NewTicker(cmp.Or(m.DrainPoll, drainPoll))
	defer t.Stop()
	// A request the old config took just before the reload may not be counted
	// yet, so nothing in flight counts only when it is read twice, a poll
	// apart, once the reload is done.
	clearReads := 0
	for {
		select {
		case <-ctx.Done():
			m.logger().InfoContext(ctx, "stopped waiting for requests in flight to an instance going away", slog.String("instance", i.String()))
			return nil
		case <-t.C:
		}
		total := 0
		for _, up := range ups {
			n, err := m.Proxy.InFlight(ctx, up)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// It can't say it's clear, so it has what it was given to finish.
				m.logger().WarnContext(ctx, "can't read what is in flight to an instance going away, so it gets all of its time to finish", slog.String("instance", i.String()), slog.String("error", err.Error()))
				n = 1
			}
			total += n
		}
		if total > 0 {
			clearReads = 0
			continue
		}
		clearReads++
		if clearReads >= drainClear {
			return nil
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
