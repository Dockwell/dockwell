// SPDX-License-Identifier: AGPL-3.0-only

// Package gitkeys implements GitDeployKeyService: project-scoped Ed25519 SSH
// deploy keys for fetching private Git repositories over SSH (ADR 0113).
// Private keys are generated and sealed on the control plane under ADR 0022
// and never leave the control plane or appear in any API response, CLI output,
// plan, log, Activity entry or error.
package gitkeys

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/gitssh"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/gitdeploykey"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

const maxBranches = 500

// Config wires GitDeployKeyService to the control plane's store, secrets keeper,
// revision/change engine, auth service and optional test overrides.
type Config struct {
	Auth    *auth.Service
	DB      *store.DB
	Keeper  *secrets.Keeper
	Changes *changes.Handler
	Logger  *slog.Logger
	// Local allows loopback SSH targets (tests and DOCKWELL_TEST=1 only).
	Local bool
	// Dial overrides netguard dialing in tests.
	Dial netguard.DialFunc
	// Now overrides time.Now in tests.
	Now func() time.Time
	// PackTempDir is a Dockwell-owned directory for temporary verified packs
	// (swept on startup). Empty falls back to os.TempDir().
	PackTempDir string
}

// Handler serves GitDeployKeyService and provides deploy-key lookup, pinning,
// ref resolution and pack fetching for the control plane.
type Handler struct {
	cfg  Config
	gate sshGate
	// changing serializes mutating operations on keys so concurrent tests,
	// rotations and pin updates cannot clobber each other.
	changing sync.Mutex
}

var _ dockwellv1connect.GitDeployKeyServiceHandler = (*Handler)(nil)

// New returns a GitDeployKeyService handler.
func New(cfg Config) *Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	sweepPackTempDir(cfg.PackTempDir, cfg.Logger)
	return &Handler{cfg: cfg}
}

// sweepPackTempDir creates the Dockwell-owned SSH pack scratch folder and
// removes any temporary packs left behind by a crashed control plane.
func sweepPackTempDir(dir string, logger *slog.Logger) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warn("couldn't create SSH pack scratch directory", slog.String("dir", dir), slog.String("error", err.Error()))
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "dockwell-ssh-pack-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func (h *Handler) now() time.Time {
	if h.cfg.Now != nil {
		return h.cfg.Now()
	}
	return time.Now()
}

type kept struct {
	r    store.Resource
	spec *resourcev1.GitDeployKey
}

// ListGitDeployKeys lists all deploy keys belonging to a project.
func (h *Handler) ListGitDeployKeys(ctx context.Context, req *connect.Request[dockwellv1.ListGitDeployKeysRequest]) (*connect.Response[dockwellv1.ListGitDeployKeysResponse], error) {
	projectID, err := h.resolveProject(ctx, req.Msg.GetProject())
	if err != nil {
		return nil, err
	}
	var out []*dockwellv1.GitDeployKeyInfo
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		keys, err := h.allInProject(ctx, tx, projectID)
		if err != nil {
			return err
		}
		usedBy, err := h.usedByMap(ctx, tx, projectID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			out = append(out, toInfo(k, usedBy[service.SSHLoginPrefix+k.r.Name]))
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "listing deploy keys", err)
	}
	return connect.NewResponse(&dockwellv1.ListGitDeployKeysResponse{Keys: out}), nil
}

// CreateGitDeployKey generates a new Ed25519 deploy key for a repository in a
// project and stores it sealed through the revision store.
func (h *Handler) CreateGitDeployKey(ctx context.Context, req *connect.Request[dockwellv1.CreateGitDeployKeyRequest]) (*connect.Response[dockwellv1.CreateGitDeployKeyResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	projectID, err := h.resolveProject(ctx, req.Msg.GetProject())
	if err != nil {
		return nil, err
	}
	ep, err := gitssh.ParseOrConvertSSH(req.Msg.GetRepository())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	if why := netguard.Forbidden(ep.Host, netguard.Options{Private: true, Loopback: h.cfg.Local}); why != "" {
		return nil, authapi.Invalid(fmt.Errorf("%s is %s, so Dockwell sends nothing there", ep.Host, why))
	}
	if _, err := h.chooseName(ctx, projectID, req.Msg.GetName(), ep); err != nil {
		return nil, err
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpCreateGitDeployKey, projectID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	h.changing.Lock()
	defer h.changing.Unlock()
	// Re-check name uniqueness under lock.
	name, err := h.chooseName(ctx, projectID, req.Msg.GetName(), ep)
	if err != nil {
		giveBack()
		return nil, err
	}
	kp, err := gitssh.GenerateKey()
	if err != nil {
		giveBack()
		return nil, h.internal(ctx, "generating an SSH deploy key", err)
	}
	spec := &resourcev1.GitDeployKey{
		Repository:     ep.CanonicalURL,
		Host:           ep.Host,
		Port:           ep.Port,
		User:           ep.User,
		KeyType:        kp.KeyType,
		PublicKey:      kp.PublicKey,
		KeyFingerprint: kp.Fingerprint,
		PrivateKey:     kp.PrivateKeyPEM,
		CreatedAt:      h.now().Unix(),
	}
	changeID, err := h.applyEdit(ctx, "Create SSH deploy key "+name+" for "+ep.Shown(), projectID, name, 0, spec)
	if err != nil {
		giveBack()
		return nil, err
	}
	k, err := h.oneByName(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "git_deploy_key.created", k.r.ID, fmt.Sprintf("%s (%s) for %s", k.r.Name, kp.Fingerprint, ep.Shown()))
	}); err != nil {
		return nil, h.internal(ctx, "recording deploy key creation", err)
	}
	return connect.NewResponse(&dockwellv1.CreateGitDeployKeyResponse{
		Key:      toInfo(k, nil),
		ChangeId: changeID,
	}), nil
}

