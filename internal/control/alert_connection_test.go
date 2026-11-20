// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/control/connections"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type testSMTPServer struct {
	host    string
	port    uint32
	tlsCfg  *tls.Config
	mu      sync.Mutex
	auths   []string
	mails   []string
	authErr string
	rcptErr map[string]string
}

func startTestSMTP(t *testing.T, bindIP string) (*testSMTPServer, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(bindIP)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: bindIP},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	srvTLS := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
	clientTLS := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	s := &testSMTPServer{host: host, port: uint32(port), tlsCfg: srvTLS}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s, clientTLS
}

func (s *testSMTPServer) serve(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), conn
	say := func(line string) { _, _ = io.WriteString(w, line+"\r\n") }
	say("220 smtp.test ready")
	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(cmd, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			if !secure {
				say("250-smtp.test\r\n250 STARTTLS")
			} else {
				say("250-smtp.test\r\n250 AUTH PLAIN")
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, s.tlsCfg)
			if tc.Handshake() != nil {
				return
			}
			conn, r, w, secure = tc, bufio.NewReader(tc), tc, true
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, "AUTH PLAIN "))
			s.mu.Lock()
			s.auths = append(s.auths, string(raw))
			fail := s.authErr
			s.mu.Unlock()
			if fail != "" {
				say(fail)
				continue
			}
			say("235 ok")
		case "MAIL":
			say("250 ok")
		case "RCPT":
			addr := strings.TrimSuffix(strings.TrimPrefix(cmd, "RCPT TO:<"), ">")
			s.mu.Lock()
			fail := s.rcptErr[addr]
			s.mu.Unlock()
			if fail != "" {
				say(fail)
				continue
			}
			say("250 ok")
		case "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.mails = append(s.mails, b.String())
			s.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 unsupported")
		}
	}
}

func (s *testSMTPServer) setAuthErr(msg string) {
	s.mu.Lock()
	s.authErr = msg
	s.mu.Unlock()
}

func (s *testSMTPServer) setRcptErr(m map[string]string) {
	s.mu.Lock()
	s.rcptErr = m
	s.mu.Unlock()
}

func (s *testSMTPServer) snapshot() (auths, mails []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auths), slices.Clone(s.mails)
}

func privateTestIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipNet.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if addr.Is4() && addr.IsPrivate() && !addr.IsLoopback() {
			return addr.String()
		}
	}
	return ""
}

func TestAlertEmailThroughAMailConnection(t *testing.T) {
	storetest.Each(t, testAlertEmailThroughAMailConnection)
}

