// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// A routeAt is a route in an environment, by its name, and its ID: "" for one a
// change makes, which comes after every route there is.
type routeAt struct {
	id, name string
	spec     *resourcev1.Route
}

// addressOf is where a browser opens web (ADR 0103): the domain of the oldest route
// that sends its whole path to web, or else the panel's Open address of web, by its
// ID, or else the port web publishes on the server, which a tunnel reaches. The zero
// Address where it has none of them.
func (h *Handler) addressOf(routes []routeAt, web, webID string, spec *resourcev1.Service) catalog.Address {
	var oldest *routeAt
	for i := range routes {
		r := &routes[i]
		if r.spec.GetService() != web || r.spec.GetPath() != "" || r.spec.GetRedirect() != nil || r.spec.GetDomain() == "" {
			continue
		}
		if oldest == nil || r.id != "" && (oldest.id == "" || r.id < oldest.id) {
			oldest = r
		}
	}
	switch {
	case oldest != nil:
		return catalog.DomainAddress(oldest.spec.GetDomain())
	case h.Open != nil && webID != "":
		if a, ok := catalog.ParseAddress(h.Open(webID)); ok {
			return a
		}
	}
	for _, p := range spec.GetApp().GetPorts() {
		if p.GetHostPort() != 0 && p.GetProtocol() != resourcev1.Protocol_PROTOCOL_UDP {
			return catalog.LocalAddress("localhost:" + strconv.FormatUint(uint64(p.GetHostPort()), 10))
		}
	}
	return catalog.Address{}
}

// installAddress is where a browser opens an install's web as it's planned, by the
// ID it takes: the panel's Open address, or else its port, as no domain routes to
// it yet.
func (h *Handler) installAddress(entry *catalogv1.Entry, variables map[string]string, webID string) catalog.Address {
	if h.Open != nil {
		if a, ok := catalog.ParseAddress(h.Open(webID)); ok {
			return a
		}
	}
	if port := catalog.WebPort(entry, variables); port != "" {
		return catalog.LocalAddress("localhost:" + port)
	}
	return catalog.Address{}
}

// routesIn are an environment's routes.
func (h *Handler) routesIn(ctx context.Context, tx *store.Tx, env string) ([]routeAt, error) {
	list, err := tx.ListResources(ctx, env, string(route.Kind))
	if err != nil {
		return nil, err
	}
	out := make([]routeAt, 0, len(list))
	for _, r := range list {
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		if rt, ok := spec.(*resourcev1.Route); ok {
			out = append(out, routeAt{id: r.ID, name: r.Name, spec: rt})
		}
	}
	return out, nil
}

// afterEdits are an environment's routes as edits leave them.
func afterEdits(routes []routeAt, env string, edits []revision.Edit) []routeAt {
	out := slices.Clone(routes)
	for i := range edits {
		e := &edits[i]
		if e.Ref.Kind != route.Kind || e.Ref.Scope != env {
			continue
		}
		at := slices.IndexFunc(out, func(r routeAt) bool { return r.name == e.Ref.Name })
		spec, _ := e.Spec.(*resourcev1.Route)
		switch {
		case e.Spec == nil && !e.Restart && e.Restore == nil && at >= 0:
			out = slices.Delete(out, at, at+1)
		case spec == nil:
		case at >= 0:
			out[at].spec = spec
		default:
			out = append(out, routeAt{name: e.Ref.Name, spec: spec})
		}
	}
	return out
}

// fileEnv is each service's environment as the install's file gives it, with the
// values it was installed with: what's Dockwell's of a variable that follows the
// address on an install from before its pin kept the address.
func fileEnv(ctx context.Context, pin *resourcev1.CatalogPin) (map[string]map[string]string, error) {
	if len(pin.GetFile()) == 0 {
		return nil, nil
	}
	variables, _, err := catalog.Given(pin)
	if err != nil {
		return nil, err
	}
	stack, err := compose.Read(ctx, compose.Input{File: pin.GetFile(), Variables: variables})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, s := range stack.Services {
		out[s.Name] = s.Spec.GetApp().GetEnv()
	}
	return out, nil
}

