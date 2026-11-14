// SPDX-License-Identifier: AGPL-3.0-only

package docker

import "testing"

// What runs a Compose project is read from its labels, so a plan can say what that
// tool's remove does (ADR 0031, ADR 0089).
func TestManagedBy(t *testing.T) {
	for want, labels := range map[string]map[string]string{
		"1Panel":  {LabelWorkingDir: "/opt/1panel/apps/mysql/mysql"},
		"Coolify": {"coolify.managed": "true", LabelWorkingDir: "/data/coolify/services/k4s9"},
		"Dokploy": {LabelWorkingDir: "/etc/dokploy/compose/wiki-ab12/code"},
		"":        {LabelWorkingDir: "/srv/mine"},
	} {
		if got := ManagedBy(labels); got != want {
			t.Errorf("ManagedBy(%v) = %q, want %q", labels, got, want)
		}
	}
	if got := ManagedBy(map[string]string{"dokploy.project": "store"}); got != "Dokploy" {
		t.Errorf("a container with Dokploy's labels is managed by %q", got)
	}
}
