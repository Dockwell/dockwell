// SPDX-License-Identifier: AGPL-3.0-only

package registrylogins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/registrylogin"
)

// Doer sends a request, as http.Client's Do does.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// ECR gets Amazon ECR's sign-in tokens with an AWS key, which stays on the control
// plane: a token lasts 12 hours, and is kept in memory until an hour before it
// ends, so a pull never starts with one about to run out.
type ECR struct {
	// Client reaches AWS: netguard's, for the internet alone.
	Client Doer
	// Endpoint is ECR's API in a region; AWS's own unless a test sets it.
	Endpoint func(region string) string
	// Now is the time, which a test sets.
	Now func() time.Time

	mu     sync.Mutex
	tokens map[string]ecrToken
}

type ecrToken struct {
	password, registry string
	ends               time.Time
}

// ecrRefresh is how long before its end a token is got again.
const ecrRefresh = time.Hour

// NewECR returns an ECR that reaches AWS through netguard.
func NewECR() *ECR {
	return &ECR{
		Client: netguard.Client(netguard.Options{MaxBody: 1 << 20, Timeout: 15 * time.Second}),
		Endpoint: func(region string) string {
			if strings.HasPrefix(region, "cn-") {
				return "https://api.ecr." + region + ".amazonaws.com.cn"
			}
			return "https://api.ecr." + region + ".amazonaws.com"
		},
		Now:    time.Now,
		tokens: map[string]ecrToken{},
	}
}

// Token is the password a pull from host signs in with as user AWS, got with key.
func (e *ECR) Token(ctx context.Context, host string, key *resourcev1.RegistryLoginEcr) (string, error) {
	region, ok := registrylogin.ECRRegion(host)
	if !ok || key.GetRegion() != "" && key.GetRegion() != region {
		return "", fmt.Errorf("%s isn't an Amazon ECR registry", host)
	}
	// Each key's token, for its registry, kept under a digest of both.
	sum := sha256.Sum256([]byte(host + "\x00" + key.GetAccessKey() + "\x00" + key.GetSecretKey()))
	cacheKey := hex.EncodeToString(sum[:])
	e.mu.Lock()
	if t, ok := e.tokens[cacheKey]; ok && e.Now().Add(ecrRefresh).Before(t.ends) {
		e.mu.Unlock()
		return t.password, nil
	}
	e.mu.Unlock()
	t, err := e.get(ctx, region, key)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(t.registry, host) {
		// A key's token signs in to its own account's registry alone.
		return "", fmt.Errorf("the AWS key belongs to another account than %s. Use a key of the registry's account", host)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, old := range e.tokens {
		if !e.Now().Before(old.ends) {
			delete(e.tokens, k)
		}
	}
	e.tokens[cacheKey] = t
	return t.password, nil
}

// Registry gets a token with key in region, and says the registry it signs in to,
// for a check of a key whose registry isn't known yet. It keeps nothing.
func (e *ECR) Registry(ctx context.Context, region string, key *resourcev1.RegistryLoginEcr) (host, password string, err error) {
	t, err := e.get(ctx, region, key)
	return t.registry, t.password, err
}

// get calls ECR's GetAuthorizationToken, signed with SigV4.
func (e *ECR) get(ctx context.Context, region string, key *resourcev1.RegistryLoginEcr) (ecrToken, error) {
	// The region goes into the address of ECR's API, so it's a region's name alone.
	if !registrylogin.IsRegion(region) {
		return ecrToken{}, fmt.Errorf("%q isn't an AWS region", region)
	}
	body := []byte("{}")
	sum := sha256.Sum256(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint(region)+"/", bytes.NewReader(body))
	if err != nil {
		return ecrToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken")
	s3.SignV4(req, hex.EncodeToString(sum[:]), key.GetAccessKey(), key.GetSecretKey(), region, "ecr", e.Now())
	resp, err := e.Client.Do(req)
	if err != nil {
		return ecrToken{}, fmt.Errorf("can't reach Amazon ECR in %s: %s", region, netguard.Words(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return ecrToken{}, errECRRefused
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return ecrToken{}, fmt.Errorf("can't get a token from Amazon ECR in %s, which answered %d. Try again in a moment", region, resp.StatusCode)
	}
	var got struct {
		AuthorizationData []struct {
			AuthorizationToken string  `json:"authorizationToken"`
			ExpiresAt          float64 `json:"expiresAt"`
			ProxyEndpoint      string  `json:"proxyEndpoint"`
		} `json:"authorizationData"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&got); err != nil || len(got.AuthorizationData) == 0 {
		return ecrToken{}, fmt.Errorf("can't get a token from Amazon ECR in %s, which answered with something else", region)
	}
	d := got.AuthorizationData[0]
	u, err := url.Parse(d.ProxyEndpoint)
	if err != nil || u.Host == "" {
		return ecrToken{}, fmt.Errorf("can't get a token from Amazon ECR in %s, which named a registry Dockwell can't read", region)
	}
	if r, ok := registrylogin.ECRRegion(strings.ToLower(u.Host)); !ok || r != region {
		return ecrToken{}, fmt.Errorf("can't get a token from Amazon ECR in %s, which named a registry outside it", region)
	}
	raw, err := base64.StdEncoding.DecodeString(d.AuthorizationToken)
	user, password, ok := strings.Cut(string(raw), ":")
	if err != nil || !ok || user != "AWS" || password == "" {
		return ecrToken{}, fmt.Errorf("can't get a token from Amazon ECR in %s, which answered with one Dockwell can't read", region)
	}
	ends := time.Unix(int64(d.ExpiresAt), 0)
	if d.ExpiresAt == 0 {
		ends = e.Now().Add(12 * time.Hour)
	}
	return ecrToken{password: password, ends: ends, registry: strings.ToLower(u.Host)}, nil
}

// errECRRefused is a key AWS refused.
var errECRRefused = errors.New("AWS refused the key: it's wrong, or it may not call ecr:GetAuthorizationToken. Give it that permission, or use another key")

// IsECRRefused says whether err is a key AWS refused.
func IsECRRefused(err error) bool { return errors.Is(err, errECRRefused) }
