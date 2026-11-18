// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"strings"
	"sync"

	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// serviceIDs finds a service's ID from the Compose project and name its
// containers carry, for the figures each sample says a container uses (#797). A
// project is its environment's ID in lowercase (docker.Project), and an ID's
// letters are uppercase after its prefix, so the environment's ID is the
// project's with them raised. What it finds it keeps, as a service's ID never
// changes; what it doesn't is asked again with the next sample.
type serviceIDs struct {
	db *store.DB

	mu    sync.Mutex
	found map[string]string
}

func (s *serviceIDs) find(ctx context.Context, project, name string) string {
	key := project + "/" + name
	s.mu.Lock()
	id, ok := s.found[key]
	s.mu.Unlock()
	if ok {
		return id
	}
	prefix, rest, ok := strings.Cut(project, "_")
	if !ok || name == "" {
		return ""
	}
	env := prefix + "_" + strings.ToUpper(rest)
	var r store.Resource
	err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		r, err = tx.GetResource(ctx, string(service.Kind), env, name)
		return err
	})
	if err != nil {
		return ""
	}
	s.mu.Lock()
	if s.found == nil {
		s.found = map[string]string{}
	}
	s.found[key] = r.ID
	s.mu.Unlock()
	return r.ID
}
