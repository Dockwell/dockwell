// SPDX-License-Identifier: AGPL-3.0-only

package websites

import (
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/website"
)

func TestDomainFrontKeepsMainRouteSettings(t *testing.T) {
	t.Parallel()
	direct := &resourcev1.RoutePublish{Way: resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS, Provider: "cloudflare"}
	was := website.Front{Proof: "dns", Access: &resourcev1.RouteAccess{}}
	got := domainFront(was, &dockwellv1.WebsiteDomainFront{Publish: direct})
	if got.Proof != "dns" || got.Access != was.Access || !proto.Equal(got.Publish, direct) {
		t.Fatalf("expected the existing certificate and sign-in settings: %+v", got)
	}
	proxy := &resourcev1.RoutePublish{Way: resourcev1.RouteWay_ROUTE_WAY_PROXY, Provider: "cloudflare"}
	was = website.Front{Proof: "http", Publish: proxy}
	got = domainFront(was, &dockwellv1.WebsiteDomainFront{Publish: proxy})
	if got.Proof != "http" {
		t.Fatal("setting the same way in should keep the existing proof")
	}
	was = website.Front{TLS: "off", PlainForAnyone: true, Publish: direct}
	got = domainFront(was, &dockwellv1.WebsiteDomainFront{Tls: "off"})
	if !got.PlainForAnyone || got.Publish != nil {
		t.Fatalf("expected to keep plain HTTP and clear publishing: %+v", got)
	}
	got = domainFront(was, &dockwellv1.WebsiteDomainFront{Tls: "off", PlainForAnyone: proto.Bool(false)})
	if got.PlainForAnyone {
		t.Fatal("explicit proxy-only access should turn off plain HTTP to anyone")
	}
	got = domainFront(was, &dockwellv1.WebsiteDomainFront{Publish: direct})
	if got.PlainForAnyone {
		t.Fatal("requesting a certificate should disable plain HTTP to anyone")
	}
	// Plain HTTP for anyone in front, then the way in moves behind a tunnel or
	// Cloudflare's proxy: the server stops answering anyone directly (re-check N2).
	front := website.Front{TLS: "off", PlainForAnyone: true}
	for _, way := range []resourcev1.RouteWay{resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, resourcev1.RouteWay_ROUTE_WAY_PROXY} {
		got = domainFront(front, &dockwellv1.WebsiteDomainFront{Tls: "off", Publish: &resourcev1.RoutePublish{Way: way, Provider: "cloudflare"}})
		if got.PlainForAnyone {
			t.Fatalf("moving behind %s should close plain HTTP to anyone: %+v", way, got)
		}
	}
}
