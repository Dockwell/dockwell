// SPDX-License-Identifier: AGPL-3.0-only

// Package agent is the agent role: the part of dockwell that runs on every
// server, holds the server's identity, and (as later work lands) streams
// its state to the control plane and runs its workloads.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/pki"
)

// Config configures Run.
type Config struct {
	Logger *slog.Logger
	// Dir is the agent's data directory, holding its identity.
	Dir string
	// Identity is this server's identity, loaded from Dir.
	Identity *enroll.Identity
}

// retryAfter bounds how long the agent waits after a failed renewal.
const retryAfter = 5 * time.Minute

// Run keeps the agent's certificate fresh until ctx is done: it renews at
// two thirds of the certificate's life, and on failure retries with jitter
// while the current certificate is still valid.
func Run(ctx context.Context, cfg Config) error {
	id := cfg.Identity
	log := cfg.Logger.With(slog.String("server", id.ServerID))
	log.Info("agent running", slog.String("name", id.ServerName), slog.String("control_plane", id.ControlURL),
		slog.Time("certificate_expires", id.Cert.NotAfter))
	for {
		wait := time.Until(pki.RenewAt(id.Cert))
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
		}
		err := renew(ctx, cfg)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // stopping; a renewal cut short isn't a failure
		}
		if err != nil {
			left := time.Until(id.Cert.NotAfter)
			log.Warn("can't renew the server certificate; retrying", slog.String("error", err.Error()),
				slog.Duration("valid_for", left.Round(time.Minute)))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(jitter(retryAfter)):
			}
		}
	}
}

func renew(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := cfg.Identity.Renew(ctx); err != nil {
		return err
	}
	if err := cfg.Identity.Save(cfg.Dir); err != nil {
		return fmt.Errorf("renewed, but can't save the new certificate: %w", err)
	}
	cfg.Logger.Info("renewed the server certificate", slog.Time("expires", cfg.Identity.Cert.NotAfter))
	return nil
}

// jitter spreads retries between 50% and 100% of d, so servers that lost the
// control plane together don't all return at once.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2))) //nolint:gosec // timing jitter, not security
}
