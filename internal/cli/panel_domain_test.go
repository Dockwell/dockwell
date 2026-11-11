// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

// Without a domain from its flags, dockwell run takes the one its settings hold,
// and one the panel can't take, as a route's, never stops it: it answers on
// loopback, and says why (ADR 0063). A domain from its flags wins.
func TestRunTakesThePanelsDomainFromItsSettings(t *testing.T) {
	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := revision.Setup(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	put := func(domain string) {
		t.Helper()
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, settings.KeyPanelDomain, []byte(domain)) }); err != nil {
			t.Fatal(err)
		}
	}
	if d, from, problem := panelDomainAtStart(ctx, logger, db, ""); d != "" || from != dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_NONE || problem != "" {
		t.Errorf("with none set: %q %v %q", d, from, problem)
	}
	put("panel.example.com")
	if d, from, problem := panelDomainAtStart(ctx, logger, db, ""); d != "panel.example.com" || from != dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_SETTING || problem != "" {
		t.Errorf("with one set: %q %v %q", d, from, problem)
	}
	if d, from, _ := panelDomainAtStart(ctx, logger, db, "run.example.com"); d != "run.example.com" || from != dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_RUN {
		t.Errorf("with one given to dockwell run: %q %v", d, from)
	}

	// A route took a name under it since.
	now := time.Now()
	if err := db.Update(ctx, func(tx *store.Tx) error {
		return tx.CreateServer(ctx, store.Server{ID: "srv_A", Name: "hz-1", CreatedAt: now, CertSerial: "x", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{})
	e := revision.New(db, kinds, anyAgent{}, secretstest.Keeper(db, kinds), logger)
	var tr scope.Tree
	if err := db.View(ctx, func(tx *store.Tx) (err error) {
		tr, err = scope.Load(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	env := tr.Projects[0].Environments[0].ID
	p, err := e.Propose(ctx, revision.Proposal{Summary: "Run", Actor: "usr_test", Edits: []revision.Edit{
		{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}},
		{Ref: resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}, Spec: &resourcev1.Route{Domain: "app.panel.example.com", Service: "web", Port: 80}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(ctx, p.Change); err != nil {
		t.Fatal(err)
	}
	d, from, problem := panelDomainAtStart(ctx, logger, db, "")
	if d != "" || from != dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_SETTING || !strings.Contains(problem, "the route www serves app.panel.example.com") {
		t.Errorf("with a route under it: %q %v %q; want loopback, and why", d, from, problem)
	}
}

// What holds port 80 or 443 is read from /proc by its process's name, leaving out
// Dockwell itself and Caddy.
func TestWhatHoldsThePanelsPortsIsReadFromProc(t *testing.T) {
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1111 1 0 100 0 0 10 0\n" +
		"   1: 00000000:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2222 1 0 100 0 0 10 0\n" +
		"   2: 0100007F:1D28 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3333 1 0 100 0 0 10 0\n" +
		"   3: 0A00000A:0050 0A00000B:C350 01 00000000:00000000 00:00000000 00000000     0        0 4444 1 0 100 0 0 10 0\n"
	link := func(target string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
	}
	proc := fstest.MapFS{
		"net/tcp":  {Data: []byte(tcp)},
		"net/tcp6": {Data: []byte(strings.SplitN(tcp, "\n", 2)[0] + "\n")},
		"100/comm": {Data: []byte("nginx\n")},
		"100/fd/3": link("socket:[1111]"),
		"100/fd/4": link("socket:[4444]"),
		"200/comm": {Data: []byte("caddy\n")},
		"200/fd/7": link("socket:[2222]"),
		"300/comm": {Data: []byte("dockwell\n")},
		"300/fd/9": link("socket:[2222]"),
		"400/comm": {Data: []byte("sshd\n")},
		"400/fd/1": link("socket:[3333]"),
		"400/fd/2": link("/dev/null"),
	}
	got := holders(proc, 300)
	if len(got) != 1 || got[0].GetPort() != 80 || got[0].GetProgram() != "nginx" {
		t.Errorf("held %v, want nginx on 80 alone", got)
	}
}

// A test resolves only the names DOCKWELL_TEST_DNS stands in for, and asks no
// DNS server.
func TestATestResolvesOnlyItsStandIns(t *testing.T) {
	env := map[string]string{envTest: "1", envTestDNS: "panel.example.com=192.0.2.10, panel.example.com=2001:db8::10"}
	resolve := resolverFor(func(k string) string { return env[k] })
	got, err := resolve(context.Background(), "panel.example.com")
	if err != nil || len(got) != 2 || got[0].String() != "192.0.2.10" {
		t.Errorf("a stand-in resolves to %v, %v", got, err)
	}
	var dnsErr *net.DNSError
	if _, err := resolve(context.Background(), "elsewhere.example.com"); !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Errorf("a name with no stand-in: %v, want not found", err)
	}
}
