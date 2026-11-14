// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Keep holds the routes this server signs people in to, by their proxy IDs, as the
// ingress puts them when it applies a route, for Server to answer the proxy. Each
// route's cookie key is 32 random bytes in Dir, readable by root alone, made the
// first time the route signs anyone in here; it never leaves the server, so a route
// moved to another signs everyone out (ADR 0087).
type Keep struct {
	// Dir holds the routes' cookie keys.
	Dir string

	mu     sync.Mutex
	routes map[string]kept
}

// kept is a route as Put was given it, and as Server reads it.
type kept struct {
	spec  *resourcev1.RouteSignIn
	route Route
}

// Put keeps a route's sign-in, at its domain and path.
func (k *Keep) Put(id, domain, path string, s *resourcev1.RouteSignIn) error {
	raw, err := k.key(id)
	if err != nil {
		return err
	}
	f := s.GetFilled()
	key, err := bound(raw, f.GetIssuer(), f.GetClientId())
	if err != nil {
		return err
	}
	hours := int(s.GetSessionHours())
	if hours == 0 {
		hours = 24
	}
	r := Route{
		ID: id, Domain: domain, Path: path,
		Issuer: f.GetIssuer(), ClientID: f.GetClientId(), ClientSecret: f.GetClientSecret(),
		Scopes: slices.Clone(f.GetScopes()), GroupsClaim: f.GetGroupsClaim(), AnyAccount: f.GetAnyAccount(),
		Emails: slices.Clone(s.GetEmails()), EmailDomains: slices.Clone(s.GetEmailDomains()), Groups: slices.Clone(s.GetGroups()),
		SessionHours: hours, Generation: s.GetGeneration(), Key: key,
	}
	if r.GroupsClaim == "" {
		r.GroupsClaim = "groups"
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.routes == nil {
		k.routes = map[string]kept{}
	}
	k.routes[id] = kept{spec: proto.CloneOf(s), route: r}
	return nil
}

// Drop forgets a route's sign-in. Its key stays, so the route signing people in
// here again keeps who had signed in, until its generation says otherwise.
func (k *Keep) Drop(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.routes, id)
}

// Get returns a route's sign-in as Put was given it, or nil.
func (k *Keep) Get(id string) *resourcev1.RouteSignIn {
	k.mu.Lock()
	defer k.mu.Unlock()
	r, ok := k.routes[id]
	if !ok {
		return nil
	}
	return proto.CloneOf(r.spec)
}

// Route is what Server reads of a route, by its proxy ID.
func (k *Keep) Route(id string) (Route, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	r, ok := k.routes[id]
	return r.route, ok
}

// key reads a route's cookie key, or makes it.
func (k *Keep) key(id string) ([]byte, error) {
	if k.Dir == "" {
		return nil, errors.New("this agent has nowhere to keep a route's cookie key")
	}
	sum := sha256.Sum256([]byte(id))
	path := filepath.Join(k.Dir, hex.EncodeToString(sum[:12])+".key")
	b, err := os.ReadFile(path) //nolint:gosec // Dir and a hash of the route's ID, nothing anyone sends
	switch {
	case err == nil && len(b) == KeySize:
		return b, nil
	case err == nil:
		return nil, fmt.Errorf("the cookie key at %s isn't %d bytes. Remove it, which signs everyone out of the route, and apply the route again", path, KeySize)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("can't read a route's cookie key: %w", err)
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("can't make the folder for routes' cookie keys: %w", err)
	}
	b = make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	// Written whole or not at all, so a crash leaves no short key behind.
	tmp, err := os.CreateTemp(k.Dir, ".key-*")
	if err != nil {
		return nil, fmt.Errorf("can't keep a route's cookie key: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("can't keep a route's cookie key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("can't keep a route's cookie key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("can't keep a route's cookie key: %w", err)
	}
	// Another Put made it first: that one counts.
	if err := os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		return k.key(id)
	} else if err != nil {
		return nil, fmt.Errorf("can't keep a route's cookie key: %w", err)
	}
	return b, nil
}

// bound is a route's cookie key at an issuer and a client: another issuer or client
// signs everyone out, as its accounts are another's to say (#598's review, L5).
func bound(key []byte, issuer, client string) ([]byte, error) {
	return hkdf.Key(sha256.New, key, nil, "dockwell sign-in at "+issuer+" as "+client, KeySize)
}
