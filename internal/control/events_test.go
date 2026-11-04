// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func eventClient(c connect.HTTPClient, origin string) dockwellv1connect.EventServiceClient {
	return dockwellv1connect.NewEventServiceClient(c, origin, connect.WithProtoJSON())
}

// listEvents reads every event a filter keeps, a page at a time.
func listEvents(t *testing.T, c dockwellv1connect.EventServiceClient, req *dockwellv1.ListEventsRequest) []*dockwellv1.Event {
	t.Helper()
	var out []*dockwellv1.Event
	for range 50 {
		res, err := c.ListEvents(t.Context(), connect.NewRequest(req))
		if err != nil {
			t.Fatalf("ListEvents %v: %v", req, err)
		}
		out = append(out, res.Msg.GetEvents()...)
		if req.PageToken = res.Msg.GetNextPageToken(); req.PageToken == "" {
			return out
		}
	}
	t.Fatal("ListEvents never reached its last page")
	return nil
}

func actions(events []*dockwellv1.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.GetAction()
		if c := e.GetChange(); c != nil {
			out[i] += " " + c.GetSummary()
		}
	}
	return out
}

// eventWatch follows a watch of the timeline: each event as last sent, and
// what it removed.
type eventWatch struct {
	t       *testing.T
	msgs    chan *dockwellv1.WatchEventsResponse
	latest  map[string]*dockwellv1.Event
	removed map[string]bool
	first   *dockwellv1.WatchEventsResponse
}

func watchEvents(t *testing.T, c dockwellv1connect.EventServiceClient, req *dockwellv1.WatchEventsRequest) *eventWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := c.WatchEvents(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("WatchEvents sent nothing: %v", stream.Err())
	}
	w := &eventWatch{
		t: t, msgs: make(chan *dockwellv1.WatchEventsResponse, 64), latest: map[string]*dockwellv1.Event{},
		removed: map[string]bool{}, first: stream.Msg(),
	}
	w.take(stream.Msg())
	go func() {
		defer close(w.msgs)
		for stream.Receive() {
			w.msgs <- stream.Msg()
		}
	}()
	return w
}

func (w *eventWatch) take(m *dockwellv1.WatchEventsResponse) {
	for _, e := range m.GetEvents() {
		w.latest[e.GetId()] = e
		delete(w.removed, e.GetId())
	}
	for _, r := range m.GetRemoved() {
		w.removed[r] = true
		delete(w.latest, r)
	}
}

// until reads the watch until ok holds, or fails after a few seconds.
func (w *eventWatch) until(what string, ok func(*eventWatch) bool) {
	w.t.Helper()
	deadline := time.After(10 * time.Second)
	for !ok(w) {
		select {
		case m, open := <-w.msgs:
			if !open {
				w.t.Fatalf("the watch ended waiting until %s", what)
			}
			if m.GetSnapshot() {
				w.t.Errorf("a later message was marked as the snapshot: %v", m)
			}
			w.take(m)
		case <-deadline:
			if len(w.latest) > 20 {
				w.t.Fatalf("timed out waiting until %s; the watch has %d events", what, len(w.latest))
			}
			w.t.Fatalf("timed out waiting until %s; the watch has %v", what, w.latest)
		}
	}
}

// The timeline end to end: changes, rollbacks and undos as they move on,
// beside servers joining and sign-ins, with who and from where; filters
// and pages; a watch that follows it all, events from another process
// included; and no secret anywhere.
func TestTheTimelineThroughTheAPI(t *testing.T) { storetest.Each(t, testTheTimelineThroughTheAPI) }

func testTheTimelineThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	seen := &recorder{next: b}
	ev := eventClient(seen, b.origin)
	c := changeClient(b, b.origin)
	watch := watchEvents(t, eventClient(newBrowserLike(b), b.origin), &dockwellv1.WatchEventsRequest{})
	if !watch.first.GetSnapshot() || watch.first.GetNextPageToken() != "" {
		t.Errorf("the first message = %v, want the whole snapshot", watch.first)
	}
	// Claiming signed the owner in with a passkey; the setup change isn't
	// on the timeline.
	if got := actions(watch.first.GetEvents()); !slices.Equal(got, []string{"auth.sign_in", "passkey.add", "owner.claim", "claim_link.create"}) {
		t.Errorf("the snapshot holds %v, want the claim's events newest first", got)
	}
	signIn := watch.first.GetEvents()[0]
	if signIn.GetKind() != dockwellv1.EventKind_EVENT_KIND_ACCESS || signIn.GetActor().GetName() != "Mira Chen" ||
		signIn.GetSurface() != dockwellv1.Surface_SURFACE_PANEL {
		t.Errorf("the sign-in = %v, want Mira Chen's access from the panel", signIn)
	}

	hz1 := f.addServer(t, true)
	watch.until("hz-1 joins", func(w *eventWatch) bool {
		for _, e := range w.latest {
			if e.GetAction() == "server.join" {
				return e.GetKind() == dockwellv1.EventKind_EVENT_KIND_SERVER && e.GetSubject().GetName() == "hz-1" &&
					e.GetSubject().GetId() == hz1.id.ServerID
			}
		}
		return false
	})

	// A change is one event, which moves on as the change does.
	run := propose(t, c, "Run web", webEdit("nginx:1.27", map[string]string{"API_KEY": "s3cret-value"}))
	watch.until("the change is planned", func(w *eventWatch) bool {
		e := w.latest[run.GetId()]
		return e.GetAction() == "change.planned" && e.GetChange().GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED &&
			e.GetKind() == dockwellv1.EventKind_EVENT_KIND_CHANGE && e.GetActor().GetName() == "Mira Chen"
	})
	apply(t, c, run)
	watch.until("the change is applied", func(w *eventWatch) bool {
		e := w.latest[run.GetId()]
		steps := e.GetChange().GetSteps()
		return e.GetAction() == "change.applied" && len(steps) == 1 && steps[0].GetState() == dockwellv1.StepState_STEP_STATE_DONE &&
			e.GetChange().GetUndoDeadline() != nil && e.GetChange().GetPlan().GetSummary().GetCreate() == 1
	})
	if got := watch.latest[run.GetId()].GetTime().AsTime(); !got.Equal(watch.latest[run.GetId()].GetChange().GetApplyTime().AsTime()) {
		t.Errorf("an applied change is timed %v, want when it started applying", got)
	}

	// A discarded plan leaves the timeline.
	dropped := propose(t, c, "Run web twice", webEdit("nginx:1.27", nil))
	watch.until("the plan shows", func(w *eventWatch) bool { return w.latest[dropped.GetId()] != nil })
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: dropped.GetId()})); err != nil {
		t.Fatal(err)
	}
	watch.until("the discarded plan leaves", func(w *eventWatch) bool { return w.removed[dropped.GetId()] })

	// Undoing is a change too; once it applies, the change it undid says
	// so, and the watch sends that change again.
	undoRes, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: run.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	undo := undoRes.Msg.GetChange()
	apply(t, c, undo)
	watch.until("the undone change says so", func(w *eventWatch) bool {
		return w.latest[run.GetId()].GetChange().GetUndoneBy() == undo.GetId() && w.latest[undo.GetId()].GetAction() == "change.applied"
	})

	// A failure is rolled back by a change of Dockwell's own, which shows
	// beside it.
	hz1.docker.Broken["nginx:broken"] = true
	broken := propose(t, c, "Break web", webEdit("nginx:broken", nil))
	apply(t, c, broken)
	var rollback string
	watch.until("the failure and its rollback show", func(w *eventWatch) bool {
		failed := w.latest[broken.GetId()]
		rollback = failed.GetChange().GetRolledBackBy()
		rb := w.latest[rollback]
		return failed.GetAction() == "change.failed" && rb.GetAction() == "change.applied" &&
			rb.GetActor().GetKind() == dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL && rb.GetSurface() == dockwellv1.Surface_SURFACE_AUTOMATIC &&
			rb.GetChange().GetRollback() && rb.GetChange().GetReverts() == broken.GetId()
	})

	// An API token's change is the token's, from the API.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	byToken := propose(t, script, "Open http", ruleEdit("http", 80))
	watch.until("the token's change shows", func(w *eventWatch) bool {
		e := w.latest[byToken.GetId()]
		return e.GetActor().GetKind() == dockwellv1.ActorKind_ACTOR_KIND_TOKEN && e.GetSurface() == dockwellv1.Surface_SURFACE_API
	})
	var tokenCreated *dockwellv1.Event
	for _, e := range watch.latest {
		if e.GetAction() == "token.create" {
			tokenCreated = e
		}
	}
	if tokenCreated.GetSubject().GetName() != "CI TOKEN_SCOPE_READ_WRITE" || !strings.HasPrefix(tokenCreated.GetSubject().GetId(), "tok_") {
		t.Errorf("the token's creation = %v, want it to name the token", tokenCreated)
	}

	// Another process sharing the store, such as dockwell recover beside
	// the control plane, is read again within a few seconds.
	other, err := store.Open(t.Context(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var userID string
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		events, _ := tx.ListEvents(t.Context(), 100)
		for i := range events {
			if events[i].Action == "owner.claim" {
				userID = events[i].Subject
			}
		}
		return nil
	})
	linkEvent := id.New(id.Event)
	if err := other.Update(t.Context(), func(tx *store.Tx) error {
		return tx.RecordEvent(t.Context(), store.Event{
			ID: linkEvent, At: time.Now(), Actor: "cli:mira@hz-1", Action: "sign_in_link.create", Subject: userID, Detail: "lnk_x expires soon",
			Surface: store.SurfaceCLI, Target: userID,
		})
	}); err != nil {
		t.Fatal(err)
	}
	watch.until("the other process's event shows", func(w *eventWatch) bool {
		e := w.latest[linkEvent]
		return e.GetActor().GetKind() == dockwellv1.ActorKind_ACTOR_KIND_SHELL && e.GetActor().GetName() == "mira@hz-1" &&
			e.GetSurface() == dockwellv1.Surface_SURFACE_CLI && e.GetSubject().GetName() == "Mira Chen"
	})

	// Listing: newest first, a page at a time, the same as the watch has.
	all := listEvents(t, ev, &dockwellv1.ListEventsRequest{PageSize: 3})
	for i, e := range all {
		if i > 0 && e.GetTime().AsTime().After(all[i-1].GetTime().AsTime()) {
			t.Errorf("event %d (%s) is newer than the one before it", i, e.GetAction())
		}
		if w := watch.latest[e.GetId()]; w == nil {
			t.Errorf("ListEvents has %s %s, which the watch never sent", e.GetId(), e.GetAction())
		}
		if e.GetId() == dropped.GetId() {
			t.Error("ListEvents has the discarded plan")
		}
	}
	if len(all) != len(watch.latest) {
		t.Errorf("ListEvents has %d events, the watch %d: %v", len(all), len(watch.latest), actions(all))
	}
	if all[0].GetId() != linkEvent {
		t.Errorf("the newest event is %s, want the sign-in link", all[0].GetAction())
	}

	// Filters.
	onlyChanges := listEvents(t, ev, &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_CHANGE}})
	if got := actions(onlyChanges); !slices.Equal(got, []string{
		"change.planned Open http", "change.applied Roll back Break web", "change.failed Break web", "change.applied Undo Run web",
		"change.applied Run web",
	}) {
		t.Errorf("the changes are %v", got)
	}
	onHz1 := listEvents(t, ev, &dockwellv1.ListEventsRequest{ServerId: hz1.id.ServerID})
	if got := actions(onHz1); len(got) != 6 || got[len(got)-1] != "server.join" {
		t.Errorf("hz-1's events are %v, want its five changes and its join", got)
	}
	web := run.GetPlan().GetResources()[0].GetId()
	if got := actions(listEvents(t, ev, &dockwellv1.ListEventsRequest{ResourceId: web})); !slices.Equal(got, []string{
		"change.applied Roll back Break web", "change.failed Break web", "change.applied Undo Run web", "change.applied Run web",
	}) {
		t.Errorf("web's events are %v", got)
	}
	mira := signIn.GetActor().GetId()
	for _, e := range listEvents(t, ev, &dockwellv1.ListEventsRequest{ActorId: mira}) {
		if e.GetActor().GetId() != mira && e.GetChange().GetProposedBy().GetId() != mira {
			t.Errorf("Mira's events have %s by %v", e.GetAction(), e.GetActor())
		}
	}
	byDockwell := listEvents(t, ev, &dockwellv1.ListEventsRequest{ActorKind: dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL})
	// The test enrolls hz-1 as the control server, which Dockwell joins
	// itself.
	if got := actions(byDockwell); !slices.Equal(got, []string{"change.applied Roll back Break web", "server.join"}) {
		t.Errorf("what Dockwell did is %v, want the rollback and hz-1 joining", got)
	}
	byShell := listEvents(t, ev, &dockwellv1.ListEventsRequest{ActorId: "mira@hz-1"})
	if len(byShell) != 1 || byShell[0].GetId() != linkEvent {
		t.Errorf("mira@hz-1's events are %v", actions(byShell))
	}
	if _, err := ev.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{ActorId: "Mira Chen"})); reasonOf(err) != "invalid" {
		t.Errorf("an actor named by name = %v, want invalid", err)
	}
	if _, err := ev.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{PageToken: "nonsense"})); reasonOf(err) != "invalid" {
		t.Errorf("a made-up page token = %v, want invalid", err)
	}

	// A filtered watch sends only what passes.
	changesOnly := watchEvents(t, ev, &dockwellv1.WatchEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_CHANGE}, PageSize: 2})
	if n := changesOnly.first.GetNextPageToken(); n == "" || len(changesOnly.first.GetEvents()) != 2 {
		t.Errorf("a watch's snapshot of 2 = %v, want 2 events and a token for more", changesOnly.first)
	}
	rest := listEvents(t, ev, &dockwellv1.ListEventsRequest{
		Kinds:     []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_CHANGE},
		PageToken: changesOnly.first.GetNextPageToken(),
	})
	if len(rest) != 3 {
		t.Errorf("after the snapshot ListEvents has %v, want the three older changes", actions(rest))
	}
	apply(t, script, byToken)
	changesOnly.until("the token's change applies", func(w *eventWatch) bool {
		return w.latest[byToken.GetId()].GetAction() == "change.applied"
	})
	for _, e := range changesOnly.latest {
		if e.GetKind() != dockwellv1.EventKind_EVENT_KIND_CHANGE {
			t.Errorf("a watch of changes sent %s", e.GetAction())
		}
	}

	// No secret, in any response.
	if strings.Contains(seen.String(), "s3cret-value") {
		t.Error("a response carried the secret")
	}
}

