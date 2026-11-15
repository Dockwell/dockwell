// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
)

// lentEntry publishes on a default port in the range Linux lends to outgoing
// connections, as 1Panel's apps' defaults in the 40000s are.
const lentEntry = `id: lent
name: Lent
summary: Publishes in the 40000s
category: productivity
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: app
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "40108"
compose: |
  services:
    app:
      image: ghcr.io/acme/board:1@sha256:` + digest1 + `
      ports:
        - "127.0.0.1:${PORT}:80"
`

// A default port in the range the server lends to outgoing connections is replaced
// at install by a free one outside it, from 8080, which the entry's page suggests and
// the plan says why; a port a person gives in that range is theirs to keep (#688).
func TestAnInstallKeepsItsDefaultPortOutOfTheLentRange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "lent.yaml"), []byte(lentEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	cat := catalogClient(b)

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "lent"}))
	if err != nil {
		t.Fatal(err)
	}
	if port := got.Msg.GetValues()["PORT"]; port != "8080" {
		t.Errorf("the entry's page suggests port %s", port)
	}

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "lent", Project: "lent"})
	notes := notesOf(planned, "ephemeral_port")
	if len(notes) != 1 || !strings.Contains(notes[0], "on 8080 in place of its default 40108") || !strings.Contains(notes[0], "(32768 to 60999)") {
		t.Errorf("the plan says %q", notes)
	}
	if got := apply(t, changeClient(b, b.origin), planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var project string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			project = docker.Project(r.GetResource().GetScope())
		}
	}
	if c := e.Newest(project, "app"); c == nil || len(c.HostConfig.PortBindings["80/tcp"]) != 1 || c.HostConfig.PortBindings["80/tcp"][0].HostPort != "8080" {
		t.Errorf("the app publishes %v", c)
	}

	// A person who gives a port in the range keeps it, with nothing said of a default.
	kept := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "lent", Project: "lent-two", Values: map[string]string{"PORT": "40108"}})
	if notes := notesOf(kept, "ephemeral_port"); len(notes) != 0 {
		t.Errorf("a port given says %q", notes)
	}
}
