// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// User may sign in to a private route with a password, which the proxy checks
// against Hash, a bcrypt hash (ADR 0087).
type User struct {
	Name, Hash string
}

// Limits on who a route lets in.
const (
	// MaxAllow is more entries than a list of offices and people needs, past which
	// a list is a range written out.
	MaxAllow = 64
	// minAllowBits4 and minAllowBits6 are the widest a list's range may be. Past
	// them, so much of the internet gets in that it isn't a list.
	minAllowBits4 = 8
	minAllowBits6 = 16
)

// NormalizeAllow checks what a route lets in, and returns it canonical, masked,
// sorted and without repeats, so it compares equal to what the proxy reads back.
// Nothing a token sends becomes config without going through this: what comes out
// is a prefix netip wrote.
func NormalizeAllow(list []string) ([]string, error) {
	if len(list) > MaxAllow {
		return nil, fmt.Errorf("a route lets in %d addresses and ranges, and takes at most %d. Name a range that covers them", len(list), MaxAllow)
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		canon, err := normalizeAllowed(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, canon)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func normalizeAllowed(entry string) (string, error) {
	prefix, err := netip.ParsePrefix(entry)
	if err != nil {
		addr, aerr := netip.ParseAddr(entry)
		if aerr != nil {
			return "", fmt.Errorf("%q isn't an address or a range, such as 203.0.113.7 or 2001:db8::/64. Write one of those", entry)
		}
		if addr.Zone() != "" {
			return "", fmt.Errorf("%s names a network interface. Write the address alone", entry)
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	a := prefix.Addr()
	switch {
	case a.Is4In6() && prefix.Bits()-96 >= minAllowBits4:
		return "", fmt.Errorf("%s spells an IPv4 address in IPv6, and IPv4 visitors are compared as IPv4. Write it as %s", entry, netip.PrefixFrom(a.Unmap(), prefix.Bits()-96).Masked())
	case a.Is4In6():
		return "", fmt.Errorf("%s spells IPv4 addresses in IPv6, and IPv4 visitors are compared as IPv4. Name an IPv4 range of /%d or narrower", entry, minAllowBits4)
	case a.IsUnspecified() && prefix.Bits() == a.BitLen():
		return "", fmt.Errorf("%s is no address at all. Name the address visitors come from", entry)
	case a.IsMulticast():
		return "", fmt.Errorf("%s is a multicast address, which no visitor comes from. Name the address visitors come from", entry)
	case a.Is4() && prefix.Bits() < minAllowBits4, a.Is6() && prefix.Bits() < minAllowBits6:
		return "", fmt.Errorf("%s lets in so much of the internet that it isn't a list. Leave the list out to let in everyone, or name a range of /%d or narrower, /%d in IPv6", entry, minAllowBits4, minAllowBits6)
	case a.Is6() && prefix.Masked().Contains(mappedIPv4):
		return "", fmt.Errorf("%s covers where IPv6 spells IPv4 addresses, which visitors aren't compared as. Name a narrower range", entry)
	}
	return prefix.Masked().String(), nil
}

// Covers reports a range of list that holds every address of entry, both written
// as NormalizeAllow writes them.
func Covers(list []string, entry string) bool {
	p, err := netip.ParsePrefix(entry)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(list, func(s string) bool {
		q, err := netip.ParsePrefix(s)
		return err == nil && q.Bits() <= p.Bits() && q.Contains(p.Addr())
	})
}

// checkAccess refuses who a route lets in where it isn't as the control plane
// writes it, which no proxy should hold: a list that isn't canonical, and a user
// with no hash, which would let in no one or, worse, read as no user at all.
func checkAccess(ref resource.Ref, spec *resourcev1.Route) error {
	allow := spec.GetAccess().GetAllow()
	canon, err := NormalizeAllow(allow)
	if err != nil {
		return fmt.Errorf("the route %s: %w", ref.Name, err)
	}
	if !slices.Equal(canon, allow) {
		return fmt.Errorf("the route %s lets in %v, which isn't written as the control plane writes it. Make the plan again", ref.Name, allow)
	}
	for _, u := range spec.GetAccess().GetUsers() {
		if !IsPasswordHash(u.GetPasswordHash()) {
			return fmt.Errorf("the route %s names the user %q with no bcrypt hash of a password. Make the plan again, with the user's password", ref.Name, u.GetName())
		}
	}
	// The control plane fills in the provider as it plans, so a route without it
	// came from elsewhere, and its server couldn't sign anyone in with it.
	if s := spec.GetAccess().GetSignIn(); s != nil {
		if f := s.GetFilled(); !strings.HasPrefix(f.GetIssuer(), "https://") || f.GetClientId() == "" {
			return fmt.Errorf("the route %s signs people in with %s, but its plan has nothing of that provider for this server. Make the plan again", ref.Name, s.GetProvider())
		}
	}
	return nil
}

// IsPasswordHash reports a bcrypt hash, as $2a$10$ and 53 characters of bcrypt's
// own base64, at a cost from 10 to 12 (ADR 0087): what a route keeps of a password.
func IsPasswordHash(h string) bool {
	if len(h) != 60 || h[0] != '$' || h[1] != '2' || !strings.ContainsRune("aby", rune(h[2])) || h[3] != '$' || h[6] != '$' {
		return false
	}
	if cost := h[4:6]; cost != "10" && cost != "11" && cost != "12" {
		return false
	}
	return strings.Trim(h[7:], "./ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789") == ""
}

// accessOf is who a spec lets in, as a Route has it.
func accessOf(spec *resourcev1.Route) (allow []string, users []User) {
	a := spec.GetAccess()
	allow = slices.Clone(a.GetAllow())
	for _, u := range a.GetUsers() {
		users = append(users, User{Name: u.GetName(), Hash: u.GetPasswordHash()})
	}
	return allow, users
}

// accessSpec is accessOf the other way, or nil for a route that lets in anyone.
func accessSpec(r Route) *resourcev1.RouteAccess {
	if len(r.Allow)+len(r.Users) == 0 {
		return nil
	}
	out := &resourcev1.RouteAccess{Allow: slices.Clone(r.Allow)}
	for _, u := range r.Users {
		out.Users = append(out.Users, &resourcev1.RouteUser{Name: u.Name, PasswordHash: u.Hash})
	}
	return out
}

// SignIns keeps what the routes that sign people in need on this server, by their
// proxy IDs, for the agent that answers the proxy when it asks (ADR 0087).
type SignIns interface {
	// Put keeps a route's sign-in: who may, and the provider the control plane
	// filled in, at its domain and path.
	Put(id, domain, path string, s *resourcev1.RouteSignIn) error
	// Drop forgets a route's, if it has one.
	Drop(id string)
	// Get returns what it keeps of a route, as Put was given it, or nil.
	Get(id string) *resourcev1.RouteSignIn
}

// SignInAsker is a proxy that can ask an agent whether a visitor has signed in.
type SignInAsker interface {
	// SignInDial is the address the proxy dials to ask.
	SignInDial() string
}

// signInDial is where the proxy asks, or why a route that signs people in can't run.
func (m *Manager) signInDial() (string, error) {
	asker, ok := m.Proxy.(SignInAsker)
	if !ok || m.SignIns == nil {
		return "", fmt.Errorf("this server's %s can't ask the agent who signed in. Update the agent", m.Proxy.Name())
	}
	return asker.SignInDial(), nil
}

// rewriteBeside writes again the other routes of a host where a route signs people
// in, so each takes Dockwell's sign-in cookies out on the way to its app, as one
// written before every route did may not: the cookies go to every path of their host
// (#598's review, M4). One edited by hand stays as it is.
func (m *Manager) rewriteBeside(ctx context.Context, r Route) error {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	var beside []Route
	for i := range routes {
		if o := &routes[i]; o.Domain == r.Domain && o.ID != r.ID && !o.Edited {
			beside = append(beside, *o)
		}
	}
	if len(beside) == 0 {
		return nil
	}
	if err := m.Proxy.PutRoutes(ctx, beside); err != nil {
		return fmt.Errorf("%s can't write the other routes of %s again, to keep its sign-in cookies from their apps: %w", m.Proxy.Name(), r.Domain, err)
	}
	return nil
}

// HandEditedBeside names, by domain and path, the routes on the host of a route that
// signs people in that read as edited by hand, which applying it doesn't write
// again, so a plan says they may pass its cookies on (#598's review, New 4).
func (m *Manager) HandEditedBeside(ctx context.Context, ref resource.Ref, spec *resourcev1.Route) []string {
	if spec.GetAccess().GetSignIn() == nil {
		return nil
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for i := range routes {
		if o := &routes[i]; o.Domain == spec.GetDomain() && o.ID != RouteID(ref) && o.Edited {
			out = append(out, o.Domain+o.Path)
		}
	}
	return out
}
