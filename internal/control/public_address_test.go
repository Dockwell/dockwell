// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"strings"
	"testing"

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
	if n.GetPublicAddress() != "198.51.100.7" || n.GetSource() != dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON ||
		n.GetSetBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER || n.GetSetBy().GetName() == "" {
		t.Errorf("it reads %v", n)
	}
	if n.GetReachable() != dockwellv1.Reachable_REACHABLE_NOT_CHECKED {
		t.Errorf("a person's word made it reachable: %v", n.GetReachable())
	}

	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER},
	})
	found := false
	for _, e := range events {
		if e.GetAction() == "server.public_address" && e.GetSubject().GetId() == s.id.ServerID && e.GetSubject().GetName() == "oci-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("the timeline has %v", events)
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
