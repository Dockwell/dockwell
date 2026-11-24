// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/fail2ban"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/sshconfig"
)

func TestSSHPlanRefusesOtherAccessEditsOnThatServer(t *testing.T) {
	e := &Engine{kinds: resource.NewRegistry(&sshconfig.Driver{}, &firewall.Driver{}, &firewall.PolicyDriver{}, &fail2ban.Driver{})}
	for _, kind := range []resource.Kind{firewall.Kind, firewall.PolicyKind, fail2ban.Kind} {
		items := []item{
			{ref: resource.Ref{Kind: sshconfig.Kind, Scope: "srv_A", Name: "default"}, servers: []string{"srv_A"}},
			{ref: resource.Ref{Kind: kind, Scope: "srv_A", Name: "other"}, servers: []string{"srv_A"}},
		}
		if _, err := e.plan(t.Context(), "chg_test", items, false, planWith{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("planning SSH alongside %s: %v, want ErrInvalid", kind, err)
		}
	}
}
