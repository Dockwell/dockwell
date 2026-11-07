// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"

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

// The audit log's actions for a person changing a server's public address.
const (
	ActionPublicAddress      = "server.public_address"
	ActionPublicAddressClear = "server.public_address_clear"
)

// proofFresh is how old a read of a domain's DNS can be and still prove anything.
// An agent reads it again every ingress.IssuedDNSEvery, so this is two of those
// missed, as an agent that's still connected and has stopped reading.
const proofFresh = 2*ingress.IssuedDNSEvery + time.Hour

// ErrNotAPublicAddress reports an address that can't be a server's public one.
var ErrNotAPublicAddress = errors.New("not a public address")

// sighting is what one source knows of a server's public address.
type sighting struct {
	address string
	source  dockwellv1.PublicAddressSource
	// domain is the one that proved it, for a domain's.
	domain string
	// at is when that was so, where a source can say.
	at time.Time
	// unproven says nothing shows it's a way in (ADR 0034).
	unproven bool
}

// looking is what a source goes by: one server, one family, and the time.
type looking struct {
	l    *live
	four bool
	now  time.Time
}

// publicSources are the ways of knowing a server's public address, in the order
// they win, and they're asked for each family apart. A new one goes here, and
// nowhere else: each only looks at what the hub has heard, and says what that makes
// of the address, or nothing (ADR 0034).
var publicSources = []func(looking) (sighting, bool){
	setByPerson,
	onInterface,
	provedByDomain,
	confirmedMetadata,
	seenByControlPlane,
	unconfirmedMetadata,
	lookedUp,
}

// publicAddress is what the first source that knows says.
func publicAddress(v looking) sighting {
	for _, source := range publicSources {
		if seen, ok := source(v); ok {
			return seen
		}
	}
	return sighting{}
}

// isFour reports an IPv4 address.
func isFour(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Unmap().Is4()
}

// person is what a person set of one family.
func (v looking) person() *dockwellv1.PublicAddress {
	if v.four {
		return v.l.person.GetIpv4()
	}
	return v.l.person.GetIpv6()
}

func setByPerson(v looking) (sighting, bool) {
	address := v.person().GetAddress()
	return sighting{address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON}, address != ""
}

func onInterface(v looking) (sighting, bool) {
	for _, s := range resource.PublicAddresses(v.l.inventory.GetAddresses()) {
		if isFour(s) == v.four {
			return sighting{address: s, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_ON_INTERFACE}, true
		}
	}
	return sighting{}, false
}

// provedByDomain finds a certificate that showed a server answering at an address,
// as far as the domain's record still reads as it did then, and the read is fresh. A
// record of both families says nothing of either: a certificate authority falls back
// from one to the other, so what came may have come over the other, whatever else is
// known of it.
func provedByDomain(v looking) (sighting, bool) {
	for _, status := range v.l.domains {
		dns := status.GetDns()
		if v.l.proofOf(status) == nil || !fresh(dns, v.now) || !oneFamily(dns) {
			continue
		}
		address, ok := recordAddress(dns, v.four)
		if !ok {
			continue
		}
		return sighting{
			address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_PROVED_BY_DOMAIN,
			domain: status.GetDomain(), at: dns.GetReadTime().AsTime(),
		}, true
	}
	return sighting{}, false
}

// oneFamily reports a record of IPv4 alone or IPv6 alone.
func oneFamily(dns *dockwellv1.DomainCheck) bool {
	found := dns.GetFound()
	return !slices.ContainsFunc(found, isFour) || !slices.ContainsFunc(found, func(s string) bool { return !isFour(s) })
}

// fresh reports DNS read recently enough to go by.
func fresh(dns *dockwellv1.DomainCheck, now time.Time) bool {
	return dns.GetReadTime() != nil && now.Sub(dns.GetReadTime().AsTime()) <= proofFresh
}

