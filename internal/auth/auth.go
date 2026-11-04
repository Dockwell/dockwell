// SPDX-License-Identifier: AGPL-3.0-only

// Package auth decides who may use the panel and the API: the claim that
// makes the first owner, passkeys (WebAuthn), codes from an authenticator
// app (TOTP) with recovery codes, one-time sign-in links, and sessions.
//
// The design and its threat model are in docs/adr/0011-claim-and-sign-in.md
// and docs/security/sign-in.md. In brief:
//   - Only a claim link printed on the server can create the owner. It is
//     256 random bits, stored as a hash, works once, and expires in 24 h.
//     Spending it and creating the owner happen in one transaction, and the
//     schema allows one owner, so racing claims can't make two.
//   - Passkeys require user verification and are bound to the address
//     (relying party ID) they were made on: localhost, or the domain.
//   - Codes follow RFC 6238. Each works once, the secret is sealed at rest,
//     wrong codes slow down per account and per source, and ten wrong codes
//     in a row lock code sign-in until the owner unlocks it.
//   - A session is an opaque random token in an HttpOnly, SameSite=Strict
//     cookie, stored as a hash, bound to its address, with idle and
//     absolute limits.
//   - Every sign-in, failure, sign-out and change to how someone signs in is
//     in the audit log.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// Lifetimes.
const (
	// ClaimLinkTTL is how long a claim link works.
	ClaimLinkTTL = 24 * time.Hour
	// SignInLinkTTL is how long a link from dockwell recover works.
	SignInLinkTTL = 15 * time.Minute
	// SessionIdle ends a session that has made no request for this long.
	SessionIdle = 12 * time.Hour
	// SessionMax ends every session this long after it signed in.
	SessionMax = 7 * 24 * time.Hour
	// ceremonyTTL is how long a passkey ceremony may take, Begin to Finish.
	ceremonyTTL = 5 * time.Minute
	// touchEvery limits how often a session's last-seen time is written.
	touchEvery = time.Minute
	// streamRecheck is how often an open stream checks its session again.
	streamRecheck = 30 * time.Second
	// pruneAfter is how long ended sessions are kept for the record.
	pruneAfter = 30 * 24 * time.Hour
)

const (
	tokenSize   = 32 // bytes of randomness in sessions and links: 256 bits
	handleSize  = 32 // bytes in a WebAuthn user handle
	maxNameLen  = 100
	maxEmailLen = 254
	rpName      = "Dockwell"
)

// Errors for callers to map to API codes. Their messages are written for
// the person reading them in the panel or the CLI.
var (
	ErrUnauthenticated = errors.New("this needs you to be signed in. Sign in, then try again")
	ErrClaimed         = errors.New("this install already has an owner. Sign in instead; if you lost your passkey, run dockwell recover on the server")
	// ErrClaimLinkRejected covers a wrong, spent, replaced or expired claim
	// link: one error for all of them gives a guesser nothing to learn.
	ErrClaimLinkRejected = errors.New("this claim link is invalid, already used, replaced by a newer one, or expired. Run dockwell claim on the server for a new link")
	// ErrSignInLinkRejected is the same for sign-in links.
	ErrSignInLinkRejected = errors.New("this sign-in link is invalid, already used, replaced by a newer one, or expired. Run dockwell recover on the server for a new link")
	ErrCeremonyExpired    = errors.New("this passkey request expired or was already used. Try again")
	ErrPasskeyRejected    = errors.New("that passkey isn't registered for this address, or it couldn't be verified. Try another passkey, or sign in with a code")
	ErrPasskeyCopied      = errors.New("that passkey reported a lower use count than before, which happens when a passkey is copied, so it no longer signs in. " +
		"Sign in another way and remove it under Profile and security")
	ErrPasskeyExists = errors.New("that passkey is already registered. Use it to sign in, or pick a different authenticator")
	ErrCodeRejected  = errors.New("that code didn't work. Check the email and the code, then try again")
	ErrCodeReused    = errors.New("that code was already used. Wait for the next one in your authenticator app")
	ErrCodesLocked   = fmt.Errorf("code sign-in is locked after %d wrong codes in a row. "+
		"Sign in with a passkey, a recovery code or a link from dockwell recover, then unlock it under Profile and security", lockAfter)
	ErrLastWayIn       = errors.New("that would leave you no way to sign in. Add another passkey or an authenticator app first")
	ErrNeedsHostname   = errors.New("passkeys and sign-in need a hostname, not an IP address. Open the panel at http://localhost instead, on the same port")
	ErrTooManyRequests = errors.New("too many requests from your address. Wait a minute, then try again")
	ErrNoTOTP          = errors.New("you don't sign in with an authenticator app yet. Set one up first")
	ErrNoPendingTOTP   = errors.New("start setting up the authenticator app first")
	ErrTOTPMismatch    = errors.New("that code doesn't match the new secret. Check that your phone's clock is right, then enter the current code")
	// ErrInvalid wraps requests that are malformed: a bad email or name.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound wraps a session or passkey the caller doesn't have.
	ErrNotFound = errors.New("not found")
)

