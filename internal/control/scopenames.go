// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"

	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// scopeNames says what people call an environment, as its project's name and
// its own, "default.production", which its services' containers are named
// after on the servers, so docker ps says what each is (ADR 0035). It reads
// both each time, so a rename shows in the next container a deploy makes.
type scopeNames struct{ db *store.DB }

func (n scopeNames) ScopeName(ctx context.Context, id string) string {
	var name string
	err := n.db.View(ctx, func(tx *store.Tx) error {
		env, err := tx.GetResourceByID(ctx, id)
		if err != nil || env.Kind != string(scope.EnvironmentKind) {
			return err
		}
		project, err := tx.GetResourceByID(ctx, env.Scope)
		if err == nil && project.Kind == string(scope.ProjectKind) {
			name = project.Name + "." + env.Name
		}
		return err
	})
	if err != nil {
		// A container is named as Compose would name it, which is no worse
		// than before, and the next deploy names it again.
		return ""
	}
	return name
}
