// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Moving apps to another project or environment (ADR 0084, #572). An app is a
// service; moving it takes what only it uses, in one change: each is deleted where it
// was and made where it goes, a volume taking the old one's data (Edit.MoveFrom), so
// undoing the change moves everything back. What it shares with what stays isn't
// broken: a value both read is copied, and anything else that would part them, a
// volume or a network others use, a service it reads or that reads it, stays and
// holds the move back, each with the service to move too. What points at what moves
// from elsewhere, a link or a network's member, follows it in the same change.

// AppMove asks to move services, with what only they use, from one environment to
// another.
type AppMove struct {
	From, To string   // environment IDs
	Services []string // names in From
	// NewProject names a project the move makes, with its one environment, to go
	// to, in place of To. Making a project is an admin's (ADR 0049).
	NewProject string
	// Leaving says the change deletes From, with all else in it: nothing stays, so
	// what the services use goes with them, and nothing holds them back.
	Leaving bool
}

// MoveReport says what a move does, or what holds it back.
type MoveReport struct {
	// Moves is what goes, as it's named in From.
	Moves []resource.Ref
	// Copies are values others in From use too, which stay, and which the moved
	// apps get a copy of in To.
	Copies []resource.Ref
	// Follows is what points at what moves from elsewhere, and follows it.
	Follows []resource.Ref
	// Crosses are links of the app's own environment to a database server that stays,
	// which the move points across: the app reaches it where it is, through a link
	// network of its own, as the user it had there.
	Crosses []resource.Ref
	// Blocks is what holds the move back. With any, there's no plan.
	Blocks []MoveBlock
}

// MoveBlock is something that holds a move back.
type MoveBlock struct {
	Ref resource.Ref
	// Why, in words: the fact, then what to do.
	Why string
	// MoveToo is a service of From that moving too clears it, or "".
	MoveToo string
}

// moveFrom is what a move reads of an environment.
type moveFrom struct {
	items map[resource.Ref]*item
	refs  []resource.Ref // in the order they were listed
}

