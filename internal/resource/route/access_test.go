// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route/routehash"
)

// aHash has the shape of a bcrypt hash at cost 10, as htpasswd -B -C 10 makes one,
// and is of no password: only its shape counts here.
const aHash = "$2y$10$Q9N3oVQX2b9R5o3b0sVhVeL7m5Gz0mJ8y8dD1p6g9xY2b3c4d5e6f"

func private(access *resourcev1.RouteAccess) *resourcev1.Route {
	r := spec("app.example.com", "")
	r.Access = access
	return r
}

func hashing() *Driver { return &Driver{Hash: routehash.Hash} }

// Each way a route's access can't be, refused with what to do instead.
func TestValidateRefusesWhatCantKeepAVisitorOut(t *testing.T) {
	behind := func(proxies ...string) *resourcev1.Route {
		r := private(&resourcev1.RouteAccess{Allow: []string{"203.0.113.7"}})
		r.Tls, r.TrustedProxies = "off", proxies
		return r
	}
	users := func(n int) []*resourcev1.RouteUser {
		out := make([]*resourcev1.RouteUser, n)
		for i := range out {
			out[i] = &resourcev1.RouteUser{Name: "u" + strings.Repeat("x", i), PasswordHash: aHash}
		}
		return out
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = "198.51.100." + string(rune('0'+i%10)) + string(rune('0'+i/10))
	}
	for _, tc := range []struct {
		name string
		r    *resourcev1.Route
		want string
	}{
		{"cloudflare's word", behind("cloudflare"), "Cloudflare Worker"},
		{"only this server", behind(), "trusted_proxies first"},
		{"everyone", private(&resourcev1.RouteAccess{Allow: []string{"0.0.0.0/0"}}), "isn't a list"},
		{"everyone in IPv6", private(&resourcev1.RouteAccess{Allow: []string{"::/0"}}), "isn't a list"},
		{"half the internet", private(&resourcev1.RouteAccess{Allow: []string{"128.0.0.0/1"}}), "isn't a list"},
		{"IPv4 in IPv6", private(&resourcev1.RouteAccess{Allow: []string{"::ffff:203.0.113.7"}}), "203.0.113.7/32"},
		{"IPv4's IPv6 range", private(&resourcev1.RouteAccess{Allow: []string{"::ffff:0:0/96"}}), "IPv4"},
		{"no address", private(&resourcev1.RouteAccess{Allow: []string{"0.0.0.0"}}), "no address"},
		{"not an address", private(&resourcev1.RouteAccess{Allow: []string{"{http.request.remote.host}"}}), "isn't an address"},
		{"an interface", private(&resourcev1.RouteAccess{Allow: []string{"fe80::1%eth0"}}), "network interface"},
		{"too many", private(&resourcev1.RouteAccess{Allow: many}), "at most 64"},
		{"a user named twice", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: aHash}, {Name: "ci", PasswordHash: aHash}}}), "named twice"},
		{"a colon", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci:x", PasswordHash: aHash}}}), `has a ':'`},
		{"no name", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{PasswordHash: aHash}}}), "needs a name"},
		{"a password and a hash", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", Password: "correct horse battery", PasswordHash: aHash}}}), "Give one of them"},
		{"a short password", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", Password: "hunter2"}}}), "at least 12"},
		{"a password bcrypt cuts", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", Password: strings.Repeat("a", 73)}}}), "72 bytes"},
		{"a line break", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", Password: "correct horse\nbattery"}}}), "line break"},
		{"not a hash", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: "{SHA}abc"}}}), "isn't a bcrypt hash"},
		{"a cheap hash", private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: strings.Replace(aHash, "$10$", "$05$", 1)}}}), "cost from 10 to 12"},
		{"too many users", private(&resourcev1.RouteAccess{Users: users(21)}), "at most 20 users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := hashing().Validate(tc.r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// A list comes out as the proxy reads it back, a password as its hash, and access
// that lets in everyone as none at all.
func TestValidateKeepsAccessAsTheProxyHasIt(t *testing.T) {
	r := private(&resourcev1.RouteAccess{
		Allow: []string{"2001:db8:0:0:1::7/64", "203.0.113.7", "198.51.100.9/24", "203.0.113.7/32"},
		Users: []*resourcev1.RouteUser{{Name: "ci", Password: "correct horse battery"}, {Name: "ops.team_1", PasswordHash: aHash}},
	})
	if err := hashing().Validate(r); err != nil {
		t.Fatal(err)
	}
	if want := []string{"198.51.100.0/24", "2001:db8::/64", "203.0.113.7/32"}; !slices.Equal(r.GetAccess().GetAllow(), want) {
		t.Errorf("allow = %v, want %v", r.GetAccess().GetAllow(), want)
	}
	ci := r.GetAccess().GetUsers()[0]
	if ci.GetPassword() != "" {
		t.Error("the password stays in the spec")
	}
	if bcrypt.CompareHashAndPassword([]byte(ci.GetPasswordHash()), []byte("correct horse battery")) != nil {
		t.Errorf("the hash %q isn't of the password", ci.GetPasswordHash())
	}
	if cost, _ := bcrypt.Cost([]byte(ci.GetPasswordHash())); cost != routehash.Cost {
		t.Errorf("cost = %d, want %d", cost, routehash.Cost)
	}
	if r.GetAccess().GetUsers()[1].GetPasswordHash() != aHash {
		t.Error("a hash given as it is isn't kept as it is")
	}
	open := private(&resourcev1.RouteAccess{Allow: []string{}})
	if err := hashing().Validate(open); err != nil || open.GetAccess() != nil {
		t.Errorf("access that lets in everyone reads %v, %v; want none", open.GetAccess(), err)
	}
	// A route that answers anyone over plain HTTP sees each visitor as themselves, as
	// does one that names its tunnel.
	for _, r := range []*resourcev1.Route{
		{Domain: "app.example.com", Service: "web", Port: 80, Tls: "off", PlainForAnyone: true, Access: &resourcev1.RouteAccess{Allow: []string{"192.168.1.0/24"}}},
		{Domain: "app.example.com", Service: "web", Port: 80, Tls: "off", TrustedProxies: []string{"127.0.0.1"}, Access: &resourcev1.RouteAccess{Allow: []string{"203.0.113.7"}}},
	} {
		if err := hashing().Validate(r); err != nil {
			t.Error(err)
		}
	}
}

// The agent never hashes: a password reaches it only as a hash.
func TestOnlyTheControlPlaneHashesAPassword(t *testing.T) {
	r := private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", Password: "correct horse battery"}}})
	if err := (&Driver{}).Validate(r); err == nil || !strings.Contains(err.Error(), "only the control plane") {
		t.Errorf("err = %v", err)
	}
}

// An edit that names a user with no password keeps the user's hash, so a client that
// can't read hashes back loses nothing; one new to the route needs a password.
func TestKeepHoldsOnToEachUsersHash(t *testing.T) {
	was := private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: aHash}, {Name: "ops", PasswordHash: aHash}}})
	is := private(&resourcev1.RouteAccess{Allow: []string{"203.0.113.7/32"}, Users: []*resourcev1.RouteUser{{Name: "ci"}}})
	kept, err := hashing().Keep(was, is)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetAccess().GetUsers()[0].GetPasswordHash() != aHash {
		t.Error("ci's hash isn't kept")
	}
	if kept.Reason != "kept_password" || !strings.Contains(kept.Words, "ci") || !strings.Contains(kept.Stays, "Give ci a new password to move it") {
		t.Errorf("kept = %+v", kept)
	}
	newcomer := private(&resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "eve"}}})
	if _, err := hashing().Keep(was, newcomer); err == nil || !strings.Contains(err.Error(), "needs a password") {
		t.Errorf("a new user with no password: %v", err)
	}
	if err := hashing().CheckNew(newcomer); err == nil {
		t.Error("a new route's user with no password is taken")
	}
	if kept, err := hashing().Keep(was, private(nil)); err != nil || kept.Reason != "" {
		t.Errorf("taking the users off keeps %+v, %v", kept, err)
	}
}

