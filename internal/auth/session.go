// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

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

// Principal is a signed-in user and the session they are using.
type Principal struct {
	User    store.User
	Session store.Session
}

// Actor names the principal in the audit log.
func (p Principal) Actor() string { return "user:" + p.User.ID }

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

// startSession records a new session for user in tx and audits the sign-in.
func (s *Service) startSession(ctx context.Context, tx *store.Tx, user store.User, client Client, method string) (SignedIn, error) {
	now := s.now()
	token, hash := newToken()
	ip := client.remoteIP()
	ua := client.UserAgent
	if len(ua) > 512 {
		ua = ua[:512]
	}
	sess := store.Session{
		ID:         id.New(id.Session),
		TokenHash:  hash,
		UserID:     user.ID,
		RPID:       client.Site.RPID,
		Method:     method,
		UserAgent:  ua,
		CreatedIP:  ip,
		LastIP:     ip,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(SessionMax),
	}
	if err := tx.PruneSessions(ctx, now.Add(-pruneAfter)); err != nil {
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

// sessionEnd is when a session ends unless revoked first.
func sessionEnd(sess store.Session) time.Time {
	idle := sess.LastSeenAt.Add(SessionIdle)
	if idle.Before(sess.ExpiresAt) {
		return idle
	}
	return sess.ExpiresAt
}

// SessionEnd is when a session ends unless it is used or revoked first.
func SessionEnd(sess store.Session) time.Time { return sessionEnd(sess) }

// live reports whether a session still works at now, on site.
func live(sess store.Session, site Site, now time.Time) bool {
	return sess.RevokedAt == nil && now.Before(sessionEnd(sess)) && sess.RPID == site.RPID && site.CanSignIn()
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
		if err := s.db.Update(ctx, func(tx *store.Tx) error { return tx.TouchSession(ctx, p.Session.ID, now, ip) }); err != nil {
			return Principal{}, err
		}
		p.Session.LastSeenAt, p.Session.LastIP = now, ip
	}
	return p, nil
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
	now := s.now()
	var out []store.Session
	err := s.db.View(ctx, func(tx *store.Tx) error {
		all, err := tx.ListActiveSessions(ctx, p.User.ID, now)
		if err != nil {
			return err
		}
		for i := range all {
			if now.Before(sessionEnd(all[i])) {
				out = append(out, all[i])
			}
		}
		return nil
	})
	return out, err
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
		all, err := tx.ListActiveSessions(ctx, p.User.ID, now)
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

// Track ties a stream's context to its session: the returned context ends
// when the session is revoked, signed out or expires. Call stop when the
// stream ends.
func (s *Service) Track(ctx context.Context, client Client, p Principal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	entry := s.streams.add(p.Session.ID, cancel)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(streamRecheck)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.stillLive(ctx, client, p.Session.ID) {
					s.logger.DebugContext(ctx, "closing a stream whose session ended", slog.String("session", p.Session.ID))
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(done)
		s.streams.remove(p.Session.ID, entry)
		cancel()
	}
}

func (s *Service) stillLive(ctx context.Context, client Client, sessionID string) bool {
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
	default:
		return live(sess, client.Site, s.now())
	}
}

// streams are the open streams of each session, so revoking one closes them.
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
