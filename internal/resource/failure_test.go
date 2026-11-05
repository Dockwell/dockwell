// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
)

// A failure in plain words keeps the chain behind it, and the words nearest
// to what happened win over any said further out.
func TestFailuresKeepWhatTheServerSaid(t *testing.T) {
	raw := errors.New("pull access denied")
	inner := resource.Plainly("nginx:9 can't be pulled.", raw)
	wrapped := &resource.ServerError{Server: "hz-1", Err: fmt.Errorf("can't create service web in env_01: %w", inner)}

	plain, detail := resource.Explain(wrapped)
	if plain != "nginx:9 can't be pulled." {
		t.Errorf("plain = %q", plain)
	}
	if detail != "hz-1: can't create service web in env_01: pull access denied" {
		t.Errorf("detail = %q", detail)
	}
	if !errors.Is(wrapped, raw) {
		t.Error("the chain lost what it wraps")
	}
	if plain, _ := resource.Explain(resource.Plainly("Something further out.", inner)); plain != "nginx:9 can't be pulled." {
		t.Errorf("Plainly over plain words reads %q, want them kept", plain)
	}

	// An error nobody put in plain words is its own message.
	plain, detail = resource.Explain(errors.New("hz-1's agent disconnected. Check that hz-1 is up"))
	if plain != "hz-1's agent disconnected. Check that hz-1 is up" || detail != "" {
		t.Errorf("plain %q, detail %q, want the message alone", plain, detail)
	}
}
