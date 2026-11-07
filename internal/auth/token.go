// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// API tokens are secrets for scripts, the CLI and integrations. Each acts
// for the user who made it, within its scope, and authenticates every
// protocol alike as "Authorization: Bearer <secret>". See
// docs/adr/0016-scoped-api-tokens.md.
//
// A secret is "dwt_", 43 base62 digits holding 256 random bits, and 6 more
// holding a CRC-32 of those 43: 53 characters a secret scanner can find by
// the prefix and confirm by the checksum, with no false positives worth the
// name. Only a SHA-256 of the whole secret is stored.
const (
	// TokenPrefix starts every API token's secret.
	TokenPrefix = "dwt_"
	// TokenLen is the length of every secret.
	TokenLen      = len(TokenPrefix) + tokenBodyLen + tokenSumLen
	tokenBodyLen  = 43 // base62 digits for 256 bits: 62^43 > 2^256
	tokenSumLen   = 6  // base62 digits for a CRC-32: 62^6 > 2^32
	maxTokens     = 100
	maxTokenName  = 100
	maxRefusalIDs = 10_000
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Access is what a call needs of the credential that makes it, by the
// method's effect.
type Access int

const (
	// AccessRead is for calls that only read (EFFECT_READ). Any token may
	// make them.
	AccessRead Access = iota + 1
	// AccessWrite is for calls that plan or apply a change that can be
	// undone (EFFECT_PLAN and EFFECT_APPLY). A read-write token may make
	// them.
	AccessWrite
	// AccessLogs is for calls that read logs, which often hold secrets and
	// personal data that apps print, so reading them counts as more than any
	// other read: a token of either scope needs the logs grant. Only the
	// token's maker can ask for it.
	AccessLogs
	// AccessSession is for calls that remove data or cut off access
	// (EFFECT_DESTRUCTIVE), and for managing sign-in and tokens. Only a
	// signed-in session may make them, never a token.
	AccessSession
)

// Errors for API tokens. Their messages are written for whoever runs the
// script or the CLI.
var (
	// ErrTokenRejected covers an unknown, revoked or expired token, and one
	// that isn't a token at all: whoever holds it can tell which.
	ErrTokenRejected = errors.New("this API token is unknown, revoked or expired. " +
		"Check that you copied all of it, or create a new one under Profile and security")
	ErrTokenScope = errors.New("this API token is read-only, so it can't make changes. " +
		"Create a read-write token under Profile and security")
	// ErrTokenLogs is a token without the logs grant reading logs.
	ErrTokenLogs = errors.New("this API token can't read logs, which often hold secrets that apps print. " +
		"Create a token that can read logs under Profile and security")
	ErrNeedsSession = errors.New("API tokens can't remove data, cut off access, reveal a secret, reach a server's files, rename the install, or change how you sign in and your tokens. " +
		"Sign in and do it in the panel")
)

// NewToken is a token just made, with the secret that is shown only once.
type NewToken struct {
	Token  store.APIToken
	Secret string
}

// newTokenSecret returns a new secret and its hash.
func newTokenSecret() (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	body := base62Digits(new(big.Int).SetBytes(b), tokenBodyLen)
	secret := TokenPrefix + body + tokenChecksum(body)
	return secret, hashToken(secret)
}

func tokenChecksum(body string) string {
	return base62Digits(new(big.Int).SetUint64(uint64(crc32.ChecksumIEEE([]byte(body)))), tokenSumLen)
}

// base62Digits writes n in base62 with exactly width digits, zeros first.
// n must fit; it is consumed.
func base62Digits(n *big.Int, width int) string {
	out := make([]byte, width)
	base, digit := big.NewInt(62), new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, digit)
		out[i] = base62[digit.Int64()]
	}
	return string(out)
}

// WellFormedToken reports whether s has the shape of an API token's secret,
// checksum included. A typo fails it without a round trip; a well-formed
// secret may still be unknown.
func WellFormedToken(s string) bool {
	if len(s) != TokenLen || !strings.HasPrefix(s, TokenPrefix) {
		return false
	}
	rest := s[len(TokenPrefix):]
	for i := range len(rest) {
		if strings.IndexByte(base62, rest[i]) < 0 {
			return false
		}
	}
	return tokenChecksum(rest[:tokenBodyLen]) == rest[tokenBodyLen:]
}

// TokenHint is the prefix and the first and last 4 of a secret's random
// digits, such as "dwt_4f9K…x9Ta", for telling tokens apart. It never shows
// the checksum, which is a function of the random digits and would give
// away more of them. 8 of the 43 random digits leave about 208 of the 256
// bits unknown.
func TokenHint(secret string) string {
	body := secret[len(TokenPrefix) : len(TokenPrefix)+tokenBodyLen]
	return TokenPrefix + body[:4] + "…" + body[len(body)-4:]
}

