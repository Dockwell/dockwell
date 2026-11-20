// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestScrubSMTPText(t *testing.T) {
	t.Parallel()

	// 1. Long password (> 200 bytes, up to maxLine 256): scrubbed on the full
	// reply BEFORE clipping so no truncated prefix survives.
	longPass := strings.Repeat("P@ssw0rd!", 28) // 252 bytes
	longB64 := base64.StdEncoding.EncodeToString([]byte(longPass))
	longErr := &textproto.Error{Code: 550, Msg: "5.7.1 rejected payload " + longPass + " b64=" + longB64 + " tail"}
	emLong := &resourcev1.AlertEmail{Username: "alerts-user", Password: longPass}
	gotLong := smtpWordsFor(longErr, emLong)
	if strings.Contains(gotLong, "P@ssw0rd!") || strings.Contains(gotLong, longB64[:12]) || !strings.HasPrefix(gotLong, "550 ") {
		t.Errorf("long password survived smtpWordsFor: %q", gotLong)
	}

	// 2. Short 1..3 byte user/password whose AUTH LOGIN base64 is exactly 4 chars.
	shortUser, shortPass := "op", "pw1"
	b64ShortUser := base64.StdEncoding.EncodeToString([]byte(shortUser)) // "b3A=" (4 chars)
	b64ShortPass := base64.StdEncoding.EncodeToString([]byte(shortPass)) // "cHcx" (4 chars)
	b64ShortPlain := base64.StdEncoding.EncodeToString([]byte("\x00" + shortUser + "\x00" + shortPass))
	if len(b64ShortUser) != 4 || len(b64ShortPass) != 4 {
		t.Fatalf("expected 4-char AUTH LOGIN base64 tokens, got %q %q", b64ShortUser, b64ShortPass)
	}
	shortErr := &textproto.Error{
		Code: 550,
		Msg:  "rejected user=" + shortUser + " pass=" + shortPass + " login=" + b64ShortUser + "," + b64ShortPass + " plain=" + b64ShortPlain,
	}
	gotShort := smtpWordsFor(shortErr, &resourcev1.AlertEmail{Username: shortUser, Password: shortPass})
	for _, needle := range []string{shortUser, shortPass, b64ShortUser, b64ShortPass, b64ShortPlain} {
		if strings.Contains(gotShort, needle) {
			t.Errorf("4-char AUTH LOGIN / short credential leaked %q in %q", needle, gotShort)
		}
	}

	// 3. Single-pass scrubbing: even when a raw password appears as a substring
	// inside the AUTH PLAIN base64 token, the entire base64 token is replaced cleanly.
	b64Base := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00"))
	subPass := b64Base[2:6]
	b64Full := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00" + subPass))
	if !strings.Contains(b64Full, subPass) {
		t.Fatalf("test setup: expected %q inside %q", subPass, b64Full)
	}
	gotSingle := scrubSMTPText("rejected "+b64Full, "alice", subPass)
	if gotSingle != "rejected …" {
		t.Errorf("single-pass scrub of overlapping base64 = %q, want %q", gotSingle, "rejected …")
	}

	// 4. Auth failure wording uses Dockwell-owned text with status code alone,
	// never echoing any provider reply body.
	authErr := &textproto.Error{Code: 535, Msg: "5.7.8 provider diagnostic with secret-hint-xyz"}
	connSaid := authFailureWords(authErr, &resourcev1.AlertEmail{Connection: "postmark", Username: "u", Password: "p"})
	if !strings.Contains(connSaid, "postmark") || !strings.Contains(connSaid, "535") || strings.Contains(connSaid, "provider diagnostic") || strings.Contains(connSaid, "secret-hint") {
		t.Errorf("connection authFailureWords = %q", connSaid)
	}
	inlineSaid := authFailureWords(authErr, &resourcev1.AlertEmail{Host: "smtp.example.com", Username: "u", Password: "p"})
	if !strings.Contains(inlineSaid, "535") || strings.Contains(inlineSaid, "provider diagnostic") || strings.Contains(inlineSaid, "secret-hint") {
		t.Errorf("inline authFailureWords = %q", inlineSaid)
	}
}

