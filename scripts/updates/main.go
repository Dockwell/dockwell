// SPDX-License-Identifier: AGPL-3.0-only

// Command updates reads the package updates of the system it runs on with the
// agent's own reader, refreshing the lists first, and prints what it found as JSON.
// scripts/docker-e2e.sh runs it in containers of each distribution Dockwell reads
// updates on, so the agent's commands and parsers meet real apt, dnf and apk.
//
//	CGO_ENABLED=0 go build -o /tmp/updates ./scripts/updates
//	docker run --rm -v /tmp/updates:/updates:ro ubuntu:24.04 /updates
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/dockwell/dockwell/internal/agent/updates"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "updates: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	found, err := updates.New(updates.Options{Root: "/", Logger: logger}).Read(ctx, true, true)
	if err != nil {
		return err
	}
	out, err := protojson.Marshal(found)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
