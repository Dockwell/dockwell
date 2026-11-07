// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestAPublicAddressIsSetThroughTheAPI(t *testing.T) {
	storetest.Each(t, testAPublicAddressIsSetThroughTheAPI)
}

func testAPublicAddressIsSetThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	s := f.addNamedServer(t, "oci-1", false)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)

	_, err := servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID, Address: "10.0.0.5"}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "10.0.0.5 is private") {
		t.Errorf("a private address: %v", err)
	}
	_, err = servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: "srv_nobody", Address: "198.51.100.7"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v", err)
	}

	res, err := servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID, Address: " 198.51.100.7 "}))
	if err != nil {
		t.Fatal(err)
	}
	n := res.Msg.GetServer().GetNetwork()
	four := n.GetIpv4()
	if four.GetAddress() != "198.51.100.7" || four.GetSource() != dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON ||
		four.GetSetBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER || four.GetSetBy().GetName() == "" {
		t.Errorf("it reads %v", n)
	}
	if n.GetReachable() != dockwellv1.Reachable_REACHABLE_NOT_CHECKED {
		t.Errorf("a person's word made it reachable: %v", n.GetReachable())
	}

	// Each family is set and cleared by itself.
	res, err = servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID, Address: "2001:db8::7"}))
	if err != nil {
		t.Fatal(err)
	}
	if n := res.Msg.GetServer().GetNetwork(); n.GetIpv4().GetAddress() != "198.51.100.7" || n.GetIpv6().GetAddress() != "2001:db8::7" {
		t.Errorf("with an IPv6 address set, it reads %v", n)
	}
	res, err = servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID, Ipv6: true}))
	if err != nil {
		t.Fatal(err)
	}
	if n := res.Msg.GetServer().GetNetwork(); n.GetIpv4().GetAddress() != "198.51.100.7" || n.GetIpv6() != nil {
		t.Errorf("with IPv6 cleared, it reads %v", n)
	}

	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER},
	})
	found := false
	for _, e := range events {
		if e.GetAction() == "server.public_address" && e.GetSubject().GetId() == s.id.ServerID && e.GetSubject().GetName() == "oci-1" &&
			e.GetPublicAddress().GetAddress() == "198.51.100.7" {
			found = true
		}
	}
	if !found {
		t.Errorf("the timeline has %v", events)
	}

	// Clearing says what it cleared, so it can be put back.
	if _, err := servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID})); err != nil {
		t.Fatal(err)
	}
	cleared := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER},
	})
	if !slices.ContainsFunc(cleared, func(e *dockwellv1.Event) bool {
		return e.GetAction() == "server.public_address_clear" && e.GetPublicAddress().GetPrevious() == "198.51.100.7"
	}) {
		t.Errorf("the timeline has %v", cleared)
	}

	// A token can only read what the address is, unless it's made to change things.
	reader := dockwellv1connect.NewServerServiceClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	_, err = reader.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a read-only token: %v", err)
	}

	// REST takes the same path, as a client of its own would.
	code, _, body := callREST(t, b, http.MethodPost, "/v1/servers/"+s.id.ServerID+":setPublicAddress", nil, `{"address":"198.51.100.8"}`)
	if code != http.StatusOK || !strings.Contains(body, "198.51.100.8") {
		t.Errorf("POST: %d %s", code, body)
	}
}

func TestADomainIsCheckedAgainstAServerBeforeItHasARoute(t *testing.T) {
	storetest.Each(t, testADomainIsChecked)
}

func testADomainIsChecked(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	s := f.addNamedServer(t, "oci-1", false)
	domains := dockwellv1connect.NewDomainServiceClient(b, b.origin)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)
	checkOf := func(domain string) *dockwellv1.DomainCheck {
		t.Helper()
		res, err := domains.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.id.ServerID, Domain: domain}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetCheck()
	}

	if got := checkOf("nothing-here.example.com"); got.GetDomain() != "nothing-here.example.com" || got.GetState() != dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD {
		t.Errorf("a name with no record reads %v", got)
	}

	// It goes by what's known of the server, as a plan would.
	if _, err := servers.SetServerPublicAddress(t.Context(), connect.NewRequest(&dockwellv1.SetServerPublicAddressRequest{Id: s.id.ServerID, Address: "198.51.100.7"})); err != nil {
		t.Fatal(err)
	}
	f.names.set("app.example.com", "198.51.100.7")
	deadline := time.Now().Add(5 * time.Second)
	for checkOf("app.example.com").GetState() != dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE {
		if time.Now().After(deadline) {
			t.Fatalf("a record at the address set reads %v", checkOf("app.example.com"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.names.set("inside.example.com", "10.9.9.9")
	if got := checkOf("inside.example.com"); len(got.GetFound()) != 0 {
		t.Errorf("a private answer that isn't the server's is shown: %v", got)
	}

	_, err := domains.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.id.ServerID, Domain: "not a domain"}))
	if reasonOf(err) != "invalid" {
		t.Errorf("a bad name: %v", err)
	}
	_, err = domains.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: "srv_nobody", Domain: "app.example.com"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v", err)
	}

	// It resolves names through the control server, so a token that can only read can't.
	reader := dockwellv1connect.NewDomainServiceClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	_, err = reader.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.id.ServerID, Domain: "app.example.com"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a read-only token: %v", err)
	}
}
