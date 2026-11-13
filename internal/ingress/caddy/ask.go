// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

var _ ingress.Asker = (*Caddy)(nil)

// askTimeout is how long asking a route waits, as Watch asks one after another.
const askTimeout = 3 * time.Second

// Ask asks one of Caddy's routes what a visitor gets, on this server's loopback,
// over the port it serves that route on. It checks no certificate, as it only reads
// the status Caddy itself answers with, from Caddy itself (ADR 0087).
func (c *Caddy) Ask(ctx context.Context, domain, path string, plain bool, header http.Header) (status int, said string, err error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	httpPort, httpsPort := c.ports()
	scheme, port := "https", httpsPort
	if plain {
		scheme, port = "http", httpPort
	}
	u := url.URL{Scheme: scheme, Host: domain, Path: "/"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return 0, "", err
	}
	// As written, with nothing escaped or cleaned, as a stranger's odd spellings are
	// asked too.
	req.URL.Opaque = path
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "dockwell-stranger-check")
	dial := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, dial)
			},
			TLSClientConfig:   &tls.Config{ServerName: domain, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // Caddy on this server's loopback, read for its status alone
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, "", fmt.Errorf("nothing listens on port %d", port)
		}
		return 0, "", err
	}
	_, _ = io.CopyN(io.Discard, res.Body, 4096)
	_ = res.Body.Close()
	return res.StatusCode, res.Status, nil
}
