// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// serviceIDs finds a service's ID from the Compose project and name its
// containers carry, for the figures each sample says a container uses (#797). A
// project is its environment's ID in lowercase (docker.Project), and an ID's
// letters are uppercase after its prefix, so the environment's ID is the
// project's with them raised. What it finds it keeps, as a service's ID never
// changes. What it doesn't find it remembers for missFor, so a container no
// service is placed for, such as a deleted environment's left behind, costs a
// store read a minute, not one a sample (#848's re-check, L-B), and a service
// made after it still resolves within the minute.
type serviceIDs struct {
	db *store.DB
	// now is the clock, for a test; nil is time.Now.
	now func() time.Time

	mu     sync.Mutex
	found  map[string]string
	missed map[string]time.Time // until when each miss holds
}

// missFor is how long a miss is remembered.
const missFor = time.Minute

func (s *serviceIDs) find(ctx context.Context, project, name string) string {
	key := project + "/" + name
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.mu.Lock()
	id, ok := s.found[key]
	until, missed := s.missed[key]
	s.mu.Unlock()
	if ok {
		return id
	}
	if missed && now().Before(until) {
		return ""
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, store.ErrNotFound) {
		if s.missed == nil {
			s.missed = map[string]time.Time{}
		}
		// Those that have run out go as one is added, so the map holds no more
		// than what was missed within the minute.
		at := now()
		for k, until := range s.missed {
			if !at.Before(until) {
				delete(s.missed, k)
			}
		}
		s.missed[key] = at.Add(missFor)
		return ""
	}
	if err != nil {
		return ""
	}
	if s.found == nil {
		s.found = map[string]string{}
	}
	s.found[key] = r.ID
	delete(s.missed, key)
	return r.ID
}
