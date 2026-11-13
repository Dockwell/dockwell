// SPDX-License-Identifier: AGPL-3.0-only

// Package registry reads images from their registries for a form (ADR 0062):
// whether one is there, what it declares, and a repository's tags. It speaks
// the registry API (/v2/) and Docker Hub's own API, anonymously, as Dockwell
// has no registry logins, with GET and HEAD alone, through netguard. It keeps
// nothing but the short-lived tokens a registry hands any client: the agent's
// reader keeps what it read.
package registry

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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
	// ErrDown is a registry that didn't answer at all: no connection, or no
	// answer in time. It alone says a registry is down; what came back, however
	// odd, says something of one image.
	ErrDown = fmt.Errorf("%w: it didn't answer", ErrUnreachable)
	// ErrRefused is an answer that sends Dockwell somewhere it doesn't go: another
	// path than the registry's, or the server's own networks from a registry that
	// isn't on them.
	ErrRefused = fmt.Errorf("%w: it sends Dockwell where it doesn't go", ErrUnreachable)
	// ErrTooLarge is an answer longer than Dockwell reads of it.
	ErrTooLarge = fmt.Errorf("%w: its answer is longer than Dockwell reads", ErrUnreachable)
	// ErrBadDigest is a digest a registry named that isn't one, which nothing may be
	// pinned by.
	ErrBadDigest = fmt.Errorf("%w: it named a digest that isn't one", ErrUnreachable)
)

// digestShape is a digest as Docker names an image by one.
var digestShape = lazyRe(`^sha256:[0-9a-f]{64}$`)

const (
	// RequestTimeout bounds one request to a registry (ADR 0062).
	RequestTimeout = 5 * time.Second
	// MaxBody caps any answer, such as a manifest or Docker Hub's page of tags.
	MaxBody = 1 << 20
	// MaxConfig caps an image's config, which is a few kilobytes.
	MaxConfig = 256 << 10
	// MaxEnv is the most variables kept of a config, and maxListed the most
	// ports and volumes: an image declares a handful.
	MaxEnv    = 1000
	maxListed = 256
	// maxTagPage caps a page of a registry's list of tags.
	maxTagPage = 256 << 10
	// maxRedirects bounds following a blob to where a registry keeps it.
	maxRedirects = 3
	// maxTagPages bounds reading a repository's tags, a page at a time.
	maxTagPages = 10
	// tagsPerPage is the most names kept of a page, as many as a page is asked
	// for, and maxTagNames of all of them, after which no page is read.
	tagsPerPage = 1000
	maxTagNames = 2000
	// MaxTags is the most tags a list returns.
	MaxTags = 200
	// maxToken is the longest token kept, and tokenBytes what all of them may
	// take, the oldest going first.
	maxToken   = 8 << 10
	tokenBytes = 128 << 10
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
	// Secure and Plain send requests to a registry over HTTPS and plain HTTP.
	// Nil takes netguard's clients, which allow the server's own networks, as a
	// registry is often there and the pull reaches it anyway, and refuse the rest
	// of what a request must never reach.
	Secure, Plain Doer
	// Public sends what a registry sends Dockwell to on another host: a blob
	// kept elsewhere, or a token's realm. Nil takes netguard's client for the
	// internet alone, so a registry can't send Dockwell into the server's own
	// networks unless it is there itself.
	Public Doer
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
	// held is what the tokens take, in bytes.
	held int
}

type token struct {
	value string
	until time.Time
	at    time.Time
}

