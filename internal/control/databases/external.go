// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// firewallRule is the kind of a server's firewall rule, which external access
// keeps two of on each server a database runs on.
const firewallRule = "firewall.rule"

// SetDatabaseExternalAccess plans letting the addresses listed reach a database from
// beyond its server, or taking that away, as three edits that go together: its port on
// every address of its server, a rule that lets the list in there, and one that keeps
// everyone else out (ADR 0038, "External access, as built"). It changes nothing until
// the plan is applied.
func (h *Handler) SetDatabaseExternalAccess(ctx context.Context, req *connect.Request[dockwellv1.SetDatabaseExternalAccessRequest]) (*connect.Response[dockwellv1.SetDatabaseExternalAccessResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	allow, err := allowList(m.GetAllowFrom())
	if err != nil {
		return nil, invalidField("allow_from", err)
	}
	switch {
	case m.GetOff() && len(allow) > 0:
		return nil, invalidField("allow_from", errors.New("the request turns external access off and lists addresses to let in. Leave allow_from out to turn it off"))
	case !m.GetOff() && len(allow) == 0:
		return nil, invalidField("allow_from", errors.New("external access lets in only the addresses listed, and none are. Name one at least, such as 203.0.113.7 or 203.0.113.0/24"))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	db, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if db.spec.GetApp() == nil || len(db.servers) == 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s runs on no server now, so nothing can reach it there. Run it first", db.ref.Name), "not_running")
	}
	spec := proto.CloneOf(db.spec)
	ref := db.ref
	was := db.db.GetExternalAccess()
	edits := []*dockwellv1.Edit{}
	// The service is edited from the revision read, which only it has: the rules are new or go.
	also := changes.Also{}
	if m.GetOff() {
		if was == nil {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s has no external access to take away", ref.Name), "not_open")
		}
		app := spec.GetApp()
		app.Ports = slices.DeleteFunc(app.Ports, func(p *resourcev1.PortMapping) bool {
			return p.GetHostPort() == was.GetPort() && revision.EveryAddress(p.GetHostIp())
		})
		edits = append(edits, serviceEdit(ref, spec, db.revision))
		for i := range db.servers {
			for _, which := range []string{revision.ExternalAllow, revision.ExternalDeny} {
				edits = append(edits, &dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: firewallRule, Scope: db.servers[i].ID, Name: revision.ExternalRule(db.db.GetId(), which)},
					Delete:   true,
				})
			}
		}
		change, err := h.changes.Propose(ctx, fmt.Sprintf("Close %s to everyone beyond its server", ref.Name), edits, nil, also)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&dockwellv1.SetDatabaseExternalAccessResponse{Change: change}), nil
	}

	port := was.GetPort()
	if port == 0 {
		if port, err = h.freePort(ctx, db); err != nil {
			return nil, h.fail(ctx, err)
		}
	}
	app := spec.GetApp()
	if !slices.ContainsFunc(app.GetPorts(), func(p *resourcev1.PortMapping) bool {
		return p.GetHostPort() == port && revision.EveryAddress(p.GetHostIp())
	}) {
		app.Ports = append(app.Ports, &resourcev1.PortMapping{HostPort: port, ContainerPort: db.db.GetPort(), Protocol: resourcev1.Protocol_PROTOCOL_TCP})
		// A port on every address is public on purpose, which a person approves (ADR 0101).
		also.Public = map[resource.Ref][]string{ref: {strconv.FormatUint(uint64(port), 10)}}
	}
	edits = append(edits, serviceEdit(ref, spec, db.revision))
	comment := "External access to " + ref.Name
	for i := range db.servers {
		s := &db.servers[i]
		edits = append(edits,
			ruleEdit(s.ID, revision.ExternalRule(db.db.GetId(), revision.ExternalAllow), &resourcev1.FirewallRule{
				Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Protocol: resourcev1.Protocol_PROTOCOL_TCP, Port: port, Sources: allow, Comment: comment,
			}),
			ruleEdit(s.ID, revision.ExternalRule(db.db.GetId(), revision.ExternalDeny), &resourcev1.FirewallRule{
				Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY, Protocol: resourcev1.Protocol_PROTOCOL_TCP, Port: port, Comment: comment,
			}),
		)
	}
	// What crosses the network to it is unencrypted, which the plan says once, with the
	// safer way beside it.
	also.Notes = []revision.Note{{
		Reason:  "unencrypted",
		Ref:     ref,
		Message: fmt.Sprintf("What reaches %s on port %d crosses the network unencrypted. An SSH tunnel is the safer way", ref.Name, port),
	}}
	summary := fmt.Sprintf("Let %s reach %s on port %d", human.List(allow), ref.Name, port)
	change, err := h.changes.Propose(ctx, summary, edits, nil, also)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetDatabaseExternalAccessResponse{Change: change}), nil
}

