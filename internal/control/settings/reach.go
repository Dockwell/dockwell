// SPDX-License-Identifier: AGPL-3.0-only

package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/netguard"
)

// A domain behind a tunnel or a proxy that ends TLS is checked by the way in, not
// DNS and ports (ADR 0063's amendment): the control plane asks the domain for a
// token it just made, and sees whether this panel answered it, through the proxy.

// ReachPath is where the panel answers a reach check's token, on loopback.
const ReachPath = "/.well-known/dockwell-reach/"

// reachWait is how long a token answers, and how long the check waits for it.
const reachWait = 30 * time.Second

// reachHeader marks a reach answer as Dockwell's: "unknown" for a token this
// panel didn't make, as another Dockwell's would be.
const reachHeader = "Dockwell-Reach"

// Doer sends a request, as an *http.Client or a netguard client does.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ReachClient is the client a reach check asks the domain with: the network a
// person names, guarded as anything a person names is, though the private network
// and this server's own addresses are allowed, as a proxy that ends TLS may be on
// either.
func ReachClient() Doer {
	return netguard.Client(netguard.Options{Private: true, Loopback: true, MaxBody: 1 << 10, Timeout: 15 * time.Second})
}

// Reach holds the tokens reach checks are waiting for, and answers them.
type Reach struct {
	mu   sync.Mutex
	open map[string]*reachTicket
	now  func() time.Time
}

// reachTicket is a token's answer, and what its request looked like once it
// came.
type reachTicket struct {
	answer string
	until  time.Time
	came   bool
	host   string
	proto  string
	fromLo bool
	// forwarded says it carried X-Forwarded-For, where the visitor's address is read.
	forwarded bool
}

// NewReach returns a Reach with no tokens.
func NewReach() *Reach {
	return &Reach{open: map[string]*reachTicket{}, now: time.Now}
}

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// mint makes a token, and the answer only this panel gives for it.
func (r *Reach) mint() (string, *reachTicket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, t := range r.open {
		if now.After(t.until) {
			delete(r.open, k)
		}
	}
	token := random()
	t := &reachTicket{answer: random(), until: now.Add(reachWait)}
	r.open[token] = t
	return token, t
}

// done forgets a token.
func (r *Reach) done(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.open, token)
}

// seen reads what a token's request looked like, once it came.
func (r *Reach) seen(t *reachTicket) reachTicket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *t
}

// Serve answers a reach check's token on the loopback listener, whatever host
// it's for, as the panel's domain isn't its own until it's saved; everything
// else goes to next. A token answers once, within reachWait. What came over TLS,
// or through the ingress's hand-over, isn't a tunnel's, so goes to next too.
func (r *Reach) Serve(next http.Handler) http.Handler {
	if r == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, ok := strings.CutPrefix(req.URL.Path, ReachPath)
		if !ok || req.TLS != nil {
			next.ServeHTTP(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.mu.Lock()
		t, found := r.open[token]
		if found && (t.came || r.now().After(t.until)) {
			found = false
		}
		if found {
			t.came = true
			t.host = req.Host
			t.proto = req.Header.Get("X-Forwarded-Proto")
			t.fromLo = loopbackRemote(req.RemoteAddr)
			t.forwarded = strings.TrimSpace(req.Header.Get("X-Forwarded-For")) != ""
		}
		r.mu.Unlock()
		if !found {
			w.Header().Set(reachHeader, "unknown")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set(reachHeader, "answer")
		_, _ = io.WriteString(w, t.answer)
	})
}

func loopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// CheckPanelReach asks the domain for a token through the tunnel or proxy, and
// says whether this panel answered it as a request for the domain over HTTPS.
func (h *Handler) CheckPanelReach(ctx context.Context, req *connect.Request[dockwellv1.CheckPanelReachRequest]) (*connect.Response[dockwellv1.CheckPanelReachResponse], error) {
	d := normalDomain(req.Msg.GetDomain())
	if d == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a domain to check is needed, such as panel.example.com"))
	}
	out := &dockwellv1.CheckPanelReachResponse{Target: "http://" + h.serving.Listen}
	if err := auth.ValidDomain(d); err != nil {
		out.Problem = err.Error()
		return connect.NewResponse(out), nil //nolint:nilerr // the refusal is the answer, not a failure to read
	}
	if h.domain.Reach == nil || h.domain.ReachClient == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane can't check a tunnel"))
	}
	out.Reached, out.Problem = h.reach(ctx, d, out.GetTarget())
	return connect.NewResponse(out), nil
}

