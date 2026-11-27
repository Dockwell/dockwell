// SPDX-License-Identifier: AGPL-3.0-only

package gitdeploykey_test

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/gitssh"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/gitdeploykey"
)

func TestGitDeployKeyType(t *testing.T) {
	kp, err := gitssh.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var typ gitdeploykey.Type
	if !resource.Sealed(typ) {
		t.Fatal("git.deploy_key must be sealed at rest (ADR 0022)")
	}
	if typ.Level() != resource.LevelProject {
		t.Fatalf("Level() = %v, want LevelProject", typ.Level())
	}

	spec := &resourcev1.GitDeployKey{
		Repository:     "git@gitlab.com:acme/web.git",
		KeyType:        kp.KeyType,
		PublicKey:      kp.PublicKey,
		KeyFingerprint: kp.Fingerprint,
		PrivateKey:     kp.PrivateKeyPEM,
		CreatedAt:      1700000000,
	}
	if err := typ.Validate(spec); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if spec.GetRepository() != "ssh://git@gitlab.com/acme/web.git" || spec.GetHost() != "gitlab.com" || spec.GetPort() != 22 || spec.GetUser() != "git" {
		t.Fatalf("unexpected normalized spec: %+v", spec)
	}
	if err := typ.CheckNew(spec); err != nil {
		t.Fatalf("CheckNew: %v", err)
	}

	// Keep preserves private key when updating host key pin.
	pinUpdate := &resourcev1.GitDeployKey{
		Repository:         spec.GetRepository(),
		HostKeyAlgorithm:   "ssh-ed25519",
		HostKeyFingerprint: kp.Fingerprint,
	}
	kept, err := typ.Keep(spec, pinUpdate)
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if kept.Reason != "kept_key" || pinUpdate.GetPrivateKey() != kp.PrivateKeyPEM {
		t.Fatalf("expected private key to be kept, got %+v", kept)
	}

	// Retargeting to a different repository is refused.
	if _, err := typ.Keep(spec, &resourcev1.GitDeployKey{Repository: "ssh://git@gitlab.com/other/repo.git"}); err == nil {
		t.Fatal("expected retargeting to another repository to fail")
	}

	// ActsElsewhere blocks undo on create, delete, and rotation, but allows host key pin update.
	if typ.ActsElsewhere(nil, spec) == "" {
		t.Fatal("expected create to refuse automatic undo")
	}
	if typ.ActsElsewhere(spec, nil) == "" {
		t.Fatal("expected delete to refuse automatic undo")
	}
	if typ.ActsElsewhere(spec, pinUpdate) != "" {
		t.Fatal("expected host-key pin update with same private key to not block undo")
	}
}
