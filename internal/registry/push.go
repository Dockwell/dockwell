// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

var pushAccept = manifestTypes + ", application/vnd.docker.distribution.manifest.v1+prettyjws, application/vnd.docker.distribution.manifest.v1+json, application/vnd.oci.artifact.manifest.v1+json, */*"

const (
	// OCIManifest is the media type of a single-platform OCI image manifest.
	OCIManifest = "application/vnd.oci.image.manifest.v1+json"
	// PushTimeout bounds a registry write, including its streamed body.
	PushTimeout = 2 * time.Hour
)

var (
	// ErrPushConflict says the destination is occupied by a different digest.
	ErrPushConflict = errors.New("the destination serves a different digest. Choose another tag")
	// ErrPushUnknown means publication was attempted and remains unverified.
	ErrPushUnknown = errors.New("the registry did not confirm what it published. Check the result before reviewing again")
	// ErrPushRefused is a refusal or invalid response before publication.
	ErrPushRefused = errors.New("the registry refused the upload")
	// ErrPushTimeout indicates a network deadline before a publication was confirmed.
	ErrPushTimeout = errors.New("the registry request timed out")
)

// Publisher sends one repository's bytes. Its login and cached authorization
// live only for this operation. Blob storage redirects carry neither.
type Publisher struct {
	login                        Auth
	origin                       url.URL
	path, scope                  string
	repository                   string
	canWrite                     bool
	read, write, public, storage Doer
	auth                         string
	authUntil                    time.Time
}

// PublisherFor binds an approved HTTPS login to an exact repository. Check uses
// a pull scope. Apply uses the same repository with pull and push access.
func (c *Client) PublisherFor(r Ref, login Auth, write bool) (*Publisher, error) {
	canonical, err := Parse(r.String())
	if err != nil || canonical != r || login.Registry != r.Registry || r.Repository == "" || login.Password == "" {
		return nil, ErrSignIn
	}
	host := r.Registry
	if host == Hub {
		host = "registry-1.docker.io"
	}
	p := &Publisher{login: login, origin: url.URL{Scheme: "https", Host: host}, path: "/v2/" + r.Repository, repository: r.Repository, canWrite: write, scope: "repository:" + r.Repository + ":pull", read: c.o.Secure, write: c.write, public: c.o.Public}
	if write {
		p.scope += ",push"
	}
	p.storage = c.o.Storage
	if p.storage == nil {
		p.storage = netguard.Client(netguard.Options{MaxBody: 64 << 10, Timeout: PushTimeout})
	}
	if p.write == nil {
		p.write = netguard.Client(netguard.Options{Private: true, Loopback: true, MaxBody: 64 << 10, Timeout: PushTimeout})
	}
	return p, nil
}

func (p *Publisher) at(path string) url.URL {
	u := p.origin
	u.Path = path
	return u
}

// request deliberately returns only typed errors. Upstream payloads, headers and
// URLs can contain credentials even when a registry is otherwise trustworthy.
func request(ctx context.Context, client Doer, method string, target url.URL, authorization, contentType string, body io.Reader, size int64) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, ErrPushRefused
	}
	r.ContentLength = size
	r.GetBody = nil
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.Header.Set("Accept", pushAccept)
	resp, err := client.Do(r)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrPushTimeout
		}
		return nil, ErrUnreachable
	}
	return resp, nil
}

