// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"encoding/binary"
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

// give records an address for an instance, the lowest free one past the
// gateway, and returns it. busy are addresses that aren't to be given, whatever
// the table says: what the proxy still dials above all, as an address it dials
// goes to whichever container has it.
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
	for _, r := range routes {
		for _, up := range r.Upstreams {
			if host, _, err := splitUpstream(up); err == nil {
				busy = append(busy, host)
			}
		}
	}
	return m.table.give(m.Dir, i, m.gateway(ctx), busy)
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
	return slices.ContainsFunc(routes, func(r Route) bool { return len(r.Upstreams) > 0 }), nil
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
func (m *Manager) ensureKeeper(ctx context.Context) {
	if m.KeeperImage == "" {
		return
	}
	if err := m.keepNetwork(ctx); err != nil && ctx.Err() == nil {
		m.logger().WarnContext(ctx, "can't run the container that keeps the ingress network from being pruned, so docker network prune could remove it. If it does, the agent makes it again on its own subnet; the agent tries again within a minute",
			slog.String("container", KeeperName), slog.String("error", err.Error()))
	}
}

func (m *Manager) keepNetwork(ctx context.Context) error {
	addr, ok := keeperAddress(m.table.Subnet)
	if !ok {
		return nil
	}
	got, err := m.Client.ContainerNamed(ctx, KeeperName)
	switch {
	case err == nil:
		if got.Config.Labels[docker.OwnPrefix+"ingress"] == "" {
			return fmt.Errorf("a container named %[1]s is on this server, and Dockwell didn't make it. Rename or remove it with docker rename %[1]s or docker rm %[1]s", KeeperName)
		}
		if got.Config.Image != m.KeeperImage {
			if err := m.Client.RemoveContainer(ctx, got.ID, true); err != nil && !docker.IsNotFound(err) {
				return err
			}
			break
		}
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
		// Asleep on a network that has gone: it can't start, so it goes.
		if err := m.Client.RemoveContainer(ctx, got.ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	case !docker.IsNotFound(err):
		return err
	}
	if _, err := m.Client.InspectImage(ctx, m.KeeperImage); docker.IsNotFound(err) {
		if err := m.Client.PullImage(ctx, m.KeeperImage, func(string) {}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	id, err := m.Client.CreateContainer(ctx, KeeperName, &docker.CreateRequest{
		Config: docker.Config{
			Image: m.KeeperImage, Cmd: []string{"sleep", "infinity"}, User: "65532:65532",
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

// reserveAll gives a service's instances their addresses, its first if it has
// none, and puts each on the network.
func (m *Manager) reserveAll(ctx context.Context, s Service) ([]Instance, error) {
	instances := m.table.instancesOf(s)
	if len(instances) == 0 {
		instances = []Instance{{Project: s.Project, Service: s.Name, Number: 1}}
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
