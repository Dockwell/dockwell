// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store/storetest"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
)

func TestTokenSecretsHaveTheirShape(t *testing.T) {
	scanner := regexp.MustCompile(`^dwt_[0-9A-Za-z]{49}$`)
	seen := map[string]bool{}
	for range 200 {
		secret, hash := newTokenSecret()
		if !scanner.MatchString(secret) || len(secret) != TokenLen || TokenLen != 53 {
			t.Fatalf("secret %q doesn't match the pattern secret scanners look for", secret)
		}
		if !WellFormedToken(secret) {
			t.Fatalf("WellFormedToken(%q) = false", secret)
		}
		if seen[secret] {
			t.Fatalf("secret %q made twice", secret)
		}
		seen[secret] = true
		if !bytes.Equal(hash, hashToken(secret)) {
			t.Fatal("the hash isn't the secret's")
		}
		// The hint shows random digits only, the first 4 and the last 4,
		// never the checksum after them.
		if hint := TokenHint(secret); hint != secret[:8]+"…"+secret[43:47] {
			t.Fatalf("hint %q for %q, want the prefix and the first and last 4 random digits", hint, secret)
		}
	}
	good, _ := newTokenSecret()
	for _, bad := range []string{
		"",
		good[:52],
		good + "0",
		"dwx_" + good[4:],
		good[:20] + "-" + good[21:],            // not base62
		good[:20] + flip(good[20]) + good[21:], // a typo in the random part
		good[:50] + flip(good[50]) + good[51:], // a typo in the checksum
		strings.ToUpper(good[:4]) + good[4:],   // DWT_
		"Bearer " + good,
	} {
		if WellFormedToken(bad) {
			t.Errorf("WellFormedToken(%q) = true", bad)
		}
	}
}

// flip changes one base62 digit to another.
func flip(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

func (f *fixture) token(t *testing.T, p Principal, scope string, expires *time.Time) NewToken {
	t.Helper()
	made, err := f.svc.CreateToken(t.Context(), p, "GitHub Actions deploy", scope, expires)
	if err != nil {
		t.Fatal(err)
	}
	return made
}

func TestTokenScopes(t *testing.T) { storetest.Each(t, testTokenScopes) }

func testTokenScopes(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	read := f.token(t, owner, store.ScopeReadOnly, nil)
	write := f.token(t, owner, store.ScopeReadWrite, nil)
	for _, tt := range []struct {
		name   string
		secret string
		need   Access
		want   error
	}{
		{"read-only reads", read.Secret, AccessRead, nil},
		{"read-only writes", read.Secret, AccessWrite, ErrTokenScope},
		{"read-only and a session's call", read.Secret, AccessSession, ErrNeedsSession},
		{"read-write reads", write.Secret, AccessRead, nil},
		{"read-write writes", write.Secret, AccessWrite, nil},
		{"read-write and a session's call", write.Secret, AccessSession, ErrNeedsSession},
		{"an unknown need", write.Secret, Access(0), ErrNeedsSession},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := f.svc.AuthenticateToken(t.Context(), localhost, tt.secret, tt.need, "/test/Call")
			if !errors.Is(err, tt.want) {
				t.Fatalf("AuthenticateToken = %v, want %v", err, tt.want)
			}
			if err == nil && (p.User.ID != owner.User.ID || p.Token == nil || p.Session.ID != "" || !strings.HasPrefix(p.Actor(), "token:tok_")) {
				t.Errorf("principal = %+v, actor %s", p, p.Actor())
			}
		})
	}
	// The secret is stored only as its hash.
	var stored store.APIToken
	_ = f.db.View(t.Context(), func(tx *store.Tx) (err error) {
		stored, err = tx.GetAPIToken(t.Context(), read.Token.ID)
		return err
	})
	if strings.Contains(string(stored.TokenHash), read.Secret[4:]) || stored.Hint != TokenHint(read.Secret) {
		t.Errorf("stored token %+v", stored)
	}
}

