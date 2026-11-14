// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
)

// Limits on a route's users (ADR 0087).
const (
	// MaxUsers is more than a team shares a password among, past which signing in
	// with a provider is the way.
	MaxUsers = 20
	// maxUserName is as long as a user's name may be.
	maxUserName = 64
	// MinPassword is the shortest password a route takes. Nothing limits guesses at
	// one, so a short one is a matter of time. The panel makes 24 characters.
	MinPassword = 12
	// maxPassword is what bcrypt reads of a password: it ignores the rest, so a
	// longer one would let in whatever starts the same.
	maxPassword = 72
)

// Hasher makes the bcrypt hash a route keeps of a password, on the control plane.
// The agent links no bcrypt, and never sees a password.
type Hasher func(password string) (string, error)

var (
	_ resource.Keeper      = (*Driver)(nil)
	_ resource.NewChecker  = (*Driver)(nil)
	_ resource.Opener      = (*Driver)(nil)
	_ resource.SealedLater = (*Driver)(nil)
)

// SealedLater says routes were kept in the clear until they held users' password
// hashes, so one stored before then reads as it is.
func (*Driver) SealedLater() {}

// checkAccess checks who a route lets in and normalizes it: the list canonical, a
// password replaced with its hash, and access that lets in everyone left out, so it
// compares equal to a route that names none.
func (d *Driver) checkAccess(r *resourcev1.Route) error {
	a := r.GetAccess()
	if a == nil {
		return nil
	}
	allow, err := ingress.NormalizeAllow(a.GetAllow())
	if err != nil {
		return err
	}
	a.Allow = allow
	if len(allow) == 0 {
		a.Allow = nil
	}
	if len(allow) > 0 {
		if err := checkListClient(r); err != nil {
			return err
		}
	}
	if err := d.checkUsers(r.GetDomain(), a.GetUsers()); err != nil {
		return err
	}
	if err := checkSignIn(r); err != nil {
		return err
	}
	if len(a.GetUsers()) > 0 && PlainAtTheBrowser(r) {
		return fmt.Errorf("%s is reached over plain HTTP, so its users' passwords would cross the network in the clear. Turn on HTTPS for this domain, or name the tunnel or proxy in front of it, whose edge serves HTTPS", Where(r))
	}
	if len(a.GetAllow())+len(a.GetUsers()) == 0 && a.GetSignIn() == nil {
		r.Access = nil
	}
	return nil
}

// PlainAtTheBrowser reports a route a browser may reach over plain HTTP: one that
// takes no certificate here and answers anyone, or names no tunnel or proxy in
// front, whose edge would serve HTTPS. A password needs HTTPS where the browser
// sends it, as ADR 0069 said and signing in with a provider does (ADR 0087).
func PlainAtTheBrowser(r *resourcev1.Route) bool {
	return r.GetTls() == ingress.TLSOff && (r.GetPlainForAnyone() || len(r.GetTrustedProxies()) == 0)
}

// checkListClient refuses a list on a route whose client it can't tell apart: one
// that believes Cloudflare's word, which anyone with a Worker can write, and one
// that only this server reaches, where every visitor is the tunnel in front.
func checkListClient(r *resourcev1.Route) error {
	if slices.Contains(r.GetTrustedProxies(), ingress.PresetCloudflare) {
		return fmt.Errorf("%s believes Cloudflare on who its visitors are, and anyone with a Cloudflare Worker can write that, so a list of addresses can't keep them out. Ask for a password instead, or name your tunnel's own address in trusted_proxies in place of %s", Where(r), ingress.PresetCloudflare)
	}
	if r.GetTls() == ingress.TLSOff && len(r.GetTrustedProxies()) == 0 && !r.GetPlainForAnyone() {
		return fmt.Errorf("%s answers only this server, as for a tunnel on it, so every visitor comes from the tunnel's address and a list of addresses can't tell them apart. Name the tunnel's address in trusted_proxies first, such as 127.0.0.1, and its visitors read as themselves", Where(r))
	}
	return nil
}

// checkUsers checks a route's users, and hashes each password it's given.
func (d *Driver) checkUsers(domain string, users []*resourcev1.RouteUser) error {
	if len(users) > MaxUsers {
		return fmt.Errorf("a route takes at most %d users, and this one names %d. Share one among those who need it, or leave some out", MaxUsers, len(users))
	}
	seen := map[string]bool{}
	for _, u := range users {
		if err := checkUserName(u.GetName()); err != nil {
			return err
		}
		if seen[u.GetName()] {
			return fmt.Errorf("the user %s is named twice. Name each once", u.GetName())
		}
		seen[u.GetName()] = true
		switch pw, hash := u.GetPassword(), u.GetPasswordHash(); {
		case pw != "" && hash != "":
			return fmt.Errorf("the user %s has a password and a hash. Give one of them", u.GetName())
		case pw != "":
			if err := checkPassword(u.GetName(), pw); err != nil {
				return err
			}
			if d.Hash == nil {
				return fmt.Errorf("the user %s has a password, and only the control plane hashes one. Propose the route there", u.GetName())
			}
			h, err := d.Hash(pw)
			if err != nil {
				return fmt.Errorf("can't hash the password of %s: %w", u.GetName(), err)
			}
			u.Password, u.PasswordHash = "", h
		case hash != "" && !ingress.IsPasswordHash(hash):
			return fmt.Errorf("the hash given for %s isn't a bcrypt hash at a cost from 10 to 12, as htpasswd -B -C 10 makes. Give the password instead, and %s hashes it", u.GetName(), domain)
		}
	}
	return nil
}

