// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

func TestOnlyAPanelOnThePublicPortsHandsThemOver(t *testing.T) {
	for _, tc := range []struct {
		https, http string
		want        bool
	}{
		{":443", ":80", true},
		{"0.0.0.0:443", "0.0.0.0:80", true},
		{":https", ":http", true},
		{":8443", ":80", true},
		{":443", ":8080", true},
		{":8443", ":8080", false},
		{"127.0.0.1:0", "127.0.0.1:0", false},
	} {
		if got := onPublicPorts(runFlags{httpsListen: tc.https, httpListen: tc.http}); got != tc.want {
			t.Errorf("%s and %s: %v, want %v", tc.https, tc.http, got, tc.want)
		}
	}
}

// With its proxy holding the public ports, dockwell run leaves them alone,
// whether the proxy is up or not, and serves the panel over a socket only the
// proxy and root reach. Taking them back would stop Caddy starting.
func TestRunLeavesThePortsToItsProxy(t *testing.T) {
	dir, err := os.MkdirTemp("", "dwr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := datadir.Dir{Root: dir}
	if err := caddy.EnsureDirs(d.Ingress()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Ingress(), "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Ports it could never bind in this test, as they are privileged or taken.
	out, logs, stop := startRunWithLogs(t, dir, "/claim#",
		"--domain", "panel.example.com", "--https-listen", "127.0.0.1:443", "--http-listen", "127.0.0.1:80",
		"--acme-directory-url", "https://acme.invalid/directory")
	defer stop()
	socket := caddy.PanelSocket(d.Ingress())
	deadline := time.Now().Add(10 * time.Second)
	var info os.FileInfo
	for time.Now().Before(deadline) {
		if info, err = os.Stat(socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no panel socket: %v\n%s\n%s", err, out.String(), logs.String())
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("the panel's socket is %v, want a socket with mode 0600", info.Mode())
	}
	run, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode().Perm() != 0o700 {
		t.Errorf("the socket's directory has mode %v, want 0700", run.Mode().Perm())
	}
	if !strings.Contains(logs.String(), "SSH tunnel") || !strings.Contains(logs.String(), "ssh -L ") {
		t.Errorf("the log doesn't say how to reach the panel if the proxy is down:\n%s", logs.String())
	}
}

// anAgent applies anything, as what is read is the store.
type anAgent struct{}

func (anAgent) Plan(_ context.Context, desired []resource.Desired) ([]resource.Step, error) {
	out := make([]resource.Step, len(desired))
	for i, d := range desired {
		out[i] = resource.Step{Ref: d.Ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	}
	return out, nil
}

func (anAgent) Apply(context.Context, resource.Step, proto.Message) error { return nil }

func (anAgent) Observe(context.Context, []resource.Ref) ([]proto.Message, error) { return nil, nil }

type anyAgent struct{}

func (anyAgent) Agent(string) (resource.Agent, bool) { return anAgent{}, true }

// A route that claims a name can't find itself under the panel's passkeys, so
// the panel can't take it, or what it is under.
func TestRunRefusesADomainARouteHolds(t *testing.T) {
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
	for _, domain := range []string{"app.panel.example.com", "panel.example.com"} {
		err := refuseClaimedDomain(ctx, db, domain)
		if err == nil || !strings.Contains(err.Error(), "the route www serves app.panel.example.com") {
			t.Errorf("--domain %s = %v, want it refused, naming the route", domain, err)
		}
	}
	if err := refuseClaimedDomain(ctx, db, "other.example.com"); err != nil {
		t.Errorf("--domain other.example.com = %v", err)
	}
}
