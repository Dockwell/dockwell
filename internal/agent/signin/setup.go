// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// SetupPath is where under a held route's path its setup link answers (ADR 0105).
const SetupPath = "/.well-known/dockwell-setup"

// maxSetupFor bounds how long a setup link works, whatever a route says.
const maxSetupFor = time.Hour

// Setup is a held route's setup link, as the agent keeps it: the hash of the key a
// person was given, until when it works, and the key its cookie is signed with,
// made here and never sent anywhere (ADR 0105).
type Setup struct {
	ID, Domain, Path string
	KeyHash          string
	Until            time.Time
	Key              []byte
}

// setupCookie is what a setup link's cookie holds: the route, its domain and the
// link's end, so it opens nothing else and nothing after.
type setupCookie struct {
	Route  string    `json:"r"`
	Domain string    `json:"d"`
	Until  time.Time `json:"u"`
}

// SetupName is a route's setup cookie: __Host- so no other subdomain can set or
// replace it, named by a short hash of the route, made apart from its sign-in's,
// which says nothing of it. Its name has the shape of a sign-in's, so every route's
// proxy takes it out of what goes to the app and out of what the app sends back, as
// it does theirs, with no new rule to write on every route.
func SetupName(route string) string {
	sum := sha256.Sum256([]byte("dockwell setup link " + route))
	return "__Host-dw_" + hex.EncodeToString(sum[:6])
}

// PutSetup keeps a held route's setup link, at its domain and path.
func (k *Keep) PutSetup(id, domain, path string, s *resourcev1.RouteSetup) error {
	if len(s.GetKeyHash()) != hex.EncodedLen(sha256.Size) {
		return errors.New("the route's setup link has no key hash of a SHA-256")
	}
	raw, err := k.key(id)
	if err != nil {
		return err
	}
	key, err := hkdf.Key(sha256.New, raw, nil, "dockwell setup link", KeySize)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.setups == nil {
		k.setups = map[string]keptSetup{}
	}
	k.setups[id] = keptSetup{spec: proto.CloneOf(s), setup: Setup{
		ID: id, Domain: domain, Path: path, KeyHash: strings.ToLower(s.GetKeyHash()), Until: time.Unix(s.GetUntil(), 0), Key: key,
	}}
	return nil
}

// DropSetup forgets a route's setup link, if it has one.
func (k *Keep) DropSetup(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.setups, id)
}

// GetSetup returns a route's setup link as PutSetup was given it, or nil.
func (k *Keep) GetSetup(id string) *resourcev1.RouteSetup {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.setups[id]
	if !ok {
		return nil
	}
	return proto.CloneOf(s.spec)
}

// Setup is what Server reads of a route's setup link, by its proxy ID.
func (k *Keep) Setup(id string) (Setup, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.setups[id]
	return s.setup, ok
}

// keptSetup is a setup link as PutSetup was given it, and as Server reads it.
type keptSetup struct {
	spec  *resourcev1.RouteSetup
	setup Setup
}

// serveSetup answers forward_auth for a held route with a setup link: the link's
// path, with its key, once, sets the browser's cookie and goes on to the route by
// itself; a request with that cookie goes on to the app; anything else is told the
// app isn't open yet. The cookie lifts the hold alone: a route's own access, which
// comes after, still applies.
func (s *Server) serveSetup(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.setupOf(id)
	now := s.now()
	if !ok || !now.Before(st.Until) {
		notOpen(w, r)
		return
	}
	uri := r.Header.Get("X-Forwarded-Uri")
	path, query, _ := strings.Cut(uri, "?")
	base := strings.TrimSuffix(st.Path, "/")
	name := SetupName(id)
	if path == base+SetupPath {
		q, _ := url.ParseQuery(query)
		key := q.Get("key")
		sum := sha256.Sum256([]byte(key))
		got := hex.EncodeToString(sum[:])
		if key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(st.KeyHash)) != 1 || !s.firstUse(got, st.Until, now) {
			end{
				Status: http.StatusForbidden, Title: "This setup link doesn't work",
				Said: "It was used already, or it ran out. Make a new one from the app's page in Dockwell.",
			}.write(w)
			return
		}
		value, err := seal(st.Key, 0, "setup", setupCookie{Route: id, Domain: st.Domain, Until: st.Until})
		if err != nil {
			end{Status: http.StatusBadGateway, Title: "This setup link can't be used here", Said: sentence(err)}.write(w)
			return
		}
		// Strict, as the page that goes on is the site's own, so the next request
		// carries it.
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: value, Path: "/", MaxAge: int(st.Until.Sub(now).Seconds()) + 1,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		// Never 2xx, which would send the link and its key on to the app, and never
		// a redirect: a page that goes on by itself, with no referrer.
		end{
			Status: http.StatusForbidden, Title: "Setting up " + st.Domain + base,
			Said: "Going on to the app.", Action: "Go on", Href: base + "/", Tone: "out", Goes: true,
		}.write(w)
		return
	}
	if c, err := r.Cookie(name); err == nil {
		var got setupCookie
		if open(st.Key, 0, "setup", c.Value, &got) == nil && got.Route == id && got.Domain == st.Domain && got.Until.Equal(st.Until) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	notOpen(w, r)
}

// setupOf is a route's setup link, held to an hour at most.
func (s *Server) setupOf(id string) (Setup, bool) {
	if s.Setups == nil {
		return Setup{}, false
	}
	st, ok := s.Setups(id)
	if !ok {
		return Setup{}, false
	}
	if most := s.now().Add(maxSetupFor); st.Until.After(most) {
		st.Until = most
	}
	return st, true
}

// firstUse says whether a key, by its hash, opens for the first time, and remembers
// it until the link it's for runs out, so a key works once.
func (s *Server) firstUse(hash string, until, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, end := range s.used {
		if !now.Before(end) {
			delete(s.used, h)
		}
	}
	if _, seen := s.used[hash]; seen {
		return false
	}
	if s.used == nil {
		s.used = map[string]time.Time{}
	}
	s.used[hash] = until
	return true
}

// notOpen is what a held route answers anyone without its setup cookie: the app
// isn't open yet, as a page to a browser and a line to anything else.
func notOpen(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		end{Status: http.StatusServiceUnavailable, Title: "This app isn't open yet", Said: "Its owner is setting it up.", Action: "Try again", Href: "/", Tone: "out"}.write(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(HeldBody))
}

// HeldBody is what a held route answers anything but a browser (ADR 0084).
const HeldBody = "This app isn't open yet: its owner is setting it up. Try again later.\n"
