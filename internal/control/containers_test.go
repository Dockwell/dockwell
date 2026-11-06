// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func containerClient(c connect.HTTPClient, origin string) dockwellv1connect.ContainerServiceClient {
	return dockwellv1connect.NewContainerServiceClient(c, origin, connect.WithProtoJSON())
}

func composeLabels(project, service string) map[string]string {
	return map[string]string{docker.LabelProject: project, docker.LabelService: service, docker.LabelOneoff: "False"}
}

func names(list []*dockwellv1.ServerContainer) []string {
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = c.GetName()
	}
	return out
}

// A person who moves a server sees everything Dockwell didn't make, once, and
// nothing Dockwell did. What decides that is what this control plane has made, not
// what a label looks like: another Dockwell's leftovers, a service this one never
// heard of and a one-off are all someone else's.
func TestTheListIsWhatDockwellDidntMake(t *testing.T) {
	storetest.Each(t, testTheListIsWhatDockwellDidntMake)
}

func testTheListIsWhatDockwellDidntMake(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)

	empty, err := cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Msg.GetContainers()) != 0 || empty.Msg.GetOwnContainers() != 0 {
		t.Errorf("a server with nothing on it lists %v", empty.Msg)
	}

	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	project := docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope())
	alone := webEdit("ghcr.io/acme/worker:1.0", nil)
	alone.Resource = &dockwellv1.ResourceRef{Name: "alone"}
	alone.GetSpec().GetContainer().Ports = nil
	apply(t, c, propose(t, c, "Run alone", alone))

	e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", Config: docker.Config{Image: "nginx:1.27", Env: []string{"API_KEY=hunter2"}}})
	e.AddContainer(docker.Container{ID: "a2", Name: "/shop-db-1", Config: docker.Config{Image: "postgres:17", Labels: composeLabels("shop", "db")}})
	e.AddContainer(docker.Container{ID: "a3", Name: "/other-install", Config: docker.Config{Image: "redis:7", Labels: composeLabels("env_01aaaaaaaaaaaaaaaaaaaaaaaa", "web")}})
	e.AddContainer(docker.Container{ID: "a4", Name: "/stray", Config: docker.Config{Image: "redis:7", Labels: composeLabels(project, "never-made")}})
	oneOff := composeLabels(project, "web")
	oneOff[docker.LabelOneoff] = "True"
	e.AddContainer(docker.Container{ID: "a5", Name: "/migrate", Config: docker.Config{Image: "acme/migrate:1", Labels: oneOff}})
	e.AddContainer(docker.Container{ID: "a6", Name: "/dockwell-ingress", Config: docker.Config{Image: "caddy:2", Labels: map[string]string{docker.LabelIngress: "caddy"}}})

	seen := len(e.Seen())
	res, err := cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"by-hand", "migrate", "other-install", "shop-db-1", "stray"}
	if got := names(res.Msg.GetContainers()); !slices.Equal(got, want) {
		t.Errorf("the list has %v, want %v", got, want)
	}
	if got := res.Msg.GetOwnContainers(); got != 3 {
		t.Errorf("it left out %d of Dockwell's own, want the service's, the one on its own and the ingress's", got)
	}
	for _, r := range e.Seen()[seen:] {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("looking at the list asked Docker to %s", r)
		}
	}
	if !strings.Contains(res.Msg.String(), "postgres:17") || strings.Contains(res.Msg.String(), "hunter2") {
		t.Errorf("the list reads %v", res.Msg)
	}

	// What a deletion hasn't finished removing is Dockwell's own all the same: it made
	// that container, and it reads as nobody's for no moment of the deletion.
	apply(t, c, propose(t, c, "Delete web", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Delete: true}))
	e.AddContainer(docker.Container{ID: "a7", Name: "/" + docker.ContainerName(project, "web"), Config: docker.Config{Image: "ghcr.io/acme/shop:1.4", Labels: composeLabels(project, "web")}})
	res, err = cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Msg.GetContainers()); !slices.Equal(got, want) {
		t.Errorf("with a deletion under way, the list has %v, want %v", got, want)
	}
}

// A container's logs read as a service's do: the same three methods, the same grant
// and the same audit log. A service's own container isn't read this way, as its logs
// are its service's, with what a deletion put out of reach.
func TestAContainersLogsReadAsAServicesDo(t *testing.T) {
	storetest.Each(t, testAContainersLogsReadAsAServicesDo)
}

