// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// What a store fills in with the app's address, which the converter writes as
// loopback at web's port, follows the address from then on: its origin, or its
// host, in either form of an environment. A value that names more than the address
// stays as the file writes it (ADR 0103).
func TestAStoresAddressFollowsTheApps(t *testing.T) {
	file := []byte(`services:
  web:
    image: example/web:1.0
    environment:
      - PUBLIC_URL=http://localhost:${PORT}
      - ORIGINS=http://localhost:${PORT},https://localhost:${PORT}/
      - API=http://localhost:${PORT}/api?key=${KEY}
      - TWICE=https://localhost:${PORT},http://localhost:${PORT}
  worker:
    image: example/worker:1.0
    environment:
      HOST: localhost:${PORT}
      OTHER: localhost:${OTHER_PORT}
`)
	got := addressVariables(file, "PORT")
	want := []*catalogv1.AddressVariable{
		{Service: "web", Variable: "ORIGINS", Value: "{origin},{origin}/"},
		{Service: "web", Variable: "PUBLIC_URL", Value: "{origin}"},
		{Service: "web", Variable: "TWICE", Value: "{origin}"},
		{Service: "worker", Variable: "HOST", Value: "{host}"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("%d: got %v, want %v", i, got[i], want[i])
		}
	}
	if addressVariables(file, "") != nil {
		t.Error("an app with nothing to open follows an address")
	}
}

// A Runtipi app's address, as ${APP_DOMAIN} wrote it, follows the app's, and its
// entry passes the catalog's check with it.
func TestRuntipisAppDomainFollowsTheAppsAddress(t *testing.T) {
	store := storeOf(t, FormatRuntipi)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("a", 40), License: store.License}
	run, err := OnServer(t.Context(), store, listingOf(t, store, "testdata/runtipi", nil), src, committed, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := resultsByID(run)["notes"].Entry
	if e == nil {
		t.Fatal("notes was left out")
	}
	if err := catalog.CheckTagged(e); err != nil {
		t.Errorf("its entry fails the catalog's check: %v", err)
	}
	vars := e.GetAddressVariables()
	if len(vars) != 1 || vars[0].GetService() != "notes" || vars[0].GetVariable() != "PUBLIC_URL" || vars[0].GetValue() != "{origin}" {
		t.Errorf("its address variables are %v", vars)
	}
}

// A variable an app takes once as its identity never follows the address: a Matrix
// server's name, and a federated server's own domain, which a domain would change
// under accounts made with it (#779's review, H3).
func TestAnIdentitySetOnceDoesntFollowTheAddress(t *testing.T) {
	file := []byte(`services:
  homeserver:
    image: example/conduit:1.0
    environment:
      CONDUIT_SERVER_NAME: localhost:${PORT}
      CONDUIT_URL: http://localhost:${PORT}
  tuwunel:
    image: example/tuwunel:1.0
    environment:
      - TUWUNEL_SERVER_NAME=localhost:${PORT}
`)
	got := addressVariables(file, "PORT")
	if len(got) != 1 || got[0].GetVariable() != "CONDUIT_URL" {
		t.Errorf("got %v, want CONDUIT_URL alone", got)
	}
	// Each one on the list says why, from its app's docs.
	for name, why := range setOnce {
		if why == "" {
			t.Errorf("%s is set once with no reason", name)
		}
	}
}

// A host its app reads as an address with its scheme takes the origin, so behind
// HTTPS it says https, as TubeArchivist's TA_HOST does (#779's review, M3).
func TestAHostItsAppReadsWithItsSchemeTakesTheOrigin(t *testing.T) {
	file := []byte(`services:
  tubearchivist:
    image: example/tubearchivist:1.0
    environment:
      - TA_HOST=localhost:${PORT}
`)
	got := addressVariables(file, "PORT")
	if len(got) != 1 || got[0].GetVariable() != "TA_HOST" || got[0].GetValue() != "{origin}" {
		t.Errorf("got %v, want TA_HOST as {origin}", got)
	}
}

// What a service on the server's own network, or another container's, names at the
// app's port is that service reaching the app, never a browser's address, so it
// stays loopback, as Beszel's agent's HUB_URL does (#779's review, M5).
func TestAServiceOnTheServersNetworkKeepsLoopback(t *testing.T) {
	file := []byte(`services:
  hub:
    image: example/hub:1.0
    environment:
      APP_URL: http://localhost:${PORT}
  agent:
    image: example/agent:1.0
    network_mode: host
    environment:
      HUB_URL: http://localhost:${PORT}
  sidecar:
    image: example/sidecar:1.0
    network_mode: "service:hub"
    environment:
      HUB_URL: http://localhost:${PORT}
`)
	got := addressVariables(file, "PORT")
	if len(got) != 1 || got[0].GetService() != "hub" {
		t.Errorf("got %v, want hub's APP_URL alone", got)
	}
}

// A value that puts its own scheme or port around the address renders into nothing
// that works, so it stays as the file writes it (#779's review, L1).
func TestAValueAroundTheAddressIsntMarked(t *testing.T) {
	file := []byte(`services:
  web:
    image: example/web:1.0
    environment:
      - WS=wss://localhost:${PORT}/cable
      - WS_HOST=wss://localhost:${PORT}
      - SSH=ssh://localhost:${PORT}:23231
      - BASE=http:localhost:${PORT}
      - URL=http://localhost:${PORT}
`)
	got := addressVariables(file, "PORT")
	if len(got) != 1 || got[0].GetVariable() != "URL" {
		t.Errorf("got %v, want URL alone", got)
	}
}

// Zitadel's external domain, and Bonfire's HOSTNAME, by its image as other apps read
// a HOSTNAME at every start, are set once (#779's check, N4).
func TestZitadelsAndBonfiresDomainAreSetOnce(t *testing.T) {
	file := []byte(`services:
  zitadel:
    image: ghcr.io/zitadel/zitadel:v4.16.0
    environment:
      ZITADEL_EXTERNALDOMAIN: localhost:${PORT}
  bonfire:
    image: bonfirenetworks/bonfire:1.0.5-social-amd64
    environment:
      HOSTNAME: localhost:${PORT}
  other:
    image: example/other:1.0
    environment:
      HOSTNAME: localhost:${PORT}
`)
	got := addressVariables(file, "PORT")
	if len(got) != 1 || got[0].GetService() != "other" || got[0].GetVariable() != "HOSTNAME" {
		t.Errorf("got %v, want other's HOSTNAME alone", got)
	}
}