func (p *Publisher) authorization(ctx context.Context) (string, error) {
	if p.auth != "" && time.Now().Before(p.authUntil) {
		return p.auth, nil
	}
	ctx, done := context.WithTimeout(ctx, RequestTimeout)
	defer done()
	base := p.origin.String()
	challenge, err := request(ctx, p.read, http.MethodGet, p.at("/v2/"), "", "", nil, 0)
	if err != nil {
		return "", err
	}
	headers, status := challenge.Header.Values("WWW-Authenticate"), challenge.StatusCode
	drain(challenge)
	if status == http.StatusOK && len(headers) == 0 {
		return p.remember(basic(p.login), time.Now().Add(tokenFor)), nil
	}
	if status != http.StatusUnauthorized && status != http.StatusOK {
		return "", ErrUnreachable
	}
	challenges, err := parseChallenges(headers)
	if err != nil {
		return "", ErrUnreadChallenge
	}
	bearer, hasBasic, err := pickBearer(challenges)
	if err != nil {
		return "", ErrUnreadChallenge
	}
	if bearer == nil {
		if hasBasic {
			return p.remember(basic(p.login), time.Now().Add(tokenFor)), nil
		}
		return "", ErrUnreadChallenge
	}
	realm, err := url.Parse(bearer.params["realm"])
	if err != nil || realm.Scheme != "https" || realm.User != nil || realm.Fragment != "" || !tokenServiceAllowed(p.login, base, realm) {
		return "", ErrMoved
	}
	q := realm.Query()
	q.Set("scope", p.scope)
	if service := bearer.params["service"]; service != "" {
		q.Set("service", service)
	}
	realm.RawQuery = q.Encode()
	client := p.read
	if !ownHostRealm(base, realm) {
		client = p.public
	}
	r, err := request(ctx, client, http.MethodGet, *realm, basic(p.login), "", nil, 0)
	if err != nil {
		return "", err
	}
	defer drain(r)
	if r.StatusCode != http.StatusOK {
		return "", ErrSignIn
	}
	var reply struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&reply) != nil {
		return "", ErrSignIn
	}
	token := reply.Token
	if token == "" {
		token = reply.AccessToken
	}
	if token == "" || len(token) > maxToken || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r >= 127 }) {
		return "", ErrSignIn
	}
	claims, distribution := distributionClaims(token)
	if distribution && p.canWrite && !claims.pushes(p.repository) {
		return "", ErrSignIn
	}
	now := time.Now()
	until := now.Add(tokenFor)
	if reply.ExpiresIn > 0 {
		issued := now
		if at, err := time.Parse(time.RFC3339, reply.IssuedAt); err == nil && at.Before(now) {
			issued = at
		}
		until = issued.Add(time.Duration(min(reply.ExpiresIn, int64(PushTimeout/time.Second))) * time.Second)
	}
	if claims.Expires > 0 {
		expiry := time.Unix(claims.Expires, 0)
		if reply.ExpiresIn <= 0 || expiry.Before(until) {
			until = expiry
		}
	}
	return p.remember("Bearer "+token, until), nil
}

func (p *Publisher) remember(auth string, until time.Time) string {
	p.auth, p.authUntil = auth, until
	return auth
}

type pushClaims struct {
	Expires int64 `json:"exp"`
	Access  []struct {
		Type    string   `json:"type"`
		Name    string   `json:"name"`
		Actions []string `json:"actions"`
	} `json:"access"`
}

// These unverified claims can refuse a push early. Only the registry can grant it.
func distributionClaims(token string) (pushClaims, bool) {
	_, tail, ok := strings.Cut(token, ".")
	raw, _, jwt := strings.Cut(tail, ".")
	if !ok || !jwt {
		return pushClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	var claims pushClaims
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return pushClaims{}, false
	}
	return claims, claims.Access != nil
}

func (c pushClaims) pushes(repository string) bool {
	for _, entry := range c.Access {
		if entry.Type == "repository" && entry.Name == repository {
			for _, action := range entry.Actions {
				if action == "push" || action == "*" {
					return true
				}
			}
		}
	}
	return false
}

func (p *Publisher) do(ctx context.Context, method string, target url.URL, contentType string, body io.ReaderAt, size int64) (*http.Response, error) {
	if target.Scheme != "https" || target.Host != p.origin.Host || target.User != nil || target.Fragment != "" || !strings.HasPrefix(target.Path, p.path+"/") {
		return nil, ErrPushRefused
	}
	auth, err := p.authorization(ctx)
	if err != nil {
		return nil, err
	}
	client := p.read
	if body != nil {
		client = p.write
	}
	send := func(auth string) (*http.Response, error) {
		return request(ctx, client, method, target, auth, contentType, blobBody(body, size), size)
	}
	resp, err := send(auth)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		drain(resp)
		p.auth = ""
		fresh, err := p.authorization(ctx)
		if err != nil {
			return nil, err
		}
		resp, err = send(fresh)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		drain(resp)
		return nil, ErrSignIn
	}
	return resp, nil
}

// ManifestDigest reads the exact registry binding. Empty means absent. It
// refuses a registry that serves content but omits its digest evidence.
func (p *Publisher) ManifestDigest(ctx context.Context, reference string) (string, error) {
	if !IsDigest(reference) && !tagRe().MatchString(reference) {
		return "", ErrPushRefused
	}
	r, err := p.do(ctx, http.MethodHead, p.at(p.path+"/manifests/"+reference), "", nil, 0)
	if err != nil {
		return "", err
	}
	defer drain(r)
	if r.StatusCode == http.StatusNotFound {
		return "", nil
	}
	got := r.Header.Get("Docker-Content-Digest")
	if r.StatusCode != http.StatusOK || !IsDigest(got) {
		return "", ErrPushRefused
	}
	return got, nil
}

