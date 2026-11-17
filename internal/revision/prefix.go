// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
)

// A link that records the name that tells its app the prefix owns that name's value:
// the value reads the link's prefix through the connection, as the bucket's does, so
// it can't say other than the link does, and a change to the link's prefix reaches
// the app (ADR 0099, the re-check's N3).

// recordsPrefix says whether a link to a connection records name as the one that
// tells its app the prefix. A link to a database's key prefix, as Redis's, is the
// database's to write (ADR 0084).
func recordsPrefix(l *resourcev1.Link, name string) bool {
	return l.GetConnection() != "" && slices.ContainsFunc(l.GetSets(), func(s *resourcev1.LinkSet) bool {
		return s.GetPart() == resourcev1.LinkPart_LINK_PART_KEY_PREFIX && s.GetName() == name
	})
}

// prefixNames are the names a link to a connection records as telling its app the
// prefix.
func prefixNames(l *resourcev1.Link) []string {
	if l.GetConnection() == "" {
		return nil
	}
	var out []string
	for _, s := range l.GetSets() {
		if s.GetPart() == resourcev1.LinkPart_LINK_PART_KEY_PREFIX {
			out = append(out, s.GetName())
		}
	}
	return out
}

// readsPrefixOf says whether a value reads the named link's prefix.
func readsPrefixOf(spec proto.Message, linkName string) bool {
	c := variable.FromOf(spec).GetConnection()
	return c.GetPart() == resourcev1.LinkPart_LINK_PART_KEY_PREFIX && c.GetLink() == linkName
}

// checkPrefixHeld refuses what would let a link's record of its prefix and its app's
// value of it drift apart:
//   - an edit of a value a link records as its prefix's name, to anything but that
//     link's prefix, or deleting it while the link records it;
//   - a link that records a prefix's name no service of its environment reads, or
//     whose value doesn't read the link's prefix, once the change applies.
//
// It asks only what the change edits, so a link and a value from before, which
// record nothing, keep working.
func (e *Engine) checkPrefixHeld(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	switch it.ref.Kind {
	case variable.VariableKind, variable.SecretKind:
		if it.follows || it.current == nil && it.spec == nil {
			return nil
		}
		links, err := e.linksAfter(ctx, tx, items, it.ref.Scope)
		if err != nil {
			return err
		}
		for _, name := range slicesSorted(links) {
			if !recordsPrefix(links[name], it.ref.Name) || it.spec != nil && readsPrefixOf(it.spec, name) {
				continue
			}
			return fault.New(ErrInvalid, "%s is the setting that tells the app of link %s its prefix, which the link writes, so it changes with the link. Change the prefix of link %s instead",
				it.ref.Name, name, name)
		}
	case "service":
		return e.checkServiceReads(ctx, tx, items, it)
	case variable.LinkKind:
		l, ok := it.spec.(*resourcev1.Link)
		if !ok {
			return nil
		}
		was, _ := it.current.(*resourcev1.Link)
		for _, name := range prefixNames(l) {
			if recordsPrefix(was, name) {
				continue
			}
			read, err := e.serviceReads(ctx, tx, items, it.ref.Scope, name)
			if err != nil {
				return err
			}
			if !read {
				return fault.New(ErrInvalid, "link %s names %s as the setting that tells its app the prefix, and no app in this environment reads %s. Name the setting its app reads, or leave it out",
					it.ref.Name, name, name)
			}
			value, err := e.valueAfter(ctx, tx, items, it.ref.Scope, name)
			if err != nil {
				return err
			}
			if !readsPrefixOf(value, it.ref.Name) {
				return fault.New(ErrInvalid, "link %s names %s as the setting that tells its app the prefix, which the link writes. Set %s to read the link's prefix, as Connect storage does",
					it.ref.Name, name, name)
			}
		}
	}
	return nil
}