// PlanAppMove works out the edits that move apps, and what they do, without planning
// them. With anything in the way, the report says what, and there are no edits.
func (e *Engine) PlanAppMove(ctx context.Context, ask AppMove) ([]Edit, *MoveReport, error) {
	if ask.From == ask.To && ask.NewProject == "" {
		return nil, nil, fault.New(ErrInvalid, "the app is in that environment already. Choose another project or environment")
	}
	if len(ask.Services) == 0 {
		return nil, nil, fault.New(ErrInvalid, "the move names no service. Name the app's service")
	}
	var edits []Edit
	report := &MoveReport{}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		var made []Edit
		if ask.NewProject != "" {
			if slices.ContainsFunc(tree.Projects, func(p scope.Project) bool { return p.Name == ask.NewProject }) {
				return fault.New(ErrInvalid, "there's a project named %s already. Choose it, or another name", ask.NewProject)
			}
			if made, ask.To, err = e.NewProject(ctx, tx, tree.Organization.ID, ask.NewProject, ""); err != nil {
				return err
			}
		}
		fromProject, toProject := projectOf(tree, ask.From), projectOf(tree, ask.To)
		if toProject == nil && ask.NewProject != "" {
			toProject = &scope.Project{Name: ask.NewProject}
		}
		if fromProject == nil || toProject == nil {
			return fault.New(ErrNotFound, "there's no such environment. Choose one from the list")
		}
		from, err := e.readEnv(ctx, tx, ask.From)
		if err != nil {
			return err
		}
		to, err := e.readEnv(ctx, tx, ask.To)
		if err != nil {
			return err
		}
		moving := map[resource.Ref]bool{}
		for _, name := range ask.Services {
			ref := resource.Ref{Kind: serviceKind, Scope: ask.From, Name: name}
			if from.items[ref] == nil {
				return fault.New(ErrNotFound, "there's no service %s in that environment", name)
			}
			moving[ref] = true
		}
		var pin *resourcev1.CatalogPin
		if pin, err = e.pinOf(ctx, tx, fromProject); err != nil {
			return err
		}
		if pin != nil {
			// An install moves whole, as its pin is its project's (ADR 0084).
			for _, ref := range from.refs {
				if ref.Kind == serviceKind && !moving[ref] {
					report.Blocks = append(report.Blocks, MoveBlock{
						Ref: ref, MoveToo: ref.Name,
						Why: ref.Name + " is part of the same install, which moves whole. Move it too",
					})
				}
			}
			if ask.NewProject != "" {
				// A new project has no install of its own.
			} else if toPin, err := e.pinOf(ctx, tx, toProject); err != nil {
				return err
			} else if toPin != nil {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: resource.Ref{Kind: scope.ProjectKind, Name: toProject.Name},
					Why: toProject.Name + " is an install of its own, and a project holds one install. Choose another project",
				})
			}
		}
		copies, crossings := e.whatMoves(from, moving, report, ask.Leaving)
		e.checkPlace(from, to, moving, copies, report)
		taken, err := e.linkSubnets(ctx, tx)
		if err != nil {
			return err
		}
		for i := range crossings {
			c := &crossings[i]
			net := resource.Ref{Kind: "network", Scope: ask.To, Name: c.link.Name + "-link"}
			if to.items[net] != nil {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: net,
					Why: fmt.Sprintf("there's a network named %s where the app goes already, which its link %s would reach %s on. Rename it first, or choose another place", net.Name, c.link.Name, c.server.Name),
				})
			}
			for _, pw := range c.passwords {
				if there := (resource.Ref{Kind: pw.Kind, Scope: ask.To, Name: pw.Name}); to.items[there] != nil && !moving[pw] && !copies[pw] {
					report.Blocks = append(report.Blocks, MoveBlock{
						Ref: there,
						Why: fmt.Sprintf("there's a secret named %s where the app goes already, which the app reads %s's password as. Rename it first, or choose another place", pw.Name, c.server.Name),
					})
				}
			}
			if c.subnet = freeLinkSubnet(taken); c.subnet == "" {
				return fault.New(ErrInvalid, "every one of the networks Dockwell keeps addresses for links is taken, so the app can't reach %s where it stays. Remove a link you don't use, then try again", c.server.Name)
			}
			taken[c.subnet] = true
		}
		if len(report.Blocks) > 0 {
			return nil
		}
		edits = slices.Concat(made, e.moveEdits(from, to, ask, moving, copies, crossings))
		follows, err := e.whatFollows(ctx, tx, ask, moving)
		if err != nil {
			return err
		}
		for i := range follows {
			report.Follows = append(report.Follows, follows[i].Ref)
		}
		edits = append(edits, follows...)
		if pin != nil {
			pinEdits, err := e.movePin(ctx, tx, fromProject, toProject, pin)
			if err != nil {
				return err
			}
			for i := range pinEdits {
				pe := &pinEdits[i]
				// The project the move makes takes the pin as it's made.
				if j := slices.IndexFunc(edits, func(ed Edit) bool { return ed.Ref == pe.Ref }); j >= 0 {
					edits[j].Spec = pe.Spec
					continue
				}
				edits = append(edits, *pe)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sortRefs(report.Moves)
	sortRefs(report.Copies)
	sortRefs(report.Follows)
	sortRefs(report.Crosses)
	if len(report.Blocks) > 0 {
		return nil, report, nil
	}
	return edits, report, nil
}

func sortRefs(refs []resource.Ref) {
	slices.SortFunc(refs, func(a, b resource.Ref) int {
		if a.Kind != b.Kind {
			if a.Kind < b.Kind {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
}

func projectOf(tree scope.Tree, env string) *scope.Project {
	for i := range tree.Projects {
		for _, e := range tree.Projects[i].Environments {
			if e.ID == env {
				return &tree.Projects[i]
			}
		}
	}
	return nil
}

// readEnv reads what lives in an environment, as it is now.
func (e *Engine) readEnv(ctx context.Context, tx *store.Tx, env string) (*moveFrom, error) {
	list, err := tx.ListResources(ctx, env, "")
	if err != nil {
		return nil, err
	}
	out := &moveFrom{items: map[resource.Ref]*item{}}
	for i := range list {
		if list[i].Spec == nil {
			continue
		}
		ref := resource.Ref{Kind: resource.Kind(list[i].Kind), Scope: env, Name: list[i].Name}
		if _, err := e.kinds.Type(ref.Kind); err != nil {
			continue // a kind this control plane doesn't know stays as it is
		}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		if it.current == nil {
			continue
		}
		out.items[ref] = &it
		out.refs = append(out.refs, ref)
	}
	return out, nil
}

// pinOf is a project's catalog pin, or nil.
func (e *Engine) pinOf(ctx context.Context, tx *store.Tx, p *scope.Project) (*resourcev1.CatalogPin, error) {
	it, err := e.load(ctx, tx, resource.Ref{Kind: scope.ProjectKind, Name: p.Name})
	if err != nil {
		return nil, err
	}
	project, _ := it.current.(*resourcev1.Project)
	return project.GetCatalog(), nil
}

// usersIn lists, for each resource of the environment, what in it holds on to it: a
// service that mounts a volume or joins a network, a link that reads a service, what
// uses a value, and a link that stands for one. What only follows what it names, a
// route, a task, a backup policy, a check, a database user, holds nothing. And for
// each link, what reads it: what uses the values it stands for.
func (e *Engine) usersIn(from *moveFrom) (users, readers map[resource.Ref][]resource.Ref) {
	users, readers = map[resource.Ref][]resource.Ref{}, map[resource.Ref][]resource.Ref{}
	tiedTo := map[resource.Ref]resource.Ref{}
	for _, ref := range from.refs {
		for _, tie := range e.kinds.Ties(ref.Kind, ref.Name, from.items[ref].current) {
			tie.Scope = ref.Scope
			tiedTo[tie] = ref
		}
	}
	for _, ref := range from.refs {
		it := from.items[ref]
		if ref.Kind == serviceKind || ref.Kind == linkKind {
			for _, use := range e.kinds.References(ref.Kind, it.current) {
				to := resource.Ref{Kind: use.Kind, Scope: ref.Scope, Name: use.Name}
				users[to] = append(users[to], ref)
			}
		}
		for _, use := range e.usesOf(it, it.current) {
			// One read with its environment named is read through a link, from wherever
			// that is, and follows the link rather than what reads it.
			if use.Scope != "" {
				continue
			}
			to := resource.Ref{Kind: use.Kind, Scope: ref.Scope, Name: use.Name}
			users[to] = append(users[to], ref)
			// A database user reads the password its link sets as the link's own user,
			// beside its server, not as an app that reads the link.
			if link, ok := tiedTo[to]; ok && link != ref && !e.follows(ref.Kind) {
				readers[link] = append(readers[link], ref)
			}
		}
	}
	for tie, link := range tiedTo {
		users[tie] = append(users[tie], link)
	}
	return users, readers
}

// follows reports a kind that goes where what it names goes, and holds nothing.
func (e *Engine) follows(kind resource.Kind) bool {
	return kind != serviceKind && kind != linkKind && kind != "volume" && kind != "network" && !e.valuer(kind)
}

// whatMoves adds to moving what only the moving services use, and says what's
// copied, and what holds the move back.
func (e *Engine) whatMoves(from *moveFrom, moving map[resource.Ref]bool, report *MoveReport, leaving bool) (map[resource.Ref]bool, []crossing) {
	users, readers := e.usersIn(from)
	copies := map[resource.Ref]bool{}
	allMoving := func(refs []resource.Ref) bool {
		return !slices.ContainsFunc(refs, func(r resource.Ref) bool { return !moving[r] })
	}
	anyMoving := func(refs []resource.Ref) bool {
		return slices.ContainsFunc(refs, func(r resource.Ref) bool { return moving[r] })
	}
	named := func(ref resource.Ref) []resource.Ref {
		var out []resource.Ref
		for _, use := range e.kinds.References(ref.Kind, from.items[ref].current) {
			out = append(out, resource.Ref{Kind: use.Kind, Scope: ref.Scope, Name: use.Name})
		}
		return out
	}
	for grew := true; grew; {
		grew = false
		for _, ref := range from.refs {
			if moving[ref] {
				continue
			}
			var goes bool
			switch {
			case e.follows(ref.Kind):
				// A route, a task, a backup policy, a check, a database user in a server
				// that moves: it goes where all it names goes.
				n := named(ref)
				goes = len(n) > 0 && allMoving(n)
			case ref.Kind == linkKind:
				// A link goes with what reads it, when nothing that stays does.
				r := readers[ref]
				goes = len(r) > 0 && allMoving(r)
			case ref.Kind == serviceKind && leaving:
				// Only what was asked, and what it reads, goes: the rest is deleted.
				u := users[ref]
				goes = len(u) > 0 && allMoving(u)
			default:
				// With nothing staying, what anything that moves uses goes with it.
				u := users[ref]
				goes = len(u) > 0 && (allMoving(u) || leaving && anyMoving(u))
			}
			if goes {
				moving[ref], grew = true, true
			}
		}
	}
	crossings := e.crossings(from, moving, readers, leaving)
	crossed := map[resource.Ref]bool{} // links pointed across, and the values that read the password
	passwords := map[resource.Ref]bool{}
	for i := range crossings {
		crossed[crossings[i].link] = true
		for _, p := range crossings[i].passwords {
			passwords[p] = true
		}
	}
	for _, ref := range from.refs {
		u := users[ref]
		if leaving || moving[ref] || !anyMoving(u) || passwords[ref] {
			continue
		}
		// A server that stays, which what moves reads only through links pointed
		// across, is reached where it is.
		if ref.Kind == serviceKind && !slices.ContainsFunc(u, func(r resource.Ref) bool { return moving[r] && !crossed[r] }) {
			continue
		}
		var stays []resource.Ref
		for _, other := range u {
			if !moving[other] {
				stays = append(stays, other)
			}
		}
		switch {
		case ref.Kind == "secret" && e.valuer(ref.Kind) && !slices.ContainsFunc(stays, func(r resource.Ref) bool { return r.Kind == linkKind }):
			// A secret others read too isn't copied: the copy would give it, such as a
			// database's admin password, to whoever holds where the app goes (the
			// review of #572, its L3).
			for _, other := range stays {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: other, MoveToo: serviceName(other),
					Why: fmt.Sprintf("%s %s reads the secret %s with the app, which isn't copied to where the app goes. Move it too", other.Kind, other.Name, ref.Name),
				})
			}
		case e.valuer(ref.Kind) && !slices.ContainsFunc(stays, func(r resource.Ref) bool { return r.Kind == linkKind }):
			// A variable others read too stays, and the app gets a copy of its own.
			copies[ref] = true
		case ref.Kind == "volume":
			for _, other := range stays {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: ref, MoveToo: serviceName(other),
					Why: fmt.Sprintf("the volume %s is %s's too, and a volume is in one place. Move %s too", ref.Name, other.Name, other.Name),
				})
			}
		case ref.Kind == "network":
			for _, other := range stays {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: other, MoveToo: serviceName(other),
					Why: fmt.Sprintf("%s shares the network %s with the app, so they may reach each other by name. Move %s too", other.Name, ref.Name, other.Name),
				})
			}
		case ref.Kind == serviceKind:
			// A database the app reads through a link, which others read too, or
			// which stays (ADR 0084: one in another environment is met by a link).
			report.Blocks = append(report.Blocks, MoveBlock{
				Ref: ref, MoveToo: ref.Name,
				Why: fmt.Sprintf("the app reads %s, which stays. Move %s too", ref.Name, ref.Name),
			})
		default:
			for _, other := range stays {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: other, MoveToo: serviceName(other),
					Why: fmt.Sprintf("%s %s reads %s %s with the app. Move it too", other.Kind, other.Name, ref.Kind, ref.Name),
				})
			}
		}
	}
	// A link the app reads that stays, as others read it too.
	for _, ref := range from.refs {
		if !leaving && ref.Kind == linkKind && !moving[ref] && anyMoving(readers[ref]) {
			report.Blocks = append(report.Blocks, MoveBlock{
				Ref: ref,
				Why: fmt.Sprintf("the app reads its database through the link %s, which others read too, so it can't take it", ref.Name),
			})
		}
	}
	// A service that stays and names what moves would lose it.
	for _, ref := range from.refs {
		if leaving || moving[ref] || ref.Kind != serviceKind {
			continue
		}
		for _, to := range named(ref) {
			if moving[to] && to.Kind == serviceKind {
				report.Blocks = append(report.Blocks, MoveBlock{
					Ref: ref, MoveToo: ref.Name,
					Why: fmt.Sprintf("%s reads %s, which would leave it. Move %s too", ref.Name, to.Name, ref.Name),
				})
			}
		}
	}
	report.Blocks = uniqueBlocks(report.Blocks)
	for _, ref := range from.refs {
		switch {
		case moving[ref]:
			report.Moves = append(report.Moves, ref)
		case copies[ref]:
			report.Copies = append(report.Copies, ref)
		}
	}
	for i := range crossings {
		report.Crosses = append(report.Crosses, crossings[i].link)
	}
	return copies, crossings
}

