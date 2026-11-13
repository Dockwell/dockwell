// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// One you run: a server Dockwell runs already, shared because a person chose it, on
// which the install makes the app a database and a user of its own, in the server's
// environment, and in the app's a link, a network for the two that the server joins in
// place, and a secret whose value comes from the user's (ADR 0084).

// Shareable lists the database servers Dockwell runs, as ways to meet a need.
type Shareable interface {
	Candidates(ctx context.Context) ([]catalog.Candidate, error)
}

// ShareWith has the catalog offer the servers shareable lists as ways to meet a need.
func (h *Handler) ShareWith(s Shareable) { h.shareable = s }

// candidates are the servers that might meet a need, or none where nothing lists them.
func (h *Handler) candidates(ctx context.Context) []catalog.Candidate {
	if h.shareable == nil {
		return nil
	}
	list, err := h.shareable.Candidates(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "couldn't list the database servers an app could share", "error", err.Error())
		return nil
	}
	return list
}

// asksToShare reports a request that meets a need with a server Dockwell runs.
func asksToShare(asked map[string]string) bool {
	for _, c := range asked {
		if strings.HasPrefix(c, "service:") {
			return true
		}
	}
	return false
}

// linkRange is where link networks' addresses come from, a /29 each: 8,192 of them,
// outside the pools Docker gives its own networks (ADR 0084).
var linkRange = netip.MustParsePrefix("10.233.0.0/16")

// freeSubnet is a /29 in linkRange no network on record has, or an error saying
// there's none left.
func (h *Handler) freeSubnet(ctx context.Context, tx *store.Tx) (string, error) {
	nets, err := tx.ListKind(ctx, string(network.Kind))
	if err != nil {
		return "", err
	}
	stored := make([]store.StoredSpec, 0, len(nets))
	for _, r := range nets {
		if r.Spec != nil {
			stored = append(stored, store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec})
		}
	}
	decoded, _, err := h.keeper.DecodeEach(ctx, tx, stored)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, spec := range decoded {
		if n, ok := spec.(*resourcev1.Network); ok && n.GetSubnet() != "" {
			taken[n.GetSubnet()] = true
		}
	}
	for a := linkRange.Addr(); linkRange.Contains(a); {
		p := netip.PrefixFrom(a, 29)
		if !taken[p.String()] {
			return p.String(), nil
		}
		for range 8 {
			a = a.Next()
		}
	}
	return "", authapi.ReasonError(connect.CodeResourceExhausted,
		fmt.Errorf("every one of the %d networks Dockwell keeps addresses for links is taken. Remove a link you don't use, or give the app a new database beside it", 1<<13), "no_link_network")
}