// FollowAddress has edits that move a catalog app's address, by routing its web to a
// domain or no longer, write each variable that follows it, in the same change, so
// undoing the change takes both back (ADR 0103). A variable that holds a value a
// person set stays, as one is that holds none of what the address before the change
// gave it, what the address the install's pin records gave it, and what the
// install's file gives it, and the plan says so.
func (h *Handler) FollowAddress(ctx context.Context, edits []revision.Edit) ([]revision.Edit, []revision.Note, error) {
	// An environment whose routes the edits change, or where an edit of a service
	// asks for a value as an entry writes it, {origin} and all, as an older panel's
	// Set to did, which is never written as it stands (#779's check, N5).
	envs := map[string]bool{}
	for i := range edits {
		if edits[i].Ref.Kind == route.Kind || asWritten(edits[i]) {
			envs[edits[i].Ref.Scope] = true
		}
	}
	if len(envs) == 0 {
		return edits, nil, nil
	}
	s, err := h.catalog(ctx)
	if err != nil {
		// With no catalog to read, no entry says what follows the address.
		return edits, nil, nil //nolint:nilerr // see above
	}
	var notes []revision.Note
	err = h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, env := range p.Environments {
				if !envs[env.ID] {
					continue
				}
				pin, err := h.pinOf(ctx, tx, tree, p)
				if err != nil {
					return err
				}
				entry, ok := s.entryFor(pin)
				if pin == nil || !ok || len(entry.GetAddressVariables()) == 0 || entry.GetWeb().GetService() == "" {
					continue
				}
				moved, said, err := h.follow(ctx, tx, p.Name, env.ID, pin, entry, edits)
				if err != nil {
					return err
				}
				edits, notes = moved, append(notes, said...)
			}
		}
		return nil
	})
	return edits, notes, err
}

// asWritten reports an edit of a service that holds a value that names a part of
// the address, as an entry writes it.
func asWritten(e revision.Edit) bool {
	spec, ok := e.Spec.(*resourcev1.Service)
	return ok && e.Ref.Kind == service.Kind && slices.ContainsFunc(slices.Collect(maps.Values(spec.GetApp().GetEnv())), catalog.Unrendered)
}

// pastDomains are the domains a route in env has ever sent web's whole path to,
// deleted routes' too, from every revision they had: a {domain} value one of them
// gave is Dockwell's, which a domain that comes after one that went moves on, where
// neither the address before nor the recorded one gives it (#779's check, N1).
func (h *Handler) pastDomains(ctx context.Context, tx *store.Tx, env, web string) ([]string, error) {
	revs, err := tx.RevisionsIn(ctx, env, string(route.Kind))
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range revs {
		rev := &revs[i]
		if rev.Spec == nil {
			continue
		}
		decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(rev.ID, rev.Kind, rev.Revision), rev.Spec)
		if err != nil {
			return nil, err
		}
		r, ok := decoded.(*resourcev1.Route)
		if ok && r.GetService() == web && r.GetPath() == "" && r.GetRedirect() == nil && r.GetDomain() != "" && !slices.Contains(out, r.GetDomain()) {
			out = append(out, r.GetDomain())
		}
	}
	return out, nil
}

// follow writes what follows one install's address, in env, where edits move it,
// and renders a value an edit asks for as an entry writes it at the address the
// change leaves.
func (h *Handler) follow(ctx context.Context, tx *store.Tx, project, env string, pin *resourcev1.CatalogPin, entry *catalogv1.Entry, edits []revision.Edit) ([]revision.Edit, []revision.Note, error) {
	web := entry.GetWeb().GetService()
	// What the change does to each service of its own: a deletion takes no edit of its
	// spec beside, and web going takes the app's address with it (#779's review, M1).
	// A restart becomes the edit of its spec, which restarts it anyway, and a restore
	// is refused beside a move (#779's check, N3).
	edited := func(name string) int {
		ref := resource.Ref{Kind: service.Kind, Scope: env, Name: name}
		return slices.IndexFunc(edits, func(e revision.Edit) bool { return e.Ref == ref })
	}
	deleting := func(name string) bool {
		i := edited(name)
		return i >= 0 && edits[i].Spec == nil && !edits[i].Restart && edits[i].Restore == nil
	}
	if deleting(web) {
		return edits, nil, nil
	}
	webRes, err := tx.GetResource(ctx, string(service.Kind), env, web)
	if errors.Is(err, store.ErrNotFound) {
		return edits, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	webSpec, err := h.serviceSpec(ctx, tx, webRes)
	if err != nil {
		return nil, nil, err
	}
	routes, err := h.routesIn(ctx, tx, env)
	if err != nil {
		return nil, nil, err
	}
	before := h.addressOf(routes, web, webRes.ID, webSpec)
	after := h.addressOf(afterEdits(routes, env, edits), web, webRes.ID, webSpec)
	moved := after != before && after.Host != ""
	var notes []revision.Note
	if moved {
		var err error
		if edits, notes, err = h.move(ctx, tx, project, env, pin, entry, edits, before, after, deleting, edited); err != nil {
			return nil, nil, err
		}
	}
	// A value an edit asks for as the entry writes it, {origin} and all, is rendered
	// at the address the change leaves, or refused where that gives it nothing, and
	// never written as it stands (#779's check, N5).
	for _, v := range entry.GetAddressVariables() {
		i := edited(v.GetService())
		if i < 0 {
			continue
		}
		spec, ok := edits[i].Spec.(*resourcev1.Service)
		if !ok || !catalog.Unrendered(spec.GetApp().GetEnv()[v.GetVariable()]) {
			continue
		}
		value, ok := catalog.AddressValue(v.GetValue(), after)
		if !ok || spec.GetApp().GetEnv()[v.GetVariable()] != v.GetValue() {
			return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s asks for %s as %s's entry writes it, %q, which its address can't give it now, so nothing was planned. Add a domain first, or give %s a value of its own",
				v.GetService(), v.GetVariable(), project, spec.GetApp().GetEnv()[v.GetVariable()], v.GetVariable()), "address_unrendered")
		}
		spec.GetApp().Env[v.GetVariable()] = value
		if !moved {
			notes = append(notes, revision.Note{
				Reason:  "address_follows",
				Message: fmt.Sprintf("%s follows the app's address to %s, which restarts %s", v.GetVariable(), after.Origin(), v.GetService()),
				Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: v.GetService()},
			})
		}
	}
	return edits, notes, nil
}