// L1 & H1: when a relay offers no SMTP AUTH, a connection-backed channel names
// the connection and gives the real fix rather than telling the user to clear a
// channel username field it cannot set; partial recipient refusals with long
// credentials and 4-char AUTH LOGIN tokens are scrubbed before clipping.
func TestNoAuthErrorAndPartialRefusalScrubbing(t *testing.T) {
	t.Parallel()

	// Start an implicit-TLS SMTP relay that advertises no AUTH extension.
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	srvTLS := &tls.Config{Certificates: certs.TLS.Certificates, MinVersion: tls.VersionTLS12}
	clientTLS := certs.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	certs.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvTLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				_, _ = io.WriteString(c, "220 relay.test ready\r\n")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					verb := strings.ToUpper(strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 2)[0])
					switch verb {
					case "EHLO", "HELO":
						_, _ = io.WriteString(c, "250 relay.test\r\n")
					case "QUIT":
						_, _ = io.WriteString(c, "221 bye\r\n")
						return
					default:
						_, _ = io.WriteString(c, "502 unsupported\r\n")
					}
				}
			}(conn)
		}
	}()
	noAuthHost, noAuthPortStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	noAuthPort, err := strconv.Atoi(noAuthPortStr)
	if err != nil {
		t.Fatal(err)
	}

	s := &Sender{TLS: clientTLS}
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)

	// Connection-backed channel names the connection and does not say "Clear the channel's user".
	connRes := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
		Connection: "relay-conn",
		Host:       noAuthHost,
		Port:       uint32(noAuthPort),
		Security:   resourcev1.AlertEmail_SECURITY_TLS,
		Username:   "alerts",
		Password:   "pw",
		From:       "alerts@example.com",
		To:         []string{"ops@example.com"},
	}}}, m)
	if connRes.OK || !strings.Contains(connRes.Said, "relay-conn") || strings.Contains(connRes.Said, "Clear the channel's user") {
		t.Errorf("connection-backed no-AUTH error = %+v", connRes)
	}
	// Inline channel keeps its inline instruction.
	inlineRes := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
		Host:     noAuthHost,
		Port:     uint32(noAuthPort),
		Security: resourcev1.AlertEmail_SECURITY_TLS,
		Username: "alerts",
		Password: "pw",
		From:     "alerts@example.com",
		To:       []string{"ops@example.com"},
	}}}, m)
	if inlineRes.OK || !strings.Contains(inlineRes.Said, "Clear the channel's user") {
		t.Errorf("inline no-AUTH error = %+v", inlineRes)
	}

	// Partial recipient refusal echoing a 240-byte password and 4-char AUTH LOGIN
	// user base64 is scrubbed on the full reply before clipping.
	tlsSrv, tlsRoots := newSMTP(t, true, false)
	tlsHost, tlsPortStr, err := net.SplitHostPort(tlsSrv.addr)
	if err != nil {
		t.Fatal(err)
	}
	tlsPort, err := strconv.Atoi(tlsPortStr)
	if err != nil {
		t.Fatal(err)
	}
	longPass := strings.Repeat("Z9x8Y7w6", 30) // 240 bytes
	shortUser := "op"
	b64User := base64.StdEncoding.EncodeToString([]byte(shortUser)) // "b3A=" (4 chars)
	tlsSrv.set(func(sv *smtpServer) {
		sv.mechs = "PLAIN"
		sv.refuse = map[string]string{
			"bad@example.com": "550 5.1.1 refused token=" + b64User + " pass=" + longPass,
		}
	})
	s.TLS = tlsRoots
	res := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
		Connection: "relay-conn",
		Host:       tlsHost,
		Port:       uint32(tlsPort),
		Security:   resourcev1.AlertEmail_SECURITY_TLS,
		Username:   shortUser,
		Password:   longPass,
		From:       "alerts@example.com",
		To:         []string{"ok@example.com", "bad@example.com"},
	}}}, m)
	if !res.OK || !strings.Contains(res.Said, "bad@example.com") ||
		strings.Contains(res.Said, "Z9x8Y7w6") || strings.Contains(res.Said, b64User) || strings.Contains(res.Said, shortUser) {
		t.Errorf("partial refusal with 240-byte password & 4-char LOGIN b64 = %+v", res)
	}

	// H1 residual: a server advertising only an unsupported AUTH mechanism equal
	// to a short password must never echo that mechanism name in Said.
	tlsSrv.set(func(sv *smtpServer) {
		sv.mechs = "XYZZ"
		sv.refuse = nil
	})
	unsupportedRes := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
		Connection: "relay-conn",
		Host:       tlsHost,
		Port:       uint32(tlsPort),
		Security:   resourcev1.AlertEmail_SECURITY_TLS,
		Username:   "alerts",
		Password:   "xyzz",
		From:       "alerts@example.com",
		To:         []string{"ops@example.com"},
	}}}, m)
	if unsupportedRes.OK || strings.Contains(strings.ToUpper(unsupportedRes.Said), "XYZZ") ||
		!strings.Contains(unsupportedRes.Said, "relay-conn") || !strings.Contains(unsupportedRes.Said, "PLAIN or LOGIN") {
		t.Errorf("unsupported AUTH mechanism matching short password = %+v", unsupportedRes)
	}
}