// share names what each shared way makes on its server: the database and user, from
// the entry's or the project's names and free there, and the secret there that holds
// the user's password, free in that environment.
func (h *Handler) share(ctx context.Context, project string, met map[string]catalog.Way) error {
	if !slices.ContainsFunc(slices.Collect(maps.Values(met)), func(w catalog.Way) bool { return w.Shares() && w.Password == "" }) {
		return nil
	}
	return h.db.View(ctx, func(tx *store.Tx) error {
		for _, name := range slices.Sorted(maps.Keys(met)) {
			w := met[name]
			// One the pin says keeps what it was made with.
			if !w.Shares() || w.Password != "" {
				continue
			}
			// What Dockwell made there already is taken too, as the user of the app the
			// server is beside, which a reading may not list yet.
			c := *w.Existing
			made, err := tx.ListResources(ctx, c.EnvironmentID, string(database.UserKind))
			if err != nil {
				return err
			}
			for _, r := range made {
				c.Users = append(slices.Clone(c.Users), r.Name)
				u := &resourcev1.DatabaseUser{}
				if r.Spec != nil && proto.Unmarshal(r.Spec, u) == nil {
					c.Databases = append(slices.Clone(c.Databases), database.Owned(r.Name, u))
				}
			}
			// And what the app it's beside signs in as, as its link says, as an image
			// that made that user keeps it where Dockwell has no user of its own.
			links, err := tx.ListResources(ctx, c.EnvironmentID, "link")
			if err != nil {
				return err
			}
			for _, r := range links {
				l := &resourcev1.Link{}
				if r.Spec != nil && proto.Unmarshal(r.Spec, l) == nil && l.GetService() == c.Service && l.GetMeetsEnvironment() == "" {
					c.Users = append(slices.Clone(c.Users), l.GetUser())
					c.Databases = append(slices.Clone(c.Databases), l.GetDatabaseName())
				}
			}
			w.Database, w.User = catalog.SharedNames(w.Need, project, c)
			secret := strings.ToUpper(w.User) + "_DATABASE_PASSWORD"
			for i := 2; ; i++ {
				_, errS := tx.GetResource(ctx, string(variable.SecretKind), w.Existing.EnvironmentID, secret)
				_, errV := tx.GetResource(ctx, string(variable.VariableKind), w.Existing.EnvironmentID, secret)
				_, errU := tx.GetResource(ctx, string(database.UserKind), w.Existing.EnvironmentID, w.User)
				if errors.Is(errS, store.ErrNotFound) && errors.Is(errV, store.ErrNotFound) {
					if !errors.Is(errU, store.ErrNotFound) && errU == nil {
						return authapi.ReasonError(connect.CodeAlreadyExists,
							fmt.Errorf("%s has a database user %s already. Install under another name", w.Existing.Service, w.User), "user_exists")
					}
					break
				}
				secret = fmt.Sprintf("%s_DATABASE_PASSWORD_%d", strings.ToUpper(w.User), i)
			}
			w.Password = secret
			met[name] = w
		}
		return nil
	})
}

// linkJoins are the link networks of the needs an install shares a server for, as its
// pin says, each with the names its need sets, so the app's services stay on them
// through an upgrade or a change of companions.
func linkJoins(needed *catalog.Needed) map[string][]string {
	out := map[string][]string{}
	for i := range needed.Shared {
		w := &needed.Shared[i]
		for _, s := range w.Need.GetSets() {
			out[w.Need.GetName()+"-link"] = append(out[w.Need.GetName()+"-link"], s.GetName())
		}
	}
	return out
}

// pinJoins are linkJoins from an install's pin, for a change that doesn't meet its
// needs again, as a change of companions doesn't.
func pinJoins(e *catalogv1.Entry, pin *resourcev1.CatalogPin) map[string][]string {
	out := map[string][]string{}
	for _, n := range e.GetNeeds() {
		if pin.GetNeeds()[n.GetName()].GetEnvironment() == "" {
			continue
		}
		for _, s := range n.GetSets() {
			out[n.GetName()+"-link"] = append(out[n.GetName()+"-link"], s.GetName())
		}
	}
	return out
}

// userEdits are the database users an install's plan makes on a new server beside
// the app, whose image makes only its administrator, in the app's environment.
func userEdits(needed *catalog.Needed) []*dockwellv1.Edit {
	var out []*dockwellv1.Edit
	for _, name := range slices.Sorted(maps.Keys(needed.Users)) {
		out = append(out, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(database.UserKind), Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: needed.Users[name]}},
		})
	}
	return out
}

// writtenEdits are the URLs an install's needs set, each a secret of the app's
// environment that Dockwell writes from the need's parts there. It takes the value of
// no meaning Fill made for each out of filled, so the install keeps none of it, and
// the app's services use each secret by reference.
func writtenEdits(needed *catalog.Needed, filled *catalog.Filled) []*dockwellv1.Edit {
	var out []*dockwellv1.Edit
	for _, name := range slices.Sorted(maps.Keys(needed.Written)) {
		if _, ok := filled.Secrets[name]; ok {
			delete(filled.Secrets, name)
			filled.Held[name] = filled.Variables[name]
		}
		out = append(out, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Template{Template: needed.Written[name]},
			}}}},
		})
	}
	return out
}