func testAContainersLogsReadAsAServicesDo(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)

	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	own := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")

	hand := e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", Config: docker.Config{Image: "nginx:1.27", Env: []string{"API_KEY=hunter2"}}})
	e.Print("by-hand", docker.Stdout, "first line", "second line")

	res, err := lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: "by-hand", Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetService().GetName() != "by-hand" || len(res.Msg.GetSources()) != 1 {
		t.Fatalf("sources = %v", res.Msg)
	}
	src := res.Msg.GetSources()[0]
	if src.GetServerName() != "hz-1" || src.GetRevision() != 0 || src.GetImage() != "nginx:1.27" || src.GetContainerName() != "by-hand" {
		t.Errorf("source = %v", src)
	}

	page, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: hand.ID, Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, chunk := range page.Msg.GetChunks() {
		for _, l := range chunk.GetLines() {
			lines = append(lines, l.GetText())
		}
	}
	if !slices.Equal(lines, []string{"first line", "second line"}) {
		t.Errorf("a page read %q", lines)
	}

	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "by-hand", Server: "hz-1", Tail: 100})
	r.waitLines("first line", "second line")
	r.waitCaughtUp()
	seen := len(e.Seen())
	e.Print("by-hand", docker.Stdout, "third line")
	r.waitLines("first line", "second line", "third line")

	// Reading changed nothing, on a server that stays as it was.
	for _, s := range e.Seen()[seen:] {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("following asked Docker to %s", s)
		}
	}

	// It goes on the record, by the server, naming the container.
	var read *store.Event
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err := tx.ListEvents(t.Context(), 200)
		for i := range events {
			if events[i].Action == "logs.read" && strings.Contains(events[i].Detail, "by-hand") {
				read = &events[i]
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if read == nil || read.Detail != "read the container by-hand on hz-1" || read.Target != res.Msg.GetSources()[0].GetServerId() {
		t.Errorf("the audit log has %+v", read)
	}

	// What Dockwell runs, and what isn't there, say so.
	for name, says := range map[string]string{
		own:      "which Dockwell runs",
		"absent": "there's no container absent on hz-1",
	} {
		_, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: name, Server: "hz-1"}))
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("reading %s: %v, want %q", name, err, says)
		}
	}
	if _, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "by-hand", Server: "hz-1", Project: "default"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a server with a project: %v", err)
	}

	// A token needs the grant logs need.
	secret, _ := logToken(t, b)
	script := logClient(bearer{token: secret, rt: h2cTransport(t, f.api)}, "http://"+f.api)
	_, err = script.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "by-hand", Server: "hz-1"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || reason(err) != authapi.ReasonTokenLogs {
		t.Errorf("a token without the grant: %v", err)
	}
	withGrant, _ := logToken(t, b, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	script = logClient(bearer{token: withGrant, rt: h2cTransport(t, f.api)}, "http://"+f.api)
	if _, err = script.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "by-hand", Server: "hz-1"})); err != nil {
		t.Errorf("a token with the grant: %v", err)
	}
}

// A container that goes while it is followed stays out of the way: it is gone, and
// what follows it hears so.
func TestAFollowEndsWellWhenItsContainerGoes(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	lc := logClient(b, b.origin)
	ctr := e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", Config: docker.Config{Image: "nginx:1.27"}})
	e.Print("by-hand", docker.Stdout, "a line")
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "by-hand", Server: "hz-1", Tail: 100})
	r.waitLines("a line")
	r.waitCaughtUp()
	e.Remove("by-hand")
	id := ctr.ID[:12]
	eventually(t, "the source reading as gone", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.sources[id].GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_GONE
	})
}

// A container made again under its name is what docker compose up -d and docker rm
// then docker run leave. The page goes by the name, so its log has to: what the old
// one wrote stays, a marker says it's gone, and the new one's lines follow.
func TestAFollowGoesOnWhenItsContainerIsMadeAgain(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	lc := logClient(b, b.origin)
	old := e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", Config: docker.Config{Image: "nginx:1.27"}})
	e.Print("by-hand", docker.Stdout, "said by the old one")
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "by-hand", Server: "hz-1", Tail: 100})
	r.waitLines("said by the old one")
	r.waitCaughtUp()

	e.Remove("by-hand")
	again := e.AddContainer(docker.Container{ID: "b2", Name: "/by-hand", Config: docker.Config{Image: "nginx:1.28"}})
	e.Print("by-hand", docker.Stdout, "said by the new one")

	r.waitLines("said by the old one", "said by the new one")
	eventually(t, "both sources as they are now", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.sources[old.ID[:12]].GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_GONE &&
			r.sources[again.ID[:12]].GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_RUNNING
	})
	eventually(t, "a marker where the old one's lines stop", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return slices.ContainsFunc(r.gaps, func(g *dockwellv1.LogGap) bool {
			return g.GetReason() == dockwellv1.LogGapReason_LOG_GAP_REASON_GONE
		})
	})
}

// An agent that isn't there can't say what runs. That reads as what it is, for a
// person to act on, with how to fix it.
func TestAServerThatIsntThereSaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	cc := containerClient(b, b.origin)
	f.addDockerServer(t)
	f.lastDocker.stop()
	_, err := cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{Server: "hz-1"}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("err = %v", err)
	}
	_, err = cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{Server: "nowhere"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("err = %v", err)
	}
}
