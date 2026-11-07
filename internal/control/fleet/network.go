// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionPublicAddress is the audit log's action for a person setting a server's
// public address.
const ActionPublicAddress = "server.public_address"

// ErrNotAPublicAddress reports an address that can't be a server's public one.
var ErrNotAPublicAddress = errors.New("not a public address")

// sighting is what one source knows of a server's public address.
type sighting struct {
	address string
	source  dockwellv1.PublicAddressSource
	// domain is the one that proved it, for a domain's.
	domain string
}

// publicSources are the ways of knowing a server's public address, in the order
// they win. A new one goes here, and nowhere else: each only looks at what the
// hub has heard, and says what that makes of the address, or nothing (ADR 0034).
var publicSources = []func(*live) (sighting, bool){
	setByPerson,
	onInterface,
	provedByDomain,
	seenByControlPlane,
	fromMetadata,
	lookedUp,
}

// publicAddress is what the first source that knows says.
func publicAddress(l *live) sighting {
	for _, source := range publicSources {
		if seen, ok := source(l); ok {
			return seen
		}
	}
	return sighting{}
}

func setByPerson(l *live) (sighting, bool) {
	if l.person.GetPublicAddress() == "" {
		return sighting{}, false
	}
	return sighting{address: l.person.GetPublicAddress(), source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON}, true
}

func onInterface(l *live) (sighting, bool) {
	address, ok := preferred(resource.PublicAddresses(l.inventory.GetAddresses()))
	return sighting{address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_ON_INTERFACE}, ok
}

// provedByDomain finds a certificate that came for a name whose record points at
// a public address. The agent reads a domain's DNS as it waits for its certificate,
// and once more if it starts holding one, so that's what the record said around
// when the certificate came. One that points at a shared range such as Cloudflare's,
// or at more than one address, says nothing of which is this server's.
func provedByDomain(l *live) (sighting, bool) {
	for _, status := range l.domains {
		cert, dns := status.GetCertificate(), status.GetDns()
		if !served(cert) || !pointsHere(dns) {
			continue
		}
		found := slices.DeleteFunc(resource.PublicAddresses(dns.GetFound()), func(s string) bool {
			a, err := netip.ParseAddr(s)
			return err != nil || ingress.IsCloudflare(a)
		})
		if len(found) != len(dns.GetFound()) {
			continue
		}
		if address, ok := only(found); ok {
			return sighting{address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_PROVED_BY_DOMAIN, domain: status.GetDomain()}, true
		}
	}
	return sighting{}, false
}

func seenByControlPlane(l *live) (sighting, bool) {
	return sighting{address: l.seen, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SEEN_BY_CONTROL_PLANE}, l.seen != ""
}

func fromMetadata(l *live) (sighting, bool) {
	return sighting{address: l.outside.GetMetadataAddress(), source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_CLOUD_METADATA}, l.outside.GetMetadataAddress() != ""
}

func lookedUp(l *live) (sighting, bool) {
	return sighting{address: l.outside.GetLookedUpAddress(), source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_LOOKED_UP}, l.outside.GetLookedUpAddress() != ""
}

// shapeOf says which of the shapes a server comes in this one has, from what it
// has and what answers it. A metadata service answering means a cloud, whatever
// else its interfaces have. Carrier-grade NAT goes by the address its route to the
// internet leaves from, as a VPN such as Tailscale puts 100.64.0.0/10 on an interface
// of a server that's on a network like any other.
func shapeOf(l *live) dockwellv1.NetworkShape {
	addresses := l.inventory.GetAddresses()
	switch {
	case len(addresses) == 0:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED
	case len(resource.PublicAddresses(addresses)) > 0:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_INTERNET
	case l.outside == nil:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED
	case l.outside.GetProvider() != dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT
	case carrierNAT(l.inventory):
		return dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT
	}
	return dockwellv1.NetworkShape_NETWORK_SHAPE_LOCAL_NAT
}

// carrierNAT reports a server whose way to the internet leaves from a shared
// address. An agent that doesn't say where it leaves from counts only if every
// address it has is one.
func carrierNAT(inventory *dockwellv1.ServerInventory) bool {
	if out := inventory.GetOutboundAddress(); out != "" {
		return resource.IsSharedAddress(out)
	}
	return !slices.ContainsFunc(inventory.GetAddresses(), func(s string) bool { return !resource.IsSharedAddress(s) })
}

// served reports a certificate that's there, with nothing failing about it.
func served(c *dockwellv1.Certificate) bool {
	return c.GetState() == dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED &&
		c.GetProblem() == dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_UNSPECIFIED
}

