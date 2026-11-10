// SPDX-License-Identifier: AGPL-3.0-only

// Package registry reads images from their registries for a form (ADR 0062):
// whether one is there, what it declares, and a repository's tags. It speaks
// the registry API (/v2/) and Docker Hub's own API, anonymously, as Dockwell
// has no registry logins, with GET and HEAD alone, through netguard. It keeps
// nothing but the short-lived tokens a registry hands any client: the agent's
// reader keeps what it read.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

var (
	// ErrNotFound is an image or repository a registry hasn't got, or keeps
	// private, which a registry answers alike to a client that doesn't sign in.
	ErrNotFound = errors.New("the registry hasn't got it, or keeps it private")
	// ErrUnreachable is a registry that couldn't be reached, or didn't answer in
	// time, which says nothing of the image.
	ErrUnreachable = errors.New("the registry couldn't be reached")
	// ErrLimited is a registry that is limiting how often it's asked.
	ErrLimited = errors.New("the registry is limiting how often it's asked")
)

const (
	// RequestTimeout bounds one request to a registry (ADR 0062).
	RequestTimeout = 5 * time.Second
	// MaxBody caps what a manifest, a config or a page of tags reads.
	MaxBody = 4 << 20
	// maxRedirects bounds following a blob to where a registry keeps it.
	maxRedirects = 3
	// maxTagPages bounds reading a repository's tags, a page at a time.
	maxTagPages = 10
	// MaxTags is the most tags a list returns.
	MaxTags = 200
	// latestOf is how many of the newest versions a registry that lists names
	// alone is asked the digest of, to tell which point where "latest" does.
	latestOf = 10
	// tokenFor bounds how long a token is kept, whatever it says.
	tokenFor = 5 * time.Minute
)

// The manifests a read takes, an index first.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Doer sends a request, as netguard's client does.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Options say how a Client reaches registries.
type Options struct {
	// Mirrors are where Docker Hub's images are read first, as Docker's
	// registry-mirrors, such as "https://mirror.example.com".
	Mirrors []string
	// Insecure says which registries Docker reaches over plain HTTP, as /info
	// says; nil reaches every one over HTTPS.
	Insecure func(host string) bool
	// Test, in a test, is where every registry and Docker Hub's API are read,
	// such as "http://127.0.0.1:5001": nothing else is reached.
	Test string
	// Secure and Plain send requests over HTTPS and plain HTTP. Nil takes
	// netguard's clients, which allow the server's own networks, as a registry is
	// often there and the pull reaches it anyway, and refuse the rest of what a
	// request must never reach.
	Secure, Plain Doer
	// Hub is Docker Hub's API. Empty takes https://hub.docker.com.
	Hub string
	// Now is the clock; nil takes time.Now.
	Now func() time.Time
}

// Client reads images from registries.
type Client struct {
	o      Options
	mu     sync.Mutex
	tokens map[string]token
}

type token struct {
	value string
	until time.Time
}

// New makes a Client.
func New(o Options) *Client {
	if o.Secure == nil {
		o.Secure = netguard.Client(netguard.Options{Private: true, Loopback: true, MaxBody: MaxBody, Timeout: RequestTimeout})
	}
	if o.Plain == nil {
		o.Plain = netguard.Client(netguard.Options{Private: true, Loopback: true, Local: true, MaxBody: MaxBody, Timeout: RequestTimeout})
	}
	if o.Hub == "" {
		o.Hub = "https://hub.docker.com"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{o: o, tokens: map[string]token{}}
}

// bases are where a registry is read from, in order: Docker Hub through its
// mirrors first, as the pull goes.
func (c *Client) bases(host string) []string {
	if c.o.Test != "" {
		return []string{strings.TrimSuffix(c.o.Test, "/")}
	}
	if host == Hub {
		out := make([]string, 0, len(c.o.Mirrors)+1)
		for _, m := range c.o.Mirrors {
			out = append(out, strings.TrimSuffix(m, "/"))
		}
		return append(out, "https://registry-1.docker.io")
	}
	if c.o.Insecure != nil && c.o.Insecure(host) {
		return []string{"http://" + host}
	}
	return []string{"https://" + host}
}

// hubAPI is Docker Hub's own API.
func (c *Client) hubAPI() string {
	if c.o.Test != "" {
		return strings.TrimSuffix(c.o.Test, "/")
	}
	return strings.TrimSuffix(c.o.Hub, "/")
}

// send sends one request, through the client its URL's scheme takes.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" {
		return c.o.Plain.Do(req)
	}
	return c.o.Secure.Do(req)
}