// sharedEdits are what a shared way makes besides the app: the user's password, from
// the value Fill generated for the app, and the user, on the server, in its
// environment; and in the app's, the secret the app reads, whose value comes from the
// user's, and the link network the server joins in place. It takes the generated value
// out of filled, so no secret of the app's own holds it.
func (h *Handler) sharedEdits(ctx context.Context, needed *catalog.Needed, filled *catalog.Filled) (composeapi.Needs, []string, error) {
	var out composeapi.Needs
	var servers []string
	owners := map[string]*owner{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		for i := range needed.Shared {
			w := &needed.Shared[i]
			n, c := w.Need, w.Existing
			if c.Project == "" {
				// One the pin says is there already.
				continue
			}
			password := catalog.PasswordName(n)
			value, ok := filled.Secrets[password]
			if !ok {
				return fmt.Errorf("no password was made for %s", n.GetName())
			}
			delete(filled.Secrets, password)
			filled.Held[password] = filled.Variables[password]
			subnet, err := h.freeSubnet(ctx, tx)
			if err != nil {
				return err
			}
			user := &resourcev1.DatabaseUser{Service: c.Service, Password: w.Password, CopyFrom: w.CopyFrom}
			if w.Database != w.User {
				user.DatabaseName = w.Database
			}
			linkNet := n.GetName() + "-link"
			out.Extra = append(out.Extra,
				&dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: c.EnvironmentID, Name: w.Password},
					Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}},
				},
				&dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: string(database.UserKind), Scope: c.EnvironmentID, Name: w.User},
					Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: user}},
					Servers:  slices.Clone(c.Servers),
				},
				&dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Name: password},
					Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
						Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: c.EnvironmentID, Secret: true, Name: w.Password, Link: n.GetName()}},
					}}}},
				},
				&dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: string(network.Kind), Name: linkNet},
					Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Network{Network: &resourcev1.Network{
						Internal: true, Subnet: subnet,
						Members: []*resourcev1.NetworkMember{{Environment: c.EnvironmentID, Service: c.Service, Alias: w.Service()}},
					}}},
				},
			)
			if out.Joins == nil {
				out.Joins = map[string][]string{}
			}
			for _, s := range n.GetSets() {
				out.Joins[linkNet] = append(out.Joins[linkNet], s.GetName())
			}
			for _, s := range c.Servers {
				if !slices.Contains(servers, s) {
					servers = append(servers, s)
				}
			}
			if c.PartOf != "" && c.Pinnable {
				if err := pinApart(ctx, tx, c, owners); err != nil {
					return err
				}
			}
		}
		if len(owners) > 0 {
			org, err := tx.Organization(ctx)
			if err != nil {
				return err
			}
			for _, o := range owners {
				o.org = org.ID
			}
		}
		for _, id := range slices.Sorted(maps.Keys(owners)) {
			o := owners[id]
			out.Extra = append(out.Extra, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Scope: o.org, Name: o.name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: o.spec}},
			})
		}
		return nil
	})
	return out, servers, err
}

// owner is a catalog install whose own database the plan pins apart, by its project's
// name, with its project's spec as the plan leaves it.
type owner struct {
	name, org string
	spec      *resourcev1.Project
}

// pinApart has the install a shared server is the entry's own service of keep it in
// a pin of its own, as it runs, in the plan that first shares it, so the app's
// upgrades leave it alone (ADR 0084).
func pinApart(ctx context.Context, tx *store.Tx, c *catalog.Candidate, owners map[string]*owner) error {
	o, ok := owners[c.ProjectID]
	if !ok {
		r, err := tx.GetResourceByID(ctx, c.ProjectID)
		if err != nil {
			return err
		}
		// A project is never sealed (ADR 0022).
		spec := &resourcev1.Project{}
		if err := proto.Unmarshal(r.Spec, spec); err != nil {
			return fmt.Errorf("can't read the project %s: %w", r.Name, err)
		}
		o = &owner{name: r.Name, spec: spec}
		owners[c.ProjectID] = o
	}
	pin := o.spec.GetCatalog()
	ran, err := catalog.MetFile(string(pin.GetFile()), pin.GetNeeds())
	if err != nil {
		return err
	}
	apart, err := catalog.PinApart(ran, c.Service, pin.GetNeeds()[c.Service], c.Version)
	if err != nil {
		return err
	}
	pin.Needs[c.Service] = apart
	return nil
}
