// SPDX-License-Identifier: AGPL-3.0-only

package enroll_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/ticket"
)

// silent accepts connections and never says anything, as a firewall that drops what
// comes after does, and returns its URL.
func silent(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
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
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	return "https://" + ln.Addr().String()
}

// An address that takes the connection and never answers goes on to the next in a
// second, not the ten a TLS handshake otherwise gets.
func TestAJoinGoesOnFromAnAddressThatDropsWhatItsSent(t *testing.T) {
	defer enroll.SetJoinBounds(time.Second, 300*time.Millisecond)()
	cp := startControlPlane(t)
	tk, err := cp.auth.CreateTicket(t.Context(), silent(t), "cli:test", time.Hour, cp.url)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	id, err := enroll.Join(t.Context(), tk.Value, "hz-2", buildinfo.Read())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("it took %v to go on", took)
	}
	if id.ControlURL != cp.url {
		t.Errorf("it joined through %s", id.ControlURL)
	}
}

// slowJoin answers a join after a while, as a loaded control plane does, signing the
// key with ca.
type slowJoin struct {
	agentv1connect.UnimplementedAgentServiceHandler
	ca   *pki.CA
	wait time.Duration
}

func (s slowJoin) Join(ctx context.Context, req *connect.Request[agentv1.JoinRequest]) (*connect.Response[agentv1.JoinResponse], error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.wait):
	}
	cert, err := s.ca.SignServer(req.Msg.GetCsr(), "srv_slow", time.Now())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentv1.JoinResponse{
		ServerId: "srv_slow", ServerName: "hz-2", CaCertificate: s.ca.Certificate().Raw, Certificate: cert.Raw,
	}), nil
}

// Once the control plane has the ticket, a join waits for its answer, however long the
// bounds on reaching it are: going on to the next address would find the ticket spent.
func TestAJoinWaitsForAControlPlaneThatsSlowToAnswer(t *testing.T) {
	defer enroll.SetJoinBounds(200*time.Millisecond, 200*time.Millisecond)()
	ca, err := pki.NewCA("ins_test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	serving, err := ca.IssueServing([]string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(agentv1connect.NewAgentServiceHandler(slowJoin{ca: ca, wait: time.Second}))
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serving}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	secret := make([]byte, ticket.SecretSize)
	_, _ = rand.Read(secret)
	value := ticket.Encode(&agentv1.JoinTicket{
		ControlUrl: srv.URL, OtherUrls: []string{silent(t)},
		CaPin: ca.Pin(), TokenId: "tok_slow", Secret: secret,
	})
	id, err := enroll.Join(t.Context(), value, "hz-2", buildinfo.Read())
	if err != nil {
		t.Fatalf("it gave up on a control plane that had its ticket: %v", err)
	}
	if !strings.HasPrefix(id.ControlURL, "https://127.0.0.1:") || id.ServerID != "srv_slow" {
		t.Errorf("it joined %s through %s", id.ServerID, id.ControlURL)
	}
}