func TestTokensExpireAndAreRevoked(t *testing.T) { storetest.Each(t, testTokensExpireAndAreRevoked) }

func testTokensExpireAndAreRevoked(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	f.svc.recheck = 2 * time.Millisecond
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	in := f.clock.now().Add(time.Hour)
	expiring := f.token(t, owner, store.ScopeReadOnly, &in)
	revoked := f.token(t, owner, store.ScopeReadOnly, nil)

	p, err := f.svc.AuthenticateToken(t.Context(), localhost, revoked.Secret, AccessRead, "/test/Watch")
	if err != nil {
		t.Fatal(err)
	}
	stream, stop := f.svc.Track(t.Context(), localhost, p)
	defer stop()
	if err := f.svc.RevokeToken(t.Context(), owner, revoked.Token.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Error("a revoked token's stream stayed open")
	}
	if _, err := f.svc.AuthenticateToken(t.Context(), localhost, revoked.Secret, AccessRead, "/test/Call"); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("a revoked token = %v, want ErrTokenRejected", err)
	}
	if err := f.svc.RevokeToken(t.Context(), owner, revoked.Token.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking it again = %v, want ErrNotFound", err)
	}

	p, err = f.svc.AuthenticateToken(t.Context(), localhost, expiring.Secret, AccessRead, "/test/Watch")
	if err != nil {
		t.Fatal(err)
	}
	stream, stop = f.svc.Track(t.Context(), localhost, p)
	defer stop()
	f.clock.advance(time.Hour)
	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Error("an expired token's stream stayed open")
	}
	if _, err := f.svc.AuthenticateToken(t.Context(), localhost, expiring.Secret, AccessRead, "/test/Call"); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("an expired token = %v, want ErrTokenRejected", err)
	}
	if list, err := f.svc.ListTokens(t.Context(), owner); err != nil || len(list) != 0 {
		t.Errorf("ListTokens after both ended = %v, %v", list, err)
	}
	unknown, _ := newTokenSecret()
	if _, err := f.svc.AuthenticateToken(t.Context(), localhost, unknown, AccessRead, "/test/Call"); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("an unknown token = %v, want ErrTokenRejected", err)
	}
}

func TestCreateTokenChecksItsRequest(t *testing.T) {
	storetest.Each(t, testCreateTokenChecksItsRequest)
}

