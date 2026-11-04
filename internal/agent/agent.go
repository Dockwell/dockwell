// SPDX-License-Identifier: AGPL-3.0-only

// Package agent is the agent role: the part of dockwell that runs on every
// server. It holds the server's identity and keeps it fresh, and keeps one
// stream open to the control plane, reporting what the server is and how
// busy it is. The control plane calls the agent's drivers over the same
// stream, to plan, apply and observe the resources on the server.
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
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
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
	// Collector samples the server. Nil means this machine.
	Collector *collect.Collector
	// Drivers are the kinds of resource this agent manages, which it
	// reports in its hello. Nil manages none: the Docker and firewall
	// drivers come in M3 (#26, #27).
	Drivers *resource.Registry
	// Desired hears of each step the agent applies. Nil keeps nothing.
	Desired Desired
}

// Desired keeps what the control plane told this agent to apply, so the
// server can reconcile toward it while the control plane is away (PRD
// principle 5). The agent tells it after each step it applies and
// verifies. The cache itself, and reconciling from it, come with #32; this
// is where it plugs in.
type Desired interface {
	// Applied records a resource's desired spec; nil means it was deleted.
	Applied(ctx context.Context, ref resource.Ref, spec proto.Message) error
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
	// endWait bounds how long a stream that ends on purpose waits for the
	// control plane to end it.
	endWait = 10 * time.Second
)

// Run keeps the server's certificate fresh and its stream to the control
// plane open, until ctx is done.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Collector == nil {
		cfg.Collector = collect.New()
	}
	if cfg.Drivers == nil {
		cfg.Drivers = resource.NewRegistry()
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
func session(ctx context.Context, cfg Config) (err error) {
	renewed := cfg.Identity.Renewed()
	client, release, err := cfg.Identity.Client()
	if err != nil {
		return err
	}
	defer release() // after the stream closes, so its connection goes too
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Connect(ctx)
	defer func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() }()

	kinds := cfg.Drivers.Kinds()
	hello := &agentv1.Hello{Build: cfg.Build.Proto(), Inventory: cfg.Collector.Inventory(ctx)}
	for _, k := range kinds {
		hello.Kinds = append(hello.Kinds, string(k))
	}
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: hello}}); err != nil {
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

	// The stream carries stats up and calls down, with their results back
	// up. Sends take turns.
	var sendMu sync.Mutex
	send := func(m *agentv1.ConnectRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}
	// The calls outlive the stream's context, which stops the receiver, so
	// the stream ends in order: refuse new calls, stop receiving, then
	// cancel what still runs and wait for it.
	calls := newCalls(parent, cfg, send)
	recvErr := make(chan error, 1)
	received := make(chan struct{})
	go func() {
		defer close(received)
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			switch {
			case msg.GetCall() != nil:
				calls.start(msg.GetCall())
			case msg.GetCancel() != nil:
				calls.cancel(msg.GetCancel().GetId())
			case msg.GetDrained() != nil:
				calls.acknowledged()
			}
		}
	}()
	defer func() {
		calls.close()
		// Half-close first, so what was sent goes out. Moving to a renewed
		// certificate, the stream ends on purpose: the control plane ends
		// it once it has read everything, the last results included, which
		// closing the response at once could drop. Closing the response
		// then ends a Receive under way, and the stream's context, which
		// drops what hasn't gone out, is cancelled last.
		_ = stream.CloseRequest()
		if errors.Is(err, errRenewed) {
			wait := time.NewTimer(endWait)
			select {
			case <-received:
			case <-wait.C:
			}
			wait.Stop()
		}
		_ = stream.CloseResponse()
		<-received
		calls.stop()
	}()

	// When the control plane ends the stream, Send only says EOF; the
	// reason comes from Receive.
	sendStats := func() error {
		err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Stats{Stats: cfg.Collector.Stats(time.Now())}})
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
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-renewed:
			// Finish the calls under way first, so moving the stream
			// doesn't cut a step off halfway.
			if err := calls.drain(ctx, recvErr, ticker.C, sendStats); err != nil {
				return err
			}
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