// TestGitDeployKey connects to the repository's SSH host, inspects or verifies
// the host key, optionally pins an accepted host key, tests authentication,
// lists branches, and promotes a pending rotated key when use_pending succeeds.
func (h *Handler) TestGitDeployKey(ctx context.Context, req *connect.Request[dockwellv1.TestGitDeployKeyRequest]) (*connect.Response[dockwellv1.TestGitDeployKeyResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.oneByID(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetUsePending() && k.spec.GetPendingPrivateKey() == "" {
		return nil, authapi.Invalid(errors.New("there is no pending rotated key to test. Start rotating the deploy key first"))
	}

	accepting := m.GetAcceptHostKey()
	acceptFP := strings.TrimSpace(m.GetHostKeyFingerprint())
	acceptAlgo := strings.TrimSpace(m.GetHostKeyAlgorithm())
	if accepting {
		if !gitssh.ValidFingerprint(acceptFP) || !gitssh.ValidAlgorithm(acceptAlgo) {
			return nil, authapi.Invalid(errors.New("give the host key type and SHA256 fingerprint to accept"))
		}
	}

	// Accepting a CHANGED host key (where the key already pinned a different
	// fingerprint or algorithm) requires an admin/owner in the project and step-up.
	changingPin := accepting && k.spec.GetHostKeyFingerprint() != "" &&
		(k.spec.GetHostKeyFingerprint() != acceptFP || (k.spec.GetHostKeyAlgorithm() != "" && k.spec.GetHostKeyAlgorithm() != acceptAlgo))
	var giveBack func()
	if changingPin {
		isAdmin, err := h.isAdminIn(ctx, p, k.r.Scope)
		if err != nil {
			return nil, h.internal(ctx, "checking role for host key change", err)
		}
		if !isAdmin {
			return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("only an admin or the owner can accept a changed SSH host key. Ask an admin to verify the new fingerprint and accept it"), authapi.ReasonNeedsRole)
		}
		client, _ := authapi.ClientFrom(ctx)
		giveBack, err = h.cfg.Auth.HoldStepUp(ctx, client, p, m.GetProof(), auth.StepUpAcceptGitHostKey, k.r.ID)
		if err != nil {
			return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
		}
	}

	ep, err := gitssh.ParseSSH(k.spec.GetRepository())
	if err != nil {
		if giveBack != nil {
			giveBack()
		}
		return nil, h.internal(ctx, "parsing deploy key repository", err)
	}
	privKey := k.spec.GetPrivateKey()
	if m.GetUsePending() {
		privKey = k.spec.GetPendingPrivateKey()
	}
	pinFP, pinAlgo := k.spec.GetHostKeyFingerprint(), k.spec.GetHostKeyAlgorithm()
	if accepting {
		pinFP, pinAlgo = acceptFP, acceptAlgo
	}

	release, err := h.gate.acquireList(ctx, k.r.Scope, k.r.ID)
	if err != nil {
		if giveBack != nil {
			giveBack()
		}
		return nil, err
	}
	defer release()

	head, branches, hs, dialErr := gitssh.Branches(ctx, gitssh.Config{
		Endpoint:         ep,
		HostKeyAlgorithm: pinAlgo,
		HostKeyPin:       pinFP,
		PrivateKeyPEM:    privKey,
		Local:            h.cfg.Local,
		PackTempDir:      h.cfg.PackTempDir,
	}, h.cfg.Dial)

	var changeID string
	var verifiedSaved bool
	// If the caller accepted a host key and the server verified against that
	// exact fingerprint, persist the pin immediately (even if SSH auth later
	// fails because the public key hasn't been added yet).
	if accepting && hs.Verified {
		h.changing.Lock()
		fresh, ferr := h.oneByID(ctx, k.r.ID)
		if ferr == nil {
			freshChangingPin := fresh.spec.GetHostKeyFingerprint() != "" &&
				(fresh.spec.GetHostKeyFingerprint() != hs.Fingerprint || (fresh.spec.GetHostKeyAlgorithm() != "" && fresh.spec.GetHostKeyAlgorithm() != hs.HostKeyAlgorithm))
			if freshChangingPin && !changingPin {
				if giveBack != nil {
					giveBack()
				}
				h.changing.Unlock()
				return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("someone else already pinned a different SSH host key for this deploy key. Ask an admin to review and accept the new host key"), authapi.ReasonNeedsRole)
			}
			next := cloneMeta(fresh.spec)
			next.HostKeyFingerprint = hs.Fingerprint
			next.HostKeyAlgorithm = hs.HostKeyAlgorithm
			next.ObservedHostKeyFingerprint = ""
			next.ObservedHostKeyAlgorithm = ""
			if dialErr == nil && !m.GetUsePending() {
				next.VerifiedAt = h.now().Unix()
			}
			summary := "Pin SSH host key for deploy key " + fresh.r.Name
			if freshChangingPin {
				summary = "Accept changed SSH host key for deploy key " + fresh.r.Name
			}
			cid, cerr := h.applyEdit(ctx, summary, fresh.r.Scope, fresh.r.Name, fresh.r.Revision, next)
			if cerr == nil {
				changeID = cid
				if dialErr == nil && !m.GetUsePending() {
					verifiedSaved = true
				}
				if reloaded, rerr := h.oneByID(ctx, fresh.r.ID); rerr == nil {
					k = reloaded
				}
				action := "git_deploy_key.host_key_pinned"
				if freshChangingPin {
					action = "git_deploy_key.host_key_changed_accepted"
				}
				_ = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
					return h.audit(ctx, tx, p.Actor(), action, k.r.ID, fmt.Sprintf("%s %s %s", k.r.Name, hs.HostKeyAlgorithm, hs.Fingerprint))
				})
			} else if giveBack != nil {
				giveBack()
				h.changing.Unlock()
				return nil, cerr
			}
		}
		h.changing.Unlock()
	} else if giveBack != nil {
		giveBack()
	}

	usedBy, _ := h.usedByForKey(ctx, k.r.Scope, k.r.Name)
	resp := &dockwellv1.TestGitDeployKeyResponse{
		ObservedHostKeyAlgorithm:   hs.HostKeyAlgorithm,
		ObservedHostKeyFingerprint: hs.Fingerprint,
		Key:                        toInfo(k, usedBy),
		ChangeId:                   changeID,
	}

	if dialErr != nil {
		var hkChanged *gitssh.HostKeyChange
		var refused *netguard.Refused
		switch {
		case errors.Is(dialErr, gitssh.ErrHostKeyUnverified):
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_HOST_KEY_UNVERIFIED
			resp.Message = fmt.Sprintf("%s presented host key %s %s, which isn't pinned yet. Compare it with the host's key, then accept it and test again.", ep.Host, hs.HostKeyAlgorithm, hs.Fingerprint)
		case errors.As(dialErr, &hkChanged):
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_HOST_KEY_CHANGED
			resp.ObservedHostKeyAlgorithm = hkChanged.Algorithm
			resp.ObservedHostKeyFingerprint = hkChanged.Fingerprint
			resp.Message = fmt.Sprintf("%s presented a different host key (%s %s) than the one pinned (%s %s), so Dockwell stopped before signing in. Ask an admin to verify the new fingerprint and accept it.", ep.Host, hkChanged.Algorithm, hkChanged.Fingerprint, k.spec.GetHostKeyAlgorithm(), k.spec.GetHostKeyFingerprint())
			h.recordObservedHostKey(ctx, k.r.ID, hkChanged.Algorithm, hkChanged.Fingerprint)
			if reloaded, rerr := h.oneByID(ctx, k.r.ID); rerr == nil {
				resp.Key = toInfo(reloaded, usedBy)
			}
		case errors.Is(dialErr, gitssh.ErrAuth):
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_AUTH_FAILED
			if m.GetUsePending() {
				resp.Message = ep.Host + " refused the new deploy key, so the current key is still active. Add the new public key to the repository's deploy keys, then test again."
			} else {
				resp.Message = fmt.Sprintf("%s refused this deploy key, so Dockwell can't read %s. Add the public key to the repository's deploy keys, then test again.", ep.Host, ep.Shown())
			}
		case errors.Is(dialErr, gitssh.ErrRepositoryUnavailable):
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_REPOSITORY_UNAVAILABLE
			resp.Message = fmt.Sprintf("%s didn't let this deploy key read %s: the repository isn't there or this key has no access to it. Check the address and that the key is added to this repository, then test again.", ep.Host, ep.Shown())
		case errors.As(dialErr, &refused), errors.Is(dialErr, netguard.ErrForbidden):
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_UNREACHABLE
			resp.Message = fmt.Sprintf("Dockwell refused to connect to %s: %s.", ep.Host, dialErr.Error())
		case errors.Is(dialErr, gitssh.ErrTimeout):
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_UNREACHABLE
			resp.Message = fmt.Sprintf("%s didn't answer in time over SSH, so Dockwell couldn't read %s. Check the host and network, then test again.", ep.Host, ep.Shown())
		default:
			resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_UNREACHABLE
			resp.Message = fmt.Sprintf("Dockwell couldn't reach %s over SSH, so it couldn't read %s. Check the repository address and network, then test again.", ep.Host, ep.Shown())
		}
		return connect.NewResponse(resp), nil
	}

	// Test succeeded! If testing a pending rotated key, promote it atomically.
	if m.GetUsePending() {
		h.changing.Lock()
		fresh, ferr := h.oneByID(ctx, k.r.ID)
		if ferr != nil {
			h.changing.Unlock()
			return nil, ferr
		}
		if fresh.spec.GetPendingPrivateKey() == "" {
			h.changing.Unlock()
			return nil, authapi.Invalid(errors.New("there is no pending rotated key left to promote"))
		}
		oldFP := fresh.spec.GetKeyFingerprint()
		nowUnix := h.now().Unix()
		next := cloneMeta(fresh.spec)
		next.PrivateKey = fresh.spec.GetPendingPrivateKey()
		next.PublicKey = fresh.spec.GetPendingPublicKey()
		next.KeyType = fresh.spec.GetPendingKeyType()
		next.KeyFingerprint = fresh.spec.GetPendingKeyFingerprint()
		next.RotatedAt = nowUnix
		next.VerifiedAt = nowUnix
		next.PendingPrivateKey = ""
		next.PendingPublicKey = ""
		next.PendingKeyType = ""
		next.PendingKeyFingerprint = ""
		next.PendingCreatedAt = 0
		cid, cerr := h.applyEdit(ctx, "Rotate SSH deploy key "+fresh.r.Name, fresh.r.Scope, fresh.r.Name, fresh.r.Revision, next)
		h.changing.Unlock()
		if cerr != nil {
			return nil, cerr
		}
		changeID = cid
		if reloaded, rerr := h.oneByID(ctx, fresh.r.ID); rerr == nil {
			k = reloaded
		}
		_ = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			return h.audit(ctx, tx, p.Actor(), "git_deploy_key.rotated", k.r.ID, fmt.Sprintf("%s switched from %s to %s", k.r.Name, oldFP, k.spec.GetKeyFingerprint()))
		})
		resp.Promoted = true
		resp.OldKeyFingerprint = oldFP
		resp.Key = toInfo(k, usedBy)
		resp.ChangeId = changeID
	} else if !verifiedSaved && k.spec.GetVerifiedAt() == 0 {
		h.changing.Lock()
		if fresh, ferr := h.oneByID(ctx, k.r.ID); ferr == nil && fresh.spec.GetVerifiedAt() == 0 {
			next := cloneMeta(fresh.spec)
			next.VerifiedAt = h.now().Unix()
			if cid, cerr := h.applyEdit(ctx, "Verify SSH deploy key "+fresh.r.Name, fresh.r.Scope, fresh.r.Name, fresh.r.Revision, next); cerr == nil {
				changeID = cid
				if reloaded, rerr := h.oneByID(ctx, fresh.r.ID); rerr == nil {
					k = reloaded
					resp.Key = toInfo(k, usedBy)
					resp.ChangeId = changeID
				}
			}
		}
		h.changing.Unlock()
	}

	resp.Status = dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_OK
	resp.DefaultBranch = fit(head, 255)
	if len(branches) > maxBranches {
		resp.Total = int32(len(branches)) //nolint:gosec // bounded by maxRefs
		branches = branches[:maxBranches]
	}
	for _, b := range branches {
		if v := fit(b, 255); v != "" {
			resp.Branches = append(resp.Branches, v)
		}
	}
	return connect.NewResponse(resp), nil
}

