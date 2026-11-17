// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
)

// cloudflareToken is the made-up API token Cloudflare's stand-in takes, for
// example.com and example.org (ADR 0088).
const cloudflareToken = "e2e-cloudflare-token" //nolint:gosec // a test's made-up token

// The e2e tunnel and its run's connector, as web/e2e's stand-in /proc names them.
const (
	e2eTunnel    = "3f2d1c0b-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
	e2eConnector = "e2e-connector-1"
)

// serveCloudflare serves a stand-in for Cloudflare's API at addr, which the control
// plane writes DNS through in place of Cloudflare's (DOCKWELL_TEST_CLOUDFLARE).
func serveCloudflare(addr string) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Fatalf("can't serve Cloudflare's stand-in at the address FAKEDOCKER_CLOUDFLARE names: %v", err)
	}
	api := cftest.New(cloudflareToken, "example.com", "example.org")
	// The token may publish through tunnels too, and the account holds web-1, a
	// tunnel connected from the e2e server, whose run the agent finds in the stand-in
	// /proc (DOCKWELL_TEST_PROC) with this connector's ID (ADR 0104).
	api.Tokens[cloudflareToken] = cftest.Token{
		Reads: []string{"example.com", "example.org"}, Writes: []string{"example.com", "example.org"}, Account: true,
		Rights: []string{"Zone Read", "DNS Write", "Zone Settings Read", "Zone Settings Write", "Cloudflare Tunnel Write", "Account API Tokens Read"},
	}
	api.AddTunnel(&cftest.Tunnel{
		ID: e2eTunnel, Name: "web-1", Version: 1, Connectors: []cftest.Connector{{ID: e2eConnector}},
		Config: json.RawMessage(`{"ingress":[{"service":"http_status:404"}]}`),
	})
	srv := &http.Server{Handler: api, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
}