// liveToken reports whether a token still works at now.
func liveToken(tok *store.APIToken, now time.Time) bool {
	return tok.RevokedAt == nil && (tok.ExpiresAt == nil || now.Before(*tok.ExpiresAt))
}

// CreateToken makes a token for the principal, who must have signed in: a
// token can't make another. It needs proof of a step-up for creating a
// token, so a stolen session can't make one to keep. Unset expires makes
// one that doesn't expire. A request it refuses for anything else, such as
// an invalid name or the most tokens, leaves the proof unspent, and is
// refused before the proof is asked for.
func (s *Service) CreateToken(ctx context.Context, p Principal, name, scope string, grants []string, expires *time.Time, proof string) (NewToken, error) {
	if p.Token != nil || p.Session.ID == "" {
		return NewToken{}, ErrNeedsSession
	}
	return s.createToken(ctx, func(*store.Tx) (store.User, error) { return p.User, nil }, p.Actor(), name, scope, grants, expires,
		func() error { return s.confirmed(p, proof, StepUpCreateToken) })
}

// LocalTokenTTL is how long a token for local access lasts.
const LocalTokenTTL = 12 * time.Hour

// CreateLocalToken makes a token that lasts until a time, within LocalTokenTTL,
// or that long with none, for the install's owner, for the CLI on the control
// server. Only someone who can read the control plane's data directory can call
// it, as with CreateSignInLink, and they control the install already; actor names
// them in the audit log, as "cli:root@hz-1". Those who can read that directory
// can read logs anyway, with Docker, so the token can too. See
// docs/adr/0019-a-cli-generated-from-the-schema.md.
func (s *Service) CreateLocalToken(ctx context.Context, actor, name, scope string, until time.Time) (NewToken, error) {
	expires := s.now().Add(LocalTokenTTL)
	if !until.IsZero() && until.Before(expires) {
		expires = until
	}
	return s.createToken(ctx, func(tx *store.Tx) (store.User, error) {
		u, err := tx.Owner(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return u, errors.New("this install has no owner yet, so local access has nobody to act for. Claim it with the link from dockwell claim")
		}
		return u, err
	}, actor, name, scope, []string{store.GrantLogs}, &expires, nil)
}

// RevokeLocalToken revokes a token CreateLocalToken made, for whoever can
// read the data directory, and reports whether it did. It finds the token
// by its ID, so it works when the install has no owner, or another one,
// and does nothing for a token that is revoked already or was never made
// here, as when the control plane's state was made anew.
func (s *Service) RevokeLocalToken(ctx context.Context, actor, tokenID string) (bool, error) {
	revoked := false
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		tok, err := tx.GetAPIToken(ctx, tokenID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		err = tx.RevokeAPIToken(ctx, tok.UserID, tokenID, s.now())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		revoked = true
		return s.auditOn(ctx, tx, actor, "token.revoke", tok.UserID, tokenID, "token "+tokenID)
	})
	if err != nil {
		return false, fmt.Errorf("can't revoke local access's token %s: %w", tokenID, err)
	}
	s.streams.end(tokenID)
	return revoked, nil
}

// createToken makes a token for the user who returns, in the transaction
// that stores it. confirm, when set, is called once nothing else refuses
// it, the most tokens included, and before anything is stored, to check it
// may be made: it may spend a step-up's proof.
func (s *Service) createToken(ctx context.Context, who func(*store.Tx) (store.User, error), actor, name, scope string, grants []string, expires *time.Time,
	confirm func() error,
) (NewToken, error) {
	name = strings.Join(strings.Fields(cleanText(name)), " ")
	if name == "" || len([]rune(name)) > maxTokenName {
		return NewToken{}, fmt.Errorf("%w: name the token after what will use it, in up to %d characters, such as GitHub Actions deploy",
			ErrInvalid, maxTokenName)
	}
	if scope != store.ScopeReadOnly && scope != store.ScopeReadWrite {
		return NewToken{}, fmt.Errorf("%w: choose the token's scope, read-only or read-write", ErrInvalid)
	}
	for _, g := range grants {
		if g != store.GrantLogs {
			return NewToken{}, fmt.Errorf("%w: a token can't be granted %q. It can be granted %s", ErrInvalid, g, store.GrantLogs)
		}
	}
	now := s.now()
	if expires != nil {
		at := expires.UTC().Truncate(time.Millisecond)
		if !at.After(now) {
			return NewToken{}, fmt.Errorf("%w: choose an expiry in the future, or none", ErrInvalid)
		}
		expires = &at
	}
	secret, hash := newTokenSecret()
	tok := store.APIToken{
		ID:        id.New(id.Token),
		TokenHash: hash,
		Name:      name,
		Hint:      TokenHint(secret),
		Scope:     scope,
		Grants:    slices.Compact(slices.Sorted(slices.Values(grants))),
		CreatedAt: now,
		ExpiresAt: expires,
	}
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		user, err := who(tx)
		if err != nil {
			return err
		}
		tok.UserID = user.ID
		if err := tx.PruneAPITokens(ctx, now.Add(-pruneAfter)); err != nil {
			return err
		}
		live, err := tx.CountLiveAPITokens(ctx, user.ID, now)
		if err != nil {
			return err
		}
		if live >= maxTokens {
			return fmt.Errorf("%w: you have %d API tokens, the most you can have. Revoke one you no longer use, then try again",
				ErrInvalid, maxTokens)
		}
		if confirm != nil {
			if err := confirm(); err != nil {
				return err
			}
		}
		if err := tx.CreateAPIToken(ctx, tok); err != nil {
			return err
		}
		expiry := "doesn't expire"
		if expires != nil {
			expiry = "expires " + expires.Format(time.RFC3339)
		}
		detail := fmt.Sprintf("token %s %q, %s, %s", tok.ID, name, scope, expiry)
		if tok.HasGrant(store.GrantLogs) {
			detail = fmt.Sprintf("token %s %q, %s, can read logs, %s", tok.ID, name, scope, expiry)
		}
		return s.auditOn(ctx, tx, actor, "token.create", user.ID, tok.ID, detail)
	})
	if err != nil {
		return NewToken{}, err
	}
	return NewToken{Token: tok, Secret: secret}, nil
}