// BeginRotateGitDeployKey generates a pending replacement Ed25519 key while
// keeping the active key in use until TestGitDeployKey succeeds with it.
func (h *Handler) BeginRotateGitDeployKey(ctx context.Context, req *connect.Request[dockwellv1.BeginRotateGitDeployKeyRequest]) (*connect.Response[dockwellv1.BeginRotateGitDeployKeyResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.oneByID(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpRotateGitDeployKey, k.r.ID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	h.changing.Lock()
	defer h.changing.Unlock()
	k, err = h.oneByID(ctx, k.r.ID)
	if err != nil {
		giveBack()
		return nil, err
	}
	kp, err := gitssh.GenerateKey()
	if err != nil {
		giveBack()
		return nil, h.internal(ctx, "generating a replacement SSH deploy key", err)
	}
	next := cloneMeta(k.spec)
	next.PendingPrivateKey = kp.PrivateKeyPEM
	next.PendingPublicKey = kp.PublicKey
	next.PendingKeyType = kp.KeyType
	next.PendingKeyFingerprint = kp.Fingerprint
	next.PendingCreatedAt = h.now().Unix()
	changeID, err := h.applyEdit(ctx, "Prepare new SSH deploy key for "+k.r.Name, k.r.Scope, k.r.Name, k.r.Revision, next)
	if err != nil {
		giveBack()
		return nil, err
	}
	k, err = h.oneByID(ctx, k.r.ID)
	if err != nil {
		return nil, err
	}
	_ = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "git_deploy_key.rotate_started", k.r.ID, fmt.Sprintf("%s pending %s", k.r.Name, kp.Fingerprint))
	})
	usedBy, _ := h.usedByForKey(ctx, k.r.Scope, k.r.Name)
	return connect.NewResponse(&dockwellv1.BeginRotateGitDeployKeyResponse{
		Key:      toInfo(k, usedBy),
		ChangeId: changeID,
	}), nil
}