// N1: replacing short matches with a 3-byte ellipsis expands the reply past
// saidMax; clip must cut on a valid UTF-8 rune boundary so delivery rows write
// cleanly on Postgres (no SQLSTATE 22021 invalid UTF-8 error).
func TestClipUTF8OnExpandingRedaction(t *testing.T) {
	storetest.Each(t, testClipUTF8OnExpandingRedaction)
}

func testClipUTF8OnExpandingRedaction(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	// 68 copies of a 1-character password expand to 204 bytes of "…" (> saidMax=200).
	errReply := &textproto.Error{Code: 550, Msg: strings.Repeat("x", 68)}
	said := smtpWordsFor(errReply, &resourcev1.AlertEmail{Username: "u", Password: "x"})
	if !utf8.ValidString(said) {
		t.Fatalf("smtpWordsFor returned invalid UTF-8: %q", said)
	}
	if strings.Contains(said, "x") || !strings.HasPrefix(said, "550 ") {
		t.Fatalf("smtpWordsFor did not scrub expanding reply cleanly: %q", said)
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.AddAlertDelivery(t.Context(), store.AlertDelivery{
			ID: id.New(id.Alert), ChannelID: "ach_test", ChannelName: "ops",
			At: t0, OK: false, Said: said, About: "Disk full",
		})
	}); err != nil {
		t.Fatalf("AddAlertDelivery rejected scrubbed Said on %s: %v", backend, err)
	}
}

// H4: Evaluator.send reads the live channel by ID and its connection in one
// store snapshot immediately before dialing. If the channel was repointed to
// another connection (and the old connection reassigned to an app) or deleted
// after the send was queued, send must not dial the old connection.
func TestOutboxSendReadsLiveChannelAndConnectionInOneSnapshot(t *testing.T) {
	storetest.Each(t, testOutboxSendReadsLiveChannelAndConnectionInOneSnapshot)
}