// serviceReads says whether a service of an environment reads name, as the change
// leaves them.
func (e *Engine) serviceReads(ctx context.Context, tx *store.Tx, items []item, env, name string) (bool, error) {
	names, err := liveIn(ctx, tx, items, env, "service")
	if err != nil {
		return false, err
	}
	for _, n := range names {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: "service", Scope: env, Name: n})
		if err != nil {
			return false, err
		}
		svc, ok := spec.(*resourcev1.Service)
		if !ok {
			continue
		}
		for _, from := range svc.GetVariables() {
			if from == name {
				return true, nil
			}
		}
		for _, from := range svc.GetSecrets() {
			if from == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// valueAfter is the variable, or else the secret, of an environment by name as the
// change leaves it, or nil.
func (e *Engine) valueAfter(ctx context.Context, tx *store.Tx, items []item, env, name string) (proto.Message, error) {
	for _, kind := range []resource.Kind{variable.VariableKind, variable.SecretKind} {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: kind, Scope: env, Name: name})
		if err != nil {
			return nil, err
		}
		if spec != nil {
			return spec, nil
		}
	}
	return nil, nil
}

// slicesSorted is a map's keys, sorted, so a refusal names the same link each time.
func slicesSorted[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// serviceNamesRead are the names a service reads, as variables or secrets.
func serviceNamesRead(spec proto.Message) map[string]bool {
	svc, _ := spec.(*resourcev1.Service)
	out := map[string]bool{}
	for _, from := range svc.GetVariables() {
		out[from] = true
	}
	for _, from := range svc.GetSecrets() {
		out[from] = true
	}
	return out
}

// checkServiceReads holds a service edit to what a link change is held to (#772's
// last check, C3), as an app is a service:
//   - a service that starts reading a link's names joins that link's connection, so
//     an unshared connection another app uses refuses it;
//   - a service that reads a link's names reads the setting the link names as the one
//     that tells its app the prefix, or it would keep its files at the bucket's root;
//   - a service that stops reading that setting, where no other service of its
//     environment reads it, is refused while the link names it.
//
// It asks only what the edit changes of what the service reads.
func (e *Engine) checkServiceReads(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.follows {
		return nil
	}
	before, after := serviceNamesRead(it.current), serviceNamesRead(it.spec)
	if maps.Equal(before, after) {
		return nil
	}
	links, err := e.linksAfter(ctx, tx, items, it.ref.Scope)
	if err != nil {
		return err
	}
	for _, name := range slicesSorted(links) {
		l := links[name]
		if l.GetConnection() == "" {
			continue
		}
		var had, has bool
		for _, s := range l.GetSets() {
			had = had || before[s.GetName()]
			has = has || after[s.GetName()]
		}
		for _, setting := range prefixNames(l) {
			switch {
			case before[setting] && !after[setting]:
				read, err := e.serviceReads(ctx, tx, items, it.ref.Scope, setting)
				if err != nil {
					return err
				}
				if !read {
					return fault.New(ErrInvalid, "%s stops reading %s, the setting that tells it the prefix of link %s, so it would keep its files at the bucket's root. Remove the link first, or keep reading it",
						it.ref.Name, setting, name)
				}
			case has && !after[setting]:
				return fault.New(ErrInvalid, "%s reads the names of link %s but not %s, the setting that tells it the prefix, so it would keep its files at the bucket's root. Read %s too",
					it.ref.Name, name, setting, setting)
			}
		}
		if !had && has {
			if err := e.checkJoins(ctx, tx, items, it, l); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkJoins refuses a service that starts reading a link to a connection that isn't
// shared, where another app uses it, as checkOneApp refuses a link.
func (e *Engine) checkJoins(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: l.GetConnection()})
	if err != nil {
		return err
	}
	if c, ok := spec.(*resourcev1.ConnectionSpec); !ok || c.GetShared() {
		return nil
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	apps, err := e.appsOn(ctx, tx, items, links, l.GetConnection(), resource.Ref{})
	if err != nil {
		return err
	}
	mine := it.ref.Scope + "/" + it.ref.Name
	for app := range apps {
		if !sameApp(app, mine) {
			return fault.New(ErrInvalid, "the connection %s serves another app already, and isn't shared. Give %s a connection of its own, or have an admin share %s",
				l.GetConnection(), it.ref.Name, l.GetConnection())
		}
	}
	return nil
}
