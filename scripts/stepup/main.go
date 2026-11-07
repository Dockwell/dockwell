// SPDX-License-Identifier: AGPL-3.0-only

// Command stepup confirms an action with the passkey scripts/claim made, as a
// browser does with a person's, for a script that does what takes a passkey, such as
// scripts/docker-e2e.sh applying a plan that's as good as root. It goes through the
// same BeginStepUp and FinishStepUp the panel does, for the session the claim signed
// in with, and prints the proof. It is never part of a release.
//
//	go run ./scripts/stepup http://localhost:7697 <cookie file> <passkey file> STEP_UP_ACTION_APPLY_ROOT_CHANGE chg_…
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
	if len(os.Args) < 5 || len(os.Args) > 6 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/stepup <panel origin> <cookie file> <passkey file> <action> [target]")
		os.Exit(2)
	}
	target := ""
	if len(os.Args) == 6 {
		target = os.Args[5]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	proof, err := stepUp(ctx, os.Args[1], os.Args[2], os.Args[3], os.Args[4], target)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stepup:", err)
		os.Exit(1)
	}
	fmt.Println(proof)
}

// stepUp confirms action on target, in the session in cookieFile, with the passkey in
// passkeyFile. The panel answers on loopback under localhost, as scripts/claim's link
// says, so the client dials 127.0.0.1 and sends the origin, which the panel checks.
func stepUp(ctx context.Context, origin, cookieFile, passkeyFile, action, target string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q isn't the panel's origin, such as http://localhost:7697", origin)
	}
	value, ok := dockwellv1.StepUpAction_value[action]
	if !ok {
		return "", fmt.Errorf("%q isn't a step-up action, such as STEP_UP_ACTION_APPLY_ROOT_CHANGE", action)
	}
	cookie, err := os.ReadFile(cookieFile) //nolint:gosec // a path the script gave
	if err != nil {
		return "", fmt.Errorf("can't read the session from %s: %w", cookieFile, err)
	}
	exported, err := os.ReadFile(passkeyFile) //nolint:gosec // a path the script gave
	if err != nil {
		return "", fmt.Errorf("can't read the passkey from %s: %w", passkeyFile, err)
	}
	cred, err := authtest.ImportCredential(exported)
	if err != nil {
		return "", fmt.Errorf("can't read the passkey in %s: %w", passkeyFile, err)
	}
	var d net.Dialer
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", u.Port()))
	}}}
	asPerson := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Origin", origin)
			req.Header().Set("Cookie", strings.TrimSpace(string(cookie)))
			return next(ctx, req)
		}
	})
	auth := dockwellv1connect.NewAuthServiceClient(client, origin, connect.WithInterceptors(asPerson))
	begin, err := auth.BeginStepUp(ctx, connect.NewRequest(&dockwellv1.BeginStepUpRequest{Action: dockwellv1.StepUpAction(value), Target: target}))
	if err != nil {
		return "", fmt.Errorf("can't begin confirming: %w", err)
	}
	answer, err := (&authtest.Authenticator{}).Get(begin.Msg.GetOptionsJson(), origin, cred)
	if err != nil {
		return "", fmt.Errorf("the passkey can't answer: %w", err)
	}
	done, err := auth.FinishStepUp(ctx, connect.NewRequest(&dockwellv1.FinishStepUpRequest{CeremonyId: begin.Msg.GetCeremonyId(), CredentialJson: answer}))
	if err != nil {
		return "", fmt.Errorf("can't finish confirming: %w", err)
	}
	return done.Msg.GetProof(), nil
}
