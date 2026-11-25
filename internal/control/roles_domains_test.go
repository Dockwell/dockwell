// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Domain certificate operations place a request by its domain's route, so a
// caller reaches only their own projects' domains, developers renew open
// environments within limits while protected environments and forced renewals
// need an admin, custom certificates need an admin on that project, and
// viewers can download only public certificates in their projects.
func TestDomainCertificateCallsKeepToTheirProjectAndRole(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()

		routeEdit := func(env, name, domain, svc string) *dockwellv1.Edit {
			return &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: env, Name: name},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{
					Domain: domain, Service: svc, Port: 80,
				}}},
			}
		}
		apply(t, s.c, propose(t, s.c, "Add routes",
			routeEdit(s.production, "prod", "prod.example.com", "web"),
			routeEdit(s.staging, "staging", "staging.example.com", "api"),
		))

		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		reached := func(what string, err error) {
			t.Helper()
			if r := reasonOf(err); r == "not_found" || r == "needs_role" {
				t.Errorf("%s: %v, want to pass role and scope checks", what, err)
			}
		}

		// Blog developer: reaches staging.example.com (open), not prod.example.com (other project).
		s.hold(in(s.blogID, store.RoleDeveloper))
		_, err := dc.RenewCertificate(ctx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{Domain: "prod.example.com"}))
		notFound(t, "blog developer renewing default's domain", err)
		_, err = dc.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{Domain: "prod.example.com"}))
		notFound(t, "blog developer downloading default's certificate", err)
		_, err = dc.RenewCertificate(ctx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{Domain: "absent.example.com"}))
		notFound(t, "blog developer renewing absent domain", err)

		_, err = dc.RenewCertificate(ctx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{Domain: "staging.example.com"}))
		reached("blog developer renewing staging domain", err)
		_, err = dc.RenewCertificate(ctx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{Domain: "staging.example.com", Force: true}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("blog developer forcing renewal: %v, want permission denied", err)
		}
		_, err = dc.SetCustomCertificate(ctx, connect.NewRequest(&dockwellv1.SetCustomCertificateRequest{Domain: "staging.example.com", Clear: true}))
		needsRole(t, "blog developer setting custom certificate", err)
		_, err = dc.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{Domain: "staging.example.com"}))
		reached("blog developer downloading staging public certificate", err)
		_, err = dc.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{Domain: "staging.example.com", IncludeKey: true}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("blog developer downloading private key: %v, want permission denied", err)
		}

		// Default developer: production is protected, so RenewCertificate requires an admin.
		s.hold(in(s.defaultID, store.RoleDeveloper))
		_, err = dc.RenewCertificate(ctx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{Domain: "prod.example.com"}))
		needsRole(t, "default developer renewing protected production domain", err)

		// Blog viewer: can download public cert in blog, not renew or read another project's.
		s.hold(in(s.blogID, store.RoleViewer))
		_, err = dc.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{Domain: "staging.example.com"}))
		reached("blog viewer downloading staging public certificate", err)
		_, err = dc.DownloadCertificate(ctx, connect.NewRequest(&dockwellv1.DownloadCertificateRequest{Domain: "prod.example.com"}))
		notFound(t, "blog viewer downloading default's certificate", err)
		_, err = dc.RenewCertificate(ctx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{Domain: "staging.example.com"}))
		needsRole(t, "blog viewer renewing staging domain", err)

		// Project-narrowed admin API token for blog: reaches blog's domain, not default's.
		s.hold(onInstall(store.RoleAdmin))
		secret, _ := narrowedToken(t, s.b, &dockwellv1.CreateTokenRequest{Projects: []string{s.blogID}})
		tokDC := dockwellv1connect.NewDomainServiceClient(bearer{token: secret, rt: s.b.client.Transport}, s.b.origin, connect.WithProtoJSON())
		_, err = tokDC.SetCustomCertificate(ctx, connect.NewRequest(&dockwellv1.SetCustomCertificateRequest{Domain: "prod.example.com", Clear: true}))
		notFound(t, "blog-scoped admin token setting custom cert on default's domain", err)
		_, err = tokDC.SetCustomCertificate(ctx, connect.NewRequest(&dockwellv1.SetCustomCertificateRequest{Domain: "staging.example.com", Clear: true}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "automatic certificate") {
			t.Errorf("blog-scoped admin token clearing custom cert on staging: %v, want handler's automatic-certificate error", err)
		}
	})
}