// pointsHere reports DNS that may point at this server, as far as its agent can
// tell. A server behind NAT can't tell, as its public address is on no interface.
func pointsHere(c *dockwellv1.DomainCheck) bool {
	return c.GetState() == dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE ||
		c.GetState() == dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL
}

// preferred is the IPv4 address of a list, as most people's records are, or else
// its first.
func preferred(list []string) (string, bool) {
	if len(list) == 0 {
		return "", false
	}
	for _, s := range list {
		if a, err := netip.ParseAddr(s); err == nil && a.Unmap().Is4() {
			return s, true
		}
	}
	return list[0], true
}

// only is the one address of a record, or of its IPv4 side, as a server with both
// has an A and an AAAA. More than that is several servers, and can't be told apart.
func only(list []string) (string, bool) {
	var four []string
	for _, s := range list {
		if a, err := netip.ParseAddr(s); err == nil && a.Unmap().Is4() {
			four = append(four, s)
		}
	}
	switch {
	case len(four) == 1:
		return four[0], true
	case len(four) == 0 && len(list) == 1:
		return list[0], true
	}
	return "", false
}

// reachable says what's proved of the internet reaching a server at address, and
// by which domain. Nothing short of a certificate authority's try says yes or no
// (ADR 0034). A domain that proves it wins over one that failed, which may have
// other reasons.
func reachable(l *live, address string) (dockwellv1.Reachable, string) {
	failed := ""
	if address != "" {
		for _, status := range l.domains {
			cert, dns := status.GetCertificate(), status.GetDns()
			if !slices.Contains(dns.GetFound(), address) {
				continue
			}
			if served(cert) {
				return dockwellv1.Reachable_REACHABLE_YES, status.GetDomain()
			}
			if failed == "" && cert.GetProblem() == dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CONNECTION {
				failed = status.GetDomain()
			}
		}
	}
	if failed != "" {
		return dockwellv1.Reachable_REACHABLE_NO, failed
	}
	return dockwellv1.Reachable_REACHABLE_NOT_CHECKED, ""
}

// refreshNetworkLocked works out what's known of a server's network, and tells
// its agent if the address it goes by changed.
func (h *Hub) refreshNetworkLocked(server string, l *live) {
	was := l.network
	pick := publicAddress(l)
	next := &dockwellv1.ServerNetwork{PublicAddress: pick.address, Source: pick.source, Domain: pick.domain, Provider: l.outside.GetProvider(), Shape: shapeOf(l)}
	next.Reachable, next.ReachableDomain = reachable(l, pick.address)
	switch {
	case pick.source == dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON:
		next.KnownTime, next.SetBy = l.person.GetKnownTime(), l.person.GetSetBy()
	case pick.address == "":
	case was.GetPublicAddress() == pick.address && was.GetSource() == pick.source:
		next.KnownTime = was.GetKnownTime()
	default:
		next.KnownTime = timestamppb.New(h.now())
	}
	if proto.Equal(was, next) {
		return
	}
	l.network = next
	if !proto.Equal(tellOf(was), tellOf(next)) {
		select {
		case l.told <- struct{}{}:
		default:
		}
	}
	h.notifyLocked(server, false)
}

// wayIn reports a source that says the internet can come in at the address. What
// the control plane sees, and what a STUN server sees, is how a server goes out,
// which may be another address altogether, as behind a NAT gateway, and behind a
// router is the router's. Only something outside connecting proves a way in.
func wayIn(source dockwellv1.PublicAddressSource) bool {
	return source != dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SEEN_BY_CONTROL_PLANE &&
		source != dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_LOOKED_UP
}

// tellOf is what a server's agent should go by. An address on an interface isn't
// its to be told: it has that, with any others, and picks by what a record needs.
// Behind carrier-grade NAT, a way out is no address to point anything at at all.
func tellOf(n *dockwellv1.ServerNetwork) *agentv1.PublicAddress {
	source := n.GetSource()
	if source == dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_ON_INTERFACE {
		return &agentv1.PublicAddress{}
	}
	in := wayIn(source)
	shape := n.GetShape()
	if !in && shape == dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT {
		return &agentv1.PublicAddress{CarrierNat: true}
	}
	address := n.GetPublicAddress()
	return &agentv1.PublicAddress{
		Address: address, WayIn: address != "" && in,
		Unproven: address != "" && !in && shape != dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT,
	}
}

// SeenConnecting records the address a server's agent connected from, as
// "host:port". Only a public one says anything: one from the control plane's own
// network, or its own agent on loopback, is no address the internet knows.
func (h *Hub) SeenConnecting(server, from string) {
	seen := ""
	if at, err := netip.ParseAddrPort(from); err == nil && resource.IsPublicAddress(at.Addr()) {
		seen = at.Addr().Unmap().String()
	}
	h.update(server, func(l *live) { l.seen = seen })
}

