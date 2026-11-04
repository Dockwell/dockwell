// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// Sign-in methods, as stored with each session.
const (
	MethodClaim         = "claim"
	MethodPasskey       = "passkey"
	MethodAuthenticator = "authenticator_code"
	MethodRecoveryCode  = "recovery_code"
	MethodLink          = "link"
)

// Principal is who a call acts for: a signed-in user and the session they
// are using, or the user an API token acts for and that token.
type Principal struct {
	User store.User
	// Session is the call's session. Zero for a call with an API token.
	Session store.Session
	// Token is the call's API token. Nil for a call with a session.
	Token *store.APIToken
}

// Actor names the principal in the audit log: the user, or the token
// acting for them.
func (p Principal) Actor() string {
	if p.Token != nil {
		return "token:" + p.Token.ID
	}
	return "user:" + p.User.ID
}

// credential is the ID of the session or token the call came with, which
// its streams are tracked by.
func (p Principal) credential() string {
	if p.Token != nil {
		return p.Token.ID
	}
	return p.Session.ID
}

// SignedIn is the result of signing in: the user, the new session, and the
// token for its cookie.
type SignedIn struct {
	User    store.User
	Session store.Session
	Token   string
}

// Cookie returns the session cookie to set.
func (s SignedIn) Cookie(site Site) (string, error) {
	c, err := site.SessionCookie(s.Token, s.Session.ExpiresAt.Sub(s.Session.CreatedAt))
	if err != nil {
		return "", err
	}
	return c.String(), nil
}

func (c Client) remoteIP() string {
	if host, _, err := net.SplitHostPort(c.Addr); err == nil {
		return host
	}
	return c.Addr
}

// anonymous names a caller who hasn't signed in, in the audit log, by the
// address they came from. A failed sign-in names the account it tried as
// its subject, never as its actor: anyone can try.
func (c Client) anonymous() string { return "anonymous:" + c.remoteIP() }

// startSession records a new session for user in tx and audits the sign-in.
func (s *Service) startSession(ctx context.Context, tx *store.Tx, user store.User, client Client, method string) (SignedIn, error) {
	now := s.now()
	token, hash := newToken()
	ip := client.remoteIP()
	sess := store.Session{
		ID:         id.New(id.Session),
		TokenHash:  hash,
		UserID:     user.ID,
		RPID:       client.Site.RPID,
		Method:     method,
		UserAgent:  clientText(client.UserAgent, maxUserAgent),
		CreatedIP:  ip,
		LastIP:     ip,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(SessionMax),
	}
	// Ended sessions and tokens are kept pruneAfter for the record.
	if err := tx.PruneSessions(ctx, now.Add(-pruneAfter)); err != nil {
		return SignedIn{}, err
	}
	if err := tx.PruneAPITokens(ctx, now.Add(-pruneAfter)); err != nil {
		return SignedIn{}, err
	}
	if err := tx.CreateSession(ctx, sess); err != nil {
		return SignedIn{}, err
	}
	if err := s.audit(ctx, tx, "user:"+user.ID, "auth.sign_in", user.ID,
		fmt.Sprintf("%s on %s from %s, session %s", method, client.Site.RPID, ip, sess.ID)); err != nil {
		return SignedIn{}, err
	}
	return SignedIn{User: user, Session: sess, Token: token}, nil
}

// SessionEnd is when a session ends unless it is used or revoked first.
func SessionEnd(sess store.Session) time.Time {
	idle := sess.LastSeenAt.Add(SessionIdle)
	if idle.Before(sess.ExpiresAt) {
		return idle
	}
	return sess.ExpiresAt
}

// live reports whether a session still works at now, on site.
func live(sess store.Session, site Site, now time.Time) bool {
	return sess.RevokedAt == nil && now.Before(SessionEnd(sess)) && sess.RPID == site.RPID && site.CanSignIn()
}

// Authenticate returns the principal behind a session token presented on
// client's site. Any failure is ErrUnauthenticated: an unknown token, an
// ended session, or one made on another address. It records the session as
// seen, at most once a minute.
func (s *Service) Authenticate(ctx context.Context, client Client, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now()
	var p Principal
	err := s.db.View(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSessionByTokenHash(ctx, hashToken(token))
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if !live(sess, client.Site, now) {
			return ErrUnauthenticated
		}
		user, err := tx.GetUser(ctx, sess.UserID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		p = Principal{User: user, Session: sess}
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	ip := client.remoteIP()
	if now.Sub(p.Session.LastSeenAt) >= touchEvery || ip != p.Session.LastIP {
		if s.touch(ctx, p.Session.ID, now, ip) {
			p.Session.LastSeenAt, p.Session.LastIP = now, ip
		}
	}
	return p, nil
}

// touch records that a session was used, and reports whether it could. It
// is best effort: the request already proved its session, and the next one
// tries again, so a failed write is logged and nothing more.
func (s *Service) touch(ctx context.Context, sessionID string, now time.Time, ip string) bool {
	if err := s.db.Update(ctx, func(tx *store.Tx) error { return tx.TouchSession(ctx, sessionID, now, ip) }); err != nil {
		s.logger.WarnContext(ctx, "can't record that a session was used; its idle limit runs from its last recorded use",
			slog.String("session", sessionID), slog.Any("error", err))
		return false
	}
	return true
}

// SignOut ends the principal's session.
func (s *Service) SignOut(ctx context.Context, p Principal) error {
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.RevokeSession(ctx, p.User.ID, p.Session.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "auth.sign_out", p.User.ID, "session "+p.Session.ID)
	})
	if err != nil {
		return err
	}
	s.streams.end(p.Session.ID)
	return nil
}

