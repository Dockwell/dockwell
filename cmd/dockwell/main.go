// SPDX-License-Identifier: AGPL-3.0-only

// Command dockwell is the single binary every Dockwell server runs.
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
	code := cli.Execute(ctx, os.Args[1:], cli.IO{Out: os.Stdout, Err: os.Stderr})
	stop()
	os.Exit(code)
}
