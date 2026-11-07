// SPDX-License-Identifier: AGPL-3.0-only

// Command shell opens a shell in a container as the person scripts/claim signed in,
// through the same TerminalService the panel uses, confirming with the passkey the
// claim made where the server asks for one. It types a line, then exit, and prints
// whether a passkey was asked for, then all the shell printed. It's for
// scripts/docker-e2e.sh, and is never part of a release.
//
//	go run ./scripts/shell http://localhost:7697 <cookie file> <passkey file> srv_… web-1 'echo $0'
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth/authtest"
)

func main() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/shell <panel origin> <cookie file> <passkey file> <server ID> <container> <line>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	asked, out, err := run(ctx, os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6])
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shell: through %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	fmt.Printf("passkey: %t\n%s", asked, out)
}

// asPerson sends what a browser on the panel's origin does: its session, and the origin.
type asPerson struct {
	origin, cookie string
	next           http.RoundTripper
}

func (a asPerson) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Origin", a.origin)
	r.Header.Set("Cookie", a.cookie)
	return a.next.RoundTrip(r)
}

func run(ctx context.Context, origin, cookieFile, passkeyFile, server, container, line string) (bool, string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false, "", fmt.Errorf("%q isn't the panel's origin, such as http://localhost:7697", origin)
	}
	cookie, err := os.ReadFile(cookieFile) //nolint:gosec // a path the script gave
	if err != nil {
		return false, "", fmt.Errorf("can't read the session from %s: %w", cookieFile, err)
	}
	exported, err := os.ReadFile(passkeyFile) //nolint:gosec // a path the script gave
	if err != nil {
		return false, "", fmt.Errorf("can't read the passkey from %s: %w", passkeyFile, err)
	}
	cred, err := authtest.ImportCredential(exported)
	if err != nil {
		return false, "", fmt.Errorf("can't read the passkey in %s: %w", passkeyFile, err)
	}
	var d net.Dialer
	client := &http.Client{Transport: asPerson{origin: origin, cookie: strings.TrimSpace(string(cookie)), next: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", u.Port()))
		},
	}}}
	terminals := dockwellv1connect.NewTerminalServiceClient(client, origin)
	auth := dockwellv1connect.NewAuthServiceClient(client, origin)
	open := func(proof string) (*dockwellv1.Terminal, error) {
		res, err := terminals.OpenTerminal(ctx, connect.NewRequest(&dockwellv1.OpenTerminalRequest{
			ServerId: server, Container: container, Columns: 120, Rows: 40, Proof: proof,
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetTerminal(), nil
	}
	asked := false
	term, err := open("")
	var cerr *connect.Error
	if errors.As(err, &cerr) && cerr.Meta().Get("Dockwell-Error") == "needs_step_up" {
		asked = true
		begin, err := auth.BeginStepUp(ctx, connect.NewRequest(&dockwellv1.BeginStepUpRequest{
			Action: dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_TERMINAL, Target: server + ":" + container,
		}))
		if err != nil {
			return asked, "", fmt.Errorf("can't begin confirming: %w", err)
		}
		answer, err := (&authtest.Authenticator{}).Get(begin.Msg.GetOptionsJson(), origin, cred)
		if err != nil {
			return asked, "", fmt.Errorf("the passkey can't answer: %w", err)
		}
		done, err := auth.FinishStepUp(ctx, connect.NewRequest(&dockwellv1.FinishStepUpRequest{CeremonyId: begin.Msg.GetCeremonyId(), CredentialJson: answer}))
		if err != nil {
			return asked, "", fmt.Errorf("can't finish confirming: %w", err)
		}
		term, err = open(done.Msg.GetProof())
		if err != nil {
			return asked, "", err
		}
	} else if err != nil {
		return asked, "", err
	}
	stream, err := terminals.AttachTerminal(ctx, connect.NewRequest(&dockwellv1.AttachTerminalRequest{TerminalId: term.GetId(), Columns: 120, Rows: 40}))
	if err != nil {
		return asked, "", err
	}
	defer func() { _ = stream.Close() }()
	var out bytes.Buffer
	typed := false
	for stream.Receive() {
		msg := stream.Msg()
		out.Write(msg.GetData())
		if msg.GetAttached() && !typed {
			typed = true
			text := line + "\nexit\n"
			if _, err := terminals.WriteTerminal(ctx, connect.NewRequest(&dockwellv1.WriteTerminalRequest{
				TerminalId: term.GetId(), Offset: msg.GetInputOffset(), Data: []byte(text),
			})); err != nil {
				return asked, out.String(), err
			}
		}
		if end := msg.GetEnd(); end != nil {
			if end.GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED {
				return asked, out.String(), fmt.Errorf("the shell ended as %s: %s", end.GetReason(), end.GetMessage())
			}
			return asked, out.String(), nil
		}
	}
	return asked, out.String(), fmt.Errorf("the shell's stream ended before it did: %w", stream.Err())
}
