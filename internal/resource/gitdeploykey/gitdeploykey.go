// SPDX-License-Identifier: AGPL-3.0-only

// Package gitdeploykey is the "git.deploy_key" resource kind: a project-scoped
// Ed25519 SSH deploy key for fetching private Git repositories over SSH
// (ADR 0113). Its private keys are marked sensitive, sealed at rest under
// ADR 0022, and never leave the control plane.
package gitdeploykey

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/gitssh"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the SSH deploy key resource kind.
const Kind resource.Kind = "git.deploy_key"

// Type is the git.deploy_key resource kind implementation.
type Type struct{}

var (
	_ resource.Type           = Type{}
	_ resource.Rotates        = Type{}
	_ resource.Keeper         = Type{}
	_ resource.ReadChanger    = Type{}
	_ resource.NewChecker     = Type{}
	_ resource.ElsewhereActor = Type{}
)

// Kind returns "git.deploy_key".
func (Type) Kind() resource.Kind { return Kind }

// Level is the project: a deploy key belongs to one project and can be used
// only by services in that project.
func (Type) Level() resource.Level { return resource.LevelProject }

// IDPrefix is "gdk".
func (Type) IDPrefix() string { return "gdk" }

// New returns an empty GitDeployKey spec.
func (Type) New() proto.Message { return &resourcev1.GitDeployKey{} }

// RotatesSensitive says sensitive key fields never compare for equality in
// plans when explicitly restated, preventing any API equality oracle (ADR 0022).
func (Type) RotatesSensitive() {}

// ChangesWhatsRead returns false because git.deploy_key is not read through
// resource.ReadThrough; edits that omit private_key (such as host-key pinning
// or observation updates) keep it via Keep without reporting private_key as
// modified in the plan.
func (Type) ChangesWhatsRead(_, _ proto.Message) bool { return false }

// ActsElsewhere explains why deploy key changes cannot be undone automatically
// through ChangeService because keys and host-key trust are managed through
// GitDeployKeyService and on the Git host.
func (Type) ActsElsewhere(current, desired proto.Message) string {
	cur, _ := current.(*resourcev1.GitDeployKey)
	des, _ := desired.(*resourcev1.GitDeployKey)
	switch {
	case cur == nil && des == nil:
		return ""
	case cur == nil:
		return "creating a deploy key generates a key pair that may already be added on the Git host. Delete the key instead, and remove it on the host"
	case des == nil:
		return "deleting a deploy key removed it from Dockwell, and it may already be removed on the Git host. Create a new deploy key instead"
	case cur.GetPrivateKey() != des.GetPrivateKey() && des.GetPrivateKey() != "":
		return "rotating a deploy key switched to a new key, and the old key may already be removed on the Git host. Rotate it again if you need a new key"
	default:
		return "deploy keys and their pinned SSH host keys are managed through dockwell git-keys, so Dockwell won't undo them automatically. Test or rotate the deploy key instead"
	}
}

// CheckName validates the deploy key's name within its project.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("deploy key", name, "acme-web")
}

// Validate checks a GitDeployKey spec without leaking any private key material.
func (Type) Validate(spec proto.Message) error {
	k := spec.(*resourcev1.GitDeployKey) //nolint:forcetypeassert // kind's own message
	if k.GetRepository() != "" {
		ep, err := gitssh.ParseSSH(k.GetRepository())
		if err != nil {
			return err
		}
		k.Repository = ep.CanonicalURL
		k.Host = ep.Host
		k.Port = ep.Port
		k.User = ep.User
	}
	if k.GetPort() > 65535 {
		return errors.New("an SSH port is from 1 to 65535")
	}
	if k.GetPrivateKey() != "" && !strings.Contains(k.GetPrivateKey(), "PRIVATE KEY-----") {
		return errors.New("the deploy key's private half isn't a valid OpenSSH private key")
	}
	if k.GetPendingPrivateKey() != "" && !strings.Contains(k.GetPendingPrivateKey(), "PRIVATE KEY-----") {
		return errors.New("the pending deploy key's private half isn't a valid OpenSSH private key")
	}
	if k.GetHostKeyFingerprint() != "" && !gitssh.ValidFingerprint(k.GetHostKeyFingerprint()) {
		return errors.New("the host key fingerprint must be an OpenSSH SHA256 fingerprint")
	}
	if k.GetHostKeyAlgorithm() != "" && !gitssh.ValidAlgorithm(k.GetHostKeyAlgorithm()) {
		return fmt.Errorf("%q isn't a supported SSH host key type", k.GetHostKeyAlgorithm())
	}
	if k.GetObservedHostKeyFingerprint() != "" && !gitssh.ValidFingerprint(k.GetObservedHostKeyFingerprint()) {
		return errors.New("the observed host key fingerprint must be an OpenSSH SHA256 fingerprint")
	}
	if k.GetObservedHostKeyAlgorithm() != "" && !gitssh.ValidAlgorithm(k.GetObservedHostKeyAlgorithm()) {
		return fmt.Errorf("%q isn't a supported observed SSH host key type", k.GetObservedHostKeyAlgorithm())
	}
	return nil
}

// CheckNew requires every field a newly generated deploy key carries.
func (Type) CheckNew(spec proto.Message) error {
	k := spec.(*resourcev1.GitDeployKey) //nolint:forcetypeassert // Validate saw it
	if k.GetRepository() == "" || k.GetPrivateKey() == "" || k.GetPublicKey() == "" || k.GetKeyFingerprint() == "" {
		return errors.New("create a deploy key through GitDeployKeyService so Dockwell generates its Ed25519 key pair")
	}
	return nil
}

// Keep preserves sealed private keys when an update omits them (for example
// when pinning a host key or clearing observed mismatch state) and refuses
// retargeting an existing key to a different repository.
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.GitDeployKey)
	is, same := desired.(*resourcev1.GitDeployKey)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	if is.GetRepository() != "" && !gitssh.SameRepo(was.GetRepository(), is.GetRepository()) {
		return resource.Kept{}, errors.New("a deploy key stays on one repository. Create a new deploy key for another repository")
	}
	if is.GetRepository() == "" {
		is.Repository = was.GetRepository()
		is.Host = was.GetHost()
		is.Port = was.GetPort()
		is.User = was.GetUser()
	}
	if is.GetCreatedAt() == 0 {
		is.CreatedAt = was.GetCreatedAt()
	}
	if is.GetRotatedAt() == 0 {
		is.RotatedAt = was.GetRotatedAt()
	}
	if is.GetVerifiedAt() == 0 {
		is.VerifiedAt = was.GetVerifiedAt()
	}
	kept := false
	if is.GetPrivateKey() == "" && was.GetPrivateKey() != "" {
		is.PrivateKey = was.GetPrivateKey()
		is.PublicKey = was.GetPublicKey()
		is.KeyType = was.GetKeyType()
		is.KeyFingerprint = was.GetKeyFingerprint()
		kept = true
	}
	if is.GetPendingPrivateKey() == "" && is.GetPendingPublicKey() != "" && is.GetPendingPublicKey() == was.GetPendingPublicKey() && was.GetPendingPrivateKey() != "" {
		is.PendingPrivateKey = was.GetPendingPrivateKey()
		kept = true
	}
	if !kept {
		return resource.Kept{}, nil
	}
	return resource.Kept{Reason: "kept_key", Words: "keeps the deploy key's private half"}, nil
}
