// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
)

// UserAgentPrefix starts the CLI's User-Agent, which tells the control
// plane that a call with an API token came from the CLI, for the surface
// its changes and audit events record (auth.Client.Surface).
const UserAgentPrefix = auth.CLIUserAgent

// Client calls one control plane with one API token.
type Client struct {
	HTTP *http.Client
	// URL is the control plane's base URL, such as "https://panel.acme.dev"
	// or "http://127.0.0.1:7600".
	URL string
	// Token is the API token's secret.
	Token string
	// Local says the token came from local access on the control server.
	Local bool
	// Version is the CLI's build, for its User-Agent.
	Version string
}

// NewHTTPClient returns the HTTP client the CLI calls with.
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 0 // streams wait as long as their changes take
	return &http.Client{
		Transport: t,
		// A redirect could carry the token to another host; the API never
		// redirects.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *Client) options() []connect.ClientOption {
	return []connect.ClientOption{connect.WithInterceptors(credentials{token: c.Token, agent: UserAgentPrefix + c.Version})}
}

// Changes returns a client of ChangeService, for the plan flow.
func (c *Client) Changes() dockwellv1connect.ChangeServiceClient {
	return dockwellv1connect.NewChangeServiceClient(c.HTTP, c.URL, c.options()...)
}

// PanelURL is a page of the panel the control plane serves, as a browser
// reaches it: on loopback, at localhost, where passkeys work.
func (c *Client) PanelURL(path string) string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return c.URL + path
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		port := u.Port()
		u.Host = "localhost"
		if port != "" {
			u.Host += ":" + port
		}
	}
	return strings.TrimSuffix(u.String(), "/") + path
}

// credentials adds the API token and the CLI's User-Agent to every call.
type credentials struct{ token, agent string }

func (cr credentials) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		cr.set(req.Header())
		return next(ctx, req)
	}
}

func (cr credentials) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		cr.set(conn.RequestHeader())
		return conn
	}
}

func (credentials) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (cr credentials) set(h http.Header) {
	h.Set("User-Agent", cr.agent)
	if cr.token != "" {
		h.Set("Authorization", "Bearer "+cr.token)
	}
}

// NormalizeURL checks a control plane's URL and returns it without a path
// or a trailing slash. Only loopback takes plain HTTP: anywhere else the
// token would cross the network in the clear, and the panel serves only
// HTTPS there (ADR 0011).
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		// The panel serves plain HTTP on loopback, and HTTPS elsewhere.
		scheme := "https://"
		if u, err := url.Parse("http://" + raw); err == nil && isLoopback(u.Hostname()) {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q isn't a URL. Give the panel's address, such as https://panel.acme.dev or http://127.0.0.1:7600", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("%s uses plain HTTP, so your API token would cross the network unencrypted. Use https://%s, the panel's domain, "+
				"or reach the panel through an SSH tunnel at http://localhost", u.Redacted(), u.Host)
		}
	default:
		return "", fmt.Errorf("%s isn't an http:// or https:// URL. Give the panel's address, such as https://panel.acme.dev", u.Redacted())
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s has more than the panel's address in it. Give the address alone, such as %s://%s, without a path, query or user",
			u.Redacted(), u.Scheme, u.Host)
	}
	return u.Scheme + "://" + u.Host, nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// ClientOptions are the options that send c's token and User-Agent, for a
// client of the API made apart from the commands.
func ClientOptions(c *Client) []connect.ClientOption { return c.options() }
