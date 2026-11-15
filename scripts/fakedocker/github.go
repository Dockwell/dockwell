// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/dockwell/dockwell/internal/control/github/ghtest"
)

// serveGitHub serves a stand-in for GitHub's pages and API at addr, which the
// control plane connects as an App in place of GitHub's (DOCKWELL_TEST_GITHUB,
// ADR 0098).
func serveGitHub(addr string) {
	gh, err := ghtest.New()
	if err != nil {
		log.Fatalf("can't make GitHub's stand-in: %v", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Fatalf("can't serve GitHub's stand-in at the address FAKEDOCKER_GITHUB names: %v", err)
	}
	srv := &http.Server{Handler: gh, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
}
