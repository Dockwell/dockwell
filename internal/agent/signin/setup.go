// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// SetupPath is where under a held route's path its setup link answers (ADR 0105).
const SetupPath = "/.well-known/dockwell-setup"

// maxSetupFor bounds how long a setup link works, from when this agent is given it,
// whatever a route says, and setupSkew is the leeway its end is read with, so a
// clock a little apart from the control plane's neither ends it early nor keeps it
// longer than that (the review's M3).
const (
	maxSetupFor = time.Hour
	setupSkew   = time.Minute
)

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

// PutSetup keeps a held route's setup link, at its domain and path. Its end is the
// one the control plane set, held to an hour from now, once, so it's a fixed time
// every cookie is checked against.
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
	until := time.Unix(s.GetUntil(), 0)
	if most := k.now().Add(maxSetupFor); until.After(most) {
		until = most
	}
	k.setups[id] = keptSetup{spec: proto.CloneOf(s), setup: Setup{
		ID: id, Domain: domain, Path: path, KeyHash: strings.ToLower(s.GetKeyHash()), Until: until, Key: key,
	}}
	return nil
}

// now is the keep's clock.
func (k *Keep) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// SetupsOn are the held routes of a domain whose setup link has a key's hash: one
// key opens them all (the review's M5).
func (k *Keep) SetupsOn(domain, keyHash string) []Setup {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []Setup
	for _, s := range k.setups {
		if strings.EqualFold(s.setup.Domain, domain) && s.setup.KeyHash == keyHash {
			out = append(out, s.setup)
		}
	}
	slices.SortFunc(out, func(a, b Setup) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// UseSetupKey says whether a setup link's key, by its hash, is used for the first
// time, and keeps that it's used until the link ends, in Dir, beside the routes'
// cookie keys, so a key works once though the agent starts again (the review's M1).
// With no Dir, it keeps it in memory.
func (k *Keep) UseSetupKey(hash string, until time.Time) (bool, error) {
	now := k.now()
	if k.Dir == "" {
		k.mu.Lock()
		defer k.mu.Unlock()
		for h, end := range k.used {
			if !now.Before(end) {
				delete(k.used, h)
			}
		}
		if _, seen := k.used[hash]; seen {
			return false, nil
		}
		if k.used == nil {
			k.used = map[string]time.Time{}
		}
		k.used[hash] = until
		return true, nil
	}
	dir := filepath.Join(k.Dir, "setup-used")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("can't keep which setup links were used: %w", err)
	}
	// What's past its link's end goes.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // Dir and a name it wrote
			if end, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && perr == nil && !now.Before(time.Unix(end, 0).Add(setupSkew)) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(hash) != hex.EncodedLen(sha256.Size) {
		return false, errors.New("a setup link's key hash isn't a SHA-256")
	}
	f, err := os.OpenFile(filepath.Join(dir, hash), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a hex hash in Dir
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("can't keep that a setup link was used: %w", err)
	}
	_, werr := f.WriteString(strconv.FormatInt(until.Unix(), 10))
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return false, fmt.Errorf("can't keep that a setup link was used: %w", err)
	}
	return true, nil
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
	if !ok || !now.Before(st.Until.Add(setupSkew)) {
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
		if key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(st.KeyHash)) != 1 {
			end{
				Status: http.StatusForbidden, Title: "This setup link doesn't work",
				Said: "It's for another link, or it ran out. Make a new one from the app's page in Dockwell.",
			}.write(w)
			return
		}
		first, err := s.use(got, st.Until, now)
		if err != nil {
			end{Status: http.StatusBadGateway, Title: "This setup link can't be used here", Said: sentence(err)}.write(w)
			return
		}
		if !first {
			end{
				Status: http.StatusForbidden, Title: "This setup link was used already",
				Said: "A link works once. Make a new one from the app's page in Dockwell.",
			}.write(w)
			return
		}
		// One cookie for each held route of the domain the key is for, path routes
		// included, as one link sets the app up (the review's M5). Strict, as the page
		// that goes on is the site's own, so the next request carries them.
		for _, each := range s.setupsOn(st) {
			value, err := seal(each.Key, 0, "setup", setupCookie{Route: each.ID, Domain: each.Domain, Until: each.Until})
			if err != nil {
				end{Status: http.StatusBadGateway, Title: "This setup link can't be used here", Said: sentence(err)}.write(w)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: SetupName(each.ID), Value: value, Path: "/", MaxAge: max(int(each.Until.Add(setupSkew).Sub(now).Seconds()), 1),
				Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
		}
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
		if open(st.Key, 0, "setup", c.Value, &got) == nil && got.Route == id && got.Domain == st.Domain && got.Until.Equal(st.Until) && now.Before(got.Until.Add(setupSkew)) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	notOpen(w, r)
}

// setupOf is a route's setup link, as its keep has it, its end fixed when it came.
func (s *Server) setupOf(id string) (Setup, bool) {
	if s.Setups == nil {
		return Setup{}, false
	}
	return s.Setups(id)
}

// setupsOn are the held routes a setup link's key opens: those of its domain with
// its hash, or the one route where nothing says more.
func (s *Server) setupsOn(st Setup) []Setup {
	if s.SetupsOn == nil {
		return []Setup{st}
	}
	if all := s.SetupsOn(st.Domain, st.KeyHash); len(all) > 0 {
		return all
	}
	return []Setup{st}
}

// use says whether a key, by its hash, opens for the first time, through UseSetup,
// which keeps that past a restart, or in this server's memory where nothing does.
func (s *Server) use(hash string, until, now time.Time) (bool, error) {
	if s.UseSetup != nil {
		return s.UseSetup(hash, until)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, end := range s.used {
		if !now.Before(end) {
			delete(s.used, h)
		}
	}
	if _, seen := s.used[hash]; seen {
		return false, nil
	}
	if s.used == nil {
		s.used = map[string]time.Time{}
	}
	s.used[hash] = until
	return true, nil
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