// ListTokens returns the principal's tokens that still work, newest first.
func (s *Service) ListTokens(ctx context.Context, p Principal) ([]store.APIToken, error) {
	var out []store.APIToken
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		out, err = tx.ListLiveAPITokens(ctx, p.User.ID, s.now())
		return err
	})
	return out, err
}

// RevokeToken stops one of the principal's tokens working. Its open streams
// close.
func (s *Service) RevokeToken(ctx context.Context, p Principal, tokenID string) error {
	if p.Token != nil {
		return ErrNeedsSession
	}
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		err := tx.RevokeAPIToken(ctx, p.User.ID, tokenID, s.now())
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: you have no API token %s, or it was already revoked. List them with ListTokens", ErrNotFound, tokenID)
		}
		if err != nil {
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "token.revoke", p.User.ID, tokenID, "token "+tokenID)
	})
	if err != nil {
		return err
	}
	s.streams.end(tokenID)
	s.refusals.forget(tokenID)
	return nil
}

// AuthenticateToken returns the principal behind an API token's secret, if
// the token still works and may make a call that needs need. call names the
// call for the audit log. It fails with ErrTokenRejected for a secret that
// is malformed, unknown, revoked or expired, ErrTokenScope for a read-only
// token making a change, and ErrNeedsSession for a call no token may make.
//
// Unknown secrets are logged, not audited, so strangers can't fill the
// audit log. Refusals of a token Dockwell knows are audited, at most once an
// hour for each token, with a count of the rest. There is no limit by
// source address: 256 random bits can't be guessed, and a limit would let
// anyone sharing an address keep a working token out.
func (s *Service) AuthenticateToken(ctx context.Context, client Client, secret string, need Access, call string) (Principal, error) {
	if !WellFormedToken(secret) {
		s.logger.InfoContext(ctx, "refused a malformed API token", slog.String("call", call), slog.String("from", client.Addr))
		return Principal{}, ErrTokenRejected
	}
	now := s.now()
	var p Principal
	err := s.db.View(ctx, func(tx *store.Tx) error {
		tok, err := tx.GetAPITokenByHash(ctx, hashToken(secret))
		if errors.Is(err, store.ErrNotFound) {
			return ErrTokenRejected
		}
		if err != nil {
			return err
		}
		user, err := tx.GetUser(ctx, tok.UserID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrTokenRejected
		}
		if err != nil {
			return err
		}
		p = Principal{User: user, Token: &tok}
		return nil
	})
	if errors.Is(err, ErrTokenRejected) {
		s.logger.InfoContext(ctx, "refused an unknown API token", slog.String("call", call), slog.String("from", client.Addr))
		return Principal{}, err
	}
	if err != nil {
		return Principal{}, err
	}
	tok := p.Token
	switch {
	case tok.RevokedAt != nil:
		return Principal{}, s.refuseToken(ctx, client, p, client.anonymous(), "revoked", call, ErrTokenRejected)
	case !liveToken(tok, now):
		return Principal{}, s.refuseToken(ctx, client, p, client.anonymous(), "expired", call, ErrTokenRejected)
	case need == AccessWrite && tok.Scope != store.ScopeReadWrite:
		return Principal{}, s.refuseToken(ctx, client, p, p.Actor(), "read-only, and the call makes changes", call, ErrTokenScope)
	case need == AccessLogs && !tok.HasGrant(store.GrantLogs):
		return Principal{}, s.refuseToken(ctx, client, p, p.Actor(), "without the logs grant, and the call reads logs", call, ErrTokenLogs)
	case need != AccessRead && need != AccessWrite && need != AccessLogs:
		return Principal{}, s.refuseToken(ctx, client, p, p.Actor(), "a token, and the call needs a session", call, ErrNeedsSession)
	}
	s.touchToken(ctx, &p, client, now)
	return p, nil
}

