// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// anAgent applies anything, as what the handler reads is the store.
type anAgent struct{}

func (anAgent) Plan(_ context.Context, desired []resource.Desired) ([]resource.Step, error) {
	out := make([]resource.Step, len(desired))
	for i, d := range desired {
		out[i] = resource.Step{Ref: d.Ref, Decision: resource.Decision{Action: resource.ActionCreate}}
		if d.Spec == nil {
			out[i].Action = resource.ActionDelete
		}
	}
	return out, nil
}
func (anAgent) Apply(context.Context, resource.Step, proto.Message) error        { return nil }
func (anAgent) Observe(context.Context, []resource.Ref) ([]proto.Message, error) { return nil, nil }

type agents struct{}

func (agents) Agent(string) (resource.Agent, bool) { return anAgent{}, true }

// reports are what servers said.
type reports map[string]*dockwellv1.DomainStatus

func (r reports) Domain(server, domain string) (*dockwellv1.DomainStatus, bool) {
	return r[server+"/"+domain], server != "srv_off"
}

func TestDomainsSayWhatEachSendsWhereAndWhereItsCertificateStands(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		logger := slog.New(slog.DiscardHandler)
		if _, err := revision.Setup(t.Context(), db, logger); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := db.Update(t.Context(), func(tx *store.Tx) error {
			for _, id := range []string{"srv_on", "srv_off"} {
				if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{})
		e := revision.New(db, kinds, agents{}, logger)
		var env, staging string
		var project string
		tree := func() scope.Tree {
			var tr scope.Tree
			if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
				tr, err = scope.Load(t.Context(), tx)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return tr
		}
		project = tree().Projects[0].ID
		env = tree().Projects[0].Environments[0].ID
		apply := func(summary string, edits ...revision.Edit) {
			t.Helper()
			p, err := e.Propose(t.Context(), revision.Proposal{Summary: summary, Actor: "usr_test", Edits: edits})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Apply(t.Context(), p.Change); err != nil {
				t.Fatal(err)
			}
		}
		apply("Add staging", revision.Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{}})
		for _, en := range tree().Projects[0].Environments {
			if en.Name == "staging" {
				staging = en.ID
			}
		}
		web := func(env, name string, servers ...string) revision.Edit {
			return revision.Edit{
				Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: name},
				Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
				Servers: servers,
			}
		}
		rt := func(env, name, domain, path, svc string, port uint32, server string) revision.Edit {
			return revision.Edit{
				Ref:     resource.Ref{Kind: route.Kind, Scope: env, Name: name},
				Spec:    &resourcev1.Route{Domain: domain, Path: path, Service: svc, Port: port},
				Servers: []string{server},
			}
		}
		apply("Run", web(env, "web", "srv_on", "srv_off"), web(env, "api", "srv_on"), web(staging, "web", "srv_on"))
		apply("Route",
			rt(env, "www", "app.example.com", "", "web", 8080, "srv_on"),
			rt(env, "api", "app.example.com", "/api", "api", 9090, "srv_on"),
			rt(env, "off", "off.example.com", "", "web", 8080, "srv_off"),
			rt(staging, "www", "staging.example.com", "", "web", 8080, "srv_on"))

		issued := &dockwellv1.DomainStatus{Domain: "app.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED}}
		failed := &dockwellv1.DomainStatus{Domain: "off.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED, Problem: dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_DNS, Message: "Point it here"}}
		h := New(kinds, db, reports{"srv_on/app.example.com": issued, "srv_off/off.example.com": failed}, logger)

		list := func(env, svc string) []*dockwellv1.Domain {
			t.Helper()
			res, err := h.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{Environment: env, Service: svc}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg.GetDomains()
		}
		names := func(list []*dockwellv1.Domain) string {
			out := make([]string, len(list))
			for i, d := range list {
				out[i] = d.GetName()
			}
			return strings.Join(out, " ")
		}

		all := list("", "")
		if got := names(all); got != "app.example.com off.example.com staging.example.com" {
			t.Fatalf("domains = %s", got)
		}
		app := all[0]
		if app.GetEnvironmentId() != env || app.GetUrl() != "https://app.example.com" {
			t.Errorf("the domain is %v", app)
		}
		if len(app.GetRoutes()) != 2 || app.GetRoutes()[0].GetPath() != "" || app.GetRoutes()[1].GetPath() != "/api" {
			t.Fatalf("its routes are %v", app.GetRoutes())
		}
		r := app.GetRoutes()[1]
		if r.GetName() != "api" || r.GetService() != "api" || r.GetPort() != 9090 || r.GetUrl() != "https://app.example.com/api" || r.GetId() == "" {
			t.Errorf("the route is %v", r)
		}
		if len(app.GetServers()) != 1 || app.GetServers()[0].GetServerName() != "srv_on" || app.GetServers()[0].GetStale() {
			t.Fatalf("its servers are %v", app.GetServers())
		}
		if app.GetServers()[0].GetStatus().GetCertificate().GetState() != dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED {
			t.Errorf("its status is %v", app.GetServers()[0].GetStatus())
		}

		// An offline server shows what it last said, marked so.
		off := all[1].GetServers()[0]
		if !off.GetStale() || off.GetStatus().GetCertificate().GetMessage() != "Point it here" {
			t.Errorf("the offline server reads %v", off)
		}
		// A server that hasn't said yet has no status, which isn't a failure.
		if s := all[2].GetServers()[0]; s.GetStatus() != nil || s.GetStale() {
			t.Errorf("a server that hasn't said reads %v", s)
		}

		if got := names(list(env, "")); got != "app.example.com off.example.com" {
			t.Errorf("in one environment = %s", got)
		}
		if got := names(list(env, "api")); got != "app.example.com" {
			t.Errorf("for one service = %s", got)
		}
		if got := names(list(staging, "web")); got != "staging.example.com" {
			t.Errorf("for a service of that name in another environment = %s", got)
		}
		_, err := h.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{Service: "web"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a service with no environment = %v", err)
		}

		// Deleted, it's gone.
		apply("Remove", revision.Edit{Ref: resource.Ref{Kind: route.Kind, Scope: staging, Name: "www"}})
		if got := names(list("", "")); got != "app.example.com off.example.com" {
			t.Errorf("after deleting a route = %s", got)
		}
	})
}
