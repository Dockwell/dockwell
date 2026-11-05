// SPDX-License-Identifier: AGPL-3.0-only

// Command claim claims a new install from the claim link dockwell run
// printed, with a software passkey, as the panel would with a browser's.
// Scripts that run a throwaway install through the CLI, such as
// scripts/docker-e2e.sh, need an owner for local access to act for. It is
// never part of a release.
//
//	go run ./scripts/claim http://localhost:7697/claim#<token> [cookie file]
//
// With a cookie file, it writes the session the claim signs in with there, as a
// Cookie header's value, for what only a person may do, such as applying a plan
// that needs one.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
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
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/claim <claim link> [cookie file]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cookieFile := ""
	if len(os.Args) == 3 {
		cookieFile = os.Args[2]
	}
	err := claim(ctx, os.Args[1], cookieFile)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claim:", err)
		os.Exit(1)
	}
}

// claim claims the install at link. The panel answers on loopback under
// localhost, as the link says, so the client dials 127.0.0.1 and sends the
// link's host and origin, which the panel checks.
func claim(ctx context.Context, link, cookieFile string) error {
	u, err := url.Parse(link)
	if err != nil || u.Fragment == "" || !strings.HasSuffix(u.Path, "/claim") {
		return fmt.Errorf("%q isn't a claim link, such as http://localhost:7697/claim#<token>. Copy it from what dockwell run printed", link)
	}
	base := u.Scheme + "://" + u.Host
	var d net.Dialer
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := &http.Client{Jar: jar, Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
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
	if cookieFile == "" {
		return nil
	}
	session := jar.Cookies(u)
	if len(session) == 0 {
		return errors.New("the claim signed in with no session cookie, so there's nothing to write")
	}
	var header []string
	for _, c := range session {
		header = append(header, c.Name+"="+c.Value)
	}
	if err := os.WriteFile(cookieFile, []byte(strings.Join(header, "; ")), 0o600); err != nil { //nolint:gosec // a path the test's own caller names
		return fmt.Errorf("can't write the session to %s: %w", cookieFile, err)
	}
	return nil
}
