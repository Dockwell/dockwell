// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// A service's architectures are those Dockwell's agent runs on, each once and sorted,
// and only the control plane keeps them: its server never sees them, and drift its
// server reports keeps them as they were (ADR 0102).
func TestAServicesArchitecturesAreTheControlPlanes(t *testing.T) {
	d := &service.Driver{}
	svc := func(arch ...string) *resourcev1.Service {
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}, Architectures: arch}
	}
	s := svc("arm64", "arm64")
	if err := d.Validate(s); err != nil || !slices.Equal(s.GetArchitectures(), []string{"arm64"}) {
		t.Errorf("Validate = %v, %v", s.GetArchitectures(), err)
	}
	// Both is anywhere, as an entry has it (#570's arm64 review, L1).
	both := svc("arm64", "amd64")
	if err := d.Validate(both); err != nil || both.GetArchitectures() != nil {
		t.Errorf("both: %v, %v", both.GetArchitectures(), err)
	}
	// An edit that leaves them out keeps them; one that names others has its own
	// (#570's arm64 review, M2).
	left := svc()
	d.HoldUnset(svc("amd64"), left)
	named := svc("arm64")
	d.HoldUnset(svc("amd64"), named)
	if !slices.Equal(left.GetArchitectures(), []string{"amd64"}) || !slices.Equal(named.GetArchitectures(), []string{"arm64"}) {
		t.Errorf("held %v and %v", left.GetArchitectures(), named.GetArchitectures())
	}
	if err := d.Validate(svc("riscv64")); err == nil || !strings.Contains(err.Error(), `a service runs on amd64 or arm64, not "riscv64"`) {
		t.Errorf("riscv64: %v", err)
	}
	amd := svc("amd64")
	if got := d.OnServer(amd).(*resourcev1.Service); len(got.GetArchitectures()) != 0 || len(amd.GetArchitectures()) != 1 {
		t.Errorf("on its server it runs on %v, and the spec on %v", got.GetArchitectures(), amd.GetArchitectures())
	}
	if got := d.Readopted(amd, svc()).(*resourcev1.Service); !slices.Equal(got.GetArchitectures(), []string{"amd64"}) {
		t.Errorf("drift read back runs on %v", got.GetArchitectures())
	}
}
