// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
)

// cloudflareToken is the made-up API token Cloudflare's stand-in takes, for
// example.com and example.org (ADR 0088).
const cloudflareToken = "e2e-cloudflare-token" //nolint:gosec // a test's made-up token

// serveCloudflare serves a stand-in for Cloudflare's API at addr, which the control
// plane writes DNS through in place of Cloudflare's (DOCKWELL_TEST_CLOUDFLARE).
func serveCloudflare(addr string) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Fatalf("can't serve Cloudflare's stand-in at the address FAKEDOCKER_CLOUDFLARE names: %v", err)
	}
	srv := &http.Server{Handler: cftest.New(cloudflareToken, "example.com", "example.org"), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
}