// RecordDomains keeps what a server says of its routes' domains, as a certificate
// is how the internet is proved to reach it.
func (h *Hub) RecordDomains(server string, domains []*dockwellv1.DomainStatus) {
	h.update(server, func(l *live) { l.domains = domains })
}

// RecordNetwork keeps what a server found out for itself. An address that isn't
// public is dropped, whatever said it, and so is a provider this build doesn't know.
func (h *Hub) RecordNetwork(server string, n *agentv1.Network) {
	kept := &agentv1.Network{Provider: n.GetProvider()}
	if _, ok := dockwellv1.CloudProvider_name[int32(n.GetProvider())]; !ok {
		kept.Provider = dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED
	}
	if len(resource.PublicAddresses([]string{n.GetMetadataAddress()})) == 1 {
		kept.MetadataAddress = n.GetMetadataAddress()
	}
	if len(resource.PublicAddresses([]string{n.GetLookedUpAddress()})) == 1 {
		kept.LookedUpAddress = n.GetLookedUpAddress()
	}
	h.update(server, func(l *live) { l.outside = kept })
}

func (h *Hub) update(server string, change func(*live)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[server]
	if !ok {
		return
	}
	change(l)
	h.refreshNetworkLocked(server, l)
}

// PublicAddress is what a server's agent should go by.
func (h *Hub) PublicAddress(server string) *agentv1.PublicAddress {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[server]
	if !ok {
		return &agentv1.PublicAddress{}
	}
	return tellOf(l.network)
}

// PublicAddressChanged signals when PublicAddress may say something new.
func (h *Hub) PublicAddressChanged(server string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.servers[server]; ok {
		return l.told
	}
	return nil
}

// Setting is who sets a server's public address, as the audit log has them.
type Setting struct {
	// Actor and Surface go in the audit log, and Who is what the API shows of them.
	Actor, Surface string
	Who            *dockwellv1.Actor
}

// settingPublicAddress is the store's key for what a person set of a server's
// public address.
func settingPublicAddress(server string) string { return "server.public_address." + server }

// SetPublicAddress records a server's public address as a person says it, or
// clears it with an empty one, and puts that in the audit log with it. It returns
// the server as it now reads.
func (h *Hub) SetPublicAddress(ctx context.Context, server, address string, by Setting) (*dockwellv1.Server, error) {
	now := h.now()
	set := &dockwellv1.ServerNetwork{}
	if address != "" {
		a, err := netip.ParseAddr(address)
		if err != nil {
			return nil, fmt.Errorf("%w: %s isn't an IP address. Use the one your provider's console shows for this server", ErrNotAPublicAddress, address)
		}
		if !resource.IsPublicAddress(a) {
			return nil, fmt.Errorf("%w: %s is private, so the internet can't reach it there. Use the public address your provider's console shows for this server", ErrNotAPublicAddress, address)
		}
		set = &dockwellv1.ServerNetwork{PublicAddress: a.Unmap().String(), KnownTime: timestamppb.New(now), SetBy: by.Who}
	}
	raw, err := protojson.Marshal(set)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	_, known := h.servers[server]
	h.mu.Unlock()
	if !known {
		return nil, fmt.Errorf("server %s: %w", server, ErrNotFound)
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.PutSetting(ctx, settingPublicAddress(server), raw); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: by.Actor, Action: ActionPublicAddress, Subject: server, Target: server, Surface: by.Surface,
			Detail: "public address: " + publicWords(set),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("can't record the public address of server %s: %w", server, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[server]
	if !ok {
		return nil, fmt.Errorf("server %s: %w", server, ErrNotFound)
	}
	l.person = set
	h.refreshNetworkLocked(server, l)
	return h.toProtoLocked(server, l, false), nil
}

func publicWords(set *dockwellv1.ServerNetwork) string {
	if set.GetPublicAddress() == "" {
		return "cleared"
	}
	return set.GetPublicAddress()
}

// loadPublicAddress reads what a person set of a server's public address.
func loadPublicAddress(ctx context.Context, tx *store.Tx, server string) (*dockwellv1.ServerNetwork, error) {
	raw, err := tx.GetSetting(ctx, settingPublicAddress(server))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil //nolint:nilnil // nothing set is no error
	}
	if err != nil {
		return nil, err
	}
	set := &dockwellv1.ServerNetwork{}
	if err := protojson.Unmarshal(raw, set); err != nil {
		return nil, fmt.Errorf("can't read the public address set for server %s: %w", server, err)
	}
	return set, nil
}