// reach asks d for a fresh token and reads what came back, and what the request
// that reached the panel looked like.
func (h *Handler) reach(ctx context.Context, d, target string) (bool, string) {
	token, ticket := h.domain.Reach.mint()
	defer h.domain.Reach.done(token)
	ask, cancel := context.WithTimeout(ctx, reachWait)
	defer cancel()
	r, err := http.NewRequestWithContext(ask, http.MethodGet, "https://"+d+ReachPath+token, http.NoBody)
	if err != nil {
		return false, err.Error()
	}
	resp, err := h.domain.ReachClient.Do(r)
	if err != nil {
		return false, reachErrorWords(d, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	seen := h.domain.Reach.seen(ticket)
	switch {
	case resp.StatusCode == http.StatusOK && seen.came && string(body) == ticket.answer:
	case resp.StatusCode == 530:
		return false, fmt.Sprintf("Cloudflare answered 530 for %s: its tunnel isn't connected. Start cloudflared on this server, or check the tunnel's public hostname", d)
	case slices.Contains([]int{521, 522, 523, 525, 526}, resp.StatusCode):
		// Cloudflare's proxy, not its tunnel: it connects to this server's public
		// address, where Dockwell would have to serve the domain itself.
		return false, fmt.Sprintf("Cloudflare answered %d for %s: its proxy connects to this server's public address, not a tunnel. Run a tunnel to %s, or choose Dockwell serves it and set Cloudflare's SSL mode to Full (strict)", resp.StatusCode, d, target)
	case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
		return false, fmt.Sprintf("the tunnel or proxy answered %d for %s: it can't reach what it points at. Point it at %s", resp.StatusCode, d, target)
	case resp.Header.Get(reachHeader) != "":
		return false, fmt.Sprintf("another Dockwell answered for %s, not this one. Point the tunnel or proxy at %s on this server", d, target)
	default:
		return false, fmt.Sprintf("something else answered %d for %s, not this panel. Point the tunnel or proxy at %s", resp.StatusCode, d, target)
	}
	// It came: does the panel read it as the domain's, over HTTPS?
	host := strings.TrimSuffix(strings.ToLower(seen.host), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch {
	case !seen.fromLo:
		return false, fmt.Sprintf("it reached the panel from another address than this server's loopback, so the panel can't trust it for %s. Run the tunnel or proxy on this server", d)
	case host != d:
		return false, fmt.Sprintf("it reached the panel with %s as its host, not %s, so passkeys can't work there. Have the tunnel or proxy keep the host it was asked for", seen.host, d)
	case !strings.EqualFold(firstOf(seen.proto), "https"):
		return false, "it reached the panel without X-Forwarded-Proto https, so the panel can't tell it came over HTTPS. Have the proxy set it for " + d
	case !seen.forwarded:
		// Every visitor would read as one address, sharing one limit on sign-in.
		return false, "it reached the panel without X-Forwarded-For, so every visitor reads as one address. Have the proxy append the visitor's address for " + d
	}
	return true, ""
}

// firstOf is the first of a header's comma-separated values.
func firstOf(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// reachErrorWords says why the domain couldn't be asked.
func reachErrorWords(d string, err error) string {
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return "there's no DNS record for " + d + " yet. Add the tunnel's public hostname for it, or a record for your proxy"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing answered HTTPS where " + d + " leads. Start the tunnel or proxy, and point " + d + " at it"
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer came from " + d + " in time. Check that the tunnel or proxy is running, and that " + d + " leads to it"
	}
	return fmt.Sprintf("Dockwell couldn't reach %s: %s. Check that the tunnel or proxy is running, and that %s leads to it", d, netguard.Words(err), d)
}
