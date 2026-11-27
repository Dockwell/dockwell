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
	for _, tt := range []struct {
		name               string
		artifact           bool
		stream, connection int
	}{
		{name: "files", stream: 256 << 10, connection: 1024 << 10},
		{name: "artifacts", artifact: true, stream: 144 << 10, connection: 432 << 10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr, ok := transferHTTPClient(&tls.Config{MinVersion: tls.VersionTLS13}, tt.artifact).Transport.(*http.Transport)
			if !ok || tr.HTTP2 == nil {
				t.Fatal("a transfer's client has no HTTP/2 settings")
			}
			h2 := tr.HTTP2
			if h2.SendPingTimeout != silentFor || h2.PingTimeout != silentFor {
				t.Errorf("pings after %s, answered within %s; want %s for both", h2.SendPingTimeout, h2.PingTimeout, silentFor)
			}
			if h2.MaxReceiveBufferPerStream != tt.stream || h2.MaxReceiveBufferPerConnection != tt.connection {
				t.Errorf("windows are %d a stream and %d a connection; want %d and %d",
					h2.MaxReceiveBufferPerStream, h2.MaxReceiveBufferPerConnection, tt.stream, tt.connection)
			}
			if h2.MaxReadFrameSize != 16<<10 {
				t.Errorf("maximum HTTP/2 frame is %d bytes; want 16 KiB", h2.MaxReadFrameSize)
			}
		})
	}
}