// CancelRotateGitDeployKey discards a pending replacement key.
func (h *Handler) CancelRotateGitDeployKey(ctx context.Context, req *connect.Request[dockwellv1.CancelRotateGitDeployKeyRequest]) (*connect.Response[dockwellv1.CancelRotateGitDeployKeyResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	h.changing.Lock()
	defer h.changing.Unlock()
	k, err := h.oneByID(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if k.spec.GetPendingPrivateKey() == "" && k.spec.GetPendingPublicKey() == "" {
		usedBy, _ := h.usedByForKey(ctx, k.r.Scope, k.r.Name)
		return connect.NewResponse(&dockwellv1.CancelRotateGitDeployKeyResponse{Key: toInfo(k, usedBy)}), nil
	}
	next := cloneMeta(k.spec)
	next.PendingPrivateKey = ""
	next.PendingPublicKey = ""
	next.PendingKeyType = ""
	next.PendingKeyFingerprint = ""
	next.PendingCreatedAt = 0
	changeID, err := h.applyEdit(ctx, "Cancel rotation of SSH deploy key "+k.r.Name, k.r.Scope, k.r.Name, k.r.Revision, next)
	if err != nil {
		return nil, err
	}
	k, err = h.oneByID(ctx, k.r.ID)
	if err != nil {
		return nil, err
	}
	_ = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "git_deploy_key.rotate_cancelled", k.r.ID, k.r.Name)
	})
	usedBy, _ := h.usedByForKey(ctx, k.r.Scope, k.r.Name)
	return connect.NewResponse(&dockwellv1.CancelRotateGitDeployKeyResponse{
		Key:      toInfo(k, usedBy),
		ChangeId: changeID,
	}), nil
}