// recordAddress is the one address of a family a domain's record names. One that
// points at a shared range such as Cloudflare's, or at more than one address, says
// nothing of which is this server's.
func recordAddress(dns *dockwellv1.DomainCheck, four bool) (string, bool) {
	found := resource.PublicAddresses(dns.GetFound())
	if len(found) != len(dns.GetFound()) {
		return "", false
	}
	var family []string
	for _, s := range found {
		if a, err := netip.ParseAddr(s); err != nil || ingress.IsCloudflare(a) {
			return "", false
		}
		if isFour(s) == four {
			family = append(family, s)
		}
	}
	if len(family) != 1 {
		return "", false
	}
	return family[0], true
}

// confirmed reports metadata from the provider the machine itself says it runs on.
// Anything on a local network can answer at the metadata address.
func confirmed(l *live) bool {
	return l.outside.GetProvider() != dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED &&
		l.outside.GetProvider() == l.outside.GetMachineProvider()
}

func fromMetadata(v looking, trusted bool) (sighting, bool) {
	address := v.l.outside.GetMetadataAddress()
	if address == "" || isFour(address) != v.four || confirmed(v.l) != trusted {
		return sighting{}, false
	}
	return sighting{address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_CLOUD_METADATA, unproven: !trusted}, true
}

func confirmedMetadata(v looking) (sighting, bool)   { return fromMetadata(v, true) }
func unconfirmedMetadata(v looking) (sighting, bool) { return fromMetadata(v, false) }

func seenByControlPlane(v looking) (sighting, bool) {
	address := v.l.seen
	return sighting{address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SEEN_BY_CONTROL_PLANE, unproven: true},
		address != "" && isFour(address) == v.four
}

func lookedUp(v looking) (sighting, bool) {
	address := v.l.outside.GetLookedUpAddress()
	return sighting{address: address, source: dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_LOOKED_UP, unproven: true},
		address != "" && isFour(address) == v.four
}

// shapeOf says which of the shapes a server comes in this one has, from what it
// has and what the machine says of itself. It goes by IPv4, as a public IPv6 address
// on an interface says nothing of how the rest of the internet reaches it. A cloud is
// whatever the machine names, whatever else its interfaces have. Carrier-grade NAT goes
// by the address its route to the internet leaves from, as a VPN such as Tailscale puts
// 100.64.0.0/10 on an interface of a server that's on a network like any other.
func shapeOf(l *live) dockwellv1.NetworkShape {
	addresses := l.inventory.GetAddresses()
	four := slices.DeleteFunc(slices.Clone(addresses), func(s string) bool { return !isFour(s) })
	switch {
	case len(four) == 0:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED
	case len(resource.PublicAddresses(four)) > 0:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_INTERNET
	case l.outside == nil:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED
	case l.outside.GetMachineProvider() != dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED:
		return dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT
	case carrierNAT(l.inventory.GetOutboundAddress(), four):
		return dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT
	}
	return dockwellv1.NetworkShape_NETWORK_SHAPE_LOCAL_NAT
}

