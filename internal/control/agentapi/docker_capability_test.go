// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestDockerMaintenanceCapabilityCheckSharedBetweenPlanAndApply(t *testing.T) {
	volMaint := &v1.DockerMaintenance{
		Targets: []*v1.DockerCleanupTarget{{
			ServerId: "srv_1",
			Kind:     v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
			EngineId: "local/vol1",
			Name:     "vol1",
		}},
	}
	manyImages := make([]*v1.DockerCleanupTarget, 13)
	for i := range manyImages {
		manyImages[i] = &v1.DockerCleanupTarget{
			ServerId: "srv_1",
			Kind:     v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
			EngineId: "sha256:" + strings.Repeat("a", 62) + string(rune('a'+i/10)) + string(rune('0'+i%10)),
		}
	}
	over12Maint := &v1.DockerMaintenance{Targets: manyImages}

	downgraded := &Session{
		agents:               &Agents{},
		writesReferences:     true,
		readsTakenReferences: true,
		dockerCapabilities: &agentv1.DockerCapabilities{
			Cleanup:         true,
			ArtifactCleanup: true,
			VolumeCleanup:   false,
			ExtendedCleanup: false,
		},
	}

	// Both volume targets and >12 targets are rejected with ErrOlderAgent before
	// sending either Plan or Apply to a downgraded session.
	if err := downgraded.checkDockerMaintenanceSupport(volMaint); !errors.Is(err, ErrOlderAgent) || !strings.Contains(err.Error(), "unused Docker volumes") {
		t.Fatalf("downgraded volume support check = %v, want ErrOlderAgent for volumes", err)
	}
	if err := downgraded.checkDockerMaintenanceSupport(over12Maint); !errors.Is(err, ErrOlderAgent) || !strings.Contains(err.Error(), "more than 12 Docker artifacts") {
		t.Fatalf("downgraded >12 support check = %v, want ErrOlderAgent for >12 targets", err)
	}
	if _, err := downgraded.Plan(t.Context(), []resource.Desired{{DockerMaintenance: volMaint}}); !errors.Is(err, ErrOlderAgent) || !strings.Contains(err.Error(), "unused Docker volumes") {
		t.Fatalf("Plan on downgraded session for volume = %v, want ErrOlderAgent for volumes", err)
	}
	if _, err := downgraded.Plan(t.Context(), []resource.Desired{{DockerMaintenance: over12Maint}}); !errors.Is(err, ErrOlderAgent) || !strings.Contains(err.Error(), "more than 12 Docker artifacts") {
		t.Fatalf("Plan on downgraded session for >12 targets = %v, want ErrOlderAgent for >12 targets", err)
	}

	upgraded := &Session{
		dockerCapabilities: &agentv1.DockerCapabilities{
			Cleanup:         true,
			ArtifactCleanup: true,
			VolumeCleanup:   true,
			ExtendedCleanup: true,
		},
	}
	if err := upgraded.checkDockerMaintenanceSupport(volMaint); err != nil {
		t.Fatalf("upgraded volume support check = %v, want nil", err)
	}
	if err := upgraded.checkDockerMaintenanceSupport(over12Maint); err != nil {
		t.Fatalf("upgraded >12 support check = %v, want nil", err)
	}
}