// A project's events include its environments', and resources list by
// name with their display names, never their specs.
func TestProjectEventsAndResources(t *testing.T) { storetest.Each(t, testProjectEventsAndResources) }

func testProjectEventsAndResources(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	ev := eventClient(b, b.origin)
	rs := dockwellv1connect.NewResourceServiceClient(b, b.origin, connect.WithProtoJSON())

	shop := apply(t, c, propose(t, c, "Add the shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Shop"}}},
	}))
	staging := apply(t, c, propose(t, c, "Add staging to the shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Scope: "shop", Name: "staging"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{DisplayName: "Staging"}}},
	}))
	apply(t, c, propose(t, c, "Add qa to the default project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Scope: "default", Name: "qa"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	}))

	projectID := shop.GetPlan().GetResources()[0].GetId()
	got := listEvents(t, ev, &dockwellv1.ListEventsRequest{ResourceId: projectID})
	if ids := []string{got[0].GetId(), got[len(got)-1].GetId()}; len(got) != 2 || ids[0] != staging.GetId() || ids[1] != shop.GetId() {
		t.Errorf("the shop's events are %v, want its staging environment and itself", actions(got))
	}

	res, err := rs.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: "project"}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range res.Msg.GetResources() {
		names = append(names, r.GetResource().GetName()+"="+r.GetDisplayName())
	}
	if !slices.Equal(names, []string{"default=", "shop=Shop"}) {
		t.Errorf("the projects are %v", names)
	}
	inShop, err := rs.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: "environment", Scope: projectID, PageSize: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if r := inShop.Msg.GetResources(); len(r) != 1 || r[0].GetResource().GetName() != "production" || inShop.Msg.GetNextPageToken() == "" ||
		r[0].GetRevision() != 1 || r[0].GetUpdateTime() == nil {
		t.Errorf("the shop's first environment = %v, want production and a next page", inShop.Msg)
	}
	next, err := rs.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{
		Kind: "environment", Scope: projectID, PageSize: 1, PageToken: inShop.Msg.GetNextPageToken(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r := next.Msg.GetResources(); len(r) != 1 || r[0].GetDisplayName() != "Staging" || next.Msg.GetNextPageToken() != "" {
		t.Errorf("the shop's second environment = %v, want staging on the last page", next.Msg)
	}
	all, err := rs.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first, whatever their kinds: each project before the
	// environments made in it, not every environment first.
	var made []string
	for _, r := range all.Msg.GetResources() {
		made = append(made, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	if !slices.Equal(made, []string{
		"project default", "environment production", "project shop", "environment production", "environment staging", "environment qa",
	}) {
		t.Errorf("every resource, oldest first, is %v", made)
	}
	var paged []string
	token := ""
	for range 10 {
		res, err := rs.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{PageSize: 4, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Msg.GetResources() {
			paged = append(paged, r.GetResource().GetKind()+" "+r.GetResource().GetName())
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if !slices.Equal(paged, made) {
		t.Errorf("every resource, in pages of 4, is %v", paged)
	}
}

// The CLI's local access on the control server is on the timeline as one
// notion: the shell account that ran dockwell makes the token, and what
// the CLI does with it is the token's, from the CLI. Its changes and every
// audit event its calls cause record the CLI the same way.
func TestTheCLIOnTheTimeline(t *testing.T) { storetest.Each(t, testTheCLIOnTheTimeline) }

func testTheCLIOnTheTimeline(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ev := eventClient(b, b.origin)

	// As dockwell does with no token on the control server.
	const shell = "cli:mira@hz-1"
	local, err := f.auth.CreateLocalToken(t.Context(), shell, "Local access on hz-1, read-write", store.ScopeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	cli := &apicli.Client{HTTP: apicli.NewHTTPClient(), URL: "http://" + f.api, Token: local.Secret, Local: true, Version: "v0.4.0"}
	added := apply(t, cli.Changes(), propose(t, cli.Changes(), "Add the shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	}))

	events := listEvents(t, ev, &dockwellv1.ListEventsRequest{})
	var made, change *dockwellv1.Event
	for _, e := range events {
		switch {
		case e.GetAction() == "token.create" && e.GetSubject().GetId() == local.Token.ID:
			made = e
		case e.GetId() == added.GetId():
			change = e
		}
	}
	if made.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_SHELL || made.GetActor().GetName() != "mira@hz-1" ||
		made.GetSurface() != dockwellv1.Surface_SURFACE_CLI || made.GetSubject().GetName() != "Local access on hz-1, read-write" {
		t.Errorf("local access's token on the timeline = %v, want mira@hz-1 making it from the CLI", made)
	}
	for _, by := range []*dockwellv1.Actor{change.GetChange().GetProposedBy(), change.GetChange().GetAppliedBy(), change.GetActor()} {
		if by.GetKind() != dockwellv1.ActorKind_ACTOR_KIND_TOKEN || by.GetId() != local.Token.ID || by.GetName() != "Local access on hz-1, read-write" {
			t.Errorf("the CLI's change is by %v, want local access's token", by)
		}
	}
	if c := change.GetChange(); change.GetSurface() != dockwellv1.Surface_SURFACE_CLI ||
		c.GetProposedVia() != dockwellv1.Surface_SURFACE_CLI || c.GetAppliedVia() != dockwellv1.Surface_SURFACE_CLI {
		t.Errorf("the CLI's change came from %v, proposed from %v and applied from %v, want the CLI each time",
			change.GetSurface(), c.GetProposedVia(), c.GetAppliedVia())
	}

	// The audit log behind it: the change's events, and the token's first
	// use, which stays off the timeline, all from the CLI.
	if _, err := f.auth.RevokeLocalToken(t.Context(), shell, local.Token.ID); err != nil {
		t.Fatal(err)
	}
	from := map[string]string{}
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		all, err := tx.ListEvents(t.Context(), 100)
		for i := range all {
			if strings.HasPrefix(all[i].Action, "change.") || strings.HasPrefix(all[i].Action, "token.") {
				from[all[i].Action+" "+all[i].Actor] = all[i].Surface
			}
		}
		return err
	})
	tokenActor := "token:" + local.Token.ID
	for _, what := range []string{
		"token.create " + shell, "token.use " + tokenActor, "change.planned " + tokenActor, "change.applied " + tokenActor, "token.revoke " + shell,
	} {
		if from[what] != store.SurfaceCLI {
			t.Errorf("the audit log has %s from %q, want the CLI", what, from[what])
		}
	}
}

// A burst of events, more than a watch reads the store for at a time, all
// within the moments it reads again, reaches the watch in full: it pages
// forward through them rather than stalling on the first page.
func TestAWatchKeepsUpWithABurst(t *testing.T) { storetest.Each(t, testAWatchKeepsUpWithABurst) }

func testAWatchKeepsUpWithABurst(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	kinds := []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER}
	watch := watchEvents(t, eventClient(b, b.origin), &dockwellv1.WatchEventsRequest{Kinds: kinds})
	const burst = 1200
	write := func(from, to int) {
		t.Helper()
		if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
			for i := from; i < to; i++ {
				if err := tx.RecordEvent(t.Context(), store.Event{
					ID: id.New(id.Event), At: time.Now(), Actor: "server:srv_burst", Action: "server.renew",
					Subject: "srv_burst", Detail: strconv.Itoa(i), Surface: store.SurfaceAutomatic, Target: "srv_burst",
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(0, burst)
	renewals := func(w *eventWatch) int {
		n := 0
		for _, e := range w.latest {
			if e.GetAction() == "server.renew" {
				n++
			}
		}
		return n
	}
	watch.until("the whole burst arrives", func(w *eventWatch) bool { return renewals(w) == burst })
	// And what comes after it, while the burst is still within the moments
	// the watch reads again.
	write(burst, burst+1)
	watch.until("the event after the burst arrives", func(w *eventWatch) bool { return renewals(w) == burst+1 })
}

// A discarded plan leaves every watch that keeps it, whether the watch
// sent it or the client read it with ListEvents, and whichever process
// discarded it.
func TestAWatchSaysWhatWasDiscarded(t *testing.T) { storetest.Each(t, testAWatchSaysWhatWasDiscarded) }

func testAWatchSaysWhatWasDiscarded(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	ev := eventClient(b, b.origin)
	project := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}
	}
	older := propose(t, c, "Add the shop", project("shop"))
	propose(t, c, "Add the blog", project("blog"))

	changesOnly := []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_CHANGE}
	watch := watchEvents(t, ev, &dockwellv1.WatchEventsRequest{Kinds: changesOnly, PageSize: 1})
	if got := watch.first.GetEvents(); len(got) != 1 || got[0].GetId() == older.GetId() {
		t.Fatalf("the snapshot = %v, want the newer plan alone", actions(got))
	}
	// The client reads the older plan as an earlier page, as the panel's
	// "Show earlier events" does.
	earlier := listEvents(t, ev, &dockwellv1.ListEventsRequest{Kinds: changesOnly, PageToken: watch.first.GetNextPageToken()})
	if len(earlier) == 0 || earlier[0].GetId() != older.GetId() {
		t.Fatalf("the page after the snapshot = %v, want the older plan", actions(earlier))
	}
	access := watchEvents(t, ev, &dockwellv1.WatchEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}})

	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: older.GetId()})); err != nil {
		t.Fatal(err)
	}
	watch.until("the plan read with ListEvents leaves", func(w *eventWatch) bool { return w.removed[older.GetId()] })

	// Another process, such as a second control plane on the same Postgres,
	// discards a plan this one never hears of from its engine.
	sent := propose(t, c, "Add the docs", project("docs"))
	watch.until("the new plan shows", func(w *eventWatch) bool { return w.latest[sent.GetId()] != nil })
	other, err := store.Open(t.Context(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	kinds := resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{})
	elsewhere := revision.New(other, kinds, agentapi.NewAgents(kinds, slog.New(slog.DiscardHandler)), slog.New(slog.DiscardHandler))
	if err := elsewhere.Discard(t.Context(), sent.GetId(), revision.By{Actor: "cli:mira@hz-1", Via: store.SurfaceCLI}); err != nil {
		t.Fatal(err)
	}
	watch.until("the plan discarded elsewhere leaves", func(w *eventWatch) bool { return w.removed[sent.GetId()] })

	// A watch of access events never held a change, so it hears of none.
	select {
	case m := <-access.msgs:
		if len(m.GetRemoved()) > 0 {
			t.Errorf("a watch of access events was told %v left", m.GetRemoved())
		}
	case <-time.After(500 * time.Millisecond):
	}
}