// carrierNAT reports a server whose way to the internet leaves from a shared
// address. An agent that doesn't say where it leaves from counts only if every IPv4
// address it has is one, as IPv6 says nothing of how IPv4 gets out.
func carrierNAT(outbound string, four []string) bool {
	if outbound != "" {
		return resource.IsSharedAddress(outbound)
	}
	return !slices.ContainsFunc(four, func(s string) bool { return !resource.IsSharedAddress(s) })
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

// reachable says what's proved of the internet reaching a server at its addresses,
// by which domain, and as of when. Nothing short of a certificate authority's try says
// yes or no (ADR 0034), and a domain counts only as a proof does: its one record, read
// lately. A domain that proves it wins over one that failed, which may have other reasons.
func reachable(v looking, addresses ...string) (dockwellv1.Reachable, string, *timestamppb.Timestamp) {
	var failed *dockwellv1.DomainStatus
	for _, status := range v.l.domains {
		cert, dns := status.GetCertificate(), status.GetDns()
		if !fresh(dns, v.now) || !recordIsOneOf(dns, addresses) {
			continue
		}
		if v.l.proofOf(status) != nil {
			return dockwellv1.Reachable_REACHABLE_YES, status.GetDomain(), dns.GetReadTime()
		}
		if failed == nil && cert.GetProblem() == dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CONNECTION {
			failed = status
		}
	}
	if failed != nil {
		return dockwellv1.Reachable_REACHABLE_NO, failed.GetDomain(), failed.GetDns().GetReadTime()
	}
	return dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "", nil
}

// recordIsOneOf reports a domain whose one record of a family is one of addresses.
func recordIsOneOf(dns *dockwellv1.DomainCheck, addresses []string) bool {
	for _, four := range []bool{true, false} {
		if address, ok := recordAddress(dns, four); ok && slices.Contains(addresses, address) {
			return true
		}
	}
	return false
}

// picked is what's known of one family, as the API has it.
func picked(v looking, was *dockwellv1.PublicAddress) *dockwellv1.PublicAddress {
	pick := publicAddress(v)
	if pick.address == "" {
		return nil
	}
	out := &dockwellv1.PublicAddress{Address: pick.address, Source: pick.source, Domain: pick.domain, Unproven: pick.unproven}
	switch {
	case pick.source == dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON:
		out.KnownTime, out.SetBy = v.person().GetKnownTime(), v.person().GetSetBy()
	case !pick.at.IsZero():
		out.KnownTime = timestamppb.New(pick.at)
	case was.GetAddress() == pick.address && was.GetSource() == pick.source:
		out.KnownTime = was.GetKnownTime()
	default:
		out.KnownTime = timestamppb.New(v.now)
	}
	return out
}

// refreshNetworkLocked works out what's known of a server's network, and tells
// its agent if what it goes by changed.
func (h *Hub) refreshNetworkLocked(server string, l *live) {
	was := l.network
	now := h.now()
	v := looking{l: l, four: true, now: now}
	v6 := v
	v6.four = false
	next := &dockwellv1.ServerNetwork{
		Ipv4: picked(v, was.GetIpv4()), Ipv6: picked(v6, was.GetIpv6()),
		Provider: l.outside.GetMachineProvider(), Shape: shapeOf(l),
	}
	next.Reachable, next.ReachableDomain, next.ReachableTime = reachable(v, next.GetIpv4().GetAddress(), next.GetIpv6().GetAddress())
	l.refreshed = now
	if server == h.control {
		// What's known of it can change with no change of address, as when its agent
		// first says it has none, and whoever follows it compares for itself.
		h.controlMovedLocked()
	}
	if proto.Equal(was, next) {
		return
	}
	l.network = next
	if !proto.Equal(tellOf(was), tellOf(next)) {
		for told := range l.told {
			select {
			case told <- struct{}{}:
			default:
			}
		}
	}
	h.notifyLocked(server, false)
}

func (h *Hub) controlMovedLocked() {
	select {
	case h.controlMoved <- struct{}{}:
	default:
	}
}

// ControlAddress is one address other networks may reach the control server at.
type ControlAddress struct {
	Address string
	// WayIn says something showed the internet comes in there. Without it, it's how
	// the control server goes out, which may well work too.
	WayIn bool
}

// ControlAddresses are the addresses other networks may reach the control server at,
// in the order a join ticket tries them: ways in of either family, then ways out, which
// aren't offered behind carrier-grade NAT. Known says whether anything's been heard of
// its network, so an empty list isn't taken for one that's gone.
func (h *Hub) ControlAddresses() (addresses []ControlAddress, known bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[h.control]
	if !ok {
		return nil, false
	}
	return controlAddresses(l.network), l.outside != nil || l.person.GetIpv4() != nil || l.person.GetIpv6() != nil
}

func controlAddresses(n *dockwellv1.ServerNetwork) []ControlAddress {
	known := []*dockwellv1.PublicAddress{n.GetIpv4(), n.GetIpv6()}
	var out []ControlAddress
	for _, a := range known {
		if a != nil && !a.GetUnproven() {
			out = append(out, ControlAddress{Address: a.GetAddress(), WayIn: true})
		}
	}
	if n.GetShape() == dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT {
		return out
	}
	for _, a := range known {
		if a != nil && a.GetUnproven() {
			out = append(out, ControlAddress{Address: a.GetAddress()})
		}
	}
	return out
}

// ControlAddressMoved signals when ControlAddresses may say something new.
func (h *Hub) ControlAddressMoved() <-chan struct{} { return h.controlMoved }

// tellOf is what a server's agent should go by. An address on an interface isn't
// its to be told: it has that, with any others, and picks by what a record needs.
// Behind carrier-grade NAT, a way out is no address to point anything at at all.
func tellOf(n *dockwellv1.ServerNetwork) *agentv1.PublicAddress {
	out := &agentv1.PublicAddress{Provider: n.GetProvider()}
	wayIn := false
	carrier := n.GetShape() == dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT
	for _, a := range []*dockwellv1.PublicAddress{n.GetIpv4(), n.GetIpv6()} {
		wayIn = wayIn || (a != nil && !a.GetUnproven())
		if a == nil || a.GetSource() == dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_ON_INTERFACE || (carrier && a.GetUnproven()) {
			continue
		}
		out.Addresses = append(out.Addresses, &agentv1.KnownAddress{Address: a.GetAddress(), WayIn: !a.GetUnproven()})
	}
	out.CarrierNat = carrier && !wayIn
	return out
}

// SeenConnecting records the address a server's agent connected from, as
// "host:port". Only a public one says anything: one from the control plane's own
// network, or its own agent on loopback, is no address the internet knows.
func (h *Hub) SeenConnecting(server, from string) {
	seen := ""
	if at, err := netip.ParseAddrPort(from); err == nil && resource.IsPublicAddress(at.Addr()) {
		seen = at.Addr().Unmap().String()
	}
	h.update(server, func(l *live) { l.seen, l.seenAt = seen, h.now() })
}

// RecordDomains keeps what a server says of its routes' domains, as a certificate
// is how the internet is proved to reach it.
func (h *Hub) RecordDomains(server string, domains []*dockwellv1.DomainStatus) {
	h.update(server, func(l *live) { l.domains = domains })
}

// knownProvider is a provider this build knows, or none.
func knownProvider(p dockwellv1.CloudProvider) dockwellv1.CloudProvider {
	if _, ok := dockwellv1.CloudProvider_name[int32(p)]; ok {
		return p
	}
	return dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED
}

// RecordNetwork keeps what a server found out for itself. An address that isn't
// public is dropped, whatever said it, and so is a provider this build doesn't know.
func (h *Hub) RecordNetwork(server string, n *agentv1.Network) {
	kept := &agentv1.Network{
		Provider: knownProvider(n.GetProvider()), MachineProvider: knownProvider(n.GetMachineProvider()), LookedUpTime: n.GetLookedUpTime(),
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
	l, ok := h.servers[server]
	if !ok {
		h.mu.Unlock()
		return
	}
	change(l)
	weighed := l.weighProofs(h.now())
	h.refreshNetworkLocked(server, l)
	h.mu.Unlock()
	if weighed {
		h.saveProofs(server)
	}
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

// WatchPublicAddress signals when PublicAddress may say something new, until the
// returned function is called. Each watcher has its own signal, as an agent moving to
// a new stream has two at once, and one taking the signal mustn't leave the other short.
func (h *Hub) WatchPublicAddress(server string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	told := make(chan struct{}, 1)
	l, ok := h.servers[server]
	if !ok {
		return told, func() {}
	}
	l.told[told] = struct{}{}
	return told, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(l.told, told)
	}
}

// Setting is who sets a server's public address, as the audit log has them.
type Setting struct {
	// Actor and Surface go in the audit log, and Who is what the API shows of them.
	Actor, Surface string
	Who            *dockwellv1.Actor
}

// AddressChange is what the audit log keeps of a person changing a server's public
// address. The previous one is what lets them put it back.
type AddressChange struct {
	Address  string `json:"address,omitempty"`
	Previous string `json:"previous,omitempty"`
}

// checkAddress is address as a server's public one, or why it can't be.
func checkAddress(address string) (netip.Addr, error) {
	a, err := netip.ParseAddr(address)
	if err != nil {
		return a, fmt.Errorf("%w: %s isn't an IP address. Use the one your provider's console shows for this server", ErrNotAPublicAddress, address)
	}
	if !resource.IsPublicAddress(a) {
		return a, fmt.Errorf("%w: %s is private, so the internet can't reach it there. Use the public address your provider's console shows for this server", ErrNotAPublicAddress, address)
	}
	return a.Unmap(), nil
}

// SetPublicAddress records a server's public address as a person says it, for the
// family it's in, or with an empty one clears what they set of IPv4, or of IPv6 with
// ipv6. It goes in the audit log, with what it replaced, and it returns the server as
// it now reads.
func (h *Hub) SetPublicAddress(ctx context.Context, server, address string, ipv6 bool, by Setting) (*dockwellv1.Server, error) {
	now := h.now()
	change := AddressChange{Address: address}
	var entry *dockwellv1.PublicAddress
	if address != "" {
		a, err := checkAddress(address)
		if err != nil {
			return nil, err
		}
		ipv6 = !a.Is4()
		change.Address = a.String()
		entry = &dockwellv1.PublicAddress{Address: a.String(), KnownTime: timestamppb.New(now), SetBy: by.Who}
	}
	h.mu.Lock()
	_, known := h.servers[server]
	h.mu.Unlock()
	if !known {
		return nil, fmt.Errorf("server %s: %w", server, ErrNotFound)
	}
	action := ActionPublicAddress
	if address == "" {
		action = ActionPublicAddressClear
	}
	set := &dockwellv1.ServerNetwork{}
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		was, err := loadPublicAddress(ctx, tx, h.logger, server)
		if err != nil {
			return err
		}
		set = &dockwellv1.ServerNetwork{Ipv4: was.GetIpv4(), Ipv6: was.GetIpv6()}
		if ipv6 {
			change.Previous, set.Ipv6 = was.GetIpv6().GetAddress(), entry
		} else {
			change.Previous, set.Ipv4 = was.GetIpv4().GetAddress(), entry
		}
		raw, err := protojson.Marshal(set)
		if err != nil {
			return err
		}
		if err := tx.PutSetting(ctx, store.SettingPublicAddress(server), raw); err != nil {
			return err
		}
		detail, err := json.Marshal(change)
		if err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: by.Actor, Action: action, Subject: server, Target: server, Surface: by.Surface, Detail: string(detail),
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

// loadPublicAddress reads what a person set of a server's public address. A row
// that can't be read says nothing, and is logged: one bad row can't keep the control
// plane from starting, and a person can set the address again.
func loadPublicAddress(ctx context.Context, tx *store.Tx, logger *slog.Logger, server string) (*dockwellv1.ServerNetwork, error) {
	raw, err := tx.GetSetting(ctx, store.SettingPublicAddress(server))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil //nolint:nilnil // nothing set is no error
	}
	if err != nil {
		return nil, err
	}
	set := &dockwellv1.ServerNetwork{}
	if err := protojson.Unmarshal(raw, set); err != nil {
		logger.WarnContext(ctx, "can't read the public address set for a server, so it's left unset. Set it again on the server's page", slog.String("server", server), slog.String("error", err.Error()))
		return nil, nil //nolint:nilnil // a bad row is logged, and reads as none
	}
	return set, nil
}