// New makes a Client.
func New(o Options) *Client {
	if o.Secure == nil {
		o.Secure = netguard.Client(netguard.Options{Private: true, Loopback: true, MaxBody: MaxBody, Timeout: RequestTimeout})
	}
	if o.Plain == nil {
		o.Plain = netguard.Client(netguard.Options{Private: true, Loopback: true, Local: true, MaxBody: MaxBody, Timeout: RequestTimeout})
	}
	if o.Public == nil {
		o.Public = netguard.Client(netguard.Options{MaxBody: MaxBody, Timeout: RequestTimeout})
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

// send sends one request: to the registry through the client its URL's
// scheme takes, and elsewhere through the client for the internet.
func (c *Client) send(req *http.Request, elsewhere bool) (*http.Response, error) {
	switch {
	case elsewhere:
		return c.o.Public.Do(req)
	case req.URL.Scheme == "http":
		return c.o.Plain.Do(req)
	}
	return c.o.Secure.Do(req)
}

// get asks a registry for a path, with a token if it asks for one, from each of
// its bases in turn until one answers. Only a blob a registry keeps elsewhere
// is followed there, without the token. A base that answered says more than
// one that didn't, so a mirror that answers keeps Docker Hub from reading as
// down.
func (c *Client) get(ctx context.Context, method, host, repo, path, accept string, blob bool) (*http.Response, error) {
	var said error
	bases := c.bases(host)
	if _, ok := authFor(ctx, host); ok && len(bases) > 1 {
		// A login goes to its registry alone, never to a mirror (ADR 0086).
		bases = bases[len(bases)-1:]
	}
	for _, base := range bases {
		resp, err := c.getFrom(ctx, method, base, repo, path, accept, blob)
		if err == nil {
			return resp, nil
		}
		if said == nil || errors.Is(said, ErrDown) && !errors.Is(err, ErrDown) {
			said = err
		}
		// A mirror that hasn't got it says nothing of Docker Hub, which is asked next.
		if ctx.Err() != nil {
			break
		}
	}
	if said == nil {
		said = ErrDown
	}
	return nil, said
}

// sameHost says whether a URL is on the registry's own scheme, host and port.
func sameHost(base string, u *url.URL) bool {
	b, err := url.Parse(base)
	return err == nil && b.Scheme == u.Scheme && strings.EqualFold(b.Host, u.Host)
}

// elsewhere checks where a registry sends Dockwell: back to its own /v2/, or
// to another host over HTTPS, which goes through the client for the internet.
// It says whether the target is another host. In a test, every host is the
// test's.
func (c *Client) elsewhere(base string, u *url.URL, paths ...string) (bool, error) {
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil {
		return false, ErrRefused
	}
	if sameHost(base, u) {
		for _, p := range paths {
			if strings.HasPrefix(u.Path, p) {
				return false, nil
			}
		}
		return false, ErrRefused
	}
	// Another host is one on the internet, over HTTPS, whatever its name resolves
	// to, as the client for the internet checks as it dials.
	if c.o.Test == "" && (u.Scheme != "https" || netguard.Forbidden(u.Host, netguard.Options{}) != "") {
		return false, ErrRefused
	}
	return true, nil
}

func (c *Client) getFrom(ctx context.Context, method, base, repo, path, accept string, blob bool) (*http.Response, error) {
	target := base + path
	login, signs := authFor(ctx, hostOf(ctx, base, c.o.Test))
	key := base + " " + repo + login.key()
	away := false
	// A registry that signs in itself takes the login on each request to its own host.
	basicAuth := false
	authz := func() string {
		if away {
			return ""
		}
		if basicAuth {
			return basic(login)
		}
		if t := c.token(key); t != "" {
			return "Bearer " + t
		}
		return ""
	}
	for redirects := 0; ; redirects++ {
		resp, err := c.do(ctx, method, target, accept, authz(), away)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !away {
			// Every challenge in every header, as Docker reads them, Bearer first:
			// Dockwell's own reads and Docker's pulls obey one rule (ADR 0086).
			challenges, err := parseChallenges(resp.Header.Values("Www-Authenticate"))
			drain(resp)
			var bearer *challenge
			hasBasic := false
			if err == nil {
				bearer, hasBasic, err = pickBearer(challenges)
			}
			switch {
			case err != nil && signs:
				return nil, ErrUnreadChallenge
			case err != nil:
				return nil, ErrNotFound
			case signs && bearer == nil && hasBasic && !basicAuth:
				basicAuth = true
			case signs && (basicAuth || c.token(key) != ""):
				return nil, ErrSignIn
			case bearer == nil || c.token(key) != "":
				return nil, ErrNotFound
			default:
				if err := c.authorize(ctx, base, key, repo, bearer.params); err != nil {
					return nil, err
				}
			}
			resp, err = c.do(ctx, method, target, accept, authz(), away)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode == http.StatusUnauthorized && signs {
				drain(resp)
				return nil, ErrSignIn
			}
		}
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "":
			next, err := url.Parse(resp.Header.Get("Location"))
			drain(resp)
			// A registry moves a blob to where it keeps it, and nothing else.
			if err != nil || !blob || redirects >= maxRedirects {
				return nil, ErrRefused
			}
			from, _ := url.Parse(target)
			next = from.ResolveReference(next)
			if away, err = c.elsewhere(base, next, "/v2/"); err != nil {
				return nil, err
			}
			if away {
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

func (c *Client) do(ctx context.Context, method, target, accept, authorization string, away bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.send(req, away)
	switch {
	case errors.Is(err, netguard.ErrForbidden):
		// Where it sent Dockwell, not the registry, is what's wrong.
		return nil, fmt.Errorf("%w: %s", ErrRefused, netguard.Words(err))
	case err != nil && ctx.Err() != nil:
		// The read ran out of its own time, or whoever asked left: that says
		// nothing of the registry, which may well have answered.
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, ctx.Err())
	case err != nil:
		return nil, fmt.Errorf("%w: %s", ErrDown, netguard.Words(err))
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
		c.dropToken(key)
		return ""
	}
	return t.value
}

// dropToken lets a token go. The caller holds c.mu.
func (c *Client) dropToken(key string) {
	if t, ok := c.tokens[key]; ok {
		c.held -= len(key) + len(t.value)
		delete(c.tokens, key)
	}
}

// keepToken keeps a token within tokenBytes, letting the oldest go first.
func (c *Client) keepToken(key string, t token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropToken(key)
	for c.held+len(key)+len(t.value) > tokenBytes && len(c.tokens) > 0 {
		oldest := ""
		for k, v := range c.tokens {
			if oldest == "" || v.at.Before(c.tokens[oldest].at) {
				oldest = k
			}
		}
		c.dropToken(oldest)
	}
	c.tokens[key] = t
	c.held += len(key) + len(t.value)
}

// authorize gets the token a registry hands any client for pulling a
// repository, from the realm its challenge names: on the registry's own host,
// or on the internet over HTTPS.
func (c *Client) authorize(ctx context.Context, base, key, repo string, params map[string]string) error {
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
	// A login goes to the registry's own host, or the token service it was saved
	// with, and nowhere else (ADR 0086): checked before the test's host stands in.
	login, signs := authFor(ctx, hostOf(ctx, base, c.o.Test))
	if signs && !tokenServiceAllowed(login, base, realm) {
		return ErrMoved
	}
	if c.o.Test != "" {
		// In a test, the realm is the test registry's, whatever it says.
		test, _ := url.Parse(c.o.Test)
		realm.Scheme, realm.Host = test.Scheme, test.Host
	}
	away, err := c.elsewhere(base, realm, realm.Path)
	if err != nil {
		return err
	}
	authorization := ""
	if signs {
		authorization = basic(login)
	}
	resp, err := c.do(ctx, http.MethodGet, realm.String(), "application/json", authorization, away)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusUnauthorized && signs {
		return ErrSignIn
	}
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
	if value == "" || len(value) > maxToken {
		return ErrNotFound
	}
	// A token says how long it lasts, 60 s when it doesn't, and is kept no longer
	// than tokenFor whatever it says.
	life := 60 * time.Second
	if t.ExpiresIn > 0 {
		life = time.Duration(t.ExpiresIn) * time.Second
	}
	life = min(life, tokenFor)
	now := c.o.Now()
	c.keepToken(key, token{value: value, until: now.Add(life - 5*time.Second), at: now})
	return nil
}

// Head says whether a registry has an image, and its digest, with a HEAD of its
// manifest, which no registry counts as a pull.
func (c *Client) Head(ctx context.Context, r Ref) (string, error) {
	resp, err := c.get(ctx, http.MethodHead, r.Registry, r.Repository, "/v2/"+r.Repository+"/manifests/"+r.Reference(), manifestTypes, false)
	if err != nil {
		return "", err
	}
	drain(resp)
	return answeredDigest(resp.Header.Get("Docker-Content-Digest"), r.Digest)
}

// IsDigest says whether a digest is one Dockwell takes from a registry: sha256
// and its 64 hex digits, as what a registry says flows into a plan's pins.
func IsDigest(s string) bool { return digestShape().MatchString(s) }

// answeredDigest is the digest a registry's answer names: its
// Docker-Content-Digest, else the digest asked for. Either must be a sha256 one,
// as anything else could carry what a pin never should: one that isn't is
// ErrBadDigest. Empty where neither says.
func answeredDigest(header, asked string) (string, error) {
	digest := header
	if digest == "" {
		digest = asked
	}
	// A digest is what a plan pins an image by, so one that isn't one is no answer.
	if digest != "" && !IsDigest(digest) {
		return "", ErrBadDigest
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
	// Version is what version it says it is, in its
	// org.opencontainers.image.version label, as a tag that moves can't say.
	Version string
}

// VersionLabel is the label an image says its version in (ADR 0080).
const VersionLabel = "org.opencontainers.image.version"

// maxVersion is the most of a version label kept, as one is a few characters.
const maxVersion = 64

// VersionOf is what an image's labels say its version is, cut to what a version
// takes, or empty where they say none.
func VersionOf(labels map[string]string) string {
	v := strings.TrimSpace(labels[VersionLabel])
	if len(v) > maxVersion || strings.ContainsFunc(v, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return ""
	}
	return v
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
	if m.Config.Size > MaxConfig {
		return nil, digest, ErrTooLarge
	}
	resp, err := c.get(ctx, http.MethodGet, r.Registry, r.Repository, "/v2/"+r.Repository+"/blobs/"+m.Config.Digest, "", true)
	if err != nil {
		return nil, digest, err
	}
	defer drain(resp)
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxConfig+1))
	switch {
	case len(body) > MaxConfig:
		return nil, digest, ErrTooLarge
	case err != nil:
		return nil, digest, fmt.Errorf("%w: its config can't be read", ErrUnreachable)
	}
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
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
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
		Version:    VersionOf(raw.Config.Labels),
	}
	if raw.Config.Healthcheck != nil {
		out.Healthcheck = raw.Config.Healthcheck.Test
	}
	return out, digest, nil
}

func (c *Client) manifest(ctx context.Context, r Ref, reference string) (*manifest, string, error) {
	resp, err := c.get(ctx, http.MethodGet, r.Registry, r.Repository, "/v2/"+r.Repository+"/manifests/"+reference, manifestTypes, false)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp)
	var m manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, "", fmt.Errorf("%w: its manifest can't be read", ErrUnreachable)
	}
	// A reference by digest names what it was asked for; a tag names none.
	asked := ""
	if strings.Contains(reference, ":") {
		asked = reference
	}
	digest, err := answeredDigest(resp.Header.Get("Docker-Content-Digest"), asked)
	if err != nil {
		return nil, "", err
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
	slices.SortFunc(out, func(a, b Port) int {
		if c := cmp.Compare(a.Number, b.Number); c != 0 {
			return c
		}
		// TCP before UDP on one port.
		switch {
		case a.UDP == b.UDP:
			return 0
		case b.UDP:
			return -1
		}
		return 1
	})
	return out[:min(len(out), maxListed)]
}

// Vars reads an image's NAME=value environment, the first MaxEnv of it.
func Vars(env []string) []Var {
	out := make([]Var, 0, min(len(env), MaxEnv))
	for _, e := range env {
		if len(out) == MaxEnv {
			break
		}
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
	slices.Sort(out)
	return out[:min(len(out), maxListed)]
}