// blobRequest follows storage redirects without forwarding authentication.
func (p *Publisher) blobRequest(ctx context.Context, method string, target url.URL, body io.ReaderAt, size int64) (*http.Response, bool, error) {
	contentType := ""
	if body != nil {
		contentType = "application/octet-stream"
	}
	resp, err := p.do(ctx, method, target, contentType, body, size)
	redirected := false
	for hops := 0; err == nil && redirectStatus(resp.StatusCode); hops++ {
		location, status := resp.Header.Get("Location"), resp.StatusCode
		drain(resp)
		if hops == maxRedirects || location == "" || len(location) > 4096 ||
			method != http.MethodHead && status != http.StatusTemporaryRedirect && status != http.StatusPermanentRedirect {
			return nil, redirected, ErrPushRefused
		}
		u, parseErr := target.Parse(location)
		if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
			netguard.Forbidden(u.Host, netguard.Options{}) != "" {
			return nil, redirected, ErrPushRefused
		}
		target, redirected = *u, true
		resp, err = request(ctx, p.storage, method, target, "", contentType, blobBody(body, size), size)
	}
	return resp, redirected, err
}

// The transport may still be closing an earlier request body when we follow
// a redirect or refresh auth. Each request gets its own read position.
func blobBody(content io.ReaderAt, size int64) io.Reader {
	if content == nil {
		return nil
	}
	if size == 0 {
		return http.NoBody
	}
	return io.NewSectionReader(content, 0, size)
}

func redirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func (p *Publisher) hasBlob(ctx context.Context, digest string) (bool, error) {
	r, redirected, err := p.blobRequest(ctx, http.MethodHead, p.at(p.path+"/blobs/"+digest), nil, 0)
	if err != nil {
		return false, err
	}
	defer drain(r)
	switch {
	case r.StatusCode == http.StatusOK || redirected && r.StatusCode == http.StatusNoContent:
		got := r.Header.Get("Docker-Content-Digest")
		if got != "" && got != digest {
			return false, ErrPushRefused
		}
		return true, nil
	case !redirected && r.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, ErrPushRefused
	}
}

// PutBlob uploads a missing blob. A storage upload is confirmed at the registry.
func (p *Publisher) PutBlob(ctx context.Context, digest string, size int64, content io.ReaderAt) error {
	if !p.canWrite || !IsDigest(digest) || size < 0 || content == nil {
		return ErrPushRefused
	}
	if exists, err := p.hasBlob(ctx, digest); err != nil || exists {
		return err
	}
	start := p.at(p.path + "/blobs/uploads/")
	r, _, err := p.blobRequest(ctx, http.MethodPost, start, nil, 0)
	if err != nil {
		return err
	}
	location, status := r.Header.Get("Location"), r.StatusCode
	drain(r)
	if status == http.StatusCreated {
		if exists, err := p.hasBlob(ctx, digest); exists || err != nil {
			return err
		}
		return ErrPushRefused
	}
	if status != http.StatusAccepted || location == "" || len(location) > 4096 {
		return ErrPushRefused
	}
	u, err := start.Parse(location)
	if err != nil || u.RawPath != "" || !strings.HasPrefix(u.Path, start.Path) {
		return ErrPushRefused
	}
	uuid := strings.TrimPrefix(u.Path, start.Path)
	if uuid == "" || uuid == "." || uuid == ".." || strings.Contains(uuid, "/") {
		return ErrPushRefused
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()
	r, redirected, err := p.blobRequest(ctx, http.MethodPut, *u, content, size)
	if err != nil {
		return err
	}
	defer drain(r)
	if !redirected && r.StatusCode == http.StatusCreated && r.Header.Get("Docker-Content-Digest") == digest {
		return nil
	}
	if redirected && r.StatusCode >= 200 && r.StatusCode < 300 {
		if exists, err := p.hasBlob(ctx, digest); exists || err != nil {
			return err
		}
	}
	return ErrPushRefused
}

// PutManifest rechecks the tag just before a single write. Once sending begins,
// an ambiguous response stays unknown even if the later check sees the digest.
func (p *Publisher) PutManifest(ctx context.Context, reference, digest string, content io.Reader, size int64, before func() error) error {
	if !p.canWrite || !IsDigest(digest) {
		return ErrPushRefused
	}
	current, err := p.ManifestDigest(ctx, reference)
	if err != nil || current == digest {
		return err
	}
	if current != "" {
		return ErrPushConflict
	}
	auth, err := p.authorization(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := before(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return ErrPushTimeout
	}
	r, err := request(ctx, p.write, http.MethodPut, p.at(p.path+"/manifests/"+reference), auth, OCIManifest, content, size)
	if err != nil {
		return ErrPushUnknown
	}
	status, reported := r.StatusCode, r.Header.Get("Docker-Content-Digest")
	drain(r)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrSignIn
	case http.StatusConflict, http.StatusPreconditionFailed:
		return ErrPushConflict
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return ErrPushRefused
	}
	if status != http.StatusCreated || reported != "" && reported != digest {
		return ErrPushUnknown
	}
	got, err := p.ManifestDigest(ctx, reference)
	if err != nil || got != digest {
		return ErrPushUnknown
	}
	return nil
}
