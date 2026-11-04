// SPDX-License-Identifier: AGPL-3.0-only

// Command dockwell is the binary every Dockwell server runs, in one of two
// builds from this source. Built as it is, it is the full build, which the
// control server runs: the control plane, its own agent, the panel and every
// command. Built with -tags agent, it is the agent build, which runs only
// the agent, for every server that joins. See
// docs/adr/0023-a-full-build-and-an-agent-build.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/dockwell/dockwell/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first signal starts a graceful shutdown. Restoring the default
	// handling right away means a second one stops the process at once.
	context.AfterFunc(ctx, stop)

	code := cli.Execute(ctx, os.Args[1:], cli.IO{Out: os.Stdout, Err: os.Stderr})
	stop()
	os.Exit(code)
}