// crossing is a link of the app's own environment to a database server that stays,
// which a move points across environments (ADR 0084, #572): in its new place it meets
// the server where it is, as a link that shares one does, through a link network of
// its own on which the server answers to its own name, and the app reads its password
// from the user it had there, which stays with the server.
type crossing struct {
	link, server resource.Ref
	// password is the secret its user's password is in, beside the server.
	password string
	// readers are the services that read it, and passwords the values it sets that
	// hold the password, which read the user's from where it is.
	readers   []resource.Ref
	passwords []resource.Ref
	// subnet is its link network's, where it goes.
	subnet string
}

// crossings are the links that move and meet, in their own environment, a server
// that stays and that they have a user of their own on. One that signs in as the
// server's administrator, as with no user of its own, isn't: that would take the
// administrator's password to another project, so it holds the move back.
func (e *Engine) crossings(from *moveFrom, moving map[resource.Ref]bool, readers map[resource.Ref][]resource.Ref, leaving bool) []crossing {
	if leaving {
		return nil
	}
	var out []crossing
	for _, ref := range from.refs {
		l, ok := from.items[ref].current.(*resourcev1.Link)
		if ref.Kind != linkKind || !ok || !moving[ref] || l.GetMeetsEnvironment() != "" || l.GetUser() == "" {
			continue
		}
		server := resource.Ref{Kind: serviceKind, Scope: ref.Scope, Name: l.GetService()}
		if moving[server] || from.items[server] == nil {
			continue
		}
		u, ok := from.items[resource.Ref{Kind: "database.user", Scope: ref.Scope, Name: l.GetUser()}]
		user, _ := u.current.(*resourcev1.DatabaseUser)
		if !ok || user.GetService() != l.GetService() {
			continue
		}
		c := crossing{link: ref, server: server, password: user.GetPassword(), readers: readers[ref]}
		for _, s := range l.GetSets() {
			if s.GetPart() == resourcev1.LinkPart_LINK_PART_PASSWORD {
				c.passwords = append(c.passwords, resource.Ref{Kind: "secret", Scope: ref.Scope, Name: s.GetName()})
			}
		}
		out = append(out, c)
	}
	return out
}

