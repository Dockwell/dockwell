// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"log/slog"
	"net"
	"time"
)

// listenForIngressWhenItCan is the panel's socket, as listenWhenItCan listens: the
// panel goes without its domain until it can, as the SSH tunnel to the panel goes
// with dockwell run.
func listenForIngressWhenItCan(logger *slog.Logger, path string, every time.Duration, listen func() (net.Listener, error)) net.Listener {
	return listenWhenItCan(path, every, listenWords{
		cant: func(path string, err error, every time.Duration) {
			logger.Warn("can't listen for what the ingress sends the panel, so the panel doesn't answer on its domain until the agent can. "+
				"The SSH tunnel to the panel works, and this is tried again. If it goes on, something keeps taking the socket: look at what's running in the ingress's container",
				slog.String("socket", path), slog.String("error", err.Error()), slog.Duration("every", every))
		},
		again: func(path string) {
			logger.Info("listening for what the ingress sends the panel, so the panel answers on its domain again", slog.String("socket", path))
		},
	}, listen)
}
