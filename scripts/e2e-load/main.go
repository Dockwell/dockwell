// SPDX-License-Identifier: AGPL-3.0-only

// Command e2e-load is both ends of the load scripts/docker-e2e.sh puts on a
// rollout, on the code the tests against real Caddy use (internal/loadtest). It
// is never part of a release.
//
//	e2e-load serve
//
// is the app, in a container: on port 8080, or E2E_PORT, it answers each kind of
// request.
// E2E_READY_AFTER is how long it answers everything with a 503 for, as an app that
// takes a while to come up, and E2E_NEVER_READY is what it says for ever instead,
// with what E2E_DATABASE holds after it, as an app's message might.
// So a request that reaches it before it turns healthy counts as a failure. A
// SIGTERM ends it as an app that handles one: it stops listening, and finishes what
// it has.
//
//	e2e-load check
//
// is that app's health check as a command, for a check Docker runs.
//
//	e2e-load drive --domain app.example.net --addr 127.0.0.1:443 --roots roots.pem --report load.json
//
// makes the requests, until a SIGTERM, then writes what it counted.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/loadtest"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("say serve, check or drive, as its source says"))
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "check":
		err = check()
	case "drive":
		err = drive(os.Args[2:])
	default:
		err = fmt.Errorf("there's no %q. Say serve, check or drive", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "e2e-load:", err)
	os.Exit(1)
}

// port is where the app listens.
func port() string {
	if p := os.Getenv("E2E_PORT"); p != "" {
		return p
	}
	return "8080"
}

func serve() error {
	wait, err := readyAfter()
	if err != nil {
		return err
	}
	never := os.Getenv("E2E_NEVER_READY")
	if d := os.Getenv("E2E_DATABASE"); never != "" && d != "" {
		never += ": " + d
	}
	started := time.Now()
	name, _ := os.Hostname()
	app := &loadtest.App{Name: name, Ready: func() (int, string) {
		switch {
		case never != "":
			return http.StatusServiceUnavailable, never
		case time.Since(started) < wait:
			return http.StatusServiceUnavailable, "still coming up"
		}
		return http.StatusOK, "ok"
	}}
	answers := app.Handler()
	srv := &http.Server{Addr: ":" + port(), ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, say := app.Ready(); status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, say)
			return
		}
		answers.ServeHTTP(w, r)
	})}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		grace, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = srv.Shutdown(grace)
		app.CloseWebSockets()
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as Shutdown starts, with what is in flight to go.
	<-done
	return nil
}

func readyAfter() (time.Duration, error) {
	s := os.Getenv("E2E_READY_AFTER")
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("E2E_READY_AFTER is %q, and should be a duration such as 8s", s)
	}
	return d, nil
}

func check() error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:"+port()+"/healthz", http.NoBody)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	said, _ := io.ReadAll(io.LimitReader(res.Body, 1<<10))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%d: %s", res.StatusCode, said)
	}
	return nil
}

func drive(args []string) error {
	flags := flag.NewFlagSet("drive", flag.ContinueOnError)
	domain := flags.String("domain", "", "the site")
	addr := flags.String("addr", "127.0.0.1:443", "where it listens")
	roots := flags.String("roots", "", "a PEM file of the CAs to trust")
	report := flags.String("report", "", "where to write what was counted")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *domain == "" || *roots == "" || *report == "" {
		return errors.New("drive needs --domain, --roots and --report")
	}
	pem, err := os.ReadFile(*roots)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("%s holds no certificate", *roots)
	}
	client := func(http2 bool) *http.Client {
		return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: *domain, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", *addr)
			},
			ForceAttemptHTTP2: http2,
		}}
	}
	target := loadtest.Target{
		Domain:      *domain,
		Client:      func() *http.Client { return client(true) },
		ClientHTTP1: func() *http.Client { return client(false) },
		Dial: func(ctx context.Context) (net.Conn, error) {
			d := tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: *domain, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}}
			return d.DialContext(ctx, "tcp", *addr)
		},
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	tally := loadtest.NewTally()
	wg := target.Run(context.Background(), tally, signals.Done())
	<-signals.Done()
	wg.Wait()
	b, err := json.MarshalIndent(tally.Report(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*report, b, 0o600)
}