// serviceName is a ref's name when it's a service, so moving it too clears a block.
func serviceName(ref resource.Ref) string {
	if ref.Kind == serviceKind {
		return ref.Name
	}
	return ""
}

func uniqueBlocks(blocks []MoveBlock) []MoveBlock {
	seen := map[string]bool{}
	return slices.DeleteFunc(blocks, func(b MoveBlock) bool {
		key := b.Ref.String() + "|" + b.Why
		if seen[key] {
			return true
		}
		seen[key] = true
		return false
	})
}

// checkPlace holds back a move into an environment that has something of the same
// kind and name already. A network of the name, made the same, is shared there, as
// it would be made, as each environment's default network is.
func (e *Engine) checkPlace(from, to *moveFrom, moving, copies map[resource.Ref]bool, report *MoveReport) {
	for _, set := range []map[resource.Ref]bool{moving, copies} {
		refs := slices.Collect(maps.Keys(set))
		sortRefs(refs)
		for _, ref := range refs {
			there := resource.Ref{Kind: ref.Kind, Scope: to.scope(), Name: ref.Name}
			if to.items[there] == nil || sharesNetwork(from.items[ref], to.items[there]) {
				continue
			}
			report.Blocks = append(report.Blocks, MoveBlock{
				Ref: there,
				Why: fmt.Sprintf("there's a %s named %s where the app goes already. Rename one of them first, or choose another place", ref.Kind, ref.Name),
			})
		}
	}
}

