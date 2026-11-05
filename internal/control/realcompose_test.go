// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// shop is a service in a file of the shape real ones have, with extra under it.
func shop(extra string) string {
	return "services:\n  shop:\n    image: ghcr.io/acme/shop:1.4\n" + extra
}

const (
	onEdge      = "    networks: [edge]\n"
	edgeNetwork = "networks:\n  edge:\n    external: true\n"
)

// realDeps are a server with Docker, and clients for it: as a person, and as a
// read-write token.
type realDeps struct {
	c      dockwellv1connect.ChangeServiceClient
	cc     dockwellv1connect.ComposeServiceClient
	script dockwellv1connect.ChangeServiceClient
	scc    dockwellv1connect.ComposeServiceClient
	e      *dockertest.Engine
	f      *fullServer
}

func realServer(t *testing.T, backend store.Backend) realDeps {
	t.Helper()
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	auth := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	return realDeps{
		c:      changeClient(b, b.origin),
		cc:     dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()),
		script: changeClient(auth, b.origin),
		scc:    dockwellv1connect.NewComposeServiceClient(auth, b.origin, connect.WithProtoJSON()),
		e:      e, f: f,
	}
}

func importIn(t *testing.T, c dockwellv1connect.ComposeServiceClient, req *dockwellv1.ImportComposeRequest) *dockwellv1.Change {
	t.Helper()
	res, err := c.ImportCompose(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ImportCompose: %v", err)
	}
	return res.Msg.GetChange()
}

// tokenRefused checks a planned change is one a token can't apply, with its
// fingerprint or without, and that a person is told what it reaches.
func tokenRefused(t *testing.T, script dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change, says string) {
	t.Helper()
	if !ch.GetPlan().GetHostAccess() {
		t.Errorf("it plans no host access, so a token applies it: %v", ch.GetPlan())
		return
	}
	for _, fp := range []string{"", ch.GetPlan().GetFingerprint()} {
		_, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: fp}))
		if reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("a token applying it, with fingerprint %q = %v, want %s", fp, err, authapi.ReasonNeedsSession)
		}
		if err != nil && !strings.Contains(err.Error(), says) {
			t.Errorf("it doesn't say %q: %v", says, err)
		}
	}
}

func discard(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) {
	t.Helper()
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: ch.GetId()})); err != nil {
		t.Fatal(err)
	}
}

// Each thing a real file reaches past Docker with needs a person, through the API,
// with a real token, for a service that is new.
func TestATokenCantRunWhatReachesPastDocker(t *testing.T) {
	storetest.Each(t, testATokenCantRunWhatReachesPastDocker)
}