// ListSessions returns the principal's sessions that still work, newest
// first.
func (s *Service) ListSessions(ctx context.Context, p Principal) ([]store.Session, error) {
	var out []store.Session
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		out, err = openSessions(ctx, tx, p.User.ID, s.now())
		return err
	})
	return out, err
}

// openSessions returns a user's sessions that still work at now: neither
// revoked nor past their idle or absolute limit.
func openSessions(ctx context.Context, tx *store.Tx, userID string, now time.Time) ([]store.Session, error) {
	all, err := tx.ListActiveSessions(ctx, userID, now)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		if now.Before(SessionEnd(all[i])) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// RevokeSession ends one of the principal's sessions. Its open streams close.
func (s *Service) RevokeSession(ctx context.Context, p Principal, sessionID string) error {
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		err := tx.RevokeSession(ctx, p.User.ID, sessionID, s.now())
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: you have no open session %s. List them with ListSessions", ErrNotFound, sessionID)
		}
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "session.revoke", p.User.ID, "session "+sessionID)
	})
	if err != nil {
		return err
	}
	s.streams.end(sessionID)
	return nil
}

// RevokeOtherSessions ends every session of the principal's except the
// current one, and returns how many ended.
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal) (int, error) {
	now := s.now()
	var ended []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		all, err := openSessions(ctx, tx, p.User.ID, now)
		if err != nil {
			return err
		}
		for i := range all {
			if all[i].ID == p.Session.ID {
				continue
			}
			if err := tx.RevokeSession(ctx, p.User.ID, all[i].ID, now); err != nil {
				return err
			}
			ended = append(ended, all[i].ID)
		}
		if len(ended) == 0 {
			return nil
		}
		return s.audit(ctx, tx, p.Actor(), "session.revoke", p.User.ID, fmt.Sprintf("%d other sessions", len(ended)))
	})
	if err != nil {
		return 0, err
	}
	for _, id := range ended {
		s.streams.end(id)
	}
	return len(ended), nil
}

// Track ties a stream's context to its session or API token: the returned
// context ends when the session is revoked, signed out or expires, or the
// token is revoked or expires. Call stop when the stream ends.
//
// An open stream is a page in use, so each recheck that finds the session
// live also counts as using it, at most once a minute like any request. A
// page left open therefore keeps its session until the absolute limit, 7
// days after signing in, and the idle limit ends sessions with nothing open.
//
// The stream is registered first and its session checked again after, so a
// revocation that commits after Authenticate read the session still ends it:
// revoking commits, then ends the session's registered streams. Either the
// stream was registered by then and is ended, or the check here reads the
// committed revocation.
func (s *Service) Track(ctx context.Context, client Client, p Principal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	credential := p.credential()
	entry := s.streams.add(credential, cancel)
	if !s.stillLive(ctx, client, p) {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(s.recheck)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.stillLive(ctx, client, p) {
					s.logger.DebugContext(ctx, "closing a stream whose session or token ended", slog.String("credential", credential))
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(done)
		s.streams.remove(credential, entry)
		cancel()
	}
}

// stillLive reports whether a stream's session or token still works, and
// records the stream as a use of it.
func (s *Service) stillLive(ctx context.Context, client Client, p Principal) bool {
	if p.Token != nil {
		return s.tokenStillLive(ctx, client, p.Token.ID)
	}
	sessionID := p.Session.ID
	var sess store.Session
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		sess, err = tx.GetSession(ctx, sessionID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		return true // a failed read isn't a reason to close; the next check decides
	}
	now := s.now()
	if !live(sess, client.Site, now) {
		return false
	}
	if ip := client.remoteIP(); now.Sub(sess.LastSeenAt) >= touchEvery || ip != sess.LastIP {
		s.touch(ctx, sessionID, now, ip)
	}
	return true
}

// maxUserAgent is how much of a browser's User-Agent header is kept.
const maxUserAgent = 512

// clientText makes text a client sent fit to store and to send back, as
// cleanText does, and at most maxBytes long, cut between characters.
func clientText(text string, maxBytes int) string {
	text = cleanText(text)
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// cleanText makes text valid UTF-8, replacing anything else, with spaces
// for control characters. Protobuf strings must be valid UTF-8, so text that
// isn't would make every response that carries it fail.
func cleanText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, "\uFFFD"))
}

// streams are the open streams of each session and API token, by ID, so
// revoking one closes them.
type streams struct {
	mu   sync.Mutex
	next int
	m    map[string]map[int]context.CancelFunc
}

func newStreams() *streams { return &streams{m: map[string]map[int]context.CancelFunc{}} }

func (s *streams) add(sessionID string, cancel context.CancelFunc) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	if s.m[sessionID] == nil {
		s.m[sessionID] = map[int]context.CancelFunc{}
	}
	s.m[sessionID][s.next] = cancel
	return s.next
}

func (s *streams) remove(sessionID string, entry int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[sessionID], entry)
	if len(s.m[sessionID]) == 0 {
		delete(s.m, sessionID)
	}
}

func (s *streams) end(sessionID string) {
	s.mu.Lock()
	cancels := s.m[sessionID]
	delete(s.m, sessionID)
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