// checkUserName takes what Basic authentication can carry and a person can type:
// letters, digits, dots, dashes and underscores. A colon would end the name early.
func checkUserName(name string) error {
	switch {
	case name == "":
		return errors.New("a route's user needs a name, such as ci")
	case len(name) > maxUserName:
		return fmt.Errorf("the user name %s… is longer than %d characters. Use a shorter one", name[:20], maxUserName)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return fmt.Errorf("the user name %q has a %q, and a name takes only letters, digits, dots, dashes and underscores, such as ci", name, r)
		}
	}
	return nil
}

// checkPassword refuses a password that's easy to guess by length, or that bcrypt
// would read only the start of.
func checkPassword(name, pw string) error {
	switch {
	case len(pw) < MinPassword:
		return fmt.Errorf("the password for %s has %d characters, and nothing limits how often someone can guess at it. Use at least %d, or let the panel make one", name, len(pw), MinPassword)
	case len(pw) > maxPassword:
		return fmt.Errorf("the password for %s is longer than the %d bytes bcrypt reads, so whatever starts the same would get in. Use a shorter one", name, maxPassword)
	case strings.ContainsAny(pw, "\x00\r\n"):
		return fmt.Errorf("the password for %s has a line break or a zero byte, which a browser can't send. Use another", name)
	}
	return nil
}

// Keep holds on to each user's hash when an edit names the user with no password,
// so the panel and the CLI, which can't read hashes back, send the whole route and
// lose nothing (ADR 0087). A user the route hasn't got needs a password.
func (*Driver) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.Route)
	is, same := desired.(*resourcev1.Route)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	var kept []string
	for _, u := range is.GetAccess().GetUsers() {
		if u.GetPasswordHash() != "" {
			continue
		}
		i := slices.IndexFunc(was.GetAccess().GetUsers(), func(o *resourcev1.RouteUser) bool { return o.GetName() == u.GetName() })
		if i < 0 {
			return resource.Kept{}, fmt.Errorf("the user %s is new to %s, so it needs a password", u.GetName(), Where(is))
		}
		u.PasswordHash = was.GetAccess().GetUsers()[i].GetPasswordHash()
		kept = append(kept, u.GetName())
	}
	if len(kept) == 0 {
		return resource.Kept{}, nil
	}
	// A plan sends the spec to each server it would run on as it's made, so what was
	// kept would reach a new server on a proposal alone (ADR 0022).
	return resource.Kept{
		Reason: "kept_password", Words: "keeps the password of " + joinNames(kept) + ", as the edit gave none",
		Stays: "its users' passwords stay on the server it runs on. Give " + joinNames(kept) + " a new password to move it",
	}, nil
}

// CheckNew asks a new route's users for a password, which Keep can't fill in.
func (*Driver) CheckNew(spec proto.Message) error {
	r := spec.(*resourcev1.Route) //nolint:forcetypeassert // Validate saw it
	for _, u := range r.GetAccess().GetUsers() {
		if u.GetPasswordHash() == "" {
			return fmt.Errorf("the user %s needs a password", u.GetName())
		}
	}
	return nil
}

// Opens says how desired lets in someone was kept out: an address its list didn't
// cover, the list taken off, a user it didn't have, a password changed, or the
// passwords taken off. What a token may do is keep out more (ADR 0087).
func (*Driver) Opens(was, desired proto.Message) []string {
	from, to := was.(*resourcev1.Route).GetAccess(), desired.(*resourcev1.Route).GetAccess()
	var out []string
	if allow := from.GetAllow(); len(allow) > 0 {
		if len(to.GetAllow()) == 0 {
			out = append(out, resource.OpensListOff)
		}
		for _, p := range to.GetAllow() {
			if !ingress.Covers(allow, p) {
				out = append(out, resource.OpensAllow+p)
			}
		}
	}
	if users := from.GetUsers(); len(users) > 0 {
		if len(to.GetUsers()) == 0 {
			out = append(out, resource.OpensUsersOff)
		}
		for _, u := range to.GetUsers() {
			i := slices.IndexFunc(users, func(o *resourcev1.RouteUser) bool { return o.GetName() == u.GetName() })
			switch {
			case i < 0:
				out = append(out, resource.OpensUser+u.GetName())
			case users[i].GetPasswordHash() != u.GetPasswordHash():
				out = append(out, resource.OpensPassword+u.GetName())
			}
		}
	}
	return append(out, signInOpens(from, to)...)
}

// KeepsOut reports a route with a list, users or sign-in.
func (*Driver) KeepsOut(spec proto.Message) bool {
	a := spec.(*resourcev1.Route).GetAccess()
	return len(a.GetAllow())+len(a.GetUsers()) > 0 || a.GetSignIn() != nil
}

// joinNames lists names as people read them: "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