func testATokenCantRunWhatReachesPastDocker(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, script, scc, e := r.c, r.script, r.scc, r.e
	made := len(e.Containers())
	for name, tc := range map[string]struct {
		file, directory, says string
	}{
		"a network the server has":    {file: shop(onEdge) + edgeNetwork, says: "the network edge, which is on the server already"},
		"by network_mode":             {file: shop("    network_mode: edge\n") + edgeNetwork, says: "the network edge"},
		"by network_mode, unlisted":   {file: shop("    network_mode: edge\n"), says: "the network edge"},
		"as the default":              {file: shop("") + "networks:\n  default: {external: true, name: edge}\n", says: "the network edge"},
		"the server's own":            {file: shop("    network_mode: host\n"), says: "the server's own network"},
		"a capability":                {file: shop("    cap_add: [NET_ADMIN]\n"), says: "the Linux capability NET_ADMIN"},
		"every capability":            {file: shop("    cap_add: [ALL]\n"), says: "the Linux capability ALL, which is the same as root"},
		"a capability like root":      {file: shop("    cap_add: [SYS_ADMIN]\n"), says: "SYS_ADMIN, which is the same as root"},
		"a group":                     {file: shop("    group_add: [999]\n"), says: "the same as root if it is the group that owns Docker's socket"},
		"Docker's group":              {file: shop("    group_add: [docker]\n"), says: "the group docker, which is the same as root"},
		"privileged":                  {file: shop("    privileged: true\n"), says: "privileged mode, which gives it every device and capability on the server, the same as root"},
		"a merge key, privileged":     {file: "x-a: &a\n  privileged: true\nservices:\n  shop:\n    <<: *a\n    image: ghcr.io/acme/shop:1.4\n", says: "privileged mode"},
		"a capability by a variable":  {file: shop("    cap_add: [\"${C:-NET_ADMIN}\"]\n"), says: "NET_ADMIN"},
		"a path relative to the file": {file: shop("    volumes: [\"./data:/data\"]\n"), directory: "/srv/app", says: "the server path /srv/app/data"},
	} {
		t.Run(name, func(t *testing.T) {
			ch := importIn(t, scc, &dockwellv1.ImportComposeRequest{Compose: tc.file, Directory: tc.directory})
			tokenRefused(t, script, ch, tc.says)
			discard(t, c, ch)
		})
	}
	if got := len(e.Containers()); got != made {
		t.Errorf("a token's change made containers: %v", e.Containers())
	}
	// What needs no one's approval, a token applies.
	for name, file := range map[string]string{
		"giving up a capability": shop("    cap_drop: [ALL]\n"),
		"a limit":                shop("    deploy: {resources: {limits: {cpus: \"0.5\", memory: 64M}}}\n"),
		"a name of its own":      shop("    container_name: storefront\n"),
		"build beside image":     shop("    build: .\n"),
	} {
		t.Run(name, func(t *testing.T) {
			ch := importIn(t, scc, &dockwellv1.ImportComposeRequest{Compose: file, RemoveOrphans: true})
			if ch.GetPlan().GetHostAccess() {
				t.Fatalf("it plans host access: %v", ch.GetPlan())
			}
			if got := apply(t, script, ch); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("a token applying it = %v: %s", got.GetState(), got.GetError())
			}
		})
	}
}

// Once a person has approved what a service reaches, a token can deploy it. It
// can't add to it by a change to the service, as a file or an edit, by joining the
// same network as something else, or by a name on that network.
func TestATokenCantWidenWhatAPersonApproved(t *testing.T) {
	storetest.Each(t, testATokenCantWidenWhatAPersonApproved)
}

