// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	// renewDueWindow is Let's Encrypt's recommended renewal window before expiry.
	renewDueWindow = 30 * 24 * time.Hour
	// manualRenewCooldown prevents repeated forced renewals within an hour.
	manualRenewCooldown = time.Hour
	// manualRenewWeeklyCap stays safely under Let's Encrypt's 5 duplicate certs / week limit.
	manualRenewWeeklyCap = 4
	manualRenewWeek      = 7 * 24 * time.Hour

	// EventCertificateKeyDownloaded is the audit event recorded when an admin downloads a domain private key.
	EventCertificateKeyDownloaded = "certificate.key_downloaded"
)

type domainBinding struct {
	projectID   string
	envID       string
	protected   bool
	routes      []boundRoute
	serverIDs   []string
	primarySpec *resourcev1.Route
}

type boundRoute struct {
	res    store.Resource
	spec   *resourcev1.Route
	placed []string
}

func (h *Handler) findDomain(ctx context.Context, domain string) (*domainBinding, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := route.CheckDomain(domain); err != nil {
		return nil, authapi.Invalid(err)
	}
	var out *domainBinding
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		envProject := map[string]struct {
			projectID string
			protected bool
		}{}
		for _, p := range tree.Projects {
			for _, e := range p.Environments {
				envProject[e.ID] = struct {
					projectID string
					protected bool
				}{projectID: p.ID, protected: e.Protected}
			}
		}
		stored, err := tx.ListKind(ctx, string(route.Kind))
		if err != nil {
			return err
		}
		for i := range stored {
			r := stored[i]
			plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			spec, err := h.kinds.Decode(route.Kind, plain)
			if err != nil {
				return err
			}
			rt := spec.(*resourcev1.Route)
			if rt.GetDomain() != domain {
				continue
			}
			placed, err := tx.Placement(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			if out == nil {
				ep := envProject[r.Scope]
				out = &domainBinding{
					projectID: ep.projectID,
					envID:     r.Scope,
					protected: ep.protected,
				}
			}
			out.routes = append(out.routes, boundRoute{res: r, spec: rt, placed: placed})
			if out.primarySpec == nil || rt.GetPath() == "" {
				out.primarySpec = rt
			}
			for _, sid := range placed {
				if !slices.Contains(out.serverIDs, sid) {
					out.serverIDs = append(out.serverIDs, sid)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("couldn't read routes for this domain"))
	}
	if out == nil {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no route for %s", domain), authapi.ReasonNotFound)
	}
	return out, nil
}

func checkDomainRole(ctx context.Context, b *domainBinding, minRole string) error {
	c, ok := place.CallerFrom(ctx)
	if !ok {
		return nil
	}
	held := place.Holds(c.Grants, place.Place{Project: b.projectID})
	if held == "" {
		return authapi.ReasonError(connect.CodeNotFound, errors.New("there's no route for this domain in a project you hold"), authapi.ReasonNotFound)
	}
	if store.RoleRank(held) < store.RoleRank(minRole) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("this needs %s access on this project", minRole))
	}
	return nil
}