// move writes what follows an install's address where a change moves it from before
// to after.
func (h *Handler) move(ctx context.Context, tx *store.Tx, project, env string, pin *resourcev1.CatalogPin, entry *catalogv1.Entry, edits []revision.Edit, before, after catalog.Address, deleting func(string) bool, edited func(string) int) ([]revision.Edit, []revision.Note, error) {
	web := entry.GetWeb().GetService()
	if i := edited(web); i >= 0 && edits[i].Restore != nil {
		return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"this restores %s and moves %s's address in one change, which would leave what follows the address as the restore has it, so nothing was planned. Plan the domain change on its own, then the restore", web, project), "address_restore")
	}
	// Dockwell's own value is what the address before this gave it, which Dockwell
	// wrote as that address came, or what the recorded one gave it, or the file's, or
	// what any domain a route ever sent to web gave it.
	recorded, hasRecord := catalog.ParseAddress(pin.GetAddress())
	written, was := catalog.AddressValues(entry, recorded), catalog.AddressValues(entry, before)
	domains, err := h.pastDomains(ctx, tx, env, web)
	if err != nil {
		return nil, nil, err
	}
	var past []map[string]map[string]string
	for _, d := range domains {
		past = append(past, catalog.AddressValues(entry, catalog.DomainAddress(d)))
	}
	// The file's value counts only where there's no record, so the file is read only
	// then, and a file the current reader refuses holds nothing up but a variable
	// whose owner it alone would tell (#779's review, L4).
	var given map[string]map[string]string
	var unread error
	if !hasRecord {
		given, unread = fileEnv(ctx, pin)
	}
	var notes []revision.Note
	values := catalog.AddressValues(entry, after)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		ref := resource.Ref{Kind: service.Kind, Scope: env, Name: name}
		if deleting(name) {
			continue
		}
		r, err := tx.GetResource(ctx, string(service.Kind), env, name)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		at := edited(name)
		var spec *resourcev1.Service
		if at >= 0 {
			spec, _ = edits[at].Spec.(*resourcev1.Service)
		}
		if spec == nil {
			if spec, err = h.serviceSpec(ctx, tx, r); err != nil {
				return nil, nil, err
			}
			spec = proto.CloneOf(spec)
		}
		var set []string
		for _, variable := range slices.Sorted(maps.Keys(values[name])) {
			current, ok := spec.GetApp().GetEnv()[variable]
			value := values[name][variable]
			if ok && current == value {
				continue
			}
			// One the service doesn't have yet is Dockwell's to add, as the merge of an
			// upgrade adds a key new to both, such as Gitea's ROOT_URL on its first
			// domain (#779's review, H1).
			wrote, recordedOne := written[name][variable]
			then, wasOne := was[name][variable]
			// The file's value is Dockwell's only where the pin records no address: once
			// it does, Dockwell's own is what the record gives, and a value a person set
			// to the file's stays (#779's review, L2).
			fileOurs := !hasRecord && current == given[name][variable]
			pastOurs := slices.ContainsFunc(past, func(p map[string]map[string]string) bool {
				v, ok := p[name][variable]
				return ok && current == v
			})
			ours := !ok || catalog.Unrendered(current) || fileOurs || pastOurs || recordedOne && current == wrote || wasOne && current == then
			if !ours && unread != nil {
				return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
					"%s was installed from a file Dockwell can't read now (%w), so it can't tell whether %s holds a value you set, and nothing was planned. Upgrade %s, which records its address, then plan this again",
					project, unread, variable, project), "address_unread")
			}
			if !ours {
				notes = append(notes, revision.Note{
					Reason:  "address_kept",
					Message: fmt.Sprintf("%s holds a value you set, so it stays rather than follow the app's address to %s", variable, after.Origin()),
					Ref:     ref,
				})
				continue
			}
			if spec.GetApp().Env == nil {
				spec.GetApp().Env = map[string]string{}
			}
			spec.GetApp().Env[variable] = value
			// It's no secret, so it shows as the address it is (ADR 0081).
			if !slices.Contains(spec.GetShownEnv(), variable) {
				spec.ShownEnv = append(spec.ShownEnv, variable)
			}
			set = append(set, variable)
		}
		if len(set) == 0 {
			continue
		}
		switch {
		case at >= 0 && edits[at].Spec != nil:
			edits[at].Spec = spec
		case at >= 0:
			// A restart is the edit of its spec, which restarts it anyway.
			edits[at] = revision.Edit{Ref: ref, Spec: spec, Base: r.Revision}
		default:
			edits = append(edits, revision.Edit{Ref: ref, Spec: spec, Base: r.Revision})
		}
		verb := "follow"
		if len(set) == 1 {
			verb = "follows"
		}
		notes = append(notes, revision.Note{
			Reason:  "address_follows",
			Message: fmt.Sprintf("%s %s the app's address to %s, which restarts %s", strings.Join(set, " and "), verb, after.Origin(), name),
			Ref:     ref,
		})
	}
	// The pin keeps the address it recorded: changing the project is an admin's,
	// where a domain is a developer's to add. The address before a change is what
	// Dockwell wrote, and an upgrade records the one it has (ADR 0103).
	return edits, notes, nil
}

