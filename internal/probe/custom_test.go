// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"errors"
	"io"
	"net"
	"net/netip"
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