func serviceEdit(ref resource.Ref, spec *resourcev1.Service, base int) *dockwellv1.Edit {
	return &dockwellv1.Edit{Resource: resourcepb.Ref(ref), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}}, BaseRevision: int64(base)}
}

func ruleEdit(server, name string, rule *resourcev1.FirewallRule) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: firewallRule, Scope: server, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: rule}},
	}
}

// allowList reads the addresses to let in, each an address or a range, as ranges in
// the form a firewall rule takes, without repeats. Anywhere, as 0.0.0.0/0 or ::/0, is
// refused: external access is never open to everyone.
func allowList(given []string) ([]string, error) {
	var out []string
	for _, g := range given {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		p, err := netip.ParsePrefix(g)
		if err != nil {
			a, aerr := netip.ParseAddr(g)
			if aerr != nil {
				return nil, fmt.Errorf("%s isn't an address or a range. Use one such as 203.0.113.7 or 203.0.113.0/24", g)
			}
			p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		p = p.Masked()
		if p.Bits() == 0 {
			return nil, fmt.Errorf("%s is anywhere, and external access is never open to everyone. Name the addresses that reach it", g)
		}
		if s := p.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// freePort is the port a database's external access takes on its server: its
// engine's own, such as 5432, unless a service already publishes that one, then the
// next free above it.
func (h *Handler) freePort(ctx context.Context, db *placed) (uint32, error) {
	taken := map[uint32]bool{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		services, err := tx.ListKind(ctx, string(service.Kind))
		if err != nil {
			return err
		}
		specs := make([]store.StoredSpec, len(services))
		for i := range services {
			r := &services[i]
			specs[i] = store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec}
		}
		decoded, _, err := h.keeper.DecodeEach(ctx, tx, specs)
		if err != nil {
			return err
		}
		for _, m := range decoded {
			if svc, ok := m.(*resourcev1.Service); ok {
				for _, p := range svc.GetApp().GetPorts() {
					taken[p.GetHostPort()] = true
				}
			}
		}
		return nil
	})
	port := db.db.GetPort()
	for taken[port] && port < 65535 {
		port++
	}
	return port, err
}

// externalAccess says, of each database, where its external access lets whom in, as
// the rule its server keeps for it says (SetDatabaseExternalAccess).
func (h *Handler) externalAccess(ctx context.Context, tx *store.Tx, list []*placed) error {
	rules, err := tx.ListKind(ctx, firewallRule)
	if err != nil {
		return err
	}
	byName := map[string]*store.Resource{}
	for i := range rules {
		byName[rules[i].Scope+"/"+rules[i].Name] = &rules[i]
	}
	for _, p := range list {
		for i := range p.servers {
			r := byName[p.servers[i].ID+"/"+revision.ExternalRule(p.db.GetId(), revision.ExternalAllow)]
			if r == nil {
				continue
			}
			// A firewall rule holds nothing secret, so it's stored as it is.
			spec := &resourcev1.FirewallRule{}
			if err := proto.Unmarshal(r.Spec, spec); err != nil {
				return fmt.Errorf("can't read the firewall rule %s: %w", r.Name, err)
			}
			p.db.ExternalAccess = &dockwellv1.DatabaseExternalAccess{Port: spec.GetPort(), AllowFrom: spec.GetSources()}
			break
		}
	}
	return nil
}
