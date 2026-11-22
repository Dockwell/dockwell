// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/registry"
)

// DockerImageAuthRef binds sealed credentials to one frozen image operation and phase.
func DockerImageAuthRef(ref Ref, plan *v1.DockerMaintenance) Ref {
	data, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	return Ref{Kind: "docker.image", Scope: ref.Scope, Name: fmt.Sprintf("%x", sha256.Sum256(data))}
}

// DockerImageNeedsPasskey reports a reviewed replacement of an existing alias.
func DockerImageNeedsPasskey(action *v1.DockerImageAction) bool {
	return len(action.GetBindings()) == 1 && action.GetBindings()[0].GetImageId() != "" && DockerImageRetargetsAlias(action)
}

// DockerImageRetargetsAlias reports whether the plan would assign a different local image ID.
func DockerImageRetargetsAlias(action *v1.DockerImageAction) bool {
	if len(action.GetBindings()) != 1 || strings.Contains(action.GetPull().GetReference(), "@") {
		return false
	}
	binding := action.GetBindings()[0]
	if tag := action.GetTag(); tag != nil {
		return tag.GetSourceKey().GetImageId() != binding.GetImageId()
	}
	current, candidate := binding.GetLocal(), action.GetDescriptor_()
	// The classic store can refresh a manifest without changing its config ID.
	sameConfigID := current.GetConfigDigest() != "" && current.GetConfigDigest() == binding.GetImageId() &&
		current.GetConfigDigest() == candidate.GetConfigDigest()
	return !sameConfigID && !DockerImageMatchesDigest(current, candidate)
}

// DockerImageMatchesDigest compares registry identities of the same kind. The
// same index proves an up-to-date image only if the selected platform is present.
func DockerImageMatchesDigest(current, candidate *v1.DockerRegistryDescriptor) bool {
	if current.GetConfigDigest() != "" && current.GetConfigDigest() != candidate.GetConfigDigest() {
		return false
	}
	if current.GetManifestDigest() != "" && current.GetManifestDigest() == candidate.GetManifestDigest() {
		return true
	}
	return current.GetIndexDigest() != "" && current.GetIndexDigest() == candidate.GetIndexDigest() &&
		current.GetPlatform() != "" && current.GetPlatform() == candidate.GetPlatform()
}

// DockerAliasProtected matches aliases in retained desired state by canonical reference.
func DockerAliasProtected(ref string, held []string) bool {
	target, err := registry.Parse(ref)
	if err != nil || target.Digest != "" {
		return false
	}
	return slices.ContainsFunc(held, func(name string) bool {
		if strings.HasPrefix(name, "sha256:") {
			return false
		}
		pinned, err := registry.Parse(name)
		return err == nil && pinned.Digest == "" && pinned.Name() == target.Name() && pinned.Reference() == target.Reference()
	})
}

// DockerBuildID is the retained build record of a one-server image change.
func DockerBuildID(change string) string {
	return "bld_" + strings.TrimPrefix(change, "chg_")
}

// DockerBuildLeases lists the private inputs a standalone change needs to keep.
func DockerBuildLeases(action *v1.DockerImageAction) []string {
	if action.GetBuild() == nil {
		return nil
	}
	var leases []string
	for _, lease := range []string{action.GetStagedSourceId(), action.GetBuild().GetDockerfileOverrideArtifactId()} {
		if lease != "" {
			leases = append(leases, lease)
		}
	}
	return leases
}