func testATokenCantWidenWhatAPersonApproved(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, cc, script, scc, e := r.c, r.cc, r.script, r.scc, r.e
	e.AddNetwork(docker.Network{Name: "lan", Driver: "bridge"})
	approved := "    cap_add: [NET_ADMIN]\n    group_add: [\"999\"]\n" + onEdge
	first := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: shop(approved) + edgeNetwork})
	if !first.GetPlan().GetHostAccess() {
		t.Fatalf("the person's import plans no host access: %v", first.GetPlan())
	}
	applyConfirmed(t, c, first)
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	name := service.ContainerName(resource.Ref{Scope: env, Name: "shop"})

	// An image swap keeps what was approved, so a token deploys it.
	tag := importIn(t, scc, &dockwellv1.ImportComposeRequest{Compose: strings.Replace(shop(approved), "shop:1.4", "shop:1.5", 1) + edgeNetwork})
	if tag.GetPlan().GetHostAccess() {
		t.Errorf("a new image of what a person approved plans host access: %v", tag.GetPlan())
	}
	if got := apply(t, script, tag); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a token's new image = %v: %s", got.GetState(), got.GetError())
	}
	// Limits change in place, and aren't access.
	id := e.Container(name).ID
	limit := importIn(t, scc, &dockwellv1.ImportComposeRequest{Compose: strings.Replace(shop(approved), "shop:1.4", "shop:1.5", 1) + "    mem_limit: 512M\n" + edgeNetwork})
	if limit.GetPlan().GetHostAccess() {
		t.Errorf("a limit plans host access: %v", limit.GetPlan())
	}
	if got := apply(t, script, limit); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a token's limit = %v: %s", got.GetState(), got.GetError())
	}
	if now := e.Container(name); now.ID != id || now.HostConfig.Memory != 512<<20 {
		t.Errorf("a limit recreated it, or isn't on it: %s to %s, %d", id, now.ID, now.HostConfig.Memory)
	}

	const image = "    image: ghcr.io/acme/shop:1.5\n    mem_limit: 512M\n"
	file := func(body string) string { return "services:\n  shop:\n" + image + body + edgeNetwork }
	for name, tc := range map[string]struct{ file, says string }{
		"another capability": {file("    cap_add: [NET_ADMIN, SYS_ADMIN]\n    group_add: [\"999\"]\n" + onEdge), "SYS_ADMIN"},
		"another group":      {file("    cap_add: [NET_ADMIN]\n    group_add: [\"999\", \"988\"]\n" + onEdge), "the group 988"},
		"privileged":         {file(approved + "    privileged: true\n"), "privileged mode"},
		"another network": {
			"services:\n  shop:\n" + image + "    cap_add: [NET_ADMIN]\n    group_add: [\"999\"]\n    networks: [edge, lan]\nnetworks:\n  edge: {external: true}\n  lan: {external: true}\n",
			"the network lan",
		},
		"another name on the network": {file("    cap_add: [NET_ADMIN]\n    group_add: [\"999\"]\n    container_name: db\n" + onEdge), "and to db"},
		"the server's own network":    {"services:\n  shop:\n" + image + "    cap_add: [NET_ADMIN]\n    group_add: [\"999\"]\n    network_mode: host\n", "the server's own network"},
	} {
		t.Run(name, func(t *testing.T) {
			ch := importIn(t, scc, &dockwellv1.ImportComposeRequest{Compose: tc.file})
			tokenRefused(t, script, ch, tc.says)
			discard(t, c, ch)
		})
	}

	// The same as an edit of the service, with no file in the way.
	edit := func(mutate func(*resourcev1.Container)) *dockwellv1.Edit {
		app := &resourcev1.Container{
			Image: "ghcr.io/acme/shop:1.5", ExternalNetworks: []string{"edge"}, CapAdd: []string{"NET_ADMIN"}, GroupAdd: []string{"999"},
			Restart: resourcev1.RestartPolicy_RESTART_POLICY_NO, Resources: &resourcev1.Resources{Memory: "512M"},
		}
		mutate(app)
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: env, Name: "shop"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}}},
		}
	}
	for name, mutate := range map[string]func(*resourcev1.Container){
		"an edit that adds a capability": func(a *resourcev1.Container) { a.CapAdd = []string{"ALL", "NET_ADMIN"} },
		"an edit that adds a name":       func(a *resourcev1.Container) { a.Aliases = []string{"db"} },
		"an edit that joins a network":   func(a *resourcev1.Container) { a.ExternalNetworks = []string{"edge", "lan"} },
		"an edit that goes to the server's": func(a *resourcev1.Container) {
			a.ExternalNetworks, a.HostNetwork = nil, true
		},
		"an edit that goes privileged": func(a *resourcev1.Container) { a.Privileged = true },
	} {
		t.Run(name, func(t *testing.T) {
			p, err := script.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: name, Edits: []*dockwellv1.Edit{edit(mutate)}}))
			if err != nil {
				t.Fatal(err)
			}
			tokenRefused(t, script, p.Msg.GetChange(), "")
			discard(t, c, p.Msg.GetChange())
		})
	}

	// A container on its own reaches the same network by its name there, which a
	// token can't do either.
	alone := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "pry"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Container{Container: &resourcev1.Container{Image: "ghcr.io/acme/worker:1.0", Networks: []string{"edge"}}}},
	}
	p, err := script.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Join edge", Edits: []*dockwellv1.Edit{alone}}))
	if err != nil {
		t.Fatal(err)
	}
	tokenRefused(t, script, p.Msg.GetChange(), "the network edge")
	discard(t, c, p.Msg.GetChange())
}

// A name can't shadow what is there: in a file, against what is stored and sealed,
// through an edit, and across projects on a shared network, where only the server
// knows.
func TestANameCantShadowAnother(t *testing.T) { storetest.Each(t, testANameCantShadowAnother) }

