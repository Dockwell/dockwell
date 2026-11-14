// SPDX-License-Identifier: AGPL-3.0-only

// Package linked finds the databases an app keeps alone on servers other
// environments run, through its links, which its updates dump first (ADR 0084).
package linked

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Databases are the databases env's links have alone on servers other environments
// run, each to dump first, made for its link. uses, where set, keeps only the links
// of which it reports using a name the link sets; nil keeps every one.
func Databases(ctx context.Context, tx *store.Tx, keeper *secrets.Keeper, env string, uses func(sets []string) bool) ([]revision.OneBackup, error) {
	links, err := tx.ListResources(ctx, env, "link")
	if err != nil {
		return nil, err
	}
	var out []revision.OneBackup
	for _, r := range links {
		l := &resourcev1.Link{}
		// A link is never sealed: it names values, and holds none.
		if r.Spec == nil || proto.Unmarshal(r.Spec, l) != nil || l.GetDatabaseName() == "" || l.GetMeetsEnvironment() == "" || l.GetMeetsEnvironment() == env {
			continue
		}
		if uses != nil {
			sets := make([]string, 0, len(l.GetSets()))
			for _, s := range l.GetSets() {
				sets = append(sets, s.GetName())
			}
			if !uses(sets) {
				continue
			}
		}
		svc, err := tx.GetResource(ctx, "service", l.GetMeetsEnvironment(), l.GetService())
		if errors.Is(err, store.ErrNotFound) || err == nil && svc.Spec == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		spec, err := keeper.Decode(ctx, tx, store.InRevision(svc.ID, svc.Kind, svc.Revision), svc.Spec)
		if err != nil {
			return nil, err
		}
		s, _ := spec.(*resourcev1.Service)
		if vol := database.DataVolume(s); vol != "" {
			out = append(out, revision.OneBackup{
				Volume:   resource.Ref{Kind: "volume", Scope: l.GetMeetsEnvironment(), Name: vol},
				Database: l.GetDatabaseName(),
				For:      resource.Ref{Kind: "link", Scope: env, Name: r.Name},
			})
		}
	}
	return out, nil
}

// UsedBy reports a service that reads one of names, as a variable or a secret.
func UsedBy(s *resourcev1.Service) func(names []string) bool {
	return func(names []string) bool {
		for _, n := range s.GetVariables() {
			if slices.Contains(names, n) {
				return true
			}
		}
		for _, n := range s.GetSecrets() {
			if slices.Contains(names, n) {
				return true
			}
		}
		return false
	}
}
