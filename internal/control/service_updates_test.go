// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/website"
)

const (
	pg163   = "sha256:1630000000000000000000000000000000000000000000000000000000000000"
	pg164   = "sha256:1640000000000000000000000000000000000000000000000000000000000000"
	pg172   = "sha256:1720000000000000000000000000000000000000000000000000000000000000"
	nginxV1 = "sha256:a100000000000000000000000000000000000000000000000000000000000000"
	nginxV2 = "sha256:a200000000000000000000000000000000000000000000000000000000000000"
)

// updatesRegistry stands in for Docker Hub and its API: postgres has 16.3, 16.4
// and 17.2, and nginx's latest points at a newer image than the one that runs.
func updatesRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/postgres/manifests/16.3":
			w.Header().Set("Docker-Content-Digest", pg163)
		case "/v2/library/nginx/manifests/latest":
			w.Header().Set("Docker-Content-Digest", nginxV2)
		case "/v2/namespaces/library/repositories/postgres/tags":
			_, _ = fmt.Fprintf(w, `{"results":[{"name":"17.2","digest":%q,"tag_last_pushed":"2026-11-10T00:00:00Z"},{"name":"16.4","digest":%q,"tag_last_pushed":"2026-11-09T00:00:00Z"},{"name":"16.3","digest":%q,"tag_last_pushed":"2026-08-01T00:00:00Z"}]}`, pg172, pg164, pg163)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Each app is checked through its server's agent: postgres:16.3 learns of 16.4,
// a patch, and 17.2, a major, with the notes its image names, and nginx:latest
// of a new build. A person skips a version, and snoozes an app, and ListServices
// says the same as ServiceUpdateService (ADR 0079).
func TestAppsLearnWhatsNewerThroughTheirServer(t *testing.T) {
	storetest.Each(t, testAppsLearnWhatsNewer)
}

