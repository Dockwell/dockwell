// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func selfSignedPair(t *testing.T, domain string, notBefore, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

func setupDomainTest(t *testing.T, domain string, rep reports) *Handler {
	t.Helper()
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_on", Name: "srv_on", CreatedAt: now, CertSerial: "s1", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{})
	keeper := secretstest.Keeper(db, kinds)
	eng := revision.New(db, kinds, agents{}, keeper, logger)
	var env string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		env = tr.Projects[0].Environments[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "add route",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{
			{
				Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: "web"},
				Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
				Servers: []string{"srv_on"},
			},
			{
				Ref:     resource.Ref{Kind: route.Kind, Scope: env, Name: "main"},
				Spec:    &resourcev1.Route{Domain: domain, Service: "web", Port: 8080},
				Servers: []string{"srv_on"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), plan.Change); err != nil {
		t.Fatal(err)
	}
	ch := changes.New(eng, nil, db, logger)
	h := New(kinds, db, rep, nil, logger)
	h.Changes = ch
	return h
}

func TestRenewCertificateRefusesWhenNotDueAndRespectsRateLimit(t *testing.T) {
	t.Parallel()
	now := time.Now()
	rep := reports{
		"srv_on/blog.example.com": {
			Domain: "blog.example.com",
			Certificate: &dockwellv1.Certificate{
				State:      dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED,
				NotBefore:  timestamppb.New(now.Add(-2 * 24 * time.Hour)),
				ExpireTime: timestamppb.New(now.Add(85 * 24 * time.Hour)),
			},
		},
	}
	h := setupDomainTest(t, "blog.example.com", rep)

	// Refuses early renewal without force.
	_, err := h.RenewCertificate(t.Context(), connect.NewRequest(&dockwellv1.RenewCertificateRequest{
		Domain: "blog.example.com",
	}))
	if err == nil {
		t.Fatal("expected early renewal to be refused without force")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "isn't due for renewal yet") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Refuses when CA rate limit is active even with force.
	rep["srv_on/blog.example.com"].Certificate.Problem = dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_RATE_LIMITED
	_, err = h.RenewCertificate(t.Context(), connect.NewRequest(&dockwellv1.RenewCertificateRequest{
		Domain: "blog.example.com",
		Force:  true,
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("expected CA rate limit refusal, got %v", err)
	}
}

func TestSetCustomCertificateValidatesAndPlans(t *testing.T) {
	t.Parallel()
	h := setupDomainTest(t, "shop.example.com", reports{})
	ctx := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_1", Email: "a@example.com"}})
	now := time.Now()

	// Wrong domain fails validation.
	wrongCert, wrongKey := selfSignedPair(t, "other.example.com", now.Add(-time.Hour), now.Add(24*time.Hour))
	if _, err := h.SetCustomCertificate(ctx, connect.NewRequest(&dockwellv1.SetCustomCertificateRequest{
		Domain:  "shop.example.com",
		CertPem: wrongCert,
		KeyPem:  wrongKey,
	})); err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected wrong domain error, got %v", err)
	}

	// Expired cert fails validation.
	expCert, expKey := selfSignedPair(t, "shop.example.com", now.Add(-48*time.Hour), now.Add(-time.Hour))
	if _, err := h.SetCustomCertificate(ctx, connect.NewRequest(&dockwellv1.SetCustomCertificateRequest{
		Domain:  "shop.example.com",
		CertPem: expCert,
		KeyPem:  expKey,
	})); err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected expired cert error, got %v", err)
	}

	// Valid cert plans a change and attaches an untrusted_custom_cert note for a self-signed cert (L3).
	okCert, okKey := selfSignedPair(t, "shop.example.com", now.Add(-time.Hour), now.Add(30*24*time.Hour))
	resp, err := h.SetCustomCertificate(ctx, connect.NewRequest(&dockwellv1.SetCustomCertificateRequest{
		Domain:  "shop.example.com",
		CertPem: okCert,
		KeyPem:  okKey,
	}))
	if err != nil {
		t.Fatalf("valid custom cert failed to plan: %v", err)
	}
	if resp.Msg.GetChange().GetId() == "" {
		t.Fatal("expected planned change ID")
	}
	sawUntrustedNote := false
	for _, n := range resp.Msg.GetChange().GetPlan().GetNotes() {
		if n.GetReason() == "untrusted_custom_cert" {
			sawUntrustedNote = true
		}
	}
	if !sawUntrustedNote {
		t.Fatalf("expected untrusted_custom_cert plan note for self-signed cert, got notes=%v", resp.Msg.GetChange().GetPlan().GetNotes())
	}
}

func TestDownloadCertificateKeyRequiresStepUp(t *testing.T) {
	t.Parallel()
	h := setupDomainTest(t, "docs.example.com", reports{})
	h.Auth = &auth.Service{}
	ctx := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_1", Email: "a@example.com"}})

	_, err := h.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{
		Domain:     "docs.example.com",
		IncludeKey: true,
	}))
	if err == nil {
		t.Fatal("expected step-up requirement for downloading private key")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected permission denied, got %v", err)
	}
	var cerr *connect.Error
	if !strings.Contains(err.Error(), auth.ErrNeedsStepUp.Error()) {
		t.Fatalf("expected step-up error, got %v (%v)", err, cerr)
	}
}