// RenewCertificate requests an immediate certificate renewal for a domain, respecting
// CA rate limits and refusing early renewal unless force is set.
func (h *Handler) RenewCertificate(ctx context.Context, req *connect.Request[dockwellv1.RenewCertificateRequest]) (*connect.Response[dockwellv1.RenewCertificateResponse], error) {
	domain := strings.ToLower(strings.TrimSpace(req.Msg.GetDomain()))
	b, err := h.findDomain(ctx, domain)
	if err != nil {
		return nil, err
	}
	minRole := store.RoleDeveloper
	if b.protected {
		minRole = store.RoleAdmin
	}
	if err := checkDomainRole(ctx, b, minRole); err != nil {
		return nil, err
	}
	if b.primarySpec.GetTls() == ingress.TLSOff {
		return nil, authapi.Invalid(fmt.Errorf("%s has HTTPS off, so it has no certificate to renew", domain))
	}
	if b.primarySpec.GetCustomCert() != nil {
		return nil, authapi.Invalid(fmt.Errorf("%s uses an uploaded custom certificate, which can't be renewed automatically. Upload a new certificate or switch back to automatic HTTPS", domain))
	}
	if len(b.serverIDs) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't placed on a server yet", domain))
	}
	serverID := b.serverIDs[0]
	status, _ := h.reports.Domain(serverID, domain)
	cert := status.GetCertificate()
	if cert != nil {
		if cert.GetProblem() == dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_RATE_LIMITED {
			return nil, authapi.ReasonError(connect.CodeResourceExhausted,
				fmt.Errorf("the certificate authority's rate limit is active for %s. Wait for it to lift before requesting another renewal", domain),
				"rate_limited")
		}
		if cert.GetState() == dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED && cert.GetExpireTime() != nil && !req.Msg.GetForce() {
			now := time.Now()
			notAfter := cert.GetExpireTime().AsTime()
			remaining := notAfter.Sub(now)
			lifetime := 90 * 24 * time.Hour
			if cert.GetNotBefore() != nil {
				if d := notAfter.Sub(cert.GetNotBefore().AsTime()); d > 0 {
					lifetime = d
				}
			}
			if remaining > renewDueWindow && remaining > lifetime/3 {
				days := int(math.Floor(remaining.Hours() / 24))
				return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
					fmt.Errorf("the certificate for %s isn't due for renewal yet: it stays valid for %d more days (expires %s). Renewing early counts against certificate authority rate limits; pass force to renew anyway",
						domain, days, notAfter.UTC().Format("2 January 2006")),
					"renewal_not_due")
			}
		}
	}

	// Enforce per-domain manual renewal rate limits to protect CA quotas.
	if err := h.checkRenewLimit(domain, req.Msg.GetForce()); err != nil {
		return nil, err
	}

	// DNS-01 certificates renew through the control plane's certificate manager.
	if cert.GetByDns() != "" && h.Certificates != nil {
		handled, err := h.Certificates.RenewDomainNow(ctx, domain)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("couldn't start DNS-01 certificate renewal for %s: %w", domain, err))
		}
		if handled {
			h.recordRenewAttempt(domain)
			return connect.NewResponse(&dockwellv1.RenewCertificateResponse{
				Message:            fmt.Sprintf("Queued immediate DNS-01 certificate renewal for %s.", domain),
				PreviousExpireTime: cert.GetExpireTime(),
			}), nil
		}
	}

	if h.Agents == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no agents are available to renew certificates"))
	}
	session, ok := h.Agents.Session(serverID)
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("the server running %s isn't connected right now", domain))
	}
	res, err := session.RenewCertificate(ctx, domain, req.Msg.GetForce())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("couldn't renew certificate for %s: %w", domain, err))
	}
	h.recordRenewAttempt(domain)
	return connect.NewResponse(&dockwellv1.RenewCertificateResponse{
		Message:            cmp.Or(res.GetMessage(), fmt.Sprintf("Started certificate renewal for %s.", domain)),
		PreviousExpireTime: cert.GetExpireTime(),
	}), nil
}

func (h *Handler) checkRenewLimit(domain string, force bool) error {
	h.renewMu.Lock()
	defer h.renewMu.Unlock()
	now := time.Now()
	recent := slices.DeleteFunc(h.renewLog[domain], func(t time.Time) bool { return now.Sub(t) >= manualRenewWeek })
	h.renewLog[domain] = recent
	if len(recent) >= manualRenewWeeklyCap {
		return authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("%s already had %d manual renewals in the last 7 days, which is the cap Dockwell sets to avoid Let's Encrypt duplicate-certificate rate limits", domain, len(recent)),
			"rate_limited")
	}
	if force && len(recent) > 0 && now.Sub(recent[len(recent)-1]) < manualRenewCooldown {
		waitMin := int(math.Ceil((manualRenewCooldown - now.Sub(recent[len(recent)-1])).Minutes()))
		return authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("a forced renewal for %s ran recently. Wait %d minutes before forcing another renewal", domain, max(1, waitMin)),
			"rate_limited")
	}
	return nil
}

func (h *Handler) recordRenewAttempt(domain string) {
	h.renewMu.Lock()
	defer h.renewMu.Unlock()
	if h.renewLog == nil {
		h.renewLog = map[string][]time.Time{}
	}
	h.renewLog[domain] = append(h.renewLog[domain], time.Now())
}