// What a person approves, and what a token may do: keep out more.
func TestOpensSaysEachWayInThatWasntThere(t *testing.T) {
	other := strings.Replace(aHash, "Q9N3", "R0N3", 1)
	listed := func(allow ...string) *resourcev1.Route {
		return private(&resourcev1.RouteAccess{Allow: allow})
	}
	users := func(us ...*resourcev1.RouteUser) *resourcev1.Route {
		return private(&resourcev1.RouteAccess{Users: us})
	}
	ci := &resourcev1.RouteUser{Name: "ci", PasswordHash: aHash}
	for _, tc := range []struct {
		name     string
		was, now *resourcev1.Route
		want     []string
	}{
		{"the list taken off", listed("203.0.113.0/24"), spec("app.example.com", ""), []string{"list-off"}},
		{"an address added", listed("203.0.113.0/24"), listed("203.0.113.0/24", "198.51.100.7/32"), []string{"allow:198.51.100.7/32"}},
		{"widened", listed("203.0.113.0/25"), listed("203.0.113.0/24"), []string{"allow:203.0.113.0/24"}},
		{"narrowed", listed("203.0.113.0/24"), listed("203.0.113.7/32"), nil},
		{"a list added", spec("app.example.com", ""), listed("203.0.113.7/32"), nil},
		{"the passwords taken off", users(ci), spec("app.example.com", ""), []string{"users-off"}},
		{"a user added", users(ci), users(ci, &resourcev1.RouteUser{Name: "eve", PasswordHash: aHash}), []string{"user:eve"}},
		{"a password changed", users(ci), users(&resourcev1.RouteUser{Name: "ci", PasswordHash: other}), []string{"password:ci"}},
		{"a user taken off", users(ci, &resourcev1.RouteUser{Name: "eve", PasswordHash: aHash}), users(ci), nil},
		{"users added", listed("203.0.113.7/32"), private(&resourcev1.RouteAccess{Allow: []string{"203.0.113.7/32"}, Users: []*resourcev1.RouteUser{ci}}), nil},
		{"the same", users(ci), users(ci), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Driver{}).Opens(tc.was, tc.now); !slices.Equal(got, tc.want) {
				t.Errorf("Opens = %q, want %q", got, tc.want)
			}
		})
	}
}