// sharesNetwork reports a network where the app goes that it joins in place of
// making its own: one of the name, made the same.
func sharesNetwork(mine, there *item) bool {
	return mine != nil && there != nil && mine.ref.Kind == "network" && there.current != nil && proto.Equal(mine.current, there.current)
}

func (m *moveFrom) scope() string {
	if len(m.refs) == 0 {
		return ""
	}
	return m.refs[0].Scope
}

// moveEdits deletes what moves where it is, makes it where it goes, a volume with
// the old one's data, and makes a copy of each value it shares.
func (e *Engine) moveEdits(from, to *moveFrom, ask AppMove, moving, copies map[resource.Ref]bool, crossings []crossing) []Edit {
	var edits []Edit
	// What a link pointed across changes of what moves with it: the link meets the
	// server where it is, the values that hold the password read its user's there,
	// and what reads it joins its link network.
	crossed := map[resource.Ref]*crossing{}
	readsPassword := map[resource.Ref]*crossing{}
	joins := map[resource.Ref][]string{}
	for i := range crossings {
		c := &crossings[i]
		crossed[c.link] = c
		for _, p := range c.passwords {
			readsPassword[p] = c
		}
		for _, r := range c.readers {
			if r.Kind == serviceKind {
				joins[r] = append(joins[r], c.link.Name+"-link")
			}
		}
	}
	for _, ref := range from.refs {
		it := from.items[ref]
		c := readsPassword[ref]
		if !moving[ref] && !copies[ref] && c == nil {
			continue
		}
		spec := proto.Clone(it.current)
		pointAt(spec, ask.To)
		switch s := spec.(type) {
		case *resourcev1.Link:
			if crossed[ref] != nil {
				s.MeetsEnvironment = ask.From
			}
		case *resourcev1.Service:
			if app := s.GetApp(); app != nil {
				for _, n := range joins[ref] {
					if !slices.Contains(app.GetNetworks(), n) {
						app.Networks = append(app.Networks, n)
					}
				}
			}
		case *resourcev1.Secret:
			if c != nil {
				spec = &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{
					Environment: ask.From, Secret: true, Name: c.password, Link: c.link.Name,
				}}}}
			}
		}
		there := Edit{Ref: resource.Ref{Kind: ref.Kind, Scope: ask.To, Name: ref.Name}, Spec: spec}
		if it.runs {
			there.Servers = slices.Clone(it.placed)
		}
		// A network shared there goes on the servers the app runs on too.
		if shared := to.items[there.Ref]; sharesNetwork(it, shared) {
			there.Spec = proto.Clone(shared.current)
			there.Servers = slices.Compact(slices.Sorted(slices.Values(slices.Concat(shared.placed, it.placed))))
		}
		if moving[ref] {
			// A volume takes its data with it, and a link the backups taken for it.
			if ref.Kind == volumeKind || ref.Kind == linkKind {
				there.MoveFrom = ref
			}
			edits = append(edits, Edit{Ref: ref, Base: it.base})
		}
		edits = append(edits, there)
	}
	// Each link pointed across reaches its server on a link network of its own, where
	// the server answers to its own name, as the app knew it beside it.
	for i := range crossings {
		c := &crossings[i]
		edits = append(edits, Edit{
			Ref: resource.Ref{Kind: "network", Scope: ask.To, Name: c.link.Name + "-link"},
			Spec: &resourcev1.Network{Internal: true, Subnet: c.subnet, Members: []*resourcev1.NetworkMember{
				{Environment: ask.From, Service: c.server.Name, Alias: c.server.Name},
			}},
			Servers: slices.Clone(from.items[c.server].placed),
		})
	}
	return edits
}