// serviceSpec is a service's spec as it's stored.
func (h *Handler) serviceSpec(ctx context.Context, tx *store.Tx, r store.Resource) (*resourcev1.Service, error) {
	decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	spec, ok := decoded.(*resourcev1.Service)
	if !ok {
		return nil, fmt.Errorf("%s isn't a service", r.Name)
	}
	return spec, nil
}

// upgradeAddress is what follows an install's address on an upgrade to entry: each
// variable's value at the address it has now, and at the one Dockwell last wrote,
// which the merge reads as Dockwell's (ADR 0103). It's where the address is now.
func (h *Handler) upgradeAddress(ctx context.Context, env string, entry *catalogv1.Entry, pin *resourcev1.CatalogPin) (composeapi.Address, catalog.Address, error) {
	web := entry.GetWeb().GetService()
	if len(entry.GetAddressVariables()) == 0 || web == "" {
		return composeapi.Address{}, catalog.Address{}, nil
	}
	var now catalog.Address
	var past []map[string]map[string]string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		domains, err := h.pastDomains(ctx, tx, env, web)
		if err != nil {
			return err
		}
		for _, d := range domains {
			past = append(past, catalog.AddressValues(entry, catalog.DomainAddress(d)))
		}
		r, err := tx.GetResource(ctx, string(service.Kind), env, web)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		spec, err := h.serviceSpec(ctx, tx, r)
		if err != nil {
			return err
		}
		routes, err := h.routesIn(ctx, tx, env)
		if err != nil {
			return err
		}
		now = h.addressOf(routes, web, r.ID, spec)
		return nil
	})
	if err != nil {
		return composeapi.Address{}, catalog.Address{}, err
	}
	recorded, _ := catalog.ParseAddress(pin.GetAddress())
	return composeapi.Address{Values: catalog.AddressValues(entry, now), Written: catalog.AddressValues(entry, recorded), Past: past}, now, nil
}

// addressState says where an install is reached now, and which of the variables
// that follow its address don't hold what that address gives them, as the app's
// page shows them, with a way to set them (ADR 0103; #779's check, N2).
func (h *Handler) addressState(ctx context.Context, tx *store.Tx, install *dockwellv1.CatalogInstall, entry *catalogv1.Entry) error {
	web, env := entry.GetWeb().GetService(), install.GetEnvironmentId()
	if web == "" || len(entry.GetAddressVariables()) == 0 {
		return nil
	}
	r, err := tx.GetResource(ctx, string(service.Kind), env, web)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	spec, err := h.serviceSpec(ctx, tx, r)
	if err != nil {
		return err
	}
	routes, err := h.routesIn(ctx, tx, env)
	if err != nil {
		return err
	}
	at := h.addressOf(routes, web, r.ID, spec)
	install.Address = at.Origin()
	values := catalog.AddressValues(entry, at)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		svc := spec
		if name != web {
			sr, err := tx.GetResource(ctx, string(service.Kind), env, name)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if svc, err = h.serviceSpec(ctx, tx, sr); err != nil {
				return err
			}
		}
		for _, variable := range slices.Sorted(maps.Keys(values[name])) {
			if current, ok := svc.GetApp().GetEnv()[variable]; !ok || current != values[name][variable] {
				install.AddressKept = append(install.AddressKept, name+"/"+variable)
			}
		}
	}
	return nil
}