// get asks a registry for a path, with a token if it asks for one, from each of
// its bases in turn until one answers. A blob a registry keeps elsewhere is
// followed there, without the token.
func (c *Client) get(ctx context.Context, method, host, repo, path, accept string) (*http.Response, error) {
	last := ErrUnreachable
	for _, base := range c.bases(host) {
		resp, err := c.getFrom(ctx, method, base, repo, path, accept)
		if err == nil {
			return resp, nil
		}
		last = err
		// A mirror that hasn't got it says nothing of Docker Hub, which is asked next.
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

func (c *Client) getFrom(ctx context.Context, method, base, repo, path, accept string) (*http.Response, error) {
	target := base + path
	key := base + " " + repo
	for redirects := 0; ; redirects++ {
		resp, err := c.do(ctx, method, target, accept, c.token(key))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			challenge := resp.Header.Get("Www-Authenticate")
			drain(resp)
			if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") || c.token(key) != "" {
				return nil, ErrNotFound
			}
			if err := c.authorize(ctx, key, repo, challenge); err != nil {
				return nil, err
			}
			resp, err = c.do(ctx, method, target, accept, c.token(key))
			if err != nil {
				return nil, err
			}
		}
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" && redirects < maxRedirects:
			next, err := url.Parse(resp.Header.Get("Location"))
			drain(resp)
			if err != nil {
				return nil, ErrUnreachable
			}
			from, _ := url.Parse(target)
			next = from.ResolveReference(next)
			if next.Host != from.Host {
				// Where a registry keeps a blob takes no token of the registry's.
				key = ""
			}
			target = next.String()
			continue
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		}
		code := resp.StatusCode
		drain(resp)
		switch code {
		case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
			return nil, ErrNotFound
		case http.StatusTooManyRequests:
			return nil, ErrLimited
		}
		return nil, fmt.Errorf("%w: it answered %d", ErrUnreachable, code)
	}
}

func (c *Client) do(ctx context.Context, method, target, accept, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.send(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, netguard.Words(err))
	}
	return resp, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func (c *Client) token(key string) string {
	if key == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[key]
	if !ok || !c.o.Now().Before(t.until) {
		delete(c.tokens, key)
		return ""
	}
	return t.value
}

// authorize gets the token a registry hands any client for pulling a
// repository, from the realm its challenge names.
func (c *Client) authorize(ctx context.Context, key, repo, challenge string) error {
	params := challengeParams(challenge[len("bearer "):])
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return ErrNotFound
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	if c.o.Test != "" {
		// In a test, the realm is the test registry's, whatever it says.
		test, _ := url.Parse(c.o.Test)
		realm.Scheme, realm.Host = test.Scheme, test.Host
	}
	resp, err := c.do(ctx, http.MethodGet, realm.String(), "application/json", "")
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return ErrNotFound
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return fmt.Errorf("%w: its token can't be read", ErrUnreachable)
	}
	value := t.Token
	if value == "" {
		value = t.AccessToken
	}
	if value == "" {
		return ErrNotFound
	}
	// A token says how long it lasts, 60 s when it doesn't, and is kept no longer
	// than tokenFor whatever it says.
	life := 60 * time.Second
	if t.ExpiresIn > 0 {
		life = time.Duration(t.ExpiresIn) * time.Second
	}
	life = min(life, tokenFor)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[key] = token{value: value, until: c.o.Now().Add(life - 5*time.Second)}
	return nil
}

// challengeParams reads a challenge's parameters, such as
// realm="https://auth.docker.io/token",service="registry.docker.io".
func challengeParams(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, " ,")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			value, s = rest[1:1+end], rest[2+end:]
		} else {
			value, s, _ = strings.Cut(rest, ",")
		}
		out[key] = value
	}
	return out
}

// Head says whether a registry has an image, and its digest, with a HEAD of its
// manifest, which no registry counts as a pull.
func (c *Client) Head(ctx context.Context, r Ref) (string, error) {
	resp, err := c.get(ctx, http.MethodHead, r.Registry, r.Repository, "/v2/"+r.Repository+"/manifests/"+r.Reference(), manifestTypes)
	if err != nil {
		return "", err
	}
	drain(resp)
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" && r.Digest != "" {
		digest = r.Digest
	}
	return digest, nil
}