// SlowDown is a wrong-code error that also says how long to wait.
type SlowDown struct {
	Wait time.Duration
}

func (e *SlowDown) Error() string {
	return fmt.Sprintf("too many wrong codes. Wait %s, then try again", human.Duration(e.RetryAfter()))
}

// RetryAfter is the wait rounded up to whole seconds, as a Retry-After
// header says it, so that waiting that long is always enough: 0.3 s is 1 s,
// never 0.
func (e *SlowDown) RetryAfter() time.Duration {
	if e.Wait <= 0 {
		return 0
	}
	return (e.Wait + time.Second - 1).Truncate(time.Second)
}

// Config configures a Service.
type Config struct {
	DB     *store.DB
	Key    *seal.Key
	Logger *slog.Logger
	Sites  Sites
	// ServerName returns the control server's name, shown in authenticator
	// apps. Optional.
	ServerName func() string
	// Now replaces the clock in tests.
	Now func() time.Time
}

// Service is the control plane's authority over people.
type Service struct {
	db         *store.DB
	key        *seal.Key
	logger     *slog.Logger
	sites      Sites
	serverName func() string
	now        func() time.Time

	ceremonies *ceremonies
	sources    *failures // wrong codes by source address
	begins     *buckets  // unauthenticated ceremonies and links, by source
	streams    *streams
	unflagged  unflagged // passkeys seen to be copied, until the flag is stored
	// recheck is how often an open stream checks its session again.
	recheck time.Duration
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.Key == nil || cfg.Logger == nil {
		return nil, errors.New("auth: Config needs DB, Key and Logger")
	}
	if cfg.Sites.Domain != "" {
		if err := ValidDomain(cfg.Sites.Domain); err != nil {
			return nil, err
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ServerName == nil {
		cfg.ServerName = func() string { return "" }
	}
	return &Service{
		db:         cfg.DB,
		key:        cfg.Key,
		logger:     cfg.Logger,
		sites:      cfg.Sites,
		serverName: cfg.ServerName,
		now:        cfg.Now,
		ceremonies: newCeremonies(1024),
		sources:    newFailures(freeSourceFailures, 10_000),
		begins:     newBuckets(30, 2*time.Second, 10_000),
		streams:    newStreams(),
		recheck:    streamRecheck,
	}, nil
}

// Sites says where the panel answers.
func (s *Service) Sites() Sites { return s.sites }

// Client is who is asking: the address they reached and where they are.
type Client struct {
	Site      Site
	Addr      string // the remote address, host:port
	UserAgent string
}

// Claimed reports whether the install has an owner.
func (s *Service) Claimed(ctx context.Context) (bool, error) {
	var n int
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		n, err = tx.CountUsers(ctx)
		return err
	})
	return n > 0, err
}

