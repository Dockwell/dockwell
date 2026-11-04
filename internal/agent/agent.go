// SPDX-License-Identifier: AGPL-3.0-only

// Package agent is the agent role: the part of dockwell that runs on every
// server. It holds the server's identity and keeps it fresh, and keeps one
// stream open to the control plane, reporting what the server is and how
// busy it is. Later work runs workloads over the same stream.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/pki"
)

// Config configures Run.
type Config struct {
	Logger *slog.Logger
	// Dir is the agent's data directory, holding its identity.
	Dir string
	// Identity is this server's identity, loaded from Dir.
	Identity *enroll.Identity
	// Build is reported to the control plane.
	Build buildinfo.Info
	// Started, when set, is called once, when the first stream is open
	// and has sent its first stats: the server's startup is over.
	Started func()
	// Collector samples the server. Nil means this machine.
	Collector *collect.Collector
}

// Timing.
const (
	// retryAfter bounds how long the agent waits after a failed renewal.
	retryAfter = 5 * time.Minute
	// Reconnects back off from minBackoff to maxBackoff, and reset after a
	// session that lasted stableSession.
	minBackoff    = time.Second
	maxBackoff    = 30 * time.Second
	stableSession = time.Minute
	// defaultStatsInterval applies until the control plane says otherwise.
	defaultStatsInterval = 5 * time.Second
)

// Run keeps the server's certificate fresh and its stream to the control
// plane open, until ctx is done.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Started != nil {
		cfg.Started = sync.OnceFunc(cfg.Started)
	}
	if cfg.Collector == nil {
		cfg.Collector = collect.New()
	}
	id := cfg.Identity
	cfg.Logger = cfg.Logger.With(slog.String("server", id.ServerID))
	cfg.Logger.Info("agent running", slog.String("name", id.ServerName), slog.String("control_plane", id.ControlURL),
		slog.Time("certificate_expires", id.Cert().NotAfter))

	var wg sync.WaitGroup
	wg.Go(func() { keepRenewed(ctx, cfg) })
	wg.Go(func() { keepConnected(ctx, cfg) })
	wg.Wait()
	return nil
}

// keepRenewed renews at two thirds of the certificate's life, and on
// failure retries with jitter while the current certificate is still valid.
func keepRenewed(ctx context.Context, cfg Config) {
	id := cfg.Identity
	for {
		if !sleep(ctx, time.Until(pki.RenewAt(id.Cert()))) {
			return
		}
		err := renew(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			cfg.Logger.Warn("can't renew the server certificate; retrying", slog.String("error", err.Error()),
				slog.String("valid_for", human.Duration(time.Until(id.Cert().NotAfter).Round(time.Minute))))
			if !sleep(ctx, jitter(retryAfter)) {
				return
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
	cfg.Logger.Info("renewed the server certificate", slog.Time("expires", cfg.Identity.Cert().NotAfter))
	return nil
}

// keepConnected holds the stream open, reconnecting with jittered
// exponential backoff whenever it drops.
func keepConnected(ctx context.Context, cfg Config) {
	backoff := minBackoff
	for {
		start := time.Now()
		err := session(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errRenewed) {
			cfg.Logger.Info("moving the stream to the renewed certificate")
			continue
		}
		if time.Since(start) >= stableSession {
			backoff = minBackoff
		}
		wait := jitter(backoff)
		level, hint := slog.LevelWarn, "reconnecting"
		if connect.CodeOf(err) == connect.CodePermissionDenied {
			// Removed servers stay quiet, but keep trying in case they come back.
			level, hint, backoff = slog.LevelError, "this server was removed from Dockwell; join it again with a new ticket", maxBackoff
		}
		cfg.Logger.Log(ctx, level, "lost the control plane", slog.String("error", errText(err)), slog.String("next", hint),
			slog.String("retry_in", human.Duration(wait.Round(100*time.Millisecond))))
		if !sleep(ctx, wait) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// errRenewed ends a session whose certificate was renewed, so the next one
// connects with the new certificate.
var errRenewed = errors.New("the certificate was renewed")

// session runs one stream: hello, welcome, then stats until it breaks or
// the certificate is renewed.
func session(ctx context.Context, cfg Config) error {
	renewed := cfg.Identity.Renewed()
	client, release, err := cfg.Identity.Client()
	if err != nil {
		return err
	}
	defer release() // after the stream closes, so its connection goes too
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Connect(ctx)
	defer func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() }()

	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{
		Build:     cfg.Build.Proto(),
		Inventory: cfg.Collector.Inventory(ctx),
	}}}); err != nil {
		if errors.Is(err, io.EOF) {
			// The control plane ended the stream; Receive says why.
			_, err = stream.Receive()
		}
		return err
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	welcome := first.GetWelcome()
	if welcome == nil {
		return errors.New("the control plane didn't answer the hello with a welcome")
	}
	interval := defaultStatsInterval
	if d := welcome.GetStatsInterval().AsDuration(); d >= time.Second {
		interval = d
	}
	cfg.Logger.Info("connected to the control plane", slog.String("as", welcome.GetServerName()),
		slog.String("stats_every", human.Duration(interval)))

	// The control plane only closes the stream for now; later it sends work.
	recvErr := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Receive(); err != nil {
				recvErr <- err
				return
			}
		}
	}()

	// When the control plane ends the stream, Send only says EOF; the
	// reason comes from Receive.
	sendStats := func() error {
		err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Stats{Stats: cfg.Collector.Stats(time.Now())}})
		if !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case err := <-recvErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if err := sendStats(); err != nil {
		return err
	}
	if cfg.Started != nil {
		cfg.Started()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-renewed:
			return errRenewed
		case err := <-recvErr:
			return err
		case <-ticker.C:
			if err := sendStats(); err != nil {
				return err
			}
		}
	}
}

// sleep waits for d or ctx, and reports whether to carry on.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter spreads waits between 50% and 100% of d, so servers that lost the
// control plane together don't all return at once.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) //nolint:gosec // timing jitter, not security
}

func errText(err error) string {
	if err == nil {
		return "the stream ended"
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}