func testANameCantShadowAnother(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, cc, scc := r.c, r.cc, r.scc
	two := func(web string) string {
		return "services:\n  db:\n    image: postgres:17\n  web:\n    image: ghcr.io/acme/shop:1.4\n" + web
	}
	for name, file := range map[string]string{
		"its container_name is another's name": two("    container_name: db\n"),
		"in another case":                      two("    container_name: DB\n"),
		"its alias is another's name":          two("    networks: {default: {aliases: [db]}}\n"),
		"both take one name":                   "services:\n  a:\n    image: postgres:17\n    container_name: same\n  b:\n    image: postgres:17\n    container_name: same\n",
		"on a network the server has":          "services:\n  a:\n    image: postgres:17\n    container_name: same\n    networks: [edge]\n  b:\n    image: postgres:17\n    container_name: same\n    networks: [edge]\n" + edgeNetwork,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := scc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: file}))
			if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "would both answer to") {
				t.Errorf("ImportCompose = %v, want it refused", err)
			}
		})
	}
	// On separate networks, each is its own.
	apart := "services:\n  a:\n    image: postgres:17\n    container_name: same\n    networks: [one]\n  b:\n    image: postgres:17\n    container_name: same\n    networks: [two]\nnetworks: {one: {}, two: {}}\n"
	discard(t, c, importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: apart}))

	// What is stored is sealed, and a change that only adds a name still sees it.
	stored := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: "services:\n  db:\n    image: postgres:17\n  web:\n    image: ghcr.io/acme/shop:1.4\n"})
	applyConfirmed(t, c, stored)
	env := stored.GetPlan().GetResources()[0].GetResource().GetScope()
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Shadow db",
		Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: env, Name: "web"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "ghcr.io/acme/shop:1.4", Networks: []string{"default"}, Aliases: []string{"db"},
			}}}}},
		}},
	}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "would both answer to db") {
		t.Errorf("an alias of a stored service's name = %v, want it refused", err)
	}

	// Across projects, on a network they share, the server says.
	shared := "services:\n  " + "app:\n    image: postgres:17\n    networks: [edge]\n" + edgeNetwork
	applyConfirmed(t, c, importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: shared}))
	second := propose(t, c, "A second environment", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "staging"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	})
	if got := apply(t, c, second); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a second environment = %v: %s", got.GetState(), got.GetError())
	}
	staging := func(body string) (*dockwellv1.Change, error) {
		res, err := scc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: body, Environment: "staging"}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}
	for name, file := range map[string]string{
		"a service of another's name":   "services:\n  app:\n    image: postgres:17\n    networks: [edge]\n" + edgeNetwork,
		"an alias of another's name":    "services:\n  other:\n    image: postgres:17\n    container_name: app\n    networks: [edge]\n" + edgeNetwork,
		"an alias of another's in caps": "services:\n  other:\n    image: postgres:17\n    container_name: APP\n    networks: [edge]\n" + edgeNetwork,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := staging(file); err == nil || !strings.Contains(err.Error(), "the network edge") || !strings.Contains(err.Error(), "already does") {
				t.Errorf("ImportCompose = %v, want the server to refuse it", err)
			}
		})
	}
	ch, err := staging("services:\n  other:\n    image: postgres:17\n    networks: [edge]\n" + edgeNetwork)
	if err != nil {
		t.Fatalf("a name no one has on the network: %v", err)
	}
	if !ch.GetPlan().GetHostAccess() {
		t.Errorf("a second project on the network plans no host access: %v", ch.GetPlan())
	}
}

// A network the server has is never made, changed or deleted, by a plan, an undo
// or a deletion, and a file that needs one the server hasn't got is refused by name.
func TestDockwellLeavesTheServersNetworkAlone(t *testing.T) {
	storetest.Each(t, testDockwellLeavesTheServersNetworkAlone)
}

func testDockwellLeavesTheServersNetworkAlone(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, cc, e := r.c, r.cc, r.e
	cleanFile := shop(onEdge) + edgeNetwork
	_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: shop("    networks: [gone]\n") + "networks:\n  gone: {external: true}\n"}))
	if err == nil || !strings.Contains(err.Error(), "the network gone isn't on this server") {
		t.Errorf("a network the server hasn't got = %v", err)
	}
	edge := e.Network("edge").ID
	planned := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: cleanFile})
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "network" {
			t.Errorf("it plans a network resource: %v", r)
		}
	}
	applyConfirmed(t, c, planned)
	env := planned.GetPlan().GetResources()[0].GetResource().GetScope()
	name := service.ContainerName(resource.Ref{Scope: env, Name: "shop"})
	if got := e.Container(name).NetworkSettings.Networks; got["edge"] == nil {
		t.Fatalf("it isn't on the network: %v", got)
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: planned.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, undo.Msg.GetChange())
	if e.Container(name) != nil {
		t.Errorf("the undo left %s", name)
	}
	if n := e.Network("edge"); n == nil || n.ID != edge {
		t.Errorf("the undo took the network: %+v", n)
	}
	for _, r := range e.Seen() {
		if strings.Contains(r, "/networks/create") || strings.HasPrefix(r, "DELETE") && strings.Contains(r, "/networks/") {
			t.Errorf("something asked Docker to %s", r)
		}
	}
}