// Owner returns the install's owner, or store.ErrNotFound.
func (s *Service) Owner(ctx context.Context) (store.User, error) {
	var u store.User
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		u, err = tx.Owner(ctx)
		return err
	})
	return u, err
}

// newToken returns a random token for a cookie or a link, and its hash.
func newToken() (string, []byte) {
	b := make([]byte, tokenSize)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

// hashToken hashes a session or link token for storage. Tokens carry 256
// random bits, so a fast hash is enough: there is nothing to search.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *Service) audit(ctx context.Context, tx *store.Tx, actor, action, subject, detail string) error {
	return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: s.now(), Actor: actor, Action: action, Subject: subject, Detail: detail})
}

// normalizeEmail lowercases and trims an email and checks its shape.
func normalizeEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(e, '@')
	if len(e) > maxEmailLen || at < 1 || at == len(e)-1 || strings.ContainsAny(e, " \t\r\n<>\"") ||
		!utf8.ValidString(e) || strings.IndexFunc(e, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: enter an email address, such as mira@acme.dev", ErrInvalid)
	}
	return e, nil
}

func normalizeName(name string) (string, error) {
	n := strings.Join(strings.Fields(cleanText(name)), " ")
	if n == "" || len([]rune(n)) > maxNameLen {
		return "", fmt.Errorf("%w: enter your name, up to %d characters", ErrInvalid, maxNameLen)
	}
	return n, nil
}

// relyingParty returns the WebAuthn configuration for a site: passkeys made
// or used here belong to its RP ID, and responses must come from its origin.
func (s *Service) relyingParty(site Site) (*webauthn.WebAuthn, error) {
	if !site.CanSignIn() {
		return nil, ErrNeedsHostname
	}
	timeout := webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL}
	return webauthn.New(&webauthn.Config{
		RPID:                  site.RPID,
		RPDisplayName:         rpName,
		RPOrigins:             []string{site.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
}

// waUser adapts a user and some of their passkeys to webauthn.User.
type waUser struct {
	user        store.User
	credentials []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.user.WebAuthnHandle }
func (u *waUser) WebAuthnName() string                       { return u.user.Email }
func (u *waUser) WebAuthnDisplayName() string                { return u.user.Name }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// ceremony is a passkey ceremony between Begin and Finish.
type ceremony struct {
	kind    ceremonyKind
	site    Site
	session webauthn.SessionData
	expires time.Time
	// The user it is for: the owner-to-be for a claim, the signed-in user
	// when adding a passkey. Unset for a sign-in.
	user store.User
	// claimHash is the claim link's token hash.
	claimHash []byte
}

type ceremonyKind int

const (
	ceremonyClaim ceremonyKind = iota + 1
	ceremonySignIn
	ceremonyAddPasskey
)

// ceremonies holds ceremonies in memory, at most max of them. They are
// short-lived and single-use; a restart cancels any in flight, and the
// browser simply starts again.
type ceremonies struct {
	mu  sync.Mutex
	max int
	m   map[string]*ceremony
}

func newCeremonies(maxCeremonies int) *ceremonies {
	return &ceremonies{max: maxCeremonies, m: map[string]*ceremony{}}
}

func (c *ceremonies) put(cer *ceremony, now time.Time) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := base64.RawURLEncoding.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		var oldest string
		for k, v := range c.m {
			if now.After(v.expires) {
				delete(c.m, k)
			} else if oldest == "" || v.expires.Before(c.m[oldest].expires) {
				oldest = k
			}
		}
		if len(c.m) >= c.max {
			delete(c.m, oldest)
		}
	}
	c.m[key] = cer
	return key
}

// take removes and returns a ceremony of the given kind. It works once,
// whatever the outcome of the Finish that follows.
func (c *ceremonies) take(key string, kind ceremonyKind, now time.Time) (*ceremony, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[key]
	if !ok {
		return nil, ErrCeremonyExpired
	}
	delete(c.m, key)
	if cer.kind != kind || now.After(cer.expires) {
		return nil, ErrCeremonyExpired
	}
	return cer, nil
}
