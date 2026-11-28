// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCustomProbeTCPClosesWithoutSending(t *testing.T) {
	t.Parallel()
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	addr := netip.MustParseAddrPort(server.Addr().String())
	req := &Request{Kind: KindCustom, Custom: &Custom{Host: addr.Addr().String(), Port: uint32(addr.Port())}}
	result, err := Check(t.Context(), req)
	if err != nil || result.Passed || result.Code != CodeRefused {
		t.Fatalf("checking loopback without the app's network: %+v, %v", result, err)
	}
	child := make(chan error, 1)
	go func() {
		accepted, err := server.Accept()
		if err == nil {
			defer accepted.Close()
			_, err = accepted.Read(make([]byte, 1))
		}
		child <- err
	}()
	req.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if result, err := Check(t.Context(), req); err != nil || !result.Passed || result.Step != StepCustom {
		t.Fatalf("checking a TCP server on the app's network: %+v, %v", result, err)
	}
	if err := <-child; !errors.Is(err, io.EOF) {
		t.Errorf("the TCP check did more than connect and close: %v", err)
	}
}

func TestCustomProbeOnlyAcceptsOneCredentialFreeTarget(t *testing.T) {
	t.Parallel()
	for _, check := range []Custom{
		{HTTPS: "http://example.com/ready"},
		{HTTPS: "https://example.com/ready?secret=local-example"},
		{HTTPS: "https://user:password@example.com/ready"},
		{HTTPS: "https://example.com/ready#private"},
		{HTTPS: "https://example.com:/ready"},
		{HTTPS: "https://example.com:65536/ready"},
		{HTTPS: "https://example.com/ready", Host: "example.com", Port: 443},
		{Host: "example.com/path", Port: 443},
	} {
		if err := check.Validate(); err == nil {
			t.Errorf("accepted invalid target %+v", check)
		}
	}
	req := &Request{Kind: KindCustom, Custom: &Custom{HTTPS: "https://metadata.google.internal/ready"}}
	result, err := Check(t.Context(), req)
	if err != nil || result.Passed || result.Code != CodeRefused {
		t.Fatalf("the Custom probe did not refuse a metadata address: %+v, %v", result, err)
	}
}

func TestCustomProbeHTTPSDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	const targetVar = "DOCKWELL_TEST_CUSTOM_HTTPS_REDIRECT"
	if target := os.Getenv(targetVar); target != "" {
		result, err := Check(t.Context(), &Request{
			Kind: KindCustom, Custom: &Custom{HTTPS: target},
			Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		})
		if err != nil || result.Passed || result.Status != http.StatusFound {
			t.Fatalf("want the redirect status from the first HTTPS target, got %+v, %v", result, err)
		}
		return
	}

	var redirected atomic.Bool
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("the check sent something other than a credential-free GET")
		}
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	cert := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: source.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	// The isolated process trusts this fixture without changing the other probes' roots.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCustomProbeHTTPSDoesNotFollowRedirects$")
	child.Env = append(os.Environ(), "SSL_CERT_FILE="+cert, targetVar+"="+source.URL)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("checking a TLS redirect: %v\n%s", err, out)
	}
	if redirected.Load() {
		t.Error("the check followed a redirect")
	}
}
