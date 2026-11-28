// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
)

// checkCustomFields keeps a linked field's secrecy stable while its value may rotate.
func checkCustomFields(l *resourcev1.Link, c *resourcev1.ConnectionSpec) error {
	for _, set := range l.GetSets() {
		f := connection.CustomField(c, set.GetCustomField())
		if f == nil || connection.CustomSecret(f) != set.GetCustomSecret() {
			return fault.New(ErrInvalid, "%s on %s still uses %s from %s. Disconnect the app before removing the field or changing its secrecy",
				set.GetCustomEnv(), l.GetCustomApp(), set.GetCustomField(), l.GetConnection())
		}
	}
	return nil
}

func (e *Engine) checkCustomLink(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link) error {
	if it.ref.Name != link.CustomName(l.GetCustomApp(), l.GetConnection()) {
		return fault.New(ErrInvalid, "use Add from a connection to give this app its Custom link")
	}
	if err := e.checkConnectionReached(ctx, tx, items, it, l.GetConnection()); err != nil {
		return err
	}
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	c, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: connection.Kind, Scope: org.ID, Name: l.GetConnection()})
	if err != nil {
		return err
	}
	conn, _ := c.(*resourcev1.ConnectionSpec)
	if err := checkCustomFields(l, conn); err != nil {
		return err
	}
	if err := e.checkCustomApp(ctx, tx, items, it.ref.Scope, it.ref.Name, l); err != nil {
		return err
	}
	if before := linkOf(it.current); before.GetConnection() == l.GetConnection() {
		return nil
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	return e.checkOneApp(ctx, tx, items, it, l, links)
}

// checkCustomApp verifies that each mapping uses a matching backing resource and
// that it goes to the app under the name recorded on the link.
func (e *Engine) checkCustomApp(ctx context.Context, tx *store.Tx, items []item, env, name string, l *resourcev1.Link) error {
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: env, Name: l.GetCustomApp()})
	if err != nil {
		return err
	}
	app, _ := spec.(*resourcev1.Service)
	for _, set := range l.GetSets() {
		kind, mapped := variable.VariableKind, app.GetVariables()[set.GetCustomEnv()]
		if set.GetCustomSecret() {
			kind, mapped = variable.SecretKind, app.GetSecrets()[set.GetCustomEnv()]
		}
		v, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: kind, Scope: env, Name: set.GetName()})
		if err != nil {
			return err
		}
		ref := variable.FromOf(v).GetConnection()
		if mapped != set.GetName() || ref.GetLink() != name || ref.GetName() != l.GetConnection() || ref.GetCustomField() != set.GetCustomField() {
			return fault.New(ErrInvalid, "%s on %s belongs to the Custom connection %s. Disconnect it and add it again under the new name", set.GetCustomEnv(), l.GetCustomApp(), l.GetConnection())
		}
	}
	return nil
}

// checkCustomOwnership keeps a mapping intact when an edit arrives outside the
// connection RPCs, including through undo and app removal.
func (e *Engine) checkCustomOwnership(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	switch it.ref.Kind {
	case serviceKind:
		if it.follows || !readsCustomValues(it.current) && !readsCustomValues(it.spec) {
			return nil
		}
		links, err := e.linksAfter(ctx, tx, items, it.ref.Scope)
		if err != nil {
			return err
		}
		for _, name := range slicesSorted(links) {
			if l := links[name]; l.GetCustomApp() == it.ref.Name {
				if err := e.checkCustomApp(ctx, tx, items, it.ref.Scope, name, l); err != nil {
					return err
				}
			}
		}
	case link.Kind:
		before, after := linkOf(it.current), linkOf(it.spec)
		if before.GetKind() != resourcev1.LinkKind_LINK_KIND_CUSTOM {
			break
		}
		for _, set := range before.GetSets() {
			if slices.ContainsFunc(after.GetSets(), func(s *resourcev1.LinkSet) bool { return s.GetName() == set.GetName() }) {
				continue
			}
			v, err := e.valueAfter(ctx, tx, items, it.ref.Scope, set.GetName())
			if err != nil {
				return err
			}
			if v != nil {
				return fault.New(ErrInvalid, "remove the value for %s with its Custom link", set.GetCustomEnv())
			}
		}
	case variable.VariableKind, variable.SecretKind:
		if it.follows {
			return nil
		}
		before := variable.FromOf(it.current).GetConnection()
		if before.GetCustomField() == "" {
			break
		}
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: link.Kind, Scope: it.ref.Scope, Name: before.GetLink()})
		if err != nil {
			return err
		}
		if l := linkOf(spec); l != nil {
			if err := e.checkCustomApp(ctx, tx, items, it.ref.Scope, before.GetLink(), l); err != nil {
				return err
			}
		}
	}
	for _, use := range e.usesOf(it, it.spec) {
		if !strings.HasPrefix(use.Name, link.CustomValuePrefix) {
			continue
		}
		env := use.Scope
		if env == "" {
			env = it.ref.Scope
		}
		v, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: use.Kind, Scope: env, Name: use.Name})
		if err != nil {
			return err
		}
		ref := variable.FromOf(v).GetConnection()
		if ref.GetCustomField() == "" {
			continue
		}
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: link.Kind, Scope: env, Name: ref.GetLink()})
		if err != nil {
			return err
		}
		l := linkOf(spec)
		if it.ref.Kind != serviceKind || it.ref.Name != l.GetCustomApp() || env != it.ref.Scope ||
			!slices.ContainsFunc(l.GetSets(), func(s *resourcev1.LinkSet) bool {
				return s.GetName() == use.Name && s.GetCustomEnv() == use.As
			}) {
			return fault.New(ErrInvalid, "this value belongs to the app's Custom mapping. Add the connection to your app to use its fields")
		}
	}
	return nil
}

func readsCustomValues(spec proto.Message) bool {
	app, _ := spec.(*resourcev1.Service)
	for _, from := range []map[string]string{app.GetVariables(), app.GetSecrets()} {
		for _, name := range from {
			if strings.HasPrefix(name, link.CustomValuePrefix) {
				return true
			}
		}
	}
	return len(app.GetBuild().GetSecrets()) > 0
}