// What a file builds and what a name becomes, the plan says beside its steps. The
// notes aren't what a fingerprint covers, and they don't stop anyone.
func TestThePlanSaysWhatTheFileAskedFor(t *testing.T) {
	storetest.Each(t, testThePlanSaysWhatTheFileAskedFor)
}

func testThePlanSaysWhatTheFileAskedFor(t *testing.T, backend store.Backend) {
	cc := realServer(t, backend).cc
	ch := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: shop("    build: .\n    container_name: storefront\n    deploy: {resources: {reservations: {cpus: \"0.25\"}}}\n")})
	var reasons []string
	for _, n := range ch.GetPlan().GetNotes() {
		reasons = append(reasons, n.GetReason())
		if n.GetResource().GetKind() != string(service.Kind) || n.GetResource().GetName() != "shop" || n.GetMessage() == "" {
			t.Errorf("a note: %v", n)
		}
	}
	slices.Sort(reasons)
	if want := []string{"container_name", "cpu_reservation", "no_build"}; !slices.Equal(reasons, want) {
		t.Errorf("the plan notes %v, want %v", reasons, want)
	}
}

// An .env's values go where the file puts them, in a spec that's sealed, and no
// further. The new fields sit in the same spec: sealed at rest, and shown in plans,
// as nothing in them is a secret, while the environment beside them stays masked.
func TestAnEnvFileStaysOutOfTheDatabase(t *testing.T) {
	storetest.Each(t, testAnEnvFileStaysOutOfTheDatabase)
}

func testAnEnvFileStaysOutOfTheDatabase(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, cc, e, f := r.c, r.cc, r.e, r.f
	const value = "env-file-7Hq2mXv9Lr4Kd8Zs"
	const marker = "unmistakable-alias"
	file := shop("    environment:\n      API_KEY: ${API_KEY}\n    container_name: "+marker+"\n    cap_drop: [SYS_NICE]\n    mem_limit: 77M\n"+onEdge) + edgeNetwork
	ch := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: file, Env: "API_KEY=\"" + value + "\"\n"})
	if strings.Contains(ch.String(), value) {
		t.Fatal("the plan holds the .env's value")
	}
	shown := map[string]string{}
	for _, r := range ch.GetPlan().GetResources() {
		for _, fc := range r.GetChanges() {
			shown[fc.GetPath()] = fc.GetTo()
			if strings.HasPrefix(fc.GetPath(), "app.env") && (!fc.GetSensitive() || fc.GetTo() != "") {
				t.Errorf("the environment isn't masked in the plan: %v", fc)
			}
		}
	}
	for path, want := range map[string]string{"app.aliases": marker, "app.cap_drop": "SYS_NICE", "app.resources.memory": "77M", "app.external_networks": "edge"} {
		if shown[path] != want {
			t.Errorf("the plan shows %s as %q, want %q: %v", path, shown[path], want, shown)
		}
	}
	applyConfirmed(t, c, ch)
	env := ch.GetPlan().GetResources()[0].GetResource().GetScope()
	got := e.Container(service.ContainerName(resource.Ref{Scope: env, Name: "shop"}))
	if !slices.Contains(got.Config.Env, "API_KEY="+value) {
		t.Fatalf("it doesn't run with the value: %v", got.Config.Env)
	}
	dump := storetest.Dump(t, f.db)
	for table, rows := range dump {
		for _, row := range rows {
			for _, enc := range encodings(value) {
				if strings.Contains(row, enc) {
					t.Errorf("%s holds the .env's value, as %q: %q", table, enc, row)
				}
			}
			// A plan shows what it does, so a stored plan may name these. A stored
			// spec is sealed, these with the rest of it.
			if (table == "revisions" || table == "change_items") && strings.Contains(row, marker) {
				t.Errorf("%s holds a sealed spec's field in the clear: %q", table, row)
			}
		}
	}
	for _, table := range []string{"revisions", "change_items"} {
		if len(dump[table]) == 0 {
			t.Errorf("the dump has no %s; the test reads the wrong database", table)
		}
	}
}

