// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeySize is the size of a route's cookie key, which the agent makes for each route
// and keeps in its data directory.
const KeySize = 32

// maxCookie keeps a cookie under what every browser keeps.
const maxCookie = 4000

// Session is who signed in to a route, as its cookie holds it.
type Session struct {
	Route      string `json:"r"`
	Generation uint64 `json:"g"`
	Subject    string `json:"s"`
	Email      string `json:"e"`
	// Domain is the domain the provider vouches the account is at: the email's,
	// or at Google its hd, as a Google account's address can be anyone's.
	Domain string    `json:"d,omitempty"`
	Groups []string  `json:"gr,omitempty"`
	Made   time.Time `json:"m"`
	Ends   time.Time `json:"x"`
}

// Flow is a sign-in under way, as its cookie holds it for the callback.
type Flow struct {
	Route    string    `json:"r"`
	State    string    `json:"st"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	Back     string    `json:"b"`
	Ends     time.Time `json:"x"`
}

// flowFor is how long a sign-in may take, from the redirect to the callback.
const flowFor = 10 * time.Minute

// Names are a route's cookies: __Host- so no other subdomain can set or replace
// them, named by a short hash of the route, which says nothing of it.
func Names(route string) (session, flow string) {
	sum := sha256.Sum256([]byte(route))
	h := hex.EncodeToString(sum[:6])
	return "__Host-dw_" + h, "__Host-dw_flow_" + h
}

// cookieKey is the key a route's cookies are signed with at a generation: raising it
// signs everyone out, and leaves the provider alone.
func cookieKey(routeKey []byte, generation uint64, purpose string) ([]byte, error) {
	if len(routeKey) != KeySize {
		return nil, errors.New("the route's cookie key isn't 32 bytes")
	}
	return hkdf.Key(sha256.New, routeKey, nil, fmt.Sprintf("dockwell sign-in %s, generation %d", purpose, generation), sha256.Size)
}

// seal signs v as a cookie's value: its JSON, then its HMAC, each in base64url.
func seal(routeKey []byte, generation uint64, purpose string, v any) (string, error) {
	key, err := cookieKey(routeKey, generation, purpose)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	out := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(out) > maxCookie {
		return "", errors.New("what sign-in keeps for this person is larger than a cookie holds. Name fewer groups on the route")
	}
	return out, nil
}

// open checks a cookie's signature, and reads it into v.
func open(routeKey []byte, generation uint64, purpose, value string, v any) error {
	key, err := cookieKey(routeKey, generation, purpose)
	if err != nil {
		return err
	}
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || len(value) > maxCookie {
		return errNotOurs
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return errNotOurs
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errNotOurs
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errNotOurs
	}
	if json.Unmarshal(body, v) != nil {
		return errNotOurs
	}
	return nil
}

// errNotOurs is a cookie this route didn't sign at its generation.
var errNotOurs = errors.New("the cookie isn't one this route signed")

// SealSession signs who signed in.
func SealSession(routeKey []byte, s Session) (string, error) {
	return seal(routeKey, s.Generation, "session", s)
}

// OpenSession reads a session cookie the route signed at generation, for route,
// that hasn't run out.
func OpenSession(routeKey []byte, route string, generation uint64, value string, now time.Time) (Session, error) {
	var s Session
	if err := open(routeKey, generation, "session", value, &s); err != nil {
		return Session{}, err
	}
	switch {
	case s.Route != route || s.Generation != generation:
		return Session{}, errNotOurs
	case !now.Before(s.Ends):
		return Session{}, errors.New("the sign-in has run out")
	}
	return s, nil
}

// SealFlow signs a sign-in under way.
func SealFlow(routeKey []byte, generation uint64, f Flow) (string, error) {
	return seal(routeKey, generation, "flow", f)
}

// OpenFlow reads a flow cookie the route signed, for route, that hasn't run out.
func OpenFlow(routeKey []byte, route string, generation uint64, value string, now time.Time) (Flow, error) {
	var f Flow
	if err := open(routeKey, generation, "flow", value, &f); err != nil {
		return Flow{}, err
	}
	switch {
	case f.Route != route:
		return Flow{}, errNotOurs
	case !now.Before(f.Ends):
		return Flow{}, errors.New("the sign-in took longer than 10 minutes")
	}
	return f, nil
}