// refuseToken audits a refused call with a known token, at most once an
// hour for each token, and returns refusal. The refusal stands whether or
// not the event could be written.
func (s *Service) refuseToken(ctx context.Context, client Client, p Principal, actor, why, call string, refusal error) error {
	now := s.now()
	more, report := s.refusals.count(p.Token.ID, now)
	s.logger.InfoContext(ctx, "refused an API token", slog.String("token", p.Token.ID), slog.String("why", why),
		slog.String("call", call), slog.String("from", client.Addr))
	if !report {
		return refusal
	}
	detail := fmt.Sprintf("token %s is %s: %s from %s", p.Token.ID, why, call, client.remoteIP())
	if more > 0 {
		detail += fmt.Sprintf(", and %d more refused since the last report", more)
	}
	if err := s.db.Update(ctx, func(tx *store.Tx) error {
		return s.auditFrom(ctx, tx, actor, client.Surface(p), "token.refuse", p.User.ID, detail)
	}); err != nil {
		s.logger.WarnContext(ctx, "can't audit a refused API token", slog.String("token", p.Token.ID), slog.Any("error", err))
	}
	return refusal
}

// touchToken records that a token was used, at most once a minute from one
// address, an IPv6 /64 counting as one as it does for token.use, and audits
// its first use from each new address. Like a session's touch it is best
// effort: a failed write is logged, and the next call tries again, as the
// token's last use hasn't changed.
func (s *Service) touchToken(ctx context.Context, p *Principal, client Client, now time.Time) {
	tok, ip := p.Token, client.remoteIP()
	if tok.LastUsedAt != nil && now.Sub(*tok.LastUsedAt) < touchEvery && networkKey(ip) == networkKey(tok.LastIP) {
		return
	}
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.TouchAPIToken(ctx, tok.ID, now, ip); err != nil {
			return err
		}
		first, err := tx.SeeAPITokenAddress(ctx, tok.ID, networkKey(ip), now)
		if err != nil || !first {
			return err
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(*p), "token.use", p.User.ID, fmt.Sprintf("token %s used from %s for the first time", tok.ID, ip))
	})
	if err != nil {
		s.logger.WarnContext(ctx, "can't record that an API token was used; the next call tries again",
			slog.String("token", tok.ID), slog.Any("error", err))
		return
	}
	tok.LastUsedAt, tok.LastIP = &now, ip
}

// tokenStillLive reports whether a stream's token still works, and records
// the stream as a use of it.
func (s *Service) tokenStillLive(ctx context.Context, client Client, tokenID string) bool {
	var tok store.APIToken
	var user store.User
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		if tok, err = tx.GetAPIToken(ctx, tokenID); err != nil {
			return err
		}
		user, err = tx.GetUser(ctx, tok.UserID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		return true // a failed read isn't a reason to close; the next check decides
	}
	now := s.now()
	if !liveToken(&tok, now) {
		return false
	}
	s.touchToken(ctx, &Principal{User: user, Token: &tok}, client, now)
	return true
}

// refusals counts refused calls for each known token since its last audit
// event, in memory: a script calling in a loop with a revoked token must
// not write to the database on every call. A restart forgets the counts,
// which costs at most one more event.
//
// It holds at most maxKeys tokens. To make room it forgets tokens with
// nothing unreported first, which at worst makes their next event come
// early; only when every token has refusals to report does it lose some.
// Only someone holding that many of the install's tokens could cause that.
type refusals struct {
	mu   sync.Mutex
	keys bounded[refusalCount]
}

type refusalCount struct {
	n        int // refused since reported
	reported time.Time
}

func newRefusals(maxKeys int) *refusals {
	return &refusals{keys: newBounded(maxKeys, func(c *refusalCount, _ time.Time) bool { return c.n == 0 })}
}

// count counts a refusal for tokenID and reports whether to audit it now,
// with how many earlier ones the event should count.
func (r *refusals) count(tokenID string, now time.Time) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, made := r.keys.get(tokenID, now)
	if !made && now.Sub(c.reported) < refusedReportEvery {
		c.n++
		return 0, false
	}
	more := c.n
	c.n, c.reported = 0, now
	return more, true
}

func (r *refusals) forget(tokenID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys.forget(tokenID)
}