// DeleteGitDeployKey deletes an unused deploy key from the project.
func (h *Handler) DeleteGitDeployKey(ctx context.Context, req *connect.Request[dockwellv1.DeleteGitDeployKeyRequest]) (*connect.Response[dockwellv1.DeleteGitDeployKeyResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.oneByID(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	usedBy, err := h.usedByForKey(ctx, k.r.Scope, k.r.Name)
	if err != nil {
		return nil, h.internal(ctx, "checking services using deploy key", err)
	}
	if len(usedBy) > 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("deploy key %s is still used by %s, so it can't be deleted yet. Switch or remove those apps first", k.r.Name, strings.Join(usedBy, ", ")),
			"in_use")
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpDeleteGitDeployKey, k.r.ID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	h.changing.Lock()
	defer h.changing.Unlock()
	k, err = h.oneByID(ctx, k.r.ID)
	if err != nil {
		giveBack()
		return nil, err
	}
	changeID, err := h.applyEdit(ctx, "Delete SSH deploy key "+k.r.Name, k.r.Scope, k.r.Name, k.r.Revision, nil)
	if err != nil {
		giveBack()
		return nil, err
	}
	_ = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "git_deploy_key.deleted", k.r.ID, fmt.Sprintf("%s (%s)", k.r.Name, k.spec.GetKeyFingerprint()))
	})
	pageURL, _ := gitssh.DeployKeysPage(k.spec.GetRepository())
	return connect.NewResponse(&dockwellv1.DeleteGitDeployKeyResponse{
		OldKeyFingerprint: k.spec.GetKeyFingerprint(),
		DeployKeysUrl:     pageURL,
		ChangeId:          changeID,
	}), nil
}

