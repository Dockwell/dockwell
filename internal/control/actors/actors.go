// SPDX-License-Identifier: AGPL-3.0-only

// Package actors turns the store's names for actors, such as "user:usr_…"
// or "cli:mira@hz-1", into the API's Actor, and back. ChangeService and
// EventService share it.
package actors

import (
	"context"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// How the store names actors: a kind, a colon and an ID, or a word alone.
const (
	user      = "user:"
	token     = "token:"
	server    = "server:"
	shell     = "cli:" // someone in a shell on the control server, as "cli:mira@hz-1"
	anonymous = "anonymous:"
	// Dockwell is "dockwell" in the revision store, and "system" in the
	// audit log's older events.
	dockwell = "dockwell"
	system   = "system"
)

// Fresh is how long Names keeps a name it read before reading it again,
// so a watch that runs for hours shows a server's new name within a
// minute, without reading every name on every poll.
const Fresh = time.Minute

// Names finds the names to show for actors, and for what events are
// about: users, API tokens, servers and passkeys, by ID. It keeps what it
// reads for Fresh. One Names serves one request or one watch, never two
// at once.
type Names struct {
	seen map[string]read
	now  func() time.Time
}

// read is a name, what its object lives in, and when it was read.
type read struct {
	name, scope string
	at          time.Time
}

// New returns empty Names.
func New() *Names {
	return &Names{seen: map[string]read{}, now: time.Now}
}

// In names with tx: it reads the names it doesn't have, or has had for
// longer than Fresh, in tx.
func (n *Names) In(ctx context.Context, tx *store.Tx) Lookup {
	return Lookup{n: n, ctx: ctx, tx: tx}
}

// Lookup names actors and IDs within one transaction.
type Lookup struct {
	n   *Names
	ctx context.Context //nolint:containedctx // lives for one transaction
	tx  *store.Tx
}

// Name returns the name of the user, API token, server, passkey, variable
// or secret with this ID, or "" when it is gone, has none, or is of another
// kind.
func (l Lookup) Name(objectID string) string { return l.read(objectID).name }

// Scope returns the ID of what the variable or secret with this ID lives
// in, its environment, or "" for any other ID.
func (l Lookup) Scope(objectID string) string { return l.read(objectID).scope }

func (l Lookup) read(objectID string) read {
	now := l.n.now()
	if s, ok := l.n.seen[objectID]; ok && now.Sub(s.at) < Fresh {
		return s
	}
	name, scope := "", ""
	prefix, _, _ := strings.Cut(objectID, "_")
	switch prefix {
	case id.User:
		if u, err := l.tx.GetUser(l.ctx, objectID); err == nil {
			name = u.Name
		}
	case id.Token:
		if t, err := l.tx.GetAPIToken(l.ctx, objectID); err == nil {
			name = t.Name
		}
	case id.Server:
		if s, err := l.tx.GetServer(l.ctx, objectID); err == nil {
			name = s.Name
		}
	case id.Passkey:
		if p, err := l.tx.GetPasskey(l.ctx, objectID); err == nil {
			name = p.Name
		}
	case id.Secret, id.Variable:
		if r, err := l.tx.GetResourceByID(l.ctx, objectID); err == nil {
			name, scope = r.Name, r.Scope
		}
	default:
		return read{}
	}
	r := read{name: name, scope: scope, at: now}
	l.n.seen[objectID] = r
	return r
}

// Actor returns the API's Actor for the store's name for one, or nil for
// none. A user, token or server that is gone keeps its ID as its name.
// An anonymous actor's address is never shown.
func (l Lookup) Actor(actor string) *dockwellv1.Actor {
	if actor == "" {
		return nil
	}
	a := &dockwellv1.Actor{}
	kind, rest, _ := strings.Cut(actor, ":")
	switch kind + ":" {
	case user, token, server:
		a.Kind, a.Id, a.Name = named[kind+":"], rest, rest
		if name := l.Name(rest); name != "" {
			a.Name = name
		}
	case shell:
		a.Kind, a.Id, a.Name = dockwellv1.ActorKind_ACTOR_KIND_SHELL, rest, rest
	case anonymous:
		a.Kind = dockwellv1.ActorKind_ACTOR_KIND_UNSPECIFIED
	default:
		if actor == dockwell || actor == system {
			a.Kind, a.Name = dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL, "Dockwell"
		} else {
			a.Name = actor
		}
	}
	return a
}

// The kinds of actor that have a name to read, by how the store names them.
var named = map[string]dockwellv1.ActorKind{
	user: dockwellv1.ActorKind_ACTOR_KIND_USER, token: dockwellv1.ActorKind_ACTOR_KIND_TOKEN, server: dockwellv1.ActorKind_ACTOR_KIND_SERVER,
}

// Store returns the store's name for the actor whose Actor.id is actorID:
// a user's, token's or server's ID, or a shell account's name. It reports
// false for anything else.
func Store(actorID string) (string, bool) {
	prefix, _, _ := strings.Cut(actorID, "_")
	switch {
	case prefix == id.User:
		return user + actorID, true
	case prefix == id.Token:
		return token + actorID, true
	case prefix == id.Server:
		return server + actorID, true
	case strings.Contains(actorID, "@") && !strings.ContainsAny(actorID, " :"):
		return shell + actorID, true
	}
	return "", false
}

// Prefixes returns how the store's names for actors of a kind begin; one
// without a colon is a whole name. None for an unknown kind.
func Prefixes(kind dockwellv1.ActorKind) []string {
	switch kind {
	case dockwellv1.ActorKind_ACTOR_KIND_USER:
		return []string{user}
	case dockwellv1.ActorKind_ACTOR_KIND_TOKEN:
		return []string{token}
	case dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL:
		return []string{dockwell, system}
	case dockwellv1.ActorKind_ACTOR_KIND_SERVER:
		return []string{server}
	case dockwellv1.ActorKind_ACTOR_KIND_SHELL:
		return []string{shell}
	case dockwellv1.ActorKind_ACTOR_KIND_UNSPECIFIED:
	}
	return nil
}
