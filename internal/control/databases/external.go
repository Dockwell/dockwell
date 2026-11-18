// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

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
// beyond its server, or taking that away. On, its port is tagged external
// (PortMapping.external), and each server it runs on gets two rules: one lets the
// list in, one keeps everyone else out (ADR 0038, "External access, as built"). Off
// takes the tag's port away and every rule it has, from whatever state it's in. It
// changes nothing until the plan is applied.
func (h *Handler) SetDatabaseExternalAccess(ctx context.Context, req *connect.Request[dockwellv1.SetDatabaseExternalAccessRequest]) (*connect.Response[dockwellv1.SetDatabaseExternalAccessResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	var allow []string
	switch {
	case m.GetOff() && len(m.GetAllowFrom()) > 0:
		return nil, invalidField("allow_from", errors.New("the request turns external access off and lists addresses to let in. Leave allow_from out to turn it off"))
	case !m.GetOff():
		var err error
		if allow, err = revision.AllowFrom(m.GetAllowFrom()); err != nil {
			return nil, invalidField("allow_from", err)
		}
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	db, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if db.spec.GetApp() == nil {
		return nil, authapi.Invalid(fmt.Errorf("%s doesn't run an image, so it has no port to open", db.ref.Name))
	}
	spec := proto.CloneOf(db.spec)
	ref, id := db.ref, db.db.GetId()
	app := spec.GetApp()
	deleteRule := func(r *store.Resource) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: firewallRule, Scope: r.Scope, Name: r.Name}, Delete: true}
	}
	if m.GetOff() {
		// From any state: the tagged port goes, and so does a port on its rules' port that
		// answers beyond the server, as one from before the tag; and every rule it has.
		guarded := map[uint32]bool{}
		for i := range db.rules {
			guarded[ruleSpec(&db.rules[i]).GetPort()] = true
		}
		before := len(app.Ports)
		app.Ports = slices.DeleteFunc(app.Ports, func(p *resourcev1.PortMapping) bool {
			return p.GetExternal() || guarded[p.GetHostPort()] && revision.Public(p.GetHostIp())
		})
		if before == len(app.Ports) && len(db.rules) == 0 {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s has no external access to take away", ref.Name), "not_open")
		}
		edits := []*dockwellv1.Edit{serviceEdit(ref, spec, db.revision)}
		for i := range db.rules {
			edits = append(edits, deleteRule(&db.rules[i]))
		}
		change, err := h.changes.Propose(ctx, fmt.Sprintf("Close %s to everyone beyond its server", ref.Name), edits, nil, changes.Also{})
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&dockwellv1.SetDatabaseExternalAccessResponse{Change: change}), nil
	}
	if len(db.servers) == 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s runs on no server now, so nothing can reach it there. Run it first", ref.Name), "not_running")
	}

	// The port: the one it's tagged on; or one it publishes beyond its server already,
	// on its engine's port, which is then held to the list rather than left open beside
	// a new one; or its engine's port, or the next no other service publishes.
	also := changes.Also{}
	var port uint32
	if i := slices.IndexFunc(app.GetPorts(), func(p *resourcev1.PortMapping) bool { return p.GetExternal() }); i >= 0 {
		port = app.Ports[i].GetHostPort()
		app.Ports[i].HostIp, app.Ports[i].Held = "", false
	} else if i := slices.IndexFunc(app.GetPorts(), func(p *resourcev1.PortMapping) bool {
		return p.GetContainerPort() == db.db.GetPort() && revision.Public(p.GetHostIp()) && !p.GetHeld()
	}); i >= 0 {
		app.Ports[i].External, app.Ports[i].HostIp = true, ""
		port = app.Ports[i].GetHostPort()
	} else {
		if port, err = h.freePort(ctx, db); err != nil {
			return nil, h.fail(ctx, err)
		}
		app.Ports = append(app.Ports, &resourcev1.PortMapping{HostPort: port, ContainerPort: db.db.GetPort(), Protocol: resourcev1.Protocol_PROTOCOL_TCP, External: true})
		// A port beyond its server is public on purpose, which a person approves (ADR 0101).
		also.Public = map[resource.Ref][]string{ref: {strconv.FormatUint(uint64(port), 10)}}
	}
	edits := []*dockwellv1.Edit{serviceEdit(ref, spec, db.revision)}
	comment := "External access to " + ref.Name
	on := map[string]bool{}
	for i := range db.servers {
		s := &db.servers[i]
		on[s.ID] = true
		edits = append(edits,
			ruleEdit(s.ID, revision.ExternalRule(id, revision.ExternalAllow), &resourcev1.FirewallRule{
				Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Protocol: resourcev1.Protocol_PROTOCOL_TCP, Port: port, Sources: allow, Comment: comment + ", allow",
			}),
			ruleEdit(s.ID, revision.ExternalRule(id, revision.ExternalDeny), &resourcev1.FirewallRule{
				Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY, Protocol: resourcev1.Protocol_PROTOCOL_TCP, Port: port, Comment: comment + ", deny",
			}),
		)
	}
	// Its rules on a server it no longer runs on go.
	for i := range db.rules {
		if !on[db.rules[i].Scope] {
			edits = append(edits, deleteRule(&db.rules[i]))
		}
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

// ruleSpec reads a stored firewall rule, which holds nothing secret, so it's stored as
// it is. One that can't be read is an empty rule.
func ruleSpec(r *store.Resource) *resourcev1.FirewallRule {
	spec := &resourcev1.FirewallRule{}
	_ = proto.Unmarshal(r.Spec, spec)
	return spec
}

// freePort is the port a database's external access takes on its server: its
// engine's own, such as 5432, unless another service already publishes that one,
// then the next free above it. The database's own ports don't count, as one it
// publishes beyond its server is reused, and one on loopback alone may share it.
func (h *Handler) freePort(ctx context.Context, db *placed) (uint32, error) {
	taken := map[uint32]bool{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		services, err := tx.ListKind(ctx, string(service.Kind))
		if err != nil {
			return err
		}
		specs := make([]store.StoredSpec, 0, len(services))
		for i := range services {
			r := &services[i]
			if r.ID == db.db.GetId() {
				continue
			}
			specs = append(specs, store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec})
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
	for _, p := range db.spec.GetApp().GetPorts() {
		if !revision.Public(p.GetHostIp()) {
			taken[p.GetHostPort()] = true
		}
	}
	port := db.db.GetPort()
	for taken[port] && port < 65535 {
		port++
	}
	return port, err
}

// externalAccess says, of each database, whether it has external access, and to whom:
// from the port it tags as external, never from a rule alone, so the page can't say
// it's off while the port answers. A port from before the tag, beyond its server on
// the port its rules guard, reads as on too, so it can be turned off. The list is its
// allow rule's on the first of its servers that has one.
func (h *Handler) externalAccess(ctx context.Context, tx *store.Tx, list []*placed) error {
	rules, err := tx.ListKind(ctx, firewallRule)
	if err != nil {
		return err
	}
	byService := map[string][]store.Resource{}
	for i := range rules {
		for _, which := range []string{revision.ExternalAllow, revision.ExternalDeny} {
			if svc, ok := revision.ExternalOf(rules[i].Name, which); ok {
				byService[svc] = append(byService[svc], rules[i])
			}
		}
	}
	for _, p := range list {
		p.rules = byService[p.db.GetId()]
		guarded := map[uint32]bool{}
		var allowFrom []string
		for i := range p.rules {
			r := ruleSpec(&p.rules[i])
			guarded[r.GetPort()] = true
			if allowFrom == nil && r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW && slices.ContainsFunc(p.servers, func(s store.Server) bool { return s.ID == p.rules[i].Scope }) {
				allowFrom = r.GetSources()
			}
		}
		for _, mp := range p.spec.GetApp().GetPorts() {
			if mp.GetExternal() || guarded[mp.GetHostPort()] && revision.Public(mp.GetHostIp()) {
				p.db.ExternalAccess = &dockwellv1.DatabaseExternalAccess{Port: mp.GetHostPort(), AllowFrom: allowFrom}
				break
			}
		}
	}
	return nil
}
