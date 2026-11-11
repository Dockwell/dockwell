// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
)

// What an install sets of a service's limits and restarts takes the place of the
// file's, field by field: a memory alone keeps the file's CPUs, and a restart other
// than on-failure drops the retries only on-failure takes. A service the file
// doesn't have is refused by name.
func TestAnInstallsLimitsTakeThePlaceOfTheFiles(t *testing.T) {
	stack := func() *compose.Stack {
		return &compose.Stack{Services: []compose.Item[*resourcev1.Service]{
			{Name: "db", Spec: svc(&resourcev1.Container{
				Image: "postgres:17", Resources: &resourcev1.Resources{Cpus: "2", Memory: "4G"},
				Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE, RestartRetries: 3,
			})},
			{Name: "web", Spec: svc(&resourcev1.Container{Image: "nginx:1"})},
		}}
	}
	s := stack()
	if err := limit(s, map[string]Limits{"db": {Memory: "1G", Restart: resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS}}); err != nil {
		t.Fatal(err)
	}
	db := s.Services[0].Spec.GetApp()
	if db.GetResources().GetMemory() != "1G" || db.GetResources().GetCpus() != "2" ||
		db.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS || db.GetRestartRetries() != 0 {
		t.Errorf("db runs with %v, restart %v and %d retries, want 1G, the file's 2 CPUs, always and none", db.GetResources(), db.GetRestart(), db.GetRestartRetries())
	}
	if web := s.Services[1].Spec.GetApp(); web.GetResources() != nil || web.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		t.Errorf("web, which nobody set, runs with %v and %v", web.GetResources(), web.GetRestart())
	}

	s = stack()
	if err := limit(s, map[string]Limits{"db": {Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Services[0].Spec.GetApp().GetRestartRetries(); got != 3 {
		t.Errorf("on-failure again kept %d retries, want the file's 3", got)
	}

	err := limit(stack(), map[string]Limits{"cache": {Memory: "1G"}})
	if err == nil || !strings.Contains(err.Error(), "no service called cache") || !strings.Contains(err.Error(), "Name one of db, web") {
		t.Errorf("limits for a service the file doesn't have: %v", err)
	}
}
