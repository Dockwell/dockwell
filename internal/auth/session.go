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
	MethodInvite        = "invite"
	// MethodProvider is an account at a sign-in provider, linked to the person
	// (ADR 0095). It confirms nothing a passkey does.
	MethodProvider = "provider"
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

// ReadsLogs reports a principal that may read what logs hold: a person, or a token
// with the logs grant.
func (p Principal) ReadsLogs() bool {
	return p.Token == nil || p.Token.HasGrant(store.GrantLogs)
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
	return s.startLinkedSession(ctx, tx, user, client, method, store.ProviderLink{}, "")
}

// startLinkedSession is startSession for a sign-in with a provider, which keeps
// the link it began from, and says the provider in the audit log (ADR 0095).
func (s *Service) startLinkedSession(ctx context.Context, tx *store.Tx, user store.User, client Client, method string, link store.ProviderLink, via string) (SignedIn, error) {
	// Someone removed signs in no way at all, whatever led here (ADR 0049).
	if user.RemovedAt != nil {
		return SignedIn{}, ErrUnauthenticated
	}
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
		// A sign-in with a provider keeps the link, so unlinking ends it.
		ProviderLinkID: link.ID,
	}
	// A sign-in with a passkey, as claiming and accepting an invite make one,
	// opens the session's hour, for someone who asks once an hour (ADR 0075).
	if method == MethodPasskey || method == MethodClaim || method == MethodInvite {
		sess.PasskeyAt = &now
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
	how := method
	if via != "" {
		how = method + " " + via
	}
	if err := s.audit(ctx, tx, "user:"+user.ID, "auth.sign_in", user.ID,
		fmt.Sprintf("%s on %s from %s, session %s", how, client.Site.RPID, ip, sess.ID)); err != nil {
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
	var link *store.ProviderLink
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
		if link, err = sessionLink(ctx, tx, sess); err != nil {
			return err
		}
		user, err := tx.GetUser(ctx, sess.UserID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		// Someone removed signs in no way at all, whatever was left behind (ADR 0049).
		if user.RemovedAt != nil {
			return ErrUnauthenticated
		}
		p = Principal{User: user, Session: sess}
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	// A session begun with a provider lasts while its link signs anyone in (ADR 0095).
	// The providers are read once the session's transaction is over: a read inside it
	// would wait on a second connection from the same pool (#646's review, H1).
	if link != nil {
		stopped, err := s.sessionLinkStops(ctx, link)
		if err != nil {
			return Principal{}, err
		}
		if stopped != nil {
			s.endStoppedLink(ctx, stopped)
			return Principal{}, ErrUnauthenticated
		}
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
//
// still, when set, is asked too, at the start and on each recheck, whether the
// caller may still make the stream, as their role says now: a stream ends
// when it errs, as it does once a role is gone (ADR 0048).
func (s *Service) Track(ctx context.Context, client Client, p Principal, still ...func(context.Context) error) (context.Context, func()) {
	return s.track(ctx, client, p, true, still...)
}

// Follow is Track for what outlives the request that made it and isn't a page in
// use, such as a terminal nobody is attached to: it ends with the session, at once
// when that's revoked or signed out and within a recheck when it expires, but its
// checks aren't use. So it keeps no session alive past its idle limit, and never
// moves its last-seen time or address. still is asked as Track asks it.
func (s *Service) Follow(ctx context.Context, client Client, p Principal, still ...func(context.Context) error) (context.Context, func()) {
	return s.track(ctx, client, p, false, still...)
}

func (s *Service) track(ctx context.Context, client Client, p Principal, use bool, still ...func(context.Context) error) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	credential := p.credential()
	// A page is a session's: what a token follows, such as a CLI's log, has no page
	// to open it again.
	entry := s.streams.add(credential, p.User.ID, use && p.Token == nil, cancel)
	may := func() bool {
		for _, check := range still {
			if check != nil && check(ctx) != nil {
				return false
			}
		}
		return true
	}
	if !s.stillLive(ctx, client, p, use) || !may() {
		cancel()
	}
	done := make(chan struct{})
	// A stream from a session begun with a provider checks every providersFor, so it
	// ends within twice that of a change to the provider (#646's review, M1).
	every := s.recheck
	if p.Session.ProviderLinkID != "" {
		every = min(every, providersFor)
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.stillLive(ctx, client, p, use) {
					s.logger.DebugContext(ctx, "closing a stream whose session or token ended", slog.String("credential", credential))
					cancel()
					return
				}
				if !may() {
					s.logger.DebugContext(ctx, "closing a stream its caller may no longer make", slog.String("credential", credential))
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

// stillLive reports whether a stream's session or token still works, and with
// use, records the stream as a use of it.
func (s *Service) stillLive(ctx context.Context, client Client, p Principal, use bool) bool {
	if p.Token != nil {
		return s.tokenStillLive(ctx, client, p.Token.ID)
	}
	sessionID := p.Session.ID
	var sess store.Session
	var link *store.ProviderLink
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		if sess, err = tx.GetSession(ctx, sessionID); err != nil {
			return err
		}
		// A removed person's stream ends, whatever was left behind (ADR 0049).
		user, err := tx.GetUser(ctx, sess.UserID)
		if err != nil {
			return err
		}
		if user.RemovedAt != nil {
			return ErrUnauthenticated
		}
		link, err = sessionLink(ctx, tx, sess)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, ErrUnauthenticated):
		return false
	case err != nil:
		return true // a failed read isn't a reason to close; the next check decides
	}
	if link != nil {
		stopped, err := s.sessionLinkStops(ctx, link)
		if err != nil {
			return true // as a failed read above
		}
		if stopped != nil {
			s.endStoppedLink(ctx, stopped)
			return false
		}
	}
	now := s.now()
	if !live(sess, client.Site, now) {
		return false
	}
	if ip := client.remoteIP(); use && (now.Sub(sess.LastSeenAt) >= touchEvery || ip != sess.LastIP) {
		s.touch(ctx, sessionID, now, ip)
	}
	return true
}

// SessionLive reports whether a session still works on client's site, as a
// stream's check of it does, and records the check as a use of it.
func (s *Service) SessionLive(ctx context.Context, client Client, sessionID string) bool {
	return s.stillLive(ctx, client, Principal{Session: store.Session{ID: sessionID}}, true)
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
// revoking one closes them, and whose they are, so a change to what someone
// holds closes theirs.
type streams struct {
	mu   sync.Mutex
	next int
	m    map[string]map[int]stream
}

// stream is an open stream: whose it is, whether it's a page in use rather
// than something that outlives its request, and how it ends.
type stream struct {
	user   string
	page   bool
	cancel context.CancelFunc
}

func newStreams() *streams { return &streams{m: map[string]map[int]stream{}} }

func (s *streams) add(sessionID, userID string, page bool, cancel context.CancelFunc) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	if s.m[sessionID] == nil {
		s.m[sessionID] = map[int]stream{}
	}
	s.m[sessionID][s.next] = stream{user: userID, page: page, cancel: cancel}
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
	open := s.m[sessionID]
	delete(s.m, sessionID)
	s.mu.Unlock()
	for _, st := range open {
		st.cancel()
	}
}

// endUser ends the open streams of userID's, whatever session or token each
// rides on, the pages alone or everything, and says how many.
func (s *streams) endUser(userID string, pagesOnly bool) int {
	s.mu.Lock()
	var ending []context.CancelFunc
	for credential, open := range s.m {
		for entry, st := range open {
			if st.user == userID && (st.page || !pagesOnly) {
				ending = append(ending, st.cancel)
				delete(open, entry)
			}
		}
		if len(open) == 0 {
			delete(s.m, credential)
		}
	}
	s.mu.Unlock()
	for _, cancel := range ending {
		cancel()
	}
	return len(ending)
}

// endStreamsOf ends the open streams of the people a change to what they hold
// changed, once it commits (ADR 0049). A stream was narrowed to what its person
// held when it opened. After a lowering or a removal every one ends, as none
// outlives a grant taken away. After a change that takes nothing away, the
// pages' streams end, and a page opens them again as its person now may; what
// outlives its request, such as a terminal left running, goes on, as it never
// had more than they hold now.
func (s *Service) endStreamsOf(tookAway bool, userIDs ...string) {
	for _, id := range userIDs {
		if n := s.streams.endUser(id, !tookAway); n > 0 {
			s.logger.Debug("closed the streams of someone whose access changed", slog.String("user", id), slog.Int("streams", n))
		}
	}
}

// sessionLink reads the link a session began from, in tx, or nil for a session that
// didn't begin with a provider. A link that's gone has ended the session.
func sessionLink(ctx context.Context, tx *store.Tx, sess store.Session) (*store.ProviderLink, error) {
	if sess.ProviderLinkID == "" {
		return nil, nil
	}
	link, err := tx.GetProviderLink(ctx, sess.ProviderLinkID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}
