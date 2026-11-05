// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// asOlderAgent restarts a server's agent as one from before a command could name a
// value: it says nothing of writing references, and reads its containers as such an
// agent does, without the label that holds their commands' names, so a command reads
// as Docker has it, value and all. An older agent leaves the label where it is, so
// updating it again, which this returns, puts it back for the agent that reads it.
func asOlderAgent(t *testing.T, f *fullServer, s *testServer, e *dockertest.Engine, project string) (update func()) {
	t.Helper()
	s.stop()
	waitConnected(t, f, s.id.ServerID, false)
	kept := map[string]string{}
	for _, name := range e.Containers() {
		if strings.HasPrefix(name, project+"-") {
			e.Edit(name, func(c *docker.Container) {
				if label, ok := c.Config.Labels[container.LabelReferences]; ok {
					kept[name] = label
					delete(c.Config.Labels, container.LabelReferences)
				}
			})
		}
	}
	s.hello = func(h *agentv1.Hello) { h.WritesReferences = false }
	s.connect(t, f)
	return func() {
		t.Helper()
		s.stop()
		waitConnected(t, f, s.id.ServerID, false)
		for name, label := range kept {
			e.Edit(name, func(c *docker.Container) { c.Config.Labels[container.LabelReferences] = label })
		}
		s.hello = nil
		s.connect(t, f)
	}
}

func plainRedis(command ...string) *dockwellv1.Edit {
	return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Name: "redis"}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8", Command: command}},
	}}}}
}

// An agent from before a command could name a value reads such a command with its
// values written in. So nothing on its server that names one is read, planned or
// applied, whatever the spec asked about: drift reports the server as unchecked, and
// says to update it, a plan refuses, and keeping what the server has can't take the
// value in. A resource on it that never named a value plans as ever. Updating the
// agent brings it all back. The review of #113 found this.
func TestAnOlderAgentReadsNothingThatNamesAValue(t *testing.T) {
	storetest.Each(t, testAnOlderAgentReadsNothingThatNamesAValue)
}

func testAnOlderAgentReadsNothingThatNamesAValue(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	hz1 := f.lastDocker
	e.AddLocalImage("redis:8", dockertest.Image{})
	e.AddLocalImage("nginx:1.27", dockertest.Image{})
	c := changeClient(b, b.origin)
	const secret = "pw-alpha-7Hq2mXv9Lr4Kd8Zs"
	plainWeb := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Name: "web"}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}},
	}}}}
	made := apply(t, c, propose(t, c, "Run redis", secretSpec("DB_PASSWORD", secret), secretNaming("redis:8"), plainWeb))
	project := docker.Project(listedEnv(t, b))
	if got := commandOf(e, project, "redis"); !strings.Contains(strings.Join(got, " "), secret) {
		t.Fatalf("Docker runs %q; the test didn't run what it meant to", got)
	}
	update := asOlderAgent(t, f, hz1, e, project)

	ro := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}
	drift, err := changeClient(ro, b.origin).ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	noValueIn(t, "ListDrift, for a read-only token", drift.Msg.String(), secret)
	unchecked := drift.Msg.GetUncheckedServers()
	if len(unchecked) != 1 || unchecked[0].GetReason() != "agent_outdated" ||
		!strings.Contains(unchecked[0].GetMessage(), "until you update the agent there") || !strings.Contains(unchecked[0].GetMessage(), "redis") {
		t.Errorf("the server's drift reads %v", unchecked)
	}
	if len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift on a server that can't be read: %v", drift.Msg.GetDrift())
	}
	adopted, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: hz1.id.ServerID, ResourceId: redisID(t, b)}))
	if err == nil {
		t.Errorf("keeping what the server has was planned: %v", adopted.Msg)
	}

	// Any plan of what names a value there is refused, the one whose spec names
	// nothing, as an undo or an edit would be, included.
	for name, edit := range map[string]*dockwellv1.Edit{
		"names nothing": plainRedis("redis-server"),
		"names a value": secretNaming("redis:8"),
	} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Change redis", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "update the agent") {
			t.Errorf("a plan that %s = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal holds the value: %v", err)
		}
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: made.GetId()})); err == nil || !strings.Contains(err.Error(), "until you update the agent there") {
		t.Errorf("undoing it = %v", err)
	}
	// What never named a value plans on it as ever.
	moved := plainWeb
	moved.Spec.GetService().GetApp().Image = "nginx:1.27"
	moved.Spec.GetService().GetApp().Command = []string{"nginx", "-g", "daemon off;"}
	apply(t, c, propose(t, c, "Change web", moved))
	noValueStored(t, f, secret)

	// Updated, it reads the server again.
	update()
	drift, err = c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(drift.Msg.GetUncheckedServers()) != 0 {
		t.Errorf("once updated, the server reads %v", drift.Msg.GetUncheckedServers())
	}
	noValueIn(t, "ListDrift, once updated", drift.Msg.String(), secret)
}

// What a server runs may be older than a resource's latest revision, and an older
// agent reads that. So a resource whose latest revision names nothing, of which the
// server was given one that did, is held the same way.
func TestAnOlderAgentIsHeldToWhatItsServerWasGiven(t *testing.T) {
	storetest.Each(t, testAnOlderAgentIsHeldToWhatItsServerWasGiven)
}

func testAnOlderAgentIsHeldToWhatItsServerWasGiven(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	hz1 := f.lastDocker
	e.AddLocalImage("redis:8", dockertest.Image{})
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run redis", secretSpec("DB_PASSWORD", "pw-beta-3Fn6pWc1Tj5Yb2Ug"), secretNaming("redis:8")))
	apply(t, c, propose(t, c, "Stop naming it", plainRedis("redis-server")))
	asOlderAgent(t, f, hz1, e, docker.Project(listedEnv(t, b)))
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Change redis", Edits: []*dockwellv1.Edit{plainRedis("redis-server", "--port", "6380")}}))
	if err == nil || !strings.Contains(err.Error(), "until you update the agent there") {
		t.Errorf("a plan on a server once given what names a value = %v", err)
	}
}

func redisID(t *testing.T, b *browser) string {
	t.Helper()
	list, err := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()).ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Msg.GetServices() {
		if s.GetResource().GetName() == "redis" {
			return s.GetId()
		}
	}
	t.Fatal("there's no redis")
	return ""
}
