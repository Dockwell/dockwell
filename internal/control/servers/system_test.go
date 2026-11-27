// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
)

func TestAgentWordsTrimsRedundantServerPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		err    error
		server string
		want   string
	}{
		{
			name:   "plain error with server prefix",
			err:    errors.New("web-1: systemctl isn't on its PATH"),
			server: "web-1",
			want:   "systemctl isn't on its PATH",
		},
		{
			name:   "repeated server prefix",
			err:    errors.New("web-1: web-1: dbus is unavailable"),
			server: "web-1",
			want:   "dbus is unavailable",
		},
		{
			name:   "server error wrapping prefixed message",
			err:    &resource.ServerError{Server: "web-1", Err: errors.New("web-1: permission denied")},
			server: "web-1",
			want:   "permission denied",
		},
		{
			name:   "other server prefix untouched",
			err:    errors.New("db-1: timeout"),
			server: "web-1",
			want:   "db-1: timeout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agentWords(tc.err, tc.server).Error()
			if got != tc.want {
				t.Fatalf("agentWords(%v, %q) = %q, want %q", tc.err, tc.server, got, tc.want)
			}
		})
	}
}