func (h *Handler) applyEdit(ctx context.Context, summary, projectID, name string, base int, spec *resourcev1.GitDeployKey) (string, error) {
	edit := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{
			Kind:  string(gitdeploykey.Kind),
			Scope: projectID,
			Name:  name,
		},
		BaseRevision: int64(base),
	}
	if spec == nil {
		edit.Delete = true
	} else {
		edit.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_GitDeployKey{GitDeployKey: spec}}
	}
	c, err := h.cfg.Changes.Propose(ctx, summary, []*dockwellv1.Edit{edit}, nil, changes.Also{})
	if err != nil {
		return "", err
	}
	if err := h.cfg.Changes.ApplyOwned(ctx, c.GetId()); err != nil {
		discardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = h.cfg.Changes.DiscardOwned(discardCtx, c.GetId())
		return "", err
	}
	return c.GetId(), nil
}

// recordObservedHostKey persists an observed host key change when a fetch or
// test fails due to host key mismatch so the UI can alert an admin. Background
// fetches without a signed-in caller record the change as Dockwell.
func (h *Handler) recordObservedHostKey(ctx context.Context, keyID, algo, fp string) {
	if algo == "" || fp == "" || !gitssh.ValidFingerprint(fp) || !gitssh.ValidAlgorithm(algo) {
		return
	}
	editCtx := ctx
	var cancel context.CancelFunc
	if _, ok := authapi.PrincipalFrom(ctx); !ok {
		editCtx, cancel = context.WithTimeout(changes.AsDockwell(context.WithoutCancel(ctx)), 5*time.Second)
		defer cancel()
	}
	h.changing.Lock()
	defer h.changing.Unlock()
	k, err := h.oneByID(editCtx, keyID)
	if err != nil {
		return
	}
	if k.spec.GetObservedHostKeyAlgorithm() == algo && k.spec.GetObservedHostKeyFingerprint() == fp {
		return
	}
	next := cloneMeta(k.spec)
	next.ObservedHostKeyAlgorithm = algo
	next.ObservedHostKeyFingerprint = fp
	_, _ = h.applyEdit(editCtx, "Record changed SSH host key observed for "+k.r.Name, k.r.Scope, k.r.Name, k.r.Revision, next)
}

func (h *Handler) resolveProject(ctx context.Context, nameOrID string) (string, error) {
	var found string
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		if nameOrID == "" {
			if len(tree.Projects) == 1 {
				found = tree.Projects[0].ID
			}
			return nil
		}
		for _, p := range tree.Projects {
			if p.ID == nameOrID || p.Name == nameOrID {
				found = p.ID
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", h.internal(ctx, "reading projects", err)
	}
	if found == "" {
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no project %s", nameOrID))
	}
	return found, nil
}

func (h *Handler) isAdminIn(ctx context.Context, p auth.Principal, projectID string) (bool, error) {
	var grants []store.Grant
	if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		grants, err = tx.Grants(ctx, p.User.ID)
		return err
	}); err != nil {
		return false, err
	}
	for _, g := range grants {
		if (g.ProjectID == store.Install || g.ProjectID == projectID) && store.RoleRank(g.Role) >= store.RoleRank(store.RoleAdmin) {
			return true, nil
		}
	}
	return false, nil
}

func (h *Handler) allInProject(ctx context.Context, tx *store.Tx, projectID string) ([]kept, error) {
	stored, err := tx.ListResources(ctx, projectID, string(gitdeploykey.Kind))
	if err != nil {
		return nil, err
	}
	var out []kept
	for i := range stored {
		r := stored[i]
		if r.Spec == nil {
			continue
		}
		spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read deploy key %s: %w", r.Name, err)
		}
		if dk, ok := spec.(*resourcev1.GitDeployKey); ok {
			out = append(out, kept{r: r, spec: dk})
		}
	}
	slices.SortFunc(out, func(a, b kept) int { return strings.Compare(a.r.Name, b.r.Name) })
	return out, nil
}

