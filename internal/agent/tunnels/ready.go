// SPDX-License-Identifier: AGPL-3.0-only

// Package tunnels finds the Cloudflare tunnels a server runs with cloudflared, and
// what proves each one is this server's: the connector ID cloudflared reports on its
// /ready endpoint, which Cloudflare's connections list carries too (ADR 0104,
// section 3).
package tunnels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultMetrics are the addresses cloudflared tries for its metrics, /ready among
// them, when nothing names one: localhost on ports 20241 to 20245, or every
// interface on them in a container.
var DefaultMetrics = []string{"127.0.0.1:20241", "127.0.0.1:20242", "127.0.0.1:20243", "127.0.0.1:20244", "127.0.0.1:20245"}

// Ready is what cloudflared's /ready says: how many connections to Cloudflare are
// up, and the ID of this run of it, which changes each time it starts.
type Ready struct {
	Connections int
	ConnectorID string
}

// ErrNotCloudflared is an address that answered, but not as cloudflared's /ready.
var ErrNotCloudflared = errors.New("what answers there isn't cloudflared's metrics")

// maxReady is as much of /ready's answer as is read; it's under 100 bytes.
const maxReady = 4 << 10

// ReadReady asks cloudflared's metrics at addr, a host and port, for /ready. dial
// reaches addr, as a dialer inside the process's own network does; nil dials from
// here. It follows no redirect and reads 4 KB at most.
func ReadReady(ctx context.Context, addr string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (Ready, error) {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		Transport:     &http.Transport{DialContext: dial, Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/ready", http.NoBody)
	if err != nil {
		return Ready{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Ready{}, fmt.Errorf("can't reach cloudflared's metrics at %s: %w", addr, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReady))
	if err != nil {
		return Ready{}, fmt.Errorf("can't read cloudflared's /ready at %s: %w", addr, err)
	}
	// /ready answers 200 with connections up, and 503 with none yet: both say who.
	var got struct {
		Status      int    `json:"status"`
		Ready       *int   `json:"readyConnections"`
		ConnectorID string `json:"connectorId"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Ready == nil || got.ConnectorID == "" {
		return Ready{}, ErrNotCloudflared
	}
	return Ready{Connections: *got.Ready, ConnectorID: got.ConnectorID}, nil
}