func testAlertEmailThroughAMailConnection(t *testing.T, backend store.Backend) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := startFullConfig(t, backend, nil, logger, nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
	b := signedIn(t, f)
	rec := &recorder{next: b}
	plain := []connect.ClientOption{connect.WithProtoJSON(), connect.WithAcceptCompression("gzip", nil, nil)}
	c := dockwellv1connect.NewChangeServiceClient(rec, b.origin, plain...)
	sites := dockwellv1connect.NewAlertServiceClient(rec, b.origin, plain...)
	eventsClient := dockwellv1connect.NewEventServiceClient(rec, b.origin, plain...)
	resClient := dockwellv1connect.NewResourceServiceClient(rec, b.origin, plain...)

	// Wait for default alert rules.
	eventually(t, "default alert rules", func() bool {
		res, err := sites.ListAlertRules(t.Context(), connect.NewRequest(&dockwellv1.ListAlertRulesRequest{}))
		return err == nil && len(res.Msg.GetRules()) == len(alert.Defaults())
	})

	smtpSrv, roots := startTestSMTP(t, "127.0.0.1")
	const (
		user1 = "pm-secret-user-1"
		pass1 = "pm-secret-pass-1"
		user2 = "pm-secret-user-2"
		pass2 = "pm-secret-pass-2"
	)
	smtpSpec := func(user, pass string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			DisplayName: "Postmark",
			Kind:        resourcev1.LinkKind_LINK_KIND_SMTP,
			Reach:       resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: smtpSrv.host, Port: smtpSrv.port,
				Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS,
				User:     user, Password: pass,
			},
		}
	}

	// Create alert channel + connection in one change (channel listed first).
	made := apply(t, c, propose(t, c, "Add mail connection and alert channel",
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
				To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
					Connection: "postmark", From: "Dockwell <alerts@example.com>", To: []string{"ops@example.com"},
				}},
			}}},
		},
		connectionEdit("postmark", smtpSpec(user1, pass1)),
	))
	if made.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("create channel+conn ended %v: %s", made.GetState(), made.GetError())
	}

	// ListAlertChannels shows connection name, recipient target, and no secrets or sealed fields.
	listed, err := sites.ListAlertChannels(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChannelsRequest{}))
	if err != nil || len(listed.Msg.GetChannels()) != 1 {
		t.Fatalf("ListAlertChannels = %v, %v", listed, err)
	}
	chInfo := listed.Msg.GetChannels()[0]
	if chInfo.GetType() != "email" || chInfo.GetTarget() != "ops@example.com" ||
		chInfo.GetChannel().GetEmail().GetConnection() != "postmark" ||
		chInfo.GetChannel().GetEmail().GetHost() != "" ||
		len(chInfo.GetSealed()) != 0 {
		t.Fatalf("unexpected AlertChannelInfo: %v", chInfo)
	}

	// Drive Evaluator through a real firing alert -> SMTP server receives auth1 + mail.
	at := &clock{now: time.Now()}
	memPct := uint64(99)
	e := evaluatorOn(f, at)
	e.Sender = &alerts.Sender{TLS: roots}
	e.Fleet = evalFleet{func() []*dockwellv1.Server { return connectedAt(at.Now(), memPct) }}
	for range 60 {
		tickOn(t, e)
		at.add(alerts.Every)
		_, mails := smtpSrv.snapshot()
		if len(mails) >= 1 {
			break
		}
	}
	auths, mails := smtpSrv.snapshot()
	if len(mails) != 1 || len(auths) != 1 || auths[0] != "\x00"+user1+"\x00"+pass1 {
		t.Fatalf("first alert send: mails=%d auths=%q", len(mails), auths)
	}

	// Rotate credentials on the connection WITHOUT editing channel ops -> resolved alert uses new credentials.
	apply(t, c, propose(t, c, "Rotate postmark credentials", connectionEdit("postmark", smtpSpec(user2, pass2))))
	memPct = 40
	for range 60 {
		tickOn(t, e)
		at.add(alerts.Every)
		_, mails = smtpSrv.snapshot()
		if len(mails) >= 2 {
			break
		}
	}
	auths, mails = smtpSrv.snapshot()
	if len(mails) != 2 || len(auths) != 2 || auths[1] != "\x00"+user2+"\x00"+pass2 {
		t.Fatalf("post-rotation alert send: mails=%d auths=%q", len(mails), auths)
	}

	// H1: provider AUTH failure reports Dockwell-owned text with status code alone
	// and never echoes the provider reply body, username, password, or base64 blob.
	authBlob := base64.StdEncoding.EncodeToString([]byte("\x00" + user2 + "\x00" + pass2))
	smtpSrv.setAuthErr("535 5.7.8 provider-diagnostic rejected user " + user2 + " password " + pass2 + " raw " + authBlob)
	memPct = 99
	for range 60 {
		tickOn(t, e)
		at.add(alerts.Every)
		auths, _ = smtpSrv.snapshot()
		if len(auths) >= 3 {
			break
		}
	}
	deliveries, err := sites.ListAlertDeliveries(t.Context(), connect.NewRequest(&dockwellv1.ListAlertDeliveriesRequest{PageSize: 20}))
	if err != nil {
		t.Fatal(err)
	}
	foundOwnedAuthErr := false
	for _, d := range deliveries.Msg.GetDeliveries() {
		if !d.GetOk() && strings.Contains(d.GetSaid(), "refused sign-in for connection postmark (535)") {
			foundOwnedAuthErr = true
			for _, secret := range []string{user2, pass2, authBlob, "provider-diagnostic"} {
				if strings.Contains(d.GetSaid(), secret) {
					t.Errorf("delivery Said leaked %q: %q", secret, d.GetSaid())
				}
			}
		}
	}
	if !foundOwnedAuthErr {
		t.Fatalf("expected Dockwell-owned 535 auth failure delivery, got %v", deliveries.Msg.GetDeliveries())
	}

	// H1 on outbox: 240-byte long password and 4-char AUTH LOGIN base64 token echoed
	// in a partial recipient refusal are scrubbed BEFORE clipping.
	longPass := strings.Repeat("LongSec!", 30) // 240 bytes > saidMax (200)
	shortUser := "op"
	b64ShortUser := base64.StdEncoding.EncodeToString([]byte(shortUser)) // "b3A=" (4 chars)
	b64LongPlain := base64.StdEncoding.EncodeToString([]byte("\x00" + shortUser + "\x00" + longPass))
	smtpSrv.setAuthErr("")
	smtpSrv.setRcptErr(map[string]string{
		"bad@example.com": "550 5.1.1 refused user=" + shortUser + " tok=" + b64ShortUser + " pass=" + longPass + " plain=" + b64LongPlain,
	})
	apply(t, c, propose(t, c, "Use long password and two recipients on ops",
		connectionEdit("postmark", smtpSpec(shortUser, longPass)),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
				To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
					Connection: "postmark", From: "Dockwell <alerts@example.com>", To: []string{"ops@example.com", "bad@example.com"},
				}},
			}}},
		},
	))
	memPct = 40
	for range 60 {
		tickOn(t, e)
		at.add(alerts.Every)
		_, mails = smtpSrv.snapshot()
		if len(mails) >= 3 {
			break
		}
	}
	deliveries, err = sites.ListAlertDeliveries(t.Context(), connect.NewRequest(&dockwellv1.ListAlertDeliveriesRequest{PageSize: 20}))
	if err != nil {
		t.Fatal(err)
	}
	foundPartial := false
	for _, d := range deliveries.Msg.GetDeliveries() {
		if d.GetOk() && strings.Contains(d.GetSaid(), "bad@example.com") {
			foundPartial = true
			for _, secret := range []string{"LongSec!", b64ShortUser, b64LongPlain[:12]} {
				if strings.Contains(d.GetSaid(), secret) {
					t.Errorf("outbox partial refusal leaked %q in %q", secret, d.GetSaid())
				}
			}
		}
	}
	if !foundPartial {
		t.Fatalf("expected scrubbed partial refusal delivery on outbox, got %v", deliveries.Msg.GetDeliveries())
	}
	smtpSrv.setRcptErr(nil)

	// TestAlertChannel on saved loopback channel ops is blocked by TestGuard, records
	// the failure on ops's Last delivery, and puts resolved host 127.0.0.1 on Activity.
	testedLoopback, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Name: "ops"}))
	if err != nil {
		t.Fatalf("TestAlertChannel(ops) = %v", err)
	}
	if testedLoopback.Msg.GetDelivery().GetOk() || !strings.Contains(testedLoopback.Msg.GetDelivery().GetSaid(), "this server's own address") {
		t.Errorf("TestAlertChannel(loopback) = %v, want TestGuard loopback refusal", testedLoopback.Msg.GetDelivery())
	}
	evRes, err := eventsClient.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ALERT}, PageSize: 50,
	}))
	if err != nil {
		t.Fatal(err)
	}
	foundTestEvent := false
	for _, ev := range evRes.Msg.GetEvents() {
		if ev.GetAction() == alerts.ActionTest && ev.GetAlert().GetSubjectName() == "ops" {
			foundTestEvent = true
			if ev.GetAlert().GetVariant() != "email" || ev.GetAlert().GetWhere() != "127.0.0.1" {
				t.Errorf("Activity alert.test event = %v, want variant=email where=127.0.0.1", ev.GetAlert())
			}
		}
	}
	if !foundTestEvent {
		t.Error("no alert.test event recorded on Activity for connection-backed channel")
	}

	// If a non-loopback private IPv4 exists on this host, also exercise successful
	// TestAlertChannel, Dockwell-owned AUTH failure, and partial-refusal long-password
	// + 4-char AUTH LOGIN scrubbing through TestAlertChannel.
	const (
		privUser = "op"
	)
	privPass := strings.Repeat("PrivPw9!", 30) // 240 bytes > saidMax (200)
	privB64User := base64.StdEncoding.EncodeToString([]byte(privUser))
	if privIP := privateTestIPv4(); privIP != "" {
		privSrv, privRoots := startTestSMTP(t, privIP)
		f.server.alerts.Sender.TLS = privRoots
		apply(t, c, propose(t, c, "Add private-IP SMTP connection and channel",
			connectionEdit("priv-mail", &resourcev1.ConnectionSpec{
				Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
				Smtp: &resourcev1.ConnectionSmtp{
					Host: privSrv.host, Port: privSrv.port,
					Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS,
					User:     privUser, Password: privPass,
				},
			}),
			&dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "priv-ops"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
					To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
						Connection: "priv-mail", From: "alerts@example.com", To: []string{"ops@example.com"},
					}},
				}}},
			},
		))
		okRes, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Name: "priv-ops"}))
		if err != nil || !okRes.Msg.GetDelivery().GetOk() || okRes.Msg.GetDelivery().GetSaid() != "accepted by "+privIP {
			t.Fatalf("TestAlertChannel(priv-ops) = %v, %v", okRes, err)
		}
		privAuths, privMails := privSrv.snapshot()
		if len(privMails) != 1 || len(privAuths) != 1 || privAuths[0] != "\x00"+privUser+"\x00"+privPass {
			t.Fatalf("priv TestAlertChannel smtp got mails=%d auths=%q", len(privMails), privAuths)
		}
		// H1 on TestAlertChannel: AUTH failure reports Dockwell-owned wording with status code alone.
		privSrv.setAuthErr("535 5.7.8 provider-priv-body " + privUser + " / " + privPass)
		draftRes, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
			Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
				Connection: "priv-mail", From: "alerts@example.com", To: []string{"draft@example.com"},
			}}},
		}))
		if err != nil || draftRes.Msg.GetDelivery().GetOk() || !strings.Contains(draftRes.Msg.GetDelivery().GetSaid(), "refused sign-in for connection priv-mail (535)") {
			t.Fatalf("draft TestAlertChannel auth fail = %v, %v", draftRes, err)
		}
		if strings.Contains(draftRes.Msg.GetDelivery().GetSaid(), "provider-priv-body") || strings.Contains(draftRes.Msg.GetDelivery().GetSaid(), "PrivPw9!") {
			t.Errorf("TestAlertChannel draft leaked provider body/password: %q", draftRes.Msg.GetDelivery().GetSaid())
		}
		// H1 on TestAlertChannel: partial recipient refusal with 240-byte password & 4-char AUTH LOGIN b64.
		privSrv.setAuthErr("")
		privSrv.setRcptErr(map[string]string{
			"bad@example.com": "550 5.1.1 bad tok=" + privB64User + " pass=" + privPass,
		})
		partRes, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
			Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
				Connection: "priv-mail", From: "alerts@example.com", To: []string{"ok@example.com", "bad@example.com"},
			}}},
		}))
		if err != nil || !partRes.Msg.GetDelivery().GetOk() || !strings.Contains(partRes.Msg.GetDelivery().GetSaid(), "bad@example.com") {
			t.Fatalf("TestAlertChannel partial refusal = %v, %v", partRes, err)
		}
		for _, secret := range []string{"PrivPw9!", privB64User} {
			if strings.Contains(partRes.Msg.GetDelivery().GetSaid(), secret) {
				t.Errorf("TestAlertChannel partial refusal leaked %q in %q", secret, partRes.Msg.GetDelivery().GetSaid())
			}
		}
	}

	// TestAlertChannel refuses unsaved drafts naming missing, shared, linked, held, or non-TLS connections.
	env := listedEnv(t, b)
	apply(t, c, propose(t, c, "Seed connections for draft refusal tests",
		connectionEdit("shared-mail", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Shared: true, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "u", Password: "p"},
		}),
		connectionEdit("linked-mail", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "u", Password: "p"},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(link.Kind), Scope: env, Name: "app-mail"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Mail", Connection: "linked-mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
				Sets: []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
			}}},
		},
		connectionEdit("held-mail", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "u", Password: "held-draft-pass"},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: "APP_MAIL_PASS"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "held-draft-pass"}}},
		},
		connectionEdit("plain-auth-mail", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "10.0.0.5", Port: 25, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_NONE, User: "u", Password: "p"},
		}),
	))
	for _, tc := range []struct {
		conn string
		code connect.Code
		want string
	}{
		{"does-not-exist", connect.CodeNotFound, "isn't on this install"},
		{"shared-mail", connect.CodeInvalidArgument, "marked shared"},
		{"linked-mail", connect.CodeInvalidArgument, "used by an app link"},
		{"held-mail", connect.CodeInvalidArgument, "an app holds the password of connection held-mail"},
		{"plain-auth-mail", connect.CodeInvalidArgument, "plain-auth-mail"},
	} {
		_, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
			Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
				Connection: tc.conn, From: "alerts@example.com", To: []string{"ops@example.com"},
			}}},
		}))
		if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("TestAlertChannel(draft %s) = %v, want code=%v containing %q", tc.conn, err, tc.code, tc.want)
		}
	}

	// Read back change, resources, events, deliveries and verify ZERO credentials
	// ever appeared in any API response or debug log.
	if _, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: made.GetId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := resClient.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{PageSize: 1000})); err != nil {
		t.Fatal(err)
	}
	if _, err := eventsClient.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{PageSize: 100})); err != nil {
		t.Fatal(err)
	}
	said := rec.String() + logs.String()
	for _, secret := range []string{user1, pass1, user2, pass2, authBlob, "LongSec!", b64LongPlain[:12], "PrivPw9!"} {
		if strings.Contains(said, secret) {
			t.Errorf("credential %q leaked into API responses or logs", secret)
		}
	}
}