// Platform is what a server runs, such as linux and arm64.
type Platform struct {
	OS, Architecture, Variant string
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Port is a port an image exposes.
type Port struct {
	Number uint32
	UDP    bool
}

// Var is an environment variable an image sets.
type Var struct {
	Name, Value string
}

// Config is what an image declares for a platform.
type Config struct {
	Ports       []Port
	Env         []Var
	Volumes     []string
	Cmd         []string
	Entrypoint  []string
	User        string
	WorkingDir  string
	Healthcheck []string
	Platform    Platform
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    *descriptor  `json:"config"`
}

// Config reads what an image declares for a platform: one GET of its manifest,
// which Docker Hub counts as a pull, and of the platform's in an index, by
// digest, then its config blob, which no registry counts. It returns the
// image's digest too.
func (c *Client) Config(ctx context.Context, r Ref, want Platform) (*Config, string, error) {
	m, digest, err := c.manifest(ctx, r, r.Reference())
	if err != nil {
		return nil, "", err
	}
	if len(m.Manifests) > 0 {
		pick := pickPlatform(m.Manifests, want)
		if pick == nil {
			return nil, digest, fmt.Errorf("%w: it has nothing for %s", ErrNotFound, want)
		}
		if m, _, err = c.manifest(ctx, r, pick.Digest); err != nil {
			return nil, digest, err
		}
	}
	if m.Config == nil || m.Config.Digest == "" {
		return nil, digest, fmt.Errorf("%w: its manifest names no config", ErrNotFound)
	}
	if m.Config.Size > MaxBody {
		return nil, digest, fmt.Errorf("%w: its config is larger than Dockwell reads", ErrUnreachable)
	}
	resp, err := c.get(ctx, http.MethodGet, r.Registry, r.Repository, "/v2/"+r.Repository+"/blobs/"+m.Config.Digest, "")
	if err != nil {
		return nil, digest, err
	}
	defer drain(resp)
	var raw struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
		Config       struct {
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
			Env          []string            `json:"Env"`
			Volumes      map[string]struct{} `json:"Volumes"`
			Cmd          []string            `json:"Cmd"`
			Entrypoint   []string            `json:"Entrypoint"`
			User         string              `json:"User"`
			WorkingDir   string              `json:"WorkingDir"`
			Healthcheck  *struct {
				Test []string `json:"Test"`
			} `json:"Healthcheck"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, digest, fmt.Errorf("%w: its config can't be read", ErrUnreachable)
	}
	out := &Config{
		Ports:      Ports(raw.Config.ExposedPorts),
		Env:        Vars(raw.Config.Env),
		Volumes:    Volumes(raw.Config.Volumes),
		Cmd:        raw.Config.Cmd,
		Entrypoint: raw.Config.Entrypoint,
		User:       raw.Config.User,
		WorkingDir: raw.Config.WorkingDir,
		Platform:   Platform{OS: raw.OS, Architecture: raw.Architecture, Variant: raw.Variant},
	}
	if raw.Config.Healthcheck != nil {
		out.Healthcheck = raw.Config.Healthcheck.Test
	}
	return out, digest, nil
}

func (c *Client) manifest(ctx context.Context, r Ref, reference string) (*manifest, string, error) {
	resp, err := c.get(ctx, http.MethodGet, r.Registry, r.Repository, "/v2/"+r.Repository+"/manifests/"+reference, manifestTypes)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp)
	var m manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, "", fmt.Errorf("%w: its manifest can't be read", ErrUnreachable)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" && strings.Contains(reference, ":") {
		digest = reference
	}
	return &m, digest, nil
}

// pickPlatform is an index's manifest for a platform: its OS and architecture,
// and its variant where both say one.
func pickPlatform(list []descriptor, want Platform) *descriptor {
	var match *descriptor
	for i := range list {
		d := &list[i]
		p := d.Platform
		if p == nil || p.OS != want.OS || p.Architecture != want.Architecture {
			continue
		}
		if want.Variant == "" || p.Variant == "" || p.Variant == want.Variant {
			if p.Variant == want.Variant {
				return d
			}
			if match == nil {
				match = d
			}
		}
	}
	return match
}

// Ports reads Docker's exposed ports, such as "80/tcp", in order.
func Ports(exposed map[string]struct{}) []Port {
	var out []Port
	for spec := range exposed {
		num, proto, _ := strings.Cut(spec, "/")
		n, err := strconv.ParseUint(num, 10, 16)
		if err != nil || n == 0 {
			continue
		}
		out = append(out, Port{Number: uint32(n), UDP: proto == "udp"})
	}
	sortPorts(out)
	return out
}

func sortPorts(ports []Port) {
	for i := 1; i < len(ports); i++ {
		for j := i; j > 0 && (ports[j].Number < ports[j-1].Number || ports[j].Number == ports[j-1].Number && !ports[j].UDP && ports[j-1].UDP); j-- {
			ports[j], ports[j-1] = ports[j-1], ports[j]
		}
	}
}

// Vars reads an image's NAME=value environment.
func Vars(env []string) []Var {
	out := make([]Var, 0, len(env))
	for _, e := range env {
		name, value, _ := strings.Cut(e, "=")
		if name != "" {
			out = append(out, Var{Name: name, Value: value})
		}
	}
	return out
}

// Volumes reads an image's VOLUME paths, in order.
func Volumes(v map[string]struct{}) []string {
	out := make([]string, 0, len(v))
	for p := range v {
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
