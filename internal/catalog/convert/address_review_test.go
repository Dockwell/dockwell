// SPDX-License-Identifier: AGPL-3.0-only

package convert

import "testing"

// Review of #779: a Matrix server's name is set once, and a variable on a service
// that shares the server's network reaches the app on loopback: neither follows the
// address (H3, M5).
func TestReview779WhatShouldntFollow(t *testing.T) {
	file := []byte(`services:
  homeserver:
    image: example/conduit:1.0
    environment:
      CONDUIT_SERVER_NAME: localhost:${PORT}
  agent:
    image: example/agent:1.0
    network_mode: host
    environment:
      HUB_URL: http://localhost:${PORT}
`)
	for _, a := range addressVariables(file, "PORT") {
		t.Errorf("%s.%s follows the address as %q", a.GetService(), a.GetVariable(), a.GetValue())
	}
}