// linkRange is where link networks' addresses come from, a /29 each, as sharing a
// server gives one (ADR 0084).
var linkRange = netip.MustParsePrefix("10.233.0.0/16")

// linkSubnets are the subnets networks on record have.
func (e *Engine) linkSubnets(ctx context.Context, tx *store.Tx) (map[string]bool, error) {
	list, err := tx.ListKind(ctx, "network")
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for i := range list {
		if list[i].Spec == nil {
			continue
		}
		it, err := e.load(ctx, tx, resource.Ref{Kind: "network", Scope: list[i].Scope, Name: list[i].Name})
		if err != nil {
			return nil, err
		}
		if n, ok := it.current.(*resourcev1.Network); ok && n.GetSubnet() != "" {
			taken[n.GetSubnet()] = true
		}
	}
	return taken, nil
}

// freeLinkSubnet is a /29 of linkRange taken hasn't, or "".
func freeLinkSubnet(taken map[string]bool) string {
	for a := linkRange.Addr(); linkRange.Contains(a); {
		if p := netip.PrefixFrom(a, 29); !taken[p.String()] {
			return p.String()
		}
		for range 8 {
			a = a.Next()
		}
	}
	return ""
}

// pointAt has a link that meets a database in the environment to name its own
// instead, once it's in to. A network's member and a value read through a link name
// their environment by ID even when it's their own.
func pointAt(spec proto.Message, to string) {
	if s, ok := spec.(*resourcev1.Link); ok && s.GetMeetsEnvironment() == to {
		s.MeetsEnvironment = ""
	}
}