func (h *Handler) oneByID(ctx context.Context, keyID string) (kept, error) {
	var found kept
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, keyID)
		if err != nil {
			return err
		}
		if r.Kind != string(gitdeploykey.Kind) || r.Spec == nil {
			return store.ErrNotFound
		}
		spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		dk, ok := spec.(*resourcev1.GitDeployKey)
		if !ok {
			return store.ErrNotFound
		}
		found = kept{r: r, spec: dk}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return kept{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no deploy key %s", keyID))
	}
	if err != nil {
		return kept{}, h.internal(ctx, "reading deploy key", err)
	}
	return found, nil
}

func (h *Handler) oneByName(ctx context.Context, projectID, name string) (kept, error) {
	var found kept
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		found, err = h.oneByNameTx(ctx, tx, projectID, name)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return kept{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no deploy key %s in this project", name))
	}
	if err != nil {
		return kept{}, h.internal(ctx, "reading deploy key", err)
	}
	return found, nil
}

func (h *Handler) oneByNameTx(ctx context.Context, tx *store.Tx, projectID, name string) (kept, error) {
	r, err := tx.GetResource(ctx, string(gitdeploykey.Kind), projectID, name)
	if err != nil {
		return kept{}, err
	}
	if r.Spec == nil {
		return kept{}, store.ErrNotFound
	}
	spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return kept{}, err
	}
	dk, ok := spec.(*resourcev1.GitDeployKey)
	if !ok {
		return kept{}, store.ErrNotFound
	}
	return kept{r: r, spec: dk}, nil
}

func (h *Handler) usedByForKey(ctx context.Context, projectID, keyName string) ([]string, error) {
	var used []string
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		m, err := h.usedByMap(ctx, tx, projectID)
		if err != nil {
			return err
		}
		used = m[service.SSHLoginPrefix+keyName]
		return nil
	})
	return used, err
}

func (h *Handler) usedByMap(ctx context.Context, tx *store.Tx, projectID string) (map[string][]string, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, p := range tree.Projects {
		if p.ID != projectID {
			continue
		}
		for _, env := range p.Environments {
			svcs, err := tx.ListResources(ctx, env.ID, string(service.Kind))
			if err != nil {
				return nil, err
			}
			for i := range svcs {
				r := svcs[i]
				if r.Spec == nil {
					continue
				}
				spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					continue
				}
				if svc, ok := spec.(*resourcev1.Service); ok {
					if login := svc.GetBuild().GetGitLogin(); strings.HasPrefix(login, service.SSHLoginPrefix) {
						out[login] = append(out[login], r.Name)
					}
				}
			}
		}
	}
	for k := range out {
		slices.Sort(out[k])
		out[k] = slices.Compact(out[k])
	}
	return out, nil
}

var nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)

func (h *Handler) chooseName(ctx context.Context, projectID, requested string, ep gitssh.Endpoint) (string, error) {
	if requested != "" {
		if err := resource.CheckLabel("deploy key", requested, "acme-web"); err != nil {
			return "", authapi.Invalid(err)
		}
		var exists bool
		if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
			r, err := tx.GetResource(ctx, string(gitdeploykey.Kind), projectID, requested)
			if err == nil && r.Spec != nil {
				exists = true
			}
			return nil
		}); err != nil {
			return "", h.internal(ctx, "checking deploy key name", err)
		}
		if exists {
			return "", connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("this project already has a deploy key named %s. Pick another name", requested))
		}
		return requested, nil
	}
	base := strings.ToLower(strings.TrimSuffix(ep.RepoPath, ".git"))
	base = nonLabel.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" || (base[0] < 'a' || base[0] > 'z') {
		base = "key-" + base
	}
	if len(base) > 50 {
		base = strings.TrimRight(base[:50], "-")
	}
	if base == "" {
		base = "deploy-key"
	}
	existing := map[string]bool{}
	if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, projectID, string(gitdeploykey.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			if list[i].Spec != nil {
				existing[list[i].Name] = true
			}
		}
		return nil
	}); err != nil {
		return "", h.internal(ctx, "listing project deploy keys", err)
	}
	if !existing[base] {
		return base, nil
	}
	for n := 2; n < 1000; n++ {
		cand := base + "-" + strconv.Itoa(n)
		if !existing[cand] {
			return cand, nil
		}
	}
	return "", connect.NewError(connect.CodeResourceExhausted, errors.New("this project has too many deploy keys with similar names. Pass a name explicitly"))
}