func testCreateTokenChecksItsRequest(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	past := f.clock.now().Add(-time.Second)
	for name, try := range map[string]func() error{
		"no name": func() error {
			_, err := f.svc.CreateToken(t.Context(), owner, "  \t", store.ScopeReadOnly, nil)
			return err
		},
		"a long name": func() error {
			_, err := f.svc.CreateToken(t.Context(), owner, strings.Repeat("é", 101), store.ScopeReadOnly, nil)
			return err
		},
		"no scope": func() error { _, err := f.svc.CreateToken(t.Context(), owner, "CI", "", nil); return err },
		"an expiry gone": func() error {
			_, err := f.svc.CreateToken(t.Context(), owner, "CI", store.ScopeReadOnly, &past)
			return err
		},
		"an unknown kind": func() error { _, err := f.svc.CreateToken(t.Context(), owner, "CI", "admin", nil); return err },
	} {
		if err := try(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	made, err := f.svc.CreateToken(t.Context(), owner, "  Grafana\x00 read ", store.ScopeReadOnly, nil)
	if err != nil || made.Token.Name != "Grafana read" {
		t.Errorf("CreateToken = %q, %v, want the name tidied", made.Token.Name, err)
	}
	// A token can't make, or revoke, another.
	p, err := f.svc.AuthenticateToken(t.Context(), localhost, made.Secret, AccessRead, "/test/Call")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateToken(t.Context(), p, "Sneaky", store.ScopeReadWrite, nil); !errors.Is(err, ErrNeedsSession) {
		t.Errorf("CreateToken with a token = %v, want ErrNeedsSession", err)
	}
	if err := f.svc.RevokeToken(t.Context(), p, made.Token.ID); !errors.Is(err, ErrNeedsSession) {
		t.Errorf("RevokeToken with a token = %v, want ErrNeedsSession", err)
	}
	for i := range maxTokens - 1 {
		if _, err := f.svc.CreateToken(t.Context(), owner, "CI", store.ScopeReadOnly, nil); err != nil {
			t.Fatalf("token %d: %v", i+2, err)
		}
	}
	if _, err := f.svc.CreateToken(t.Context(), owner, "CI", store.ScopeReadOnly, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("token %d = %v, want ErrInvalid", maxTokens+1, err)
	}
	// Only tokens that still work count.
	if err := f.svc.RevokeToken(t.Context(), owner, made.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateToken(t.Context(), owner, "CI", store.ScopeReadOnly, nil); err != nil {
		t.Errorf("a token after revoking one at the limit = %v", err)
	}
}

// Revoked and expired tokens are deleted 30 days after they stopped
// working, with their addresses, when someone signs in, as sessions are:
// a token's owner may make no more tokens for years.
func TestEndedTokensArePrunedOnSignIn(t *testing.T) {
	storetest.Each(t, testEndedTokensArePrunedOnSignIn)
}

func testEndedTokensArePrunedOnSignIn(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	a := &authtest.Authenticator{}
	owner := ownerPrincipal(f.claim(t, localhost, a))
	in := f.clock.now().Add(time.Hour)
	expiring := f.token(t, owner, store.ScopeReadOnly, &in)
	revoked := f.token(t, owner, store.ScopeReadOnly, nil)
	live := f.token(t, owner, store.ScopeReadOnly, nil)
	for _, made := range []*NewToken{&expiring, &revoked, &live} {
		if _, err := f.svc.AuthenticateToken(t.Context(), localhost, made.Secret, AccessRead, "/test/Call"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.RevokeToken(t.Context(), owner, revoked.Token.ID); err != nil {
		t.Fatal(err)
	}
	stored := func() (tokens, addresses int) {
		f.execSQLScan(t, `SELECT (SELECT COUNT(*) FROM api_tokens), (SELECT COUNT(*) FROM api_token_addresses)`, &tokens, &addresses)
		return tokens, addresses
	}

	f.clock.advance(pruneAfter) // not yet 30 days since the expiry
	if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil {
		t.Fatal(err)
	}
	if tokens, addresses := stored(); tokens != 3 || addresses != 3 {
		t.Errorf("after 30 days, %d tokens and %d addresses are kept, want all 3 of each", tokens, addresses)
	}
	f.clock.advance(time.Hour + time.Minute)
	if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil {
		t.Fatal(err)
	}
	if tokens, addresses := stored(); tokens != 1 || addresses != 1 {
		t.Errorf("after a sign-in 30 days on, %d tokens and %d addresses are kept, want the live one's", tokens, addresses)
	}
	if list, err := f.svc.ListTokens(t.Context(), owner); err != nil || len(list) != 1 || list[0].ID != live.Token.ID {
		t.Errorf("ListTokens = %v, %v, want the live token", list, err)
	}
}

// A token's use is written at most once a minute from one network: an IPv6
// /64, whose privacy addresses rotate, or an IPv4 address however it is
// written. Each network's first use is audited once.
func TestTokenUseIsRecordedOnceAMinuteForEachNetwork(t *testing.T) {
	storetest.Each(t, testTokenUseIsRecordedOnceAMinuteForEachNetwork)
}

func testTokenUseIsRecordedOnceAMinuteForEachNetwork(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	made := f.token(t, owner, store.ScopeReadOnly, nil)
	from := func(addr string) Client {
		c := localhost
		c.Addr = addr
		return c
	}
	use := func(client Client) store.APIToken {
		t.Helper()
		if _, err := f.svc.AuthenticateToken(t.Context(), client, made.Secret, AccessRead, "/test/Call"); err != nil {
			t.Fatal(err)
		}
		list, err := f.svc.ListTokens(t.Context(), owner)
		if err != nil || len(list) != 1 || list[0].LastUsedAt == nil {
			t.Fatalf("ListTokens = %v, %v", list, err)
		}
		return list[0]
	}

	for _, tt := range []struct{ first, again string }{
		{"[2001:db8:0:1::1]:443", "[2001:db8:0:1::99]:443"},
		{"203.0.113.9:4000", "[::ffff:203.0.113.9]:4001"},
	} {
		first := use(from(tt.first))
		f.clock.advance(time.Second)
		if again := use(from(tt.again)); !again.LastUsedAt.Equal(*first.LastUsedAt) || again.LastIP != first.LastIP {
			t.Errorf("a use from %s a second after %s was written: last used %v from %s, want %v from %s",
				tt.again, tt.first, again.LastUsedAt, again.LastIP, first.LastUsedAt, first.LastIP)
		}
		f.clock.advance(touchEvery)
		if later := use(from(tt.again)); !later.LastUsedAt.After(*first.LastUsedAt) {
			t.Errorf("a use from %s a minute later wasn't written: last used %v", tt.again, later.LastUsedAt)
		}
	}
	if events := f.auditEvents(t, "token.use"); len(events) != 2 {
		t.Errorf("token.use = %+v, want one for the /64 and one for the IPv4 address", events)
	}
}

// Refusal counts are kept for at most so many tokens. Making room drops
// tokens with nothing unreported first, so the audit log's counts hold.
func TestRefusalCountsAreForgottenOnlyWhenReported(t *testing.T) {
	now := time.Unix(0, 0)
	r := newRefusals(2)
	if _, report := r.count("a", now); !report {
		t.Fatal("a token's first refusal isn't reported")
	}
	if _, report := r.count("a", now); report {
		t.Fatal("a token's second refusal within the hour is reported")
	}
	r.count("b", now) // reported, so nothing left to report
	now = now.Add(refusedReportEvery)
	if _, report := r.count("c", now); !report { // full: b makes room
		t.Fatal("a new token's first refusal isn't reported")
	}
	if more, report := r.count("a", now); !report || more != 1 {
		t.Errorf("a's next report = %d more, %v, want 1 more: its count was dropped to make room", more, report)
	}
	if len(r.keys.m) > 2 {
		t.Errorf("%d tokens kept, want at most 2", len(r.keys.m))
	}
	// With every token's refusals unreported, some must still go.
	r.count("a", now)
	r.count("c", now)
	if _, report := r.count("d", now); !report || len(r.keys.m) > 2 {
		t.Errorf("a new token when every count is unreported: reported %v, %d kept", report, len(r.keys.m))
	}
}

// The audit log has every token made and revoked, its first use from each
// new address, and refusals of known tokens, at most once an hour each.
func TestTokensAreAudited(t *testing.T) { storetest.Each(t, testTokensAreAudited) }

func testTokensAreAudited(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	made := f.token(t, owner, store.ScopeReadOnly, nil)
	if got := f.auditDetails(t, "token.create"); len(got) != 1 || !strings.Contains(got[0], made.Token.ID) ||
		!strings.Contains(got[0], "read_only") || !strings.Contains(got[0], "doesn't expire") || strings.Contains(got[0], made.Secret) {
		t.Errorf("token.create = %q", got)
	}

	use := func(client Client) {
		t.Helper()
		if _, err := f.svc.AuthenticateToken(t.Context(), client, made.Secret, AccessRead, "/test/Call"); err != nil {
			t.Fatal(err)
		}
	}
	ci := localhost
	ci.Addr = "203.0.113.9:4000"
	use(ci)
	use(ci)
	f.clock.advance(2 * touchEvery)
	use(ci)
	use(fromAddress(1))
	use(fromAddress(1)) // the same /64:
	other := fromAddress(1)
	other.Addr = "[2001:db8:0:1::99]:443"
	use(other)
	use(ci) // back again
	events := f.auditEvents(t, "token.use")
	if len(events) != 2 || !strings.Contains(events[0].Detail, "203.0.113.9") || !strings.Contains(events[1].Detail, "2001:db8:0:1::1") ||
		events[0].Actor != "token:"+made.Token.ID || events[0].Subject != owner.User.ID {
		t.Errorf("token.use = %+v, want one for each new address", events)
	}
	list, _ := f.svc.ListTokens(t.Context(), owner)
	if len(list) != 1 || list[0].LastIP != "203.0.113.9" || list[0].LastUsedAt == nil {
		t.Errorf("the token's last use = %+v", list)
	}

	for range 5 {
		if _, err := f.svc.AuthenticateToken(t.Context(), ci, made.Secret, AccessWrite, "/dockwell.v1.Test/Write"); !errors.Is(err, ErrTokenScope) {
			t.Fatal(err)
		}
	}
	f.clock.advance(refusedReportEvery)
	if _, err := f.svc.AuthenticateToken(t.Context(), ci, made.Secret, AccessSession, "/dockwell.v1.Test/Destroy"); !errors.Is(err, ErrNeedsSession) {
		t.Fatal(err)
	}
	refused := f.auditEvents(t, "token.refuse")
	if len(refused) != 2 || !strings.Contains(refused[0].Detail, "/dockwell.v1.Test/Write") ||
		!strings.Contains(refused[1].Detail, "4 more refused") || refused[1].Actor != "token:"+made.Token.ID {
		t.Errorf("token.refuse = %+v, want one an hour, counting the rest", refused)
	}

	if err := f.svc.RevokeToken(t.Context(), owner, made.Token.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.auditEvents(t, "token.revoke"); len(got) != 1 || got[0].Actor != owner.Actor() || !strings.Contains(got[0].Detail, made.Token.ID) {
		t.Errorf("token.revoke = %+v", got)
	}
	// Its first refusal after revoking is reported, by the address it came
	// from: anyone could have sent it.
	if _, err := f.svc.AuthenticateToken(t.Context(), ci, made.Secret, AccessRead, "/test/Call"); !errors.Is(err, ErrTokenRejected) {
		t.Fatal(err)
	}
	refused = f.auditEvents(t, "token.refuse")
	if len(refused) != 3 || refused[2].Actor != "anonymous:203.0.113.9" || !strings.Contains(refused[2].Detail, "revoked") {
		t.Errorf("token.refuse after revoking = %+v", refused)
	}
	// Unknown tokens are logged, never audited.
	unknown, _ := newTokenSecret()
	_, _ = f.svc.AuthenticateToken(t.Context(), ci, unknown, AccessRead, "/test/Call")
	_, _ = f.svc.AuthenticateToken(t.Context(), ci, "dwt_nonsense", AccessRead, "/test/Call")
	if got := f.auditEvents(t, "token.refuse"); len(got) != 3 {
		t.Errorf("unknown tokens were audited: %+v", got[3:])
	}
	if logs := f.logs.String(); !strings.Contains(logs, "refused an unknown API token") || !strings.Contains(logs, "refused a malformed API token") {
		t.Errorf("unknown tokens weren't logged:\n%s", logs)
	}
}

// Recording a token's use is best effort, as for sessions.
func TestAFailedTokenTouchStillAuthenticates(t *testing.T) {
	storetest.Each(t, testAFailedTokenTouchStillAuthenticates)
}

func testAFailedTokenTouchStillAuthenticates(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	made := f.token(t, owner, store.ScopeReadOnly, nil)
	restore := storetest.FailUpdates(t, f.path, "api_tokens", "last_used_at")
	if p, err := f.svc.AuthenticateToken(t.Context(), localhost, made.Secret, AccessRead, "/test/Call"); err != nil || p.Token.ID != made.Token.ID {
		t.Errorf("AuthenticateToken when the touch fails = %v", err)
	}
	restore()
	if _, err := f.svc.AuthenticateToken(t.Context(), localhost, made.Secret, AccessRead, "/test/Call"); err != nil {
		t.Fatal(err)
	}
	if got := f.auditEvents(t, "token.use"); len(got) != 1 {
		t.Errorf("token.use after a failed touch = %+v, want the first use recorded on the next call", got)
	}
}

func TestATokenKeepsAtMostSoManyAddresses(t *testing.T) {
	storetest.Each(t, testATokenKeepsAtMostSoManyAddresses)
}

func testATokenKeepsAtMostSoManyAddresses(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	made := f.token(t, owner, store.ScopeReadOnly, nil)
	for i := range store.MaxTokenAddresses + 10 {
		f.clock.advance(time.Millisecond)
		if _, err := f.svc.AuthenticateToken(t.Context(), fromAddress(i), made.Secret, AccessRead, "/test/Call"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	f.execSQLScan(t, `SELECT COUNT(*) FROM api_token_addresses`, &n)
	if n != store.MaxTokenAddresses {
		t.Errorf("addresses kept = %d, want %d", n, store.MaxTokenAddresses)
	}
}

func (f *fixture) execSQLScan(t *testing.T, query string, dest ...any) {
	t.Helper()
	driver, source := "sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)"
	if store.IsPostgres(f.path) {
		driver, source = "pgx", f.path
	}
	db, err := sql.Open(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(t.Context(), query).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

func TestLocalTokens(t *testing.T) { storetest.Each(t, testLocalTokens) }

// Local access on the control server makes tokens for the owner, which
// last LocalTokenTTL, and only once there is an owner.
func testLocalTokens(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	if _, err := f.svc.CreateLocalToken(t.Context(), "cli:root@hz-1", "Local access on hz-1, read-only", store.ScopeReadOnly); err == nil ||
		!strings.Contains(err.Error(), "no owner yet") {
		t.Errorf("a local token before the claim = %v, want it refused", err)
	}
	// A token kept from state that was made anew, before anyone claimed
	// the install again, is nothing to revoke, not an error.
	if revoked, err := f.svc.RevokeLocalToken(t.Context(), "cli:root@hz-1", "tok_01JD6Z3R8M9QF2K7T4XW5BNC0V"); revoked || err != nil {
		t.Errorf("revoking a token with no owner = %v, %v, want nothing to do", revoked, err)
	}
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	made, err := f.svc.CreateLocalToken(t.Context(), "cli:root@hz-1", "Local access on hz-1, read-only", store.ScopeReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if made.Token.UserID != owner.User.ID || made.Token.ExpiresAt == nil || !made.Token.ExpiresAt.Equal(f.clock.now().Add(LocalTokenTTL)) {
		t.Errorf("a local token = %+v, want the owner's, lasting %s", made.Token, LocalTokenTTL)
	}
	if _, err := f.svc.AuthenticateToken(t.Context(), localhost, made.Secret, AccessWrite, "/test/Call"); !errors.Is(err, ErrTokenScope) {
		t.Errorf("a read-only local token making a change = %v, want ErrTokenScope", err)
	}
	if got := f.auditEvents(t, "token.create"); len(got) != 1 || got[0].Actor != "cli:root@hz-1" {
		t.Errorf("token.create events = %+v, want one by the local user", got)
	}
	if revoked, err := f.svc.RevokeLocalToken(t.Context(), "cli:root@hz-1", made.Token.ID); !revoked || err != nil {
		t.Fatalf("revoking a local token = %v, %v", revoked, err)
	}
	if _, err := f.svc.AuthenticateToken(t.Context(), localhost, made.Secret, AccessRead, "/test/Call"); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("a revoked local token = %v, want ErrTokenRejected", err)
	}
	if revoked, err := f.svc.RevokeLocalToken(t.Context(), "cli:root@hz-1", made.Token.ID); revoked || err != nil {
		t.Errorf("revoking it again = %v, %v, want nothing to do", revoked, err)
	}
	if got := f.auditEvents(t, "token.revoke"); len(got) != 1 || got[0].Actor != "cli:root@hz-1" {
		t.Errorf("token.revoke events = %+v, want one by the local user", got)
	}
}