// whatFollows rewrites what points at what moves from other environments: a link
// that meets a service that moves, a network one is a member of, a value read from
// one that moves. Each is Dockwell's own item there (ADR 0084).
func (e *Engine) whatFollows(ctx context.Context, tx *store.Tx, ask AppMove, moving map[resource.Ref]bool) ([]Edit, error) {
	movingNamed := func(kind resource.Kind, name string) bool {
		return moving[resource.Ref{Kind: kind, Scope: ask.From, Name: name}]
	}
	var out []Edit
	for _, kind := range []resource.Kind{linkKind, "network", "secret", "variable"} {
		if _, err := e.kinds.Type(kind); err != nil {
			continue
		}
		list, err := tx.ListKind(ctx, string(kind))
		if err != nil {
			return nil, err
		}
		for i := range list {
			r := &list[i]
			if r.Spec == nil || r.Scope == ask.From {
				continue
			}
			ref := resource.Ref{Kind: kind, Scope: r.Scope, Name: r.Name}
			it, err := e.load(ctx, tx, ref)
			if err != nil {
				return nil, err
			}
			spec := proto.Clone(it.current)
			changed := false
			there := ask.To
			if r.Scope == ask.To {
				there = ""
			}
			switch s := spec.(type) {
			case *resourcev1.Link:
				if s.GetMeetsEnvironment() == ask.From && movingNamed(serviceKind, s.GetService()) {
					s.MeetsEnvironment, changed = there, true
				}
			case *resourcev1.Network:
				for _, m := range s.GetMembers() {
					if m.GetEnvironment() == ask.From && movingNamed(serviceKind, m.GetService()) {
						m.Environment, changed = ask.To, true
					}
				}
			case *resourcev1.Secret:
				changed = followValue(s.GetFrom(), ask.From, ask.To, movingNamed)
			case *resourcev1.Variable:
				changed = followValue(s.GetFrom(), ask.From, ask.To, movingNamed)
			}
			if changed {
				out = append(out, Edit{Ref: ref, Spec: spec, Base: it.base})
			}
		}
	}
	return out, nil
}

func followValue(f *resourcev1.ValueFrom, from, to string, moving func(resource.Kind, string) bool) bool {
	v := f.GetValue()
	if v == nil || v.GetEnvironment() != from {
		return false
	}
	kind := resource.Kind("variable")
	if v.GetSecret() {
		kind = "secret"
	}
	if !moving(kind, v.GetName()) {
		return false
	}
	v.Environment = to
	return true
}

// movePin gives the install's pin to the project it moves to, and takes it from the
// one it leaves.
func (e *Engine) movePin(ctx context.Context, tx *store.Tx, from, to *scope.Project, pin *resourcev1.CatalogPin) ([]Edit, error) {
	var out []Edit
	for _, p := range []*scope.Project{from, to} {
		ref := resource.Ref{Kind: scope.ProjectKind, Name: p.Name}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		spec, _ := proto.Clone(it.current).(*resourcev1.Project)
		if spec == nil {
			spec = &resourcev1.Project{}
		}
		if p == from {
			spec.Catalog = nil
		} else {
			spec.Catalog = proto.Clone(pin).(*resourcev1.CatalogPin)
		}
		out = append(out, Edit{Ref: ref, Spec: spec, Base: it.base})
	}
	return out, nil
}

// MoveWords says what a move report holds back, for an error.
func MoveWords(blocks []MoveBlock) string {
	words := make([]string, len(blocks))
	for i, b := range blocks {
		words[i] = b.Why
	}
	return human.List(words)
}
