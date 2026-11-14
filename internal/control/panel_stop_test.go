// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/panels"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// laterStopper is the agent's stopper on an engine that's there once its server
// is: the test's agent gets its drivers before its engine is handed back.
type laterStopper struct{ s *panels.Stopper }

func (l *laterStopper) PlanPanel(ctx context.Context, ask resource.PanelAsked) (*resource.Panel, bool, error) {
	return l.s.PlanPanel(ctx, ask)
}

func (l *laterStopper) ApplyPanel(ctx context.Context, p resource.Panel, say func(string)) error {
	return l.s.ApplyPanel(ctx, p, say)
}

// Stopping the panel an import came from is a change with a plan and no revision
// (ADR 0089): it stops Coolify's own containers, each to stay stopped, and leaves
// its proxy and what it ran; a second stop finds nothing to do; undoing it starts
// each again as it stood, and that start has no undo of its own. A token can't
// plan one, and an agent from before says to update it.
func TestStoppingThePanelIsAChangeWithAPlan(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	later := &laterStopper{}
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&host.Driver{Panels: later}} })
	later.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	for _, name := range []string{"coolify", "coolify-db", "coolify-proxy"} {
		runByHand(t, e, name, func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	}
	ghost := runByHand(t, e, "ghost-k4s9", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	id := f.lastDocker.id.ServerID
	ic, c := importClient(b), changeClient(b, b.origin)
	stop := func() (*dockwellv1.Change, error) {
		res, err := ic.PlanStopPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStopPanelRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}
	panelStep := func(ch *dockwellv1.Change) *dockwellv1.HostPanel {
		for _, s := range ch.GetPlan().GetServers() {
			for _, st := range s.GetSteps() {
				if st.GetPanel() != nil {
					return st.GetPanel()
				}
			}
		}
		return nil
	}

	change, err := stop()
	if err != nil {
		t.Fatal(err)
	}
	p := panelStep(change)
	var parts []string
	for _, part := range p.GetParts() {
		parts = append(parts, part.GetName())
	}
	if change.GetSummary() != "Stop Coolify on hz-1" || !slices.Equal(parts, []string{"coolify", "coolify-db"}) || p.GetStays() != "coolify-proxy" ||
		p.GetParts()[0].GetWasRestart() != "always" || !change.GetPlan().GetHostAccess() {
		t.Fatalf("the stop reads %q, %v", change.GetSummary(), p)
	}
	if done := actedOn(t, c, change, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the stop ended %v: %s", done.GetState(), done.GetError())
	}
	for _, name := range parts {
		if got := e.Container(name); got.State.Running || got.HostConfig.RestartPolicy.Name != "no" {
			t.Errorf("after the stop %s is %v, restart %q", name, got.State.Running, got.HostConfig.RestartPolicy.Name)
		}
	}
	for _, name := range []string{"coolify-proxy", ghost} {
		if got := e.Container(name); !got.State.Running {
			t.Errorf("%s was stopped", name)
		}
	}

	// Stopped, there's nothing left to stop, and nothing is planned.
	if _, err := stop(); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "already" || !strings.Contains(err.Error(), "Coolify's own containers are stopped on hz-1 already") {
		t.Errorf("stopping it again = %v", err)
	}

	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	back := undo.Msg.GetChange()
	if !strings.HasPrefix(back.GetSummary(), "Undo: ") || !panelStep(back).GetStart() {
		t.Fatalf("the undo reads %q, %v", back.GetSummary(), panelStep(back))
	}
	if done := actedOn(t, c, back, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", done.GetState(), done.GetError())
	}
	for _, name := range parts {
		if got := e.Container(name); !got.State.Running || got.HostConfig.RestartPolicy.Name != "always" {
			t.Errorf("after the undo %s is %v, restart %q", name, got.State.Running, got.HostConfig.RestartPolicy.Name)
		}
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: back.GetId()})); err == nil || !strings.Contains(err.Error(), "Stop Coolify again to put that back") {
		t.Errorf("undoing the start = %v", err)
	}

	// Stopped again, it starts from Dockwell with no undo to lean on, with the
	// policy Coolify's installer gives its own; running, there's nothing to start.
	again, err := stop()
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, again, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the second stop ended %v: %s", done.GetState(), done.GetError())
	}
	start := func() (*dockwellv1.Change, error) {
		res, err := ic.PlanStartPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStartPanelRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}
	started, err := start()
	if err != nil {
		t.Fatal(err)
	}
	if started.GetSummary() != "Start Coolify again on hz-1" {
		t.Errorf("the start reads %q", started.GetSummary())
	}
	if done := actedOn(t, c, started, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the start ended %v: %s", done.GetState(), done.GetError())
	}
	for _, name := range parts {
		if got := e.Container(name); !got.State.Running || got.HostConfig.RestartPolicy.Name != "always" {
			t.Errorf("after the start %s is %v, restart %q", name, got.State.Running, got.HostConfig.RestartPolicy.Name)
		}
	}
	if _, err := start(); reasonOf(err) != "already" {
		t.Errorf("starting what runs = %v", err)
	}

	// A token can't plan one: it's a person's, as an action on the server's host.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewImportServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	if _, err := script.PlanStopPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStopPanelRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a token's stop = %v", err)
	}
	if _, err := ic.PlanStopPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStopPanelRequest{ServerId: id})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a stop of no panel = %v", err)
	}

	// An agent from before stops nothing, and says to update it.
	old := f.addOnePanelServer(t, t.TempDir(), nil).id.ServerID
	if _, err := ic.PlanStopPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStopPanelRequest{ServerId: old, Panel: dockwellv1.PanelKind_PANEL_KIND_DOKPLOY})); reasonOf(err) != "agent_outdated" ||
		!strings.Contains(err.Error(), "too old to stop Dokploy") {
		t.Errorf("a stop through an agent from before = %v", err)
	}
}
