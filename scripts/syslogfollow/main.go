// SPDX-License-Identifier: AGPL-3.0-only

// Command syslogfollow follows a server's system log as the panel does, with the
// session scripts/claim wrote, and when it's stopped says how many lines it was
// given and how many the server dropped. scripts/logs-load.sh measures an agent
// with it while its journal floods. It is never part of a release.
//
//	go run ./scripts/syslogfollow http://localhost:7697 <cookie file> <server name> [search]
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
)

func main() {
	if len(os.Args) < 4 || len(os.Args) > 5 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/syslogfollow <panel URL> <cookie file> <server name> [search]")
		os.Exit(2)
	}
	query := ""
	if len(os.Args) == 5 {
		query = os.Args[4]
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3], query); err != nil {
		fmt.Fprintln(os.Stderr, "syslogfollow:", err)
		os.Exit(1)
	}
}

// run follows until it's stopped, and says what it was given.
func run(base, cookieFile, name, query string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	lines, dropped, err := follow(ctx, base, cookieFile, name, query)
	fmt.Printf("syslogfollow: %d lines, %d dropped\n", lines, dropped)
	return err
}

// session sends the claim's cookie and the panel's origin on every request.
type session struct {
	cookie, origin string
	next           http.RoundTripper
}

func (s session) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Cookie", s.cookie)
	r.Header.Set("Origin", s.origin)
	return s.next.RoundTrip(r)
}

func follow(ctx context.Context, base, cookieFile, name, query string) (lines, dropped uint64, err error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return 0, 0, fmt.Errorf("%q isn't the panel's address, such as http://localhost:7697", base)
	}
	cookie, err := os.ReadFile(cookieFile) //nolint:gosec // a path the test's own caller names
	if err != nil {
		return 0, 0, err
	}
	var d net.Dialer
	client := &http.Client{Transport: session{cookie: strings.TrimSpace(string(cookie)), origin: base, next: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", u.Port()))
		},
	}}}
	servers := dockwellv1connect.NewServerServiceClient(client, base)
	list, err := servers.ListServers(ctx, connect.NewRequest(&dockwellv1.ListServersRequest{}))
	if err != nil {
		return 0, 0, fmt.Errorf("can't list the servers: %w", err)
	}
	id := ""
	for _, s := range list.Msg.GetServers() {
		if s.GetName() == name {
			id = s.GetId()
		}
	}
	if id == "" {
		return 0, 0, fmt.Errorf("there's no server %s", name)
	}
	for ctx.Err() == nil {
		w, err := servers.WatchSystemLog(ctx, connect.NewRequest(&dockwellv1.WatchSystemLogRequest{ServerId: id, Query: query}))
		if err != nil {
			return lines, dropped, err
		}
		for w.Receive() {
			lines += uint64(len(w.Msg().GetLines()))
			dropped += w.Msg().GetDropped()
		}
		if err := w.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "syslogfollow: the follow ended, and follows again:", err)
			time.Sleep(time.Second)
		}
		_ = w.Close()
	}
	return lines, dropped, nil
}