func (h *Handler) audit(ctx context.Context, tx *store.Tx, actor, action, keyID, detail string) error {
	return tx.RecordEvent(ctx, store.Event{
		ID:      id.New(id.Event),
		At:      h.now(),
		Actor:   actor,
		Surface: store.SurfaceOf(actor),
		Action:  action,
		Subject: keyID,
		Target:  keyID,
		Detail:  detail,
	})
}

func (h *Handler) internal(ctx context.Context, doing string, err error) error {
	h.cfg.Logger.ErrorContext(ctx, "git deploy key error", slog.String("doing", doing), slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, fmt.Errorf("couldn't finish %s. Try again", doing))
}

// cloneMeta copies non-secret deploy key metadata and leaves PrivateKey and
// PendingPrivateKey empty so gitdeploykey.Type.Keep preserves them without
// marking private_key modified in the revision plan.
func cloneMeta(s *resourcev1.GitDeployKey) *resourcev1.GitDeployKey {
	return &resourcev1.GitDeployKey{
		Repository:                 s.GetRepository(),
		Host:                       s.GetHost(),
		Port:                       s.GetPort(),
		User:                       s.GetUser(),
		KeyType:                    s.GetKeyType(),
		PublicKey:                  s.GetPublicKey(),
		KeyFingerprint:             s.GetKeyFingerprint(),
		HostKeyAlgorithm:           s.GetHostKeyAlgorithm(),
		HostKeyFingerprint:         s.GetHostKeyFingerprint(),
		CreatedAt:                  s.GetCreatedAt(),
		RotatedAt:                  s.GetRotatedAt(),
		PendingPublicKey:           s.GetPendingPublicKey(),
		PendingKeyType:             s.GetPendingKeyType(),
		PendingKeyFingerprint:      s.GetPendingKeyFingerprint(),
		PendingCreatedAt:           s.GetPendingCreatedAt(),
		ObservedHostKeyAlgorithm:   s.GetObservedHostKeyAlgorithm(),
		ObservedHostKeyFingerprint: s.GetObservedHostKeyFingerprint(),
		VerifiedAt:                 s.GetVerifiedAt(),
	}
}

func toInfo(k kept, usedBy []string) *dockwellv1.GitDeployKeyInfo {
	s := k.spec
	pageURL, hostName := gitssh.DeployKeysPage(s.GetRepository())
	port := s.GetPort()
	if port == 0 {
		port = gitssh.DefaultPort
	}
	user := s.GetUser()
	if user == "" {
		user = gitssh.DefaultUser
	}
	info := &dockwellv1.GitDeployKeyInfo{
		Id:                         k.r.ID,
		Name:                       k.r.Name,
		ProjectId:                  k.r.Scope,
		Repository:                 s.GetRepository(),
		Host:                       s.GetHost(),
		Port:                       port,
		User:                       user,
		KeyType:                    s.GetKeyType(),
		PublicKey:                  s.GetPublicKey(),
		KeyFingerprint:             s.GetKeyFingerprint(),
		HostKeyAlgorithm:           s.GetHostKeyAlgorithm(),
		HostKeyFingerprint:         s.GetHostKeyFingerprint(),
		HostKeyPinned:              s.GetHostKeyFingerprint() != "",
		HostKeyChanged:             s.GetObservedHostKeyFingerprint() != "",
		ObservedHostKeyAlgorithm:   s.GetObservedHostKeyAlgorithm(),
		ObservedHostKeyFingerprint: s.GetObservedHostKeyFingerprint(),
		PendingPublicKey:           s.GetPendingPublicKey(),
		PendingKeyType:             s.GetPendingKeyType(),
		PendingKeyFingerprint:      s.GetPendingKeyFingerprint(),
		DeployKeysUrl:              pageURL,
		HostName:                   hostName,
		GitLogin:                   service.SSHLoginPrefix + k.r.Name,
		UsedBy:                     usedBy,
		Revision:                   int64(k.r.Revision),
		Verified:                   s.GetVerifiedAt() > 0 && s.GetHostKeyFingerprint() != "" && s.GetObservedHostKeyFingerprint() == "",
	}
	if ts := s.GetCreatedAt(); ts > 0 {
		info.CreateTime = timestamppb.New(time.Unix(ts, 0).UTC())
	}
	if ts := s.GetRotatedAt(); ts > 0 {
		info.RotateTime = timestamppb.New(time.Unix(ts, 0).UTC())
	}
	if ts := s.GetPendingCreatedAt(); ts > 0 {
		info.PendingCreateTime = timestamppb.New(time.Unix(ts, 0).UTC())
	}
	if ts := s.GetVerifiedAt(); ts > 0 {
		info.VerifyTime = timestamppb.New(time.Unix(ts, 0).UTC())
	}
	return info
}

func fit(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