func TestSMTPConnectionHeldAndAppExclusion(t *testing.T) {
	storetest.Each(t, testSMTPConnectionHeldAndAppExclusion)
}

func testSMTPConnectionHeldAndAppExclusion(t *testing.T, backend store.Backend) {
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "ghost-storage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	withMail := strings.Replace(string(raw),
		"      - name: S3_SECRET_ACCESS_KEY\n        part: PART_SECRET_KEY\n",
		"      - name: S3_SECRET_ACCESS_KEY\n        part: PART_SECRET_KEY\n"+
			"  - name: mail\n"+
			"    label: Mail\n"+
			"    kind: KIND_SMTP\n"+
			"    optional: true\n"+
			"    sets:\n"+
			"      - name: SMTP_URL\n"+
			"        part: PART_URL\n",
		1)
	withMail = strings.Replace(withMail,
		"        database__connection__host: ${DB_HOST}\n",
		"        database__connection__host: ${DB_HOST}\n"+
			"        mail__transport: ${SMTP_URL}\n",
		1)
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(withMail), 0o600); err != nil {
		t.Fatal(err)
	}

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	cat := catalogClient(b)
	env := listedEnv(t, b)
	proj := envProject(t, f, env)
	_, _ = webWithData(t, e, c)

	urlSpec := &resourcev1.ConnectionSmtp{
		Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS,
		User: "url-user", Password: "url-held-password",
	}
	apply(t, c, propose(t, c, "Seed SMTP and S3 connections",
		connectionEdit("free-s3", &resourcev1.ConnectionSpec{
			DisplayName: "Free S3", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "free-bkt", AccessKeyId: "AKIA", SecretKey: "s3-secret", PathStyle: true},
		}),
		connectionEdit("alert-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Alerts Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "a", Password: "alert-password"},
		}),
		connectionEdit("alert-mail-alias", &resourcev1.ConnectionSpec{
			DisplayName: "Alerts Mail Alias", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "SMTP.Example.com.", Port: 465, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "a", Password: "alert-password"},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
				To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
					Connection: "alert-mail", From: "alerts@example.com", To: []string{"ops@example.com"},
				}},
			}}},
		},
		connectionEdit("free-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Free Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "f", Password: "free-password"},
		}),
		connectionEdit("held-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Held Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "h", Password: "held-password"},
		}),
		connectionEdit("held-url-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Held URL Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: urlSpec,
		}),
		connectionEdit("linked-app-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Linked App Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.app.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "appu", Password: "app-shared-password"},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(link.Kind), Scope: env, Name: "app-smtp"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Mail", Connection: "linked-app-mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
				Sets: []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
			}}},
		},
		connectionEdit("dup-app-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Dup App Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "SMTP.App.Example.com.", Port: 465, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "appu", Password: "app-shared-password"},
		}),
		connectionEdit("private-mail", &resourcev1.ConnectionSpec{
			DisplayName: "Private Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "p", Password: "private-password"},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: "HELD_PASS"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "held-password"}}},
		},
		// Changed port & TLS scheme (smtps:465) still holds held-url-mail's password (starttls:587) (H3).
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: "HELD_URL"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "smtps://url-user:url-held-password@smtp.example.com:465"}}},
		},
	))

	// Install admin ListConnections marks held-mail, held-url-mail (changed-port URL),
	// and dup-app-mail (same account as linked-app-mail) as Held=true; free-mail as Held=false.
	heldByName := func(client dockwellv1connect.ConnectionServiceClient) map[string]bool {
		t.Helper()
		res, err := client.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, ci := range res.Msg.GetConnections() {
			out[ci.GetName()] = ci.GetHeld()
		}
		return out
	}
	held := heldByName(cc)
	if !held["held-mail"] || !held["held-url-mail"] || !held["dup-app-mail"] || held["free-mail"] || held["alert-mail"] {
		t.Errorf("admin ListConnections Held = %v", held)
	}

	// H3: draft TestAlertChannel refuses dup-app-mail and held-url-mail using the same predicate.
	alertsClient := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	for _, connName := range []string{"dup-app-mail", "held-url-mail"} {
		_, err := alertsClient.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
			Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
				Connection: connName, From: "alerts@example.com", To: []string{"ops@example.com"},
			}}},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "an app holds the password") {
			t.Errorf("draft TestAlertChannel(%s) = %v, want held refusal", connName, err)
		}
	}

	// H4: named TestAlertChannel reads the live channel and connection in one
	// snapshot. Sending Name="ops" with a stale form naming linked-app-mail
	// sees !asStored and refuses it via isolation.
	if _, err := alertsClient.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
		Name: "ops",
		Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Connection: "linked-app-mail", From: "alerts@example.com", To: []string{"ops@example.com"},
		}}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "used by an app link") {
		t.Errorf("named TestAlertChannel with stale app-linked connection = %v", err)
	}

	// H6: a project developer below install admin NEVER gets SMTP Held=true, even
	// when a secret in their project matches the SMTP connection's password.
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: proj, Role: store.RoleDeveloper})
	devHeld := heldByName(cc)
	for _, name := range []string{"held-mail", "held-url-mail", "dup-app-mail", "alert-mail", "free-mail"} {
		if devHeld[name] {
			t.Errorf("H6: project developer saw Held=true on SMTP connection %s: %v", name, devHeld)
		}
	}
	// Restore install admin grant on the shared session for the remaining admin calls.
	hold(store.Grant{ProjectID: store.Install, Role: store.RoleAdmin})

	// Rotating held-mail's password clears Held for admin.
	apply(t, c, propose(t, c, "Rotate held-mail password", connectionEdit("held-mail", &resourcev1.ConnectionSpec{
		DisplayName: "Held Mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "h", Password: "rotated-password"},
	})))
	if heldByName(cc)["held-mail"] {
		t.Error("held-mail still reports Held=true after rotating password")
	}

	// Catalog install sheet excludes alert-mail AND alert-mail-alias (H3), and offers free-mail and free-s3.
	entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	var mailChoices, s3Choices []string
	for _, o := range entry.Msg.GetNeeds() {
		switch o.GetNeed() {
		case "mail":
			mailChoices = append(mailChoices, o.GetChoice())
		case "storage":
			s3Choices = append(s3Choices, o.GetChoice())
		}
	}
	if !slices.Contains(mailChoices, catalog.ChoiceUnset) || !slices.Contains(mailChoices, "connection:free-mail") ||
		slices.Contains(mailChoices, "connection:alert-mail") || slices.Contains(mailChoices, "connection:alert-mail-alias") {
		t.Errorf("catalog mail choices = %v, want free-mail included and alert-mail/alert-mail-alias excluded", mailChoices)
	}
	if !slices.Contains(s3Choices, "connection:free-s3") {
		t.Errorf("catalog storage choices = %v, want free-s3", s3Choices)
	}

	// CheckBeforeLinking refuses alert-mail before probing.
	conns := connections.NewConnections(f.store, f.keeper)
	connSvc := connections.New(conns, f.config.Fleet, f.config.Agents, nil, f.store)
	checked, err := connSvc.CheckBeforeLinking(t.Context(), "alert-mail", f.lastDocker.id.ServerID, env, "web", "", false)
	if checked || err == nil || !strings.Contains(err.Error(), "used for alert email") {
		t.Fatalf("CheckBeforeLinking(alert-mail) = %v, %v", checked, err)
	}

	// Corrupt ops's sealed spec in the store so decoding fails:
	// - SMTP preflight/ForLinking/AlertUses fail closed (no names leaked below admin).
	// - M1: S3-only ForLinking still succeeds, and GetCatalogEntry still succeeds
	//   with S3 choices intact while withholding all SMTP connection choices.
	if backend == store.Postgres {
		storetest.Exec(t, f.db, `DROP TRIGGER revisions_never_change ON revisions`)
		storetest.Exec(t, f.db, `UPDATE revisions SET spec = decode('0a0a', 'hex') WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'alert.channel' AND name = 'ops')`)
	} else {
		storetest.Exec(t, f.db, `DROP TRIGGER revisions_never_change`)
		storetest.Exec(t, f.db, `UPDATE revisions SET spec = x'0a0a' WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'alert.channel' AND name = 'ops')`)
	}

	if checked, err := connSvc.CheckBeforeLinking(t.Context(), "free-mail", f.lastDocker.id.ServerID, env, "web", "", false); checked || err == nil {
		t.Fatalf("CheckBeforeLinking with unreadable alert channel = %v, %v", checked, err)
	}
	// M1: GetCatalogEntry succeeds, keeps free-s3, and leaves out all SMTP choices.
	degraded, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatalf("M1: GetCatalogEntry with unreadable alert channel = %v, want success withholding SMTP choices", err)
	}
	var degradedMail, degradedS3 []string
	for _, o := range degraded.Msg.GetNeeds() {
		switch o.GetNeed() {
		case "mail":
			degradedMail = append(degradedMail, o.GetChoice())
		case "storage":
			degradedS3 = append(degradedS3, o.GetChoice())
		}
	}
	if !slices.Contains(degradedS3, "connection:free-s3") {
		t.Errorf("M1: S3 choices lost when alert channel unreadable: %v", degradedS3)
	}
	for _, ch := range degradedMail {
		if strings.HasPrefix(ch, catalog.ChoiceConnection) {
			t.Errorf("M1: SMTP connection choice %q offered when alert channels unreadable: %v", ch, degradedMail)
		}
	}

	// Below install admin: S3 ForLinking succeeds (M1), out-of-reach SMTP returns ok=false,
	// reached SMTP fails closed without leaking channel or connection names.
	devCtx := place.WithCaller(t.Context(), place.Caller{
		Grants: []store.Grant{{ProjectID: proj, Role: store.RoleDeveloper}},
	})
	if _, backupUsed, alertUsed, ok, err := conns.ForLinking(devCtx, "free-s3", proj); err != nil || !ok || backupUsed || alertUsed {
		t.Fatalf("M1: ForLinking(developer, free-s3) with unreadable alert channel = ok=%v backup=%v alert=%v err=%v", ok, backupUsed, alertUsed, err)
	}
	if _, _, _, ok, err := conns.ForLinking(devCtx, "private-mail", proj); err != nil || ok {
		t.Fatalf("ForLinking(developer, private-mail) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if _, _, _, _, err := conns.ForLinking(devCtx, "free-mail", proj); err == nil || strings.Contains(err.Error(), "ops") || strings.Contains(err.Error(), "free-mail") || strings.Contains(err.Error(), "alert-mail") {
		t.Fatalf("ForLinking(developer, free-mail) with unreadable channel = %v (want fail-closed without names)", err)
	}
	if _, err := conns.AlertUses(devCtx); err == nil || strings.Contains(err.Error(), "ops") || strings.Contains(err.Error(), "alert-mail") {
		t.Fatalf("AlertUses(developer) with unreadable channel = %v (want fail-closed without names)", err)
	}
}
