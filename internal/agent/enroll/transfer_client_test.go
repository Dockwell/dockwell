// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// A transfer's connection keeps the pings that tell a silent connection from a
// slow one (ADR 0054), beside the windows that keep an upload in its memory
// (ADR 0045). Setting one must not drop the other.
func TestATransfersConnectionKeepsItsPingsAndItsWindows(t *testing.T) {
	tr, ok := transferHTTPClient(&tls.Config{MinVersion: tls.VersionTLS13}).Transport.(*http.Transport)
	if !ok || tr.HTTP2 == nil {
		t.Fatal("a transfer's client has no HTTP/2 settings")
	}
	if tr.HTTP2.SendPingTimeout != silentFor || tr.HTTP2.PingTimeout != silentFor {
		t.Errorf("pings after %s, answered within %s; want %s for both", tr.HTTP2.SendPingTimeout, tr.HTTP2.PingTimeout, silentFor)
	}
	if tr.HTTP2.MaxReceiveBufferPerStream != transferWindow || tr.HTTP2.MaxReceiveBufferPerConnection != transferConnWindow {
		t.Errorf("windows are %d a stream and %d a connection; want %d and %d",
			tr.HTTP2.MaxReceiveBufferPerStream, tr.HTTP2.MaxReceiveBufferPerConnection, transferWindow, transferConnWindow)
	}
}