// SetCustomCertificate plans uploading a custom PEM certificate and private key for a domain,
// or clearing a custom certificate to return to automatic HTTPS.
func (h *Handler) SetCustomCertificate(ctx context.Context, req *connect.Request[dockwellv1.SetCustomCertificateRequest]) (*connect.Response[dockwellv1.SetCustomCertificateResponse], error) {
	if h.Changes == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("planning changes isn't configured on this handler"))
	}
	m := req.Msg
	domain := strings.ToLower(strings.TrimSpace(m.GetDomain()))
	b, err := h.findDomain(ctx, domain)
	if err != nil {
		return nil, err
	}
	if err := checkDomainRole(ctx, b, store.RoleAdmin); err != nil {
		return nil, err
	}
	if b.primarySpec.GetTls() == ingress.TLSOff {
		return nil, authapi.Invalid(fmt.Errorf("%s has HTTPS off. Turn HTTPS on before setting a custom certificate", domain))
	}

	var custom *resourcev1.RouteCustomCert
	summary := strings.TrimSpace(m.GetSummary())
	if m.GetClear() {
		if b.primarySpec.GetCustomCert() == nil {
			return nil, authapi.Invalid(fmt.Errorf("%s already uses an automatic certificate", domain))
		}
		if summary == "" {
			summary = "Use automatic certificate for " + domain
		}
	} else {
		certPEM := strings.TrimSpace(m.GetCertPem())
		keyPEM := strings.TrimSpace(m.GetKeyPem())
		if _, err := route.CheckCustomCert(domain, certPEM, keyPEM, time.Now()); err != nil {
			return nil, authapi.Invalid(err)
		}
		custom = &resourcev1.RouteCustomCert{CertPem: certPEM, KeyPem: keyPEM}
		if summary == "" {
			summary = "Upload custom certificate for " + domain
		}
	}

	var edits []*dockwellv1.Edit
	for i := range b.routes {
		br := &b.routes[i]
		next := proto.CloneOf(br.spec)
		next.CustomCert = proto.CloneOf(custom)
		if custom != nil {
			next.Proof = ""
		}
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: br.res.Scope, Name: br.res.Name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: next}},
			Servers:  br.placed,
		})
	}
	c, err := h.Changes.Propose(ctx, summary, edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCustomCertificateResponse{Change: c}), nil
}

// DownloadCertificate returns the active PEM certificate chain for a domain, and optionally
// its private key when include_key is true (signed-in admin + passkey step-up required).
func (h *Handler) DownloadCertificate(ctx context.Context, req *connect.Request[dockwellv1.DownloadCertificateRequest]) (*connect.Response[dockwellv1.DownloadCertificateResponse], error) {
	m := req.Msg
	domain := strings.ToLower(strings.TrimSpace(m.GetDomain()))
	b, err := h.findDomain(ctx, domain)
	if err != nil {
		return nil, err
	}
	if err := checkDomainRole(ctx, b, store.RoleViewer); err != nil {
		return nil, err
	}
	if m.GetIncludeKey() {
		if err := checkDomainRole(ctx, b, store.RoleAdmin); err != nil {
			return nil, err
		}
		p, ok := authapi.PrincipalFrom(ctx)
		if !ok || p.Token != nil || h.Auth == nil {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("sign in as an admin to download a certificate's private key"))
		}
		client, _ := authapi.ClientFrom(ctx)
		stepUpOK := false
		if m.GetProof() != "" {
			if _, err := h.Auth.HoldStepUp(ctx, client, p, m.GetProof(), auth.StepUpDownloadCertificateKey, domain); err == nil {
				stepUpOK = true
			}
		}
		if !stepUpOK {
			err := authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsStepUp, authapi.ReasonNeedsStepUp)
			if detail, e := connect.NewErrorDetail(&dockwellv1.StepUpNeeded{
				Action: dockwellv1.StepUpAction_STEP_UP_ACTION_DOWNLOAD_CERTIFICATE_KEY,
				Target: domain,
			}); e == nil {
				err.AddDetail(detail)
			}
			return nil, err
		}
		if err := h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: time.Now(), Actor: p.Actor(), Target: b.projectID,
				Action: EventCertificateKeyDownloaded, Subject: domain, Detail: "downloaded TLS private key", Surface: client.Surface(p),
			})
		}); err != nil && h.logger != nil {
			h.logger.ErrorContext(ctx, "couldn't record certificate key download audit event", slog.String("domain", domain), slog.String("error", err.Error()))
		}
	}

	if len(b.serverIDs) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't placed on a server yet", domain))
	}
	if h.Agents == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no agents are available to read certificates"))
	}
	session, ok := h.Agents.Session(b.serverIDs[0])
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("the server running %s isn't connected right now", domain))
	}
	got, err := session.ReadCertificate(ctx, domain, m.GetIncludeKey())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("couldn't read certificate for %s: %w", domain, err))
	}
	return connect.NewResponse(&dockwellv1.DownloadCertificateResponse{
		CertPem:   got.GetCertPem(),
		KeyPem:    got.GetKeyPem(),
		Issuer:    got.GetIssuer(),
		Subject:   got.GetSubject(),
		Sans:      got.GetSans(),
		NotBefore: got.GetNotBefore(),
		NotAfter:  got.GetNotAfter(),
		Custom:    got.GetCustom(),
	}), nil
}

// Keep timestamppb imported if needed elsewhere in package.
var _ = timestamppb.Now
