// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"connectrpc.com/connect"
)

func TestADeployRetriesOnlyWhatCanChange(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err   error
		retry bool
	}{
		"nothing":            {nil, false},
		"unavailable":        {connect.NewError(connect.CodeUnavailable, errors.New("down")), true},
		"cut short":          {fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), true},
		"refused":            {connect.NewError(connect.CodeUnavailable, &net.OpError{Op: "dial", Err: errors.New("connection refused")}), true},
		"no such host":       {connect.NewError(connect.CodeUnavailable, &net.DNSError{Err: "no such host", Name: "panel.example.invalid", IsNotFound: true}), false},
		"unknown authority":  {connect.NewError(connect.CodeUnavailable, x509.UnknownAuthorityError{}), false},
		"wrong host":         {connect.NewError(connect.CodeUnavailable, x509.HostnameError{Host: "panel.example.com"}), false},
		"permission refused": {connect.NewError(connect.CodePermissionDenied, errors.New("no")), false},
	} {
		if got := deployRetryable(c.err); got != c.retry {
			t.Errorf("%s: retryable = %v, want %v", name, got, c.retry)
		}
	}
}
