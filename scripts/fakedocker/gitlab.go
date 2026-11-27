// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
)

// serveGitLab serves a stand-in for GitLab's REST API v4 at addr, which the
// control plane uses in place of GitLab (DOCKWELL_TEST_GITLAB, ADR 0111).
func serveGitLab(addr string) {
	gl := gltest.New()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Fatalf("can't serve GitLab's stand-in at the address FAKEDOCKER_GITLAB names: %v", err)
	}
	srv := &http.Server{Handler: gl, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
}