// A value in an .env is a value in the request, so a secret a file's service uses
// is refused the same way whether or not it is the right one: an import can't
// confirm a guess.
func TestAnEnvFileCantConfirmASecret(t *testing.T) {
	storetest.Each(t, testAnEnvFileCantConfirmASecret)
}

func testAnEnvFileCantConfirmASecret(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, cc := r.c, r.cc
	apply(t, c, propose(t, c, "Add a secret", secretSpec("DB_PASSWORD", "correct-horse")))
	file := shop("    labels: {dockwell.uses: \"DB_PASSWORD=secret:DB_PASSWORD@1\"}\n    environment:\n      DB_PASSWORD:\n")
	said := map[string]string{}
	for _, guess := range []string{"correct-horse", "battery-staple"} {
		_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: file, Env: "DB_PASSWORD=" + guess + "\n"}))
		if err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") {
			t.Fatalf("a value for a secret from an .env = %v, want it refused", err)
		}
		if strings.Contains(err.Error(), guess) {
			t.Errorf("the refusal holds the value: %v", err)
		}
		said[guess] = err.Error()
	}
	if said["correct-horse"] != said["battery-staple"] {
		t.Errorf("an import tells the right value from a wrong one: %q, %q", said["correct-horse"], said["battery-staple"])
	}
}

// A variable a file's service uses is the environment's to hold: an .env that gives
// it the value the export wrote changes nothing, and one that gives it another is
// refused as set twice, rather than dropped.
func TestAnEnvFileAndAVariableAServiceUses(t *testing.T) {
	storetest.Each(t, testAnEnvFileAndAVariableAServiceUses)
}

func testAnEnvFileAndAVariableAServiceUses(t *testing.T, backend store.Backend) {
	r := realServer(t, backend)
	c, cc := r.c, r.cc
	apply(t, c, propose(t, c, "Add a variable", variableSpec("LOG_LEVEL", "info")))
	file := shop("    labels: {dockwell.uses: \"LOG_LEVEL=variable:LOG_LEVEL@1\"}\n    environment:\n      LOG_LEVEL:\n")
	discard(t, c, importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: file, Env: "LOG_LEVEL=info\n"}))
	_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: file, Env: "LOG_LEVEL=debug\n"}))
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL is set in the app's environment and from a variable too") {
		t.Errorf("another value for a variable it uses, from an .env = %v, want it refused", err)
	}
}

// A refusal of an .env goes back to the caller, so into a CI's output, and into the
// control plane's log, at info. Neither gets any part of a value.
func TestAnEnvRefusalStaysOutOfTheLog(t *testing.T) {
	storetest.Each(t, testAnEnvRefusalStaysOutOfTheLog)
}

func testAnEnvRefusalStaysOutOfTheLog(t *testing.T, backend store.Backend) {
	logs := &syncBuffer{}
	f := startFullLogging(t, backend, nil, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	b := signedIn(t, f)
	f.addDockerServer(t)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	const tail = "kX9qTailOfThePassword"
	for name, env := range map[string]string{
		"after a $":         "DB_PASSWORD=hunter2$" + tail + "\n",
		"in double quotes":  "DB_PASSWORD=\"hunter2$" + tail + "\"\n",
		"a quote left open": "DB_PASSWORD=\"hunter2" + tail + "\n",
		"a raw line":        "hunter2" + tail + ":x$y\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{
				Compose: shop("    environment:\n      DB_PASSWORD: ${DB_PASSWORD:-x}\n"), Env: env,
			}))
			if reasonOf(err) != "invalid" {
				t.Fatalf("ImportCompose = %v, want it refused", err)
			}
			if strings.Contains(err.Error(), tail) {
				t.Errorf("the refusal holds the value: %v", err)
			}
		})
	}
	if !strings.Contains(logs.String(), "rpc failed") {
		t.Fatal("the log has none of these, so this looks at the wrong log")
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, tail) || strings.Contains(line, "hunter2") {
			t.Errorf("the control plane's log, at info, holds the value: %s", line)
		}
	}
}