func testOutboxSendReadsLiveChannelAndConnectionInOneSnapshot(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		alert.Rule{}, alert.Channel{},
	)
	keeper := secretstest.Keeper(db, kinds)
	eng := revision.New(db, kinds, nil, keeper, logger)
	var orgID, envID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		orgID = tr.Organization.ID
		envID = tr.Projects[0].Environments[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	srvA, rootsA := newSMTP(t, true, false)
	hostA, portAStr, err := net.SplitHostPort(srvA.addr)
	if err != nil {
		t.Fatal(err)
	}
	portA, err := strconv.Atoi(portAStr)
	if err != nil {
		t.Fatal(err)
	}
	srvB, _ := newSMTP(t, true, false)
	hostB, portBStr, err := net.SplitHostPort(srvB.addr)
	if err != nil {
		t.Fatal(err)
	}
	portB, err := strconv.Atoi(portBStr)
	if err != nil {
		t.Fatal(err)
	}

	applyEdits := func(summary string, edits ...revision.Edit) {
		t.Helper()
		p, err := eng.Propose(t.Context(), revision.Proposal{Summary: summary, Edits: edits})
		if err != nil {
			t.Fatalf("%s propose: %v", summary, err)
		}
		if err := eng.Apply(t.Context(), p.Change); err != nil {
			t.Fatalf("%s apply: %v", summary, err)
		}
	}

	chanOnA := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
		Connection: "conn-a", From: "alerts@example.com", To: []string{"ops@example.com"},
	}}}
	chanOnB := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
		Connection: "conn-b", From: "alerts@example.com", To: []string{"ops@example.com"},
	}}}
	applyEdits("Seed conn-a, conn-b, and ops on conn-a",
		revision.Edit{Ref: resource.Ref{Kind: connection.Kind, Scope: orgID, Name: "conn-a"}, Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: hostA, Port: uint32(portA), Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "alert-user", Password: "alert-password"},
		}},
		revision.Edit{Ref: resource.Ref{Kind: connection.Kind, Scope: orgID, Name: "conn-b"}, Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: hostB, Port: uint32(portB), Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "b-user", Password: "b-password"},
		}},
		revision.Edit{Ref: resource.Ref{Kind: alert.ChannelKind, Scope: orgID, Name: "ops"}, Spec: chanOnA},
	)

	var chanID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(alert.ChannelKind), orgID, "ops")
		chanID = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Repoint ops to conn-b, then reassign conn-a to an app with app credentials.
	applyEdits("Repoint ops to conn-b", revision.Edit{
		Ref: resource.Ref{Kind: alert.ChannelKind, Scope: orgID, Name: "ops"}, Spec: chanOnB,
	})
	applyEdits("Reassign conn-a to app with app password",
		revision.Edit{Ref: resource.Ref{Kind: connection.Kind, Scope: orgID, Name: "conn-a"}, Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: hostA, Port: uint32(portA), Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "app-user", Password: "app-password"},
		}},
		revision.Edit{Ref: resource.Ref{Kind: link.Kind, Scope: envID, Name: "app-mail"}, Spec: &resourcev1.Link{
			Label: "Mail", Connection: "conn-a", Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Sets: []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
		}},
	)

	// Attempting to send the stale queued outgoing (pointing at conn-a) must return
	// errChannelChanged and never dial conn-a.
	if _, err := resolveLiveSend(t.Context(), db, keeper, chanID, chanOnA); !errors.Is(err, errChannelChanged) {
		t.Fatalf("resolveLiveSend after repoint = %v, want errChannelChanged", err)
	}
	now := t0
	ev := &Evaluator{DB: db, Keeper: keeper, Engine: eng, Sender: &Sender{TLS: rootsA}, Logger: logger, Now: func() time.Time { return now }}
	ev.init()
	sendID := id.New(id.Alert)
	alertID := id.New(id.Alert)
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.OpenAlert(t.Context(), store.Alert{ID: alertID, RuleID: "alr_1", RuleName: "disk-full", Thing: "disk:1", Title: "Disk full", OpenedAt: now}); err != nil {
			return err
		}
		return tx.QueueSend(t.Context(), store.AlertSend{ID: sendID, AlertID: alertID, RuleID: "alr_1", ChannelID: chanID, Status: "firing", QueuedAt: now, DueAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	ev.send(t.Context(), &outgoing{
		c:     channel{id: chanID, name: "ops", spec: chanOnA},
		m:     firing(resourcev1.AlertRule_SEVERITY_WARNING),
		sends: []store.AlertSend{{ID: sendID, AlertID: alertID, ChannelID: chanID, QueuedAt: now}},
	})
	srvA.mu.Lock()
	gotAuths := len(srvA.auth)
	gotMails := len(srvA.mail)
	srvA.mu.Unlock()
	if gotAuths != 0 || gotMails != 0 {
		t.Fatalf("stale outbox send dialed reassigned conn-a: auths=%d mails=%d", gotAuths, gotMails)
	}

	// Deleting ops before send runs must return errChannelGone, finish the outbox
	// row as "the channel was removed", and never dial conn-b.
	applyEdits("Delete ops", revision.Edit{Ref: resource.Ref{Kind: alert.ChannelKind, Scope: orgID, Name: "ops"}, Spec: nil})
	if _, err := resolveLiveSend(t.Context(), db, keeper, chanID, chanOnB); !errors.Is(err, errChannelGone) {
		t.Fatalf("resolveLiveSend after delete = %v, want errChannelGone", err)
	}
	ev.send(t.Context(), &outgoing{
		c:     channel{id: chanID, name: "ops", spec: chanOnB},
		m:     firing(resourcev1.AlertRule_SEVERITY_WARNING),
		sends: []store.AlertSend{{ID: sendID, AlertID: alertID, ChannelID: chanID, QueuedAt: now}},
	})
	srvB.mu.Lock()
	gotB := len(srvB.mail)
	srvB.mu.Unlock()
	if gotB != 0 {
		t.Fatalf("send after channel delete dialed conn-b: mails=%d", gotB)
	}
	var sends []store.AlertSend
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		m, err := tx.SendsOf(t.Context(), []string{alertID})
		sends = m[alertID]
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(sends) != 1 || sends[0].DoneAt == nil || sends[0].OK || sends[0].Said != "the channel was removed" {
		t.Errorf("deleted-channel send row = %+v, want finished with 'the channel was removed'", sends)
	}
}