// TestDownloadCertificateKeyScopeH1 verifies H1:
//   - a project admin cannot download a shared/multi-name DNS-01 certificate key that covers another project,
//   - an install admin passes the scope check on a shared certificate,
//   - even an install admin is refused when the certificate covers the panel's own domain.
func TestDownloadCertificateKeyScopeH1(t *testing.T) {
	t.Parallel()
	now := time.Now()
	rep := reports{
		"srv_on/a.example.com": {
			Domain: "a.example.com",
			Certificate: &dockwellv1.Certificate{
				State:      dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED,
				NotBefore:  timestamppb.New(now.Add(-time.Hour)),
				ExpireTime: timestamppb.New(now.Add(60 * 24 * time.Hour)),
				ByDns:      "cloudflare",
				Sans:       []string{"*.example.com", "a.example.com", "b.example.com"},
			},
		},
	}
	h := setupDomainTest(t, "a.example.com", rep)
	h.Auth = &auth.Service{}

	var projectAID string
	if err := h.db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		projectAID = tr.Projects[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	baseCtx := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_1", Email: "a@example.com"}})
	projAdminCtx := place.WithCaller(baseCtx, place.Caller{Grants: []store.Grant{{ProjectID: projectAID, Role: store.RoleAdmin}}})
	installAdminCtx := place.WithCaller(baseCtx, place.Caller{Grants: []store.Grant{{Role: store.RoleAdmin}}})
	ownerCtx := place.WithCaller(baseCtx, place.Caller{Grants: []store.Grant{{Role: store.RoleOwner}}})

	// 1. Project A admin (not install admin) MUST be refused for shared/wildcard cert.
	_, err := h.DownloadCertificate(projAdminCtx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{
		Domain:     "a.example.com",
		IncludeKey: true,
	}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "install admin") {
		t.Fatalf("project admin on shared cert = %v, want PermissionDenied requiring install admin", err)
	}

	// 2. Install admin and owner pass scope check on shared cert (reach passkey step-up).
	for name, ctx := range map[string]context.Context{"install admin": installAdminCtx, "owner": ownerCtx} {
		_, err = h.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{
			Domain:     "a.example.com",
			IncludeKey: true,
		}))
		if err == nil || !strings.Contains(err.Error(), auth.ErrNeedsStepUp.Error()) {
			t.Fatalf("%s on shared cert = %v, want step-up prompt", name, err)
		}
	}

	// 3. When the wildcard covers the panel domain, EVEN an install admin is refused.
	h.PanelDomains = func() []string { return []string{"panel.example.com"} }
	_, err = h.DownloadCertificate(installAdminCtx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{
		Domain:     "a.example.com",
		IncludeKey: true,
	}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "panel domain") {
		t.Fatalf("install admin on cert covering panel domain = %v, want panel domain refusal", err)
	}
}

// TestRenewLimitPersistenceAndRoleM3H2L4 verifies H2 (no nil-map panic on checkRenewLimit),
// M3 (force requires admin, not developer), and L4 (weekly cap persists across restart).
func TestRenewLimitPersistenceAndRoleM3H2L4(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	rep := reports{
		"srv_on/cap.example.com": {
			Domain: "cap.example.com",
			Certificate: &dockwellv1.Certificate{
				State:      dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED,
				NotBefore:  timestamppb.New(now.Add(-2 * 24 * time.Hour)),
				ExpireTime: timestamppb.New(now.Add(85 * 24 * time.Hour)),
			},
		},
	}
	h := setupDomainTest(t, "cap.example.com", rep)
	clock := now
	h.Now = func() time.Time { return clock }

	var projID string
	if err := h.db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		projID = tr.Projects[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// M3: developer cannot force renewal; admin can.
	devCtx := place.WithCaller(t.Context(), place.Caller{Grants: []store.Grant{{ProjectID: projID, Role: store.RoleDeveloper}}})
	if _, err := h.RenewCertificate(devCtx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{
		Domain: "cap.example.com",
		Force:  true,
	})); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer forced renewal = %v, want PermissionDenied", err)
	}

	// H2: even on a zero-initialized renewLog, checkRenewLimit must never panic, and 4 renewals
	// over 4 days succeed while the 5th within 7 days is refused.
	h.renewLog = nil
	h.renewLoaded = false
	for i := range manualRenewWeeklyCap {
		if err := h.checkRenewLimit(t.Context(), "cap.example.com", true); err != nil {
			t.Fatalf("renewal %d unexpectedly refused: %v", i+1, err)
		}
		h.recordRenewAttempt(t.Context(), "cap.example.com")
		clock = clock.Add(2 * time.Hour)
	}
	if err := h.checkRenewLimit(t.Context(), "cap.example.com", true); err == nil || connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("5th renewal in a week = %v, want ResourceExhausted", err)
	}

	// L4: a fresh Handler created on the same DB after a control-plane restart STILL enforces the cap.
	restarted := New(h.kinds, h.db, rep, nil, h.logger)
	restarted.Now = func() time.Time { return clock }
	if err := restarted.checkRenewLimit(t.Context(), "cap.example.com", true); err == nil || connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("restarted handler 5th renewal = %v, want ResourceExhausted", err)
	}
}
