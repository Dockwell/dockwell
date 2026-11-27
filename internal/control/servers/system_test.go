// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
)

func TestAgentWordsTrimsRedundantServerPrefix(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("web-1: permission denied")
	toolErr := errors.New("systemctl: unit failed")

	cases := []struct {
		name   string
		err    error
		server string
		want   string
		target error
	}{
		{
			name:   "server error wrapping prefixed message",
			err:    &resource.ServerError{Server: "web-1", Err: sentinel},
			server: "web-1",
			want:   "permission denied",
			target: sentinel,
		},
		{
			name:   "server error with repeated server prefix",
			err:    &resource.ServerError{Server: "web-1", Err: errors.New("web-1: web-1: dbus is unavailable")},
			server: "web-1",
			want:   "dbus is unavailable",
		},
		{
			name:   "other server error untouched",
			err:    &resource.ServerError{Server: "db-1", Err: errors.New("db-1: timeout")},
			server: "web-1",
			want:   "db-1: timeout",
		},
		{
			name:   "empty server name leaves message untouched",
			err:    &resource.ServerError{Server: "web-1", Err: sentinel},
			server: "",
			want:   "web-1: permission denied",
			target: sentinel,
		},
		{
			name:   "tool-named server preserves plain tool error",
			err:    toolErr,
			server: "systemctl",
			want:   "systemctl: unit failed",
			target: toolErr,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agentWords(tc.err, tc.server)
			if got.Error() != tc.want {
				t.Fatalf("agentWords(%v, %q) = %q, want %q", tc.err, tc.server, got.Error(), tc.want)
			}
			if tc.target != nil && !errors.Is(got, tc.target) {
				t.Fatalf("agentWords(%v, %q) dropped wrapped target %v", tc.err, tc.server, tc.target)
			}
		})
	}
}