func testAppsLearnWhatsNewer(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: updatesRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	e.Registry["postgres:16.3"] = dockertest.Image{Digest: pg163, Config: docker.Config{Labels: map[string]string{"org.opencontainers.image.source": "https://github.com/docker-library/postgres"}}}
	e.Registry["nginx:latest"] = dockertest.Image{Digest: nginxV1}

	c := changeClient(b, b.origin)
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Run a database and a site",
		Edits:   []*dockwellv1.Edit{serviceEdit("db", "postgres:16.3", nil), serviceEdit("site", "nginx", nil)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(t, c, made.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}

	u := dockwellv1connect.NewServiceUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	checked, err := u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dockwellv1.ServiceUpdate{}
	ids := map[string]string{}
	for _, s := range checked.Msg.GetServices() {
		byName[s.GetResource().GetName()], ids[s.GetResource().GetName()] = s.GetUpdate(), s.GetId()
	}
	db := byName["db"]
	if db.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE || db.GetSource() != dockwellv1.UpdateSource_UPDATE_SOURCE_IMAGE || db.GetCheckTime() == nil {
		t.Fatalf("db's check = %v", db)
	}
	if got := said(db); got != "patch 16.3→16.4 postgres:16.4, major 16.3→17.2 postgres:17.2" {
		t.Errorf("db can move to %q", got)
	}
	if a := db.GetAvailable()[0]; a.GetNotesUrl() != "https://github.com/docker-library/postgres/releases" || a.GetToDigest() != pg164 ||
		a.GetFromDigest() != pg163 || a.GetReleaseTime() == nil || a.GetFoundTime() == nil {
		t.Errorf("16.4 = %v", a)
	}
	site := byName["site"]
	if got := said(site); got != "new build latest→latest nginx" || site.GetAvailable()[0].GetToDigest() != nginxV2 {
		t.Errorf("site can move to %q: %v", got, site)
	}

	// Skipping 17.2 leaves 16.4; skipping it too leaves nothing that counts.
	skip := func(version string, unskip bool) *dockwellv1.ServiceUpdate {
		t.Helper()
		res, err := u.SkipServiceUpdate(t.Context(), connect.NewRequest(&dockwellv1.SkipServiceUpdateRequest{Id: "db", Version: version, Unskip: unskip}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetUpdate()
	}
	if got := skip("17.2", false); got.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE || !got.GetAvailable()[1].GetSkipped() || got.GetAvailable()[0].GetSkipped() {
		t.Errorf("after skipping 17.2: %v", got)
	}
	if got := skip("16.4", false); got.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE {
		t.Errorf("after skipping both: %v", got)
	}
	if got := skip("16.4", true); got.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE {
		t.Errorf("after taking 16.4's skip back: %v", got)
	}

	// A snoozed app still says what's newer, and isn't listed as available.
	snoozed, err := u.SnoozeServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.SnoozeServiceUpdatesRequest{Id: ids["site"], Duration: "7d"}))
	if err != nil {
		t.Fatal(err)
	}
	if snoozed.Msg.GetUpdate().GetSnoozeTime() == nil || len(snoozed.Msg.GetUpdate().GetAvailable()) != 1 {
		t.Errorf("a snoozed site = %v", snoozed.Msg.GetUpdate())
	}
	listed, err := u.ListServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.ListServiceUpdatesRequest{Available: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetServices()) != 1 || listed.Msg.GetServices()[0].GetResource().GetName() != "db" {
		t.Errorf("available = %v, want db alone", listed.Msg.GetServices())
	}
	if _, err := u.SnoozeServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.SnoozeServiceUpdatesRequest{Id: ids["site"], Duration: "1y"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a year's snooze answered %v", err)
	}

	// ListServices carries the same on each row.
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	all, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range all.Msg.GetServices() {
		if info.GetUpdate() == nil || info.GetUpdate().GetCheckTime() == nil {
			t.Errorf("%s has no update on its row: %v", info.GetResource().GetName(), info.GetUpdate())
		}
	}

	// What a person did is on the audit log.
	var actions []string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err := tx.EventsSince(t.Context(), "service.update_skip", made.Msg.GetChange().GetCreateTime().AsTime().Add(-1), 10)
		for i := range events {
			actions = append(actions, events[i].Action)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Errorf("the audit log has %d skips, want 2", len(actions))
	}
}

// said is what a service can move to, a line.
func said(u *dockwellv1.ServiceUpdate) string {
	var out []string
	for _, a := range u.GetAvailable() {
		word := map[dockwellv1.UpdateKind]string{
			dockwellv1.UpdateKind_UPDATE_KIND_PATCH: "patch", dockwellv1.UpdateKind_UPDATE_KIND_MINOR: "minor",
			dockwellv1.UpdateKind_UPDATE_KIND_MAJOR: "major", dockwellv1.UpdateKind_UPDATE_KIND_DIGEST: "new build",
		}[a.GetKind()]
		out = append(out, fmt.Sprintf("%s %s→%s %s", word, a.GetFromVersion(), a.GetToVersion(), a.GetToImage()))
	}
	return strings.Join(out, ", ")
}

// Updates keep to a person's projects: another project's app can't be skipped,
// snoozed or checked by name or ID, and a list holds their own alone; a viewer
// reads and sets nothing aside.
func TestAppUpdatesKeepToAPersonsProjects(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	webID := web.GetPlan().GetResources()[0].GetId()
	blog := apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	}))
	blogID := blog.GetPlan().GetResources()[0].GetId()
	api := serviceEdit("api", "ghcr.io/acme/shop:1.4", nil)
	api.Project = "blog"
	apply(t, c, propose(t, c, "Run api", api))

	hold := aMember(t, f)
	hold(store.Grant{ProjectID: blogID, Role: store.RoleDeveloper})
	u := dockwellv1connect.NewServiceUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	for what, err := range map[string]error{
		"skip by ID":   second(u.SkipServiceUpdate(t.Context(), connect.NewRequest(&dockwellv1.SkipServiceUpdateRequest{Id: webID, Version: "1.5"}))),
		"skip by name": second(u.SkipServiceUpdate(t.Context(), connect.NewRequest(&dockwellv1.SkipServiceUpdateRequest{Id: "web", Project: "default", Version: "1.5"}))),
		"snooze by ID": second(u.SnoozeServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.SnoozeServiceUpdatesRequest{Id: webID, Duration: "1d"}))),
		"check by ID":  second(u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Id: webID}))),
	} {
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("another project's app, %s: %v, want not found", what, err)
		}
	}
	if _, err := u.SkipServiceUpdate(t.Context(), connect.NewRequest(&dockwellv1.SkipServiceUpdateRequest{Id: "api", Version: "2.1"})); err != nil {
		t.Errorf("their own app's skip: %v", err)
	}
	listed, err := u.ListServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.ListServiceUpdatesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetServices()) != 1 || listed.Msg.GetServices()[0].GetResource().GetName() != "api" {
		t.Errorf("their list = %v, want api alone", listed.Msg.GetServices())
	}
	hold(store.Grant{ProjectID: blogID, Role: store.RoleViewer})
	if _, err := u.SkipServiceUpdate(t.Context(), connect.NewRequest(&dockwellv1.SkipServiceUpdateRequest{Id: "api", Version: "2.2"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a viewer's skip: %v, want refused", err)
	}
	if _, err := u.ListServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.ListServiceUpdatesRequest{})); err != nil {
		t.Errorf("a viewer's list: %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

// The update-available rule starts off, and once on, sends an app's update
// once, for a major alone where it says so, and resolves once the app has none
// that counts (ADR 0079).
func TestAnUpdateIsSentOnceItsRuleIsOn(t *testing.T) {
	h := newHook(t)
	f, c, _ := evalStand(t, h.srv.URL+"/hook")
	at := &clock{now: time.Now()}
	found := []alerts.AppUpdate{
		{ServiceID: "svc_db", Name: "db", From: "16.3", To: "17.2", Major: true, Kind: "a major update"},
		{ServiceID: "svc_web", Name: "web", From: "1.27.3", To: "1.27.4", Kind: "a patch"},
	}
	e := evaluatorOn(f, at)
	e.Updates = func(context.Context) ([]alerts.AppUpdate, error) { return found, nil }
	tickOn(t, e)
	if got := h.of("update-available"); len(got) != 0 {
		t.Fatalf("a rule that's off sent %q", got)
	}
	turnOn := func(threshold uint32) {
		t.Helper()
		apply(t, c, propose(t, c, "Tell of updates", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.RuleKind), Name: "update-available"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertRule{AlertRule: &resourcev1.AlertRule{
				Condition: resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE, Threshold: threshold, Severity: resourcev1.AlertRule_SEVERITY_WARNING,
			}}},
		}))
	}
	turnOn(alert.MajorsOnly)
	ticks := func(n int) {
		for range n {
			tickOn(t, e)
			at.add(alerts.Every)
		}
	}
	ticks(12)
	var open []string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		alerted, err := tx.OpenAlerts(t.Context())
		for i := range alerted {
			if alerted[i].RuleName == "update-available" {
				open = append(open, alerted[i].Thing+" "+alerted[i].Title)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(open, ", ") != "update:svc_db db can move to 17.2" {
		t.Errorf("for majors alone, open = %q", open)
	}
	if got := h.of("update-available"); len(got) != 1 || !strings.HasPrefix(got[0], "firing:") {
		t.Errorf("sent %q, want one firing message", got)
	}
	// Updated, skipped or snoozed, it has none that counts, and resolves.
	found = nil
	ticks(12)
	if got := h.of("update-available"); len(got) != 2 || !strings.HasPrefix(got[1], "resolved:") {
		t.Errorf("sent %q, want firing then resolved", got)
	}
}

// #561: a website's own servers run images Dockwell picks and pins, as the ingress
// pins its web server's (ADR 0039). On a server with nothing but a website, a check
// asks no registry, its web server reads as kept current by Dockwell, asked of the
// server or of the service, nothing counts, and the update alert stays quiet.
func TestAWebsitesOwnServersAreKeptCurrentByDockwell(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	var asked atomic.Int32
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(reg.Close)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: reg.URL}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)

	c := changeClient(b, b.origin)
	made, err := websiteClient(b, b.origin).CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain: "blog.example.com", Website: toURL("https://backend.example.com/"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, made.Msg.GetChange())

	u := dockwellv1connect.NewServiceUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	checked, err := u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	web := ""
	for _, s := range checked.Msg.GetServices() {
		if s.GetResource().GetName() != website.WebService {
			continue
		}
		web = s.GetId()
		if got := s.GetUpdate(); got.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED || got.GetProblem() != dockwellv1.UpdateProblem_UPDATE_PROBLEM_KEPT_BY_DOCKWELL {
			t.Errorf("the website's web server reads as %v", got)
		}
	}
	if web == "" {
		t.Fatalf("the check lists no web server: %v", checked.Msg.GetServices())
	}
	if _, err := u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Id: web})); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("a website-only server's check asked a registry %d times", n)
	}
	listed, err := u.ListServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.ListServiceUpdatesRequest{Available: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetServices()) != 0 {
		t.Errorf("updates that count: %v", listed.Msg.GetServices())
	}

	at := &clock{now: time.Now()}
	ev := evaluatorOn(f, at)
	ev.Updates = appUpdates(f.server.updates)
	apply(t, c, propose(t, c, "Tell of updates", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.RuleKind), Name: "update-available"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertRule{AlertRule: &resourcev1.AlertRule{
			Condition: resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE, Severity: resourcev1.AlertRule_SEVERITY_WARNING,
		}}},
	}))
	for range 12 {
		tickOn(t, ev)
		at.add(alerts.Every)
	}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		open, err := tx.OpenAlerts(t.Context())
		for i := range open {
			if open[i].RuleName == "update-available" {
				t.Errorf("the update alert fired: %s", open[i].Title)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