// Routes hold users' password hashes, so they're sealed, and one stored before they
// could reads as it is (secrets.Keeper). What reads routes directly opens them
// through the keeper: ListDomains, a website's place, and the panel's domain check.
func TestARouteIsSealedSinceItHeldPasswords(t *testing.T) {
	d := &Driver{}
	if !resource.Sealed(d) {
		t.Error("a route isn't sealed, and its users' hashes would be kept in the clear")
	}
	if _, ok := any(d).(resource.SealedLater); !ok {
		t.Error("a route stored before 0087 would be refused")
	}
}

// A password needs HTTPS where the browser sends it: with a certificate here, or a
// tunnel or proxy in front that the route names, Cloudflare's among them. A route
// that answers anyone over plain HTTP, or names nothing in front, is refused one,
// with the fix.
func TestAPasswordNeedsHTTPSAtTheBrowser(t *testing.T) {
	users := &resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: aHash}}}
	plain := func(anyone bool, proxies ...string) *resourcev1.Route {
		return &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 80, Tls: "off", PlainForAnyone: anyone, TrustedProxies: proxies, Access: users}
	}
	for name, r := range map[string]*resourcev1.Route{
		"anyone over plain HTTP":       plain(true),
		"anyone, beside a named proxy": plain(true, "127.0.0.1"),
		"nothing named in front of it": plain(false),
	} {
		if err := hashing().Validate(r); err == nil || !strings.Contains(err.Error(), "Turn on HTTPS for this domain") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, r := range map[string]*resourcev1.Route{
		"a certificate here": private(users),
		"a named tunnel":     plain(false, "127.0.0.1"),
		"Cloudflare's edge":  plain(false, "cloudflare"),
	} {
		if err := hashing().Validate(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
