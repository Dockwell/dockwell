// SPDX-License-Identifier: AGPL-3.0-only

// Command claim claims a new install from the claim link dockwell run
// printed, with a software passkey, as the panel would with a browser's.
// Scripts that run a throwaway install through the CLI, such as
// scripts/docker-e2e.sh, need an owner for local access to act for. It is
// never part of a release.
//
//	go run ./scripts/claim http://localhost:7697/claim#<token>
package main

import (
	"context"
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
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/claim <claim link>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := claim(ctx, os.Args[1])
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claim:", err)
		os.Exit(1)
	}
}

// claim claims the install at link. The panel answers on loopback under
// localhost, as the link says, so the client dials 127.0.0.1 and sends the
// link's host and origin, which the panel checks.
func claim(ctx context.Context, link string) error {
	u, err := url.Parse(link)
	if err != nil || u.Fragment == "" || !strings.HasSuffix(u.Path, "/claim") {
		return fmt.Errorf("%q isn't a claim link, such as http://localhost:7697/claim#<token>. Copy it from what dockwell run printed", link)
	}
	base := u.Scheme + "://" + u.Host
	var d net.Dialer
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", u.Port()))
	}}}
	origin := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Origin", base)
			return next(ctx, req)
		}
	})
	auth := dockwellv1connect.NewAuthServiceClient(client, base, connect.WithInterceptors(origin))
	begin, err := auth.BeginClaim(ctx, connect.NewRequest(&dockwellv1.BeginClaimRequest{
		Token: u.Fragment, Name: "Dockwell tests", Email: "tests@dockwell.invalid",
	}))
	if err != nil {
		return fmt.Errorf("can't begin the claim: %w", err)
	}
	cred, _, err := (&authtest.Authenticator{}).Create(begin.Msg.GetOptionsJson(), base)
	if err != nil {
		return fmt.Errorf("can't make a passkey: %w", err)
	}
	if _, err := auth.FinishClaim(ctx, connect.NewRequest(&dockwellv1.FinishClaimRequest{
		CeremonyId: begin.Msg.GetCeremonyId(), Token: u.Fragment, CredentialJson: cred,
	})); err != nil {
		return fmt.Errorf("can't finish the claim: %w", err)
	}
	return nil
}
