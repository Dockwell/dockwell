// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	agentfiles "github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/agent/onepanel"
	"github.com/dockwell/dockwell/internal/agent/onepanel/onepaneltest"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/store"
)

// addOnePanelServer adds hz-1 with an agent that reads 1Panel's records under
// root, and says of itself what hello makes it say.
func (f *fullServer) addOnePanelServer(t *testing.T, root string, hello func(*agentv1.Hello)) *testServer {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, "hz-1")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		dataDir: t.TempDir(), hello: hello,
		onePanel: func(ctx context.Context) (*agentv1.ReadOnePanelResult, error) {
			return onepanel.Read(ctx, root)
		},
	}
	s.disk = agentfiles.New(s.dataDir, nil, nil)
	s.connect(t, f)
	return s
}

func importClient(b *browser) dockwellv1connect.ImportServiceClient {
	return dockwellv1connect.NewImportServiceClient(b, b.origin, connect.WithProtoJSON())
}

// An import of a server running 1Panel, plan-only as all are for now, says what it would take on, make
// again and leave, with why, and nothing secret.
func TestAPlanOnlyImportOf1Panel(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addOnePanelServer(t, onepaneltest.Make(t, "/opt", false), nil).id.ServerID
	res, err := importClient(b).ImportOnePanel(t.Context(), connect.NewRequest(&dockwellv1.ImportOnePanelRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetChange() != nil {
		t.Errorf("it planned a change: %v", res.Msg.GetChange())
	}
	if v := res.Msg.GetFound().GetVersion(); v != "v2.4.2" {
		t.Fatalf("found version %q", v)
	}
	steps := map[string]*dockwellv1.ImportStep{}
	for _, s := range res.Msg.GetSteps() {
		steps[s.GetKind().String()+" "+s.GetName()] = s
	}
	check := func(key string, action dockwellv1.ImportAction, says ...string) {
		t.Helper()
		s := steps[key]
		if s == nil {
			t.Errorf("no step for %s in %v", key, res.Msg.GetSteps())
			return
		}
		if s.GetAction() != action {
			t.Errorf("%s: %v, want %v", key, s.GetAction(), action)
		}
		text := s.GetSummary() + " | " + s.GetBecomes() + " | " + s.GetReason() + " | " + s.GetWaitsFor().String()
		for _, want := range says {
			if !strings.Contains(text, want) {
				t.Errorf("%s says %q, not %q", key, text, want)
			}
		}
	}
	take, again, leave := dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON, dockwellv1.ImportAction_IMPORT_ACTION_MAKE, dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
	// This server has no Docker for the agent to read, so nothing is planned, and it says so.
	check("IMPORT_KIND_APP wordpress", take, "WordPress 6.7.1 from 1Panel's store", "a Compose project, wordpress", "/opt/1panel/apps/wordpress/wordpress", "can't read what runs on hz-1 now")
	check("IMPORT_KIND_APP mysql", take, "Its data stays where it is")
	check("IMPORT_KIND_APP openresty", leave, "ports 80 and 443", "stop it in 1Panel")
	check("IMPORT_KIND_APP mine", take, "from your own store")
	check("IMPORT_KIND_RUNTIME php82", again, "PHP 8.2 in each website", "IMPORT_WAITS_FOR_WEBSITES")
	check("IMPORT_KIND_RUNTIME api", take, "Node.js 22.11.0", "/srv/api")
	check("IMPORT_KIND_DATABASE_SERVER mysql", take, "part of the project mysql")
	check("IMPORT_KIND_DATABASE_SERVER reports", leave, "on another machine", "IMPORT_WAITS_FOR_DATABASES")
	check("IMPORT_KIND_DATABASE wp", take, "wp_user", "sealed secret", "IMPORT_WAITS_FOR_DATABASES")
	check("IMPORT_KIND_WEBSITE blog.example.com", again, "a PHP 8.2 website on /data/www/sites/blog/index/public", "rewrite rules", "its other domain,", "IMPORT_WAITS_FOR_WEBSITES")
	check("IMPORT_KIND_WEBSITE static.example.com", again, "a website serving /data/www/sites/static/index")
	check("IMPORT_KIND_WEBSITE app.example.com", again, "a website sending to http://10.0.0.7:8080", "stopped in 1Panel")
	check("IMPORT_KIND_WEBSITE wp.example.com", again, "a route to the app wordpress")
	check("IMPORT_KIND_WEBSITE docs.blog.example.com", leave, "no subsites")
	check("IMPORT_KIND_CERTIFICATE blog.example.com", leave, "a certificate from Let's Encrypt, until 2027-01-01 19:04 UTC", "gets its own")
	check("IMPORT_KIND_CERTIFICATE internal.example.com", leave, "a self-signed certificate", "a tunnel or a proxy that ends TLS")
	check("IMPORT_KIND_COMPOSE_PROJECT stack", take, "made on 1Panel's Containers page", "a Compose project, stack")
	check("IMPORT_KIND_CONTAINERS Other containers", leave, "Containers page lists every container Dockwell didn't make")
	check("IMPORT_KIND_CRON_JOB nightly", again, "a sh script of 2 lines, in the container mine, as www-data", "IMPORT_WAITS_FOR_SCHEDULED_TASKS")
	check("IMPORT_KIND_CRON_JOB ping", again, "a call to hooks.example.com")
	check("IMPORT_KIND_CRON_JOB app backup", again, "a backup of the apps mysql, wordpress", "off in 1Panel")
	check("IMPORT_KIND_CRON_JOB site backup", again, "a backup of the websites blog.example.com, static.example.com")
	check("IMPORT_KIND_CRON_JOB db backup", again, "a backup of the MySQL database wp")
	check("IMPORT_KIND_CRON_JOB time", leave, "time service")
	check("IMPORT_KIND_PANEL Firewall", leave, "ufw or firewalld")
	check("IMPORT_KIND_PANEL 1Panel", leave, "1Panel v2.4.2, under /opt")

	text, err := protojson.Marshal(res.Msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range onepaneltest.Secrets {
		if strings.Contains(string(text), s) {
			t.Errorf("%s came back in the answer", s)
		}
	}
}

// A read-write token may ask what an import would do, over REST too, and a
// read-only one may not, as it's a plan.
func TestAPlanOnlyImportOverREST(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addOnePanelServer(t, onepaneltest.Make(t, "/opt", false), nil).id.ServerID
	path := "/v1/servers/" + id + "/imports:onepanel"
	rw := map[string]string{"Authorization": "Bearer " + createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)}
	if status, reason, body := callREST(t, b, http.MethodPost, path, rw, `{}`); status != http.StatusOK || !strings.Contains(body, "IMPORT_ACTION_TAKE_ON") {
		t.Errorf("a read-write token = %d %s %s", status, reason, body)
	}
	ro := map[string]string{"Authorization": "Bearer " + createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)}
	if status, _, body := callREST(t, b, http.MethodPost, path, ro, `{}`); status != http.StatusForbidden {
		t.Errorf("a read-only token = %d %s", status, body)
	}
}

// A server without 1Panel says so, and an agent too old to read it says to update it.
func TestNo1PanelOrAnOldAgentSaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addOnePanelServer(t, t.TempDir(), nil).id.ServerID
	_, err := importClient(b).ImportOnePanel(t.Context(), connect.NewRequest(&dockwellv1.ImportOnePanelRequest{ServerId: id}))
	if reasonOf(err) != "onepanel_not_found" || !strings.Contains(err.Error(), "hz-1 doesn't run 1Panel") || !strings.Contains(err.Error(), "/opt/1panel/db/agent.db") {
		t.Errorf("no 1Panel = %v", err)
	}

	g := startFull(t, store.SQLite, nil)
	c := signedIn(t, g)
	old := g.addOnePanelServer(t, onepaneltest.Make(t, "/opt", false), func(h *agentv1.Hello) { h.ReadsOnepanel = false }).id.ServerID
	_, err = importClient(c).ImportOnePanel(t.Context(), connect.NewRequest(&dockwellv1.ImportOnePanelRequest{ServerId: old}))
	if reasonOf(err) != "agent_outdated" || !strings.Contains(err.Error(), "Update the agent on hz-1") {
		t.Errorf("an old agent = %v", err)
	}
	if _, err := importClient(c).ImportOnePanel(t.Context(), connect.NewRequest(&dockwellv1.ImportOnePanelRequest{ServerId: "srv_nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("no such server = %v", err)
	}
}

// A person importing a server running 1Panel gets one change that takes on what
// runs as it is, with nothing touched. What can't come says why and stays: an app
// whose container holds what a spec can't, and one with no container at all.
func TestImportingA1PanelServerTakesOnWhatRuns(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.onePanelRoot = onepaneltest.Make(t, "/opt", false)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{ID: "1pnet", Name: "1panel-network", Driver: "bridge"})
	inPanel := func(project, service, name string) string {
		return composeContainer(t, e, project, service, name, func(r *docker.CreateRequest) {
			r.HostConfig.NetworkMode = "1panel-network"
			r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{"1panel-network": {Aliases: []string{service, name}}}
			r.HostConfig.Binds = []string{"/opt/1panel/apps/" + project + "/" + project + "/data:/data"}
		})
	}
	mysql := inPanel("mysql", "mysql", "1Panel-mysql-AbCd")
	inPanel("wordpress", "wordpress", "1Panel-wordpress-IjKl")
	e.Reports("1Panel-wordpress-IjKl", "HostConfig", map[string]any{"PublishAllPorts": true})
	inPanel("openresty", "openresty", "1Panel-openresty-EfGh")
	api := inPanel("api", "api", "api")
	id := f.lastDocker.id.ServerID
	seen := len(e.Seen())

	res, err := importClient(b).ImportOnePanel(t.Context(), connect.NewRequest(&dockwellv1.ImportOnePanelRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]*dockwellv1.ImportStep{}
	for _, s := range res.Msg.GetSteps() {
		steps[s.GetKind().String()+" "+s.GetName()] = s
	}
	take, leave := dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON, dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
	for key, want := range map[string]struct {
		action dockwellv1.ImportAction
		says   string
	}{
		"IMPORT_KIND_APP mysql":               {take, "the project mysql, taken on as it runs"},
		"IMPORT_KIND_RUNTIME api":             {take, "the project api, taken on as it runs"},
		"IMPORT_KIND_APP wordpress":           {leave, "publish_all_ports"},
		"IMPORT_KIND_APP mine":                {leave, "Docker runs no container of its Compose project, mine"},
		"IMPORT_KIND_APP openresty":           {leave, "ports 80 and 443"},
		"IMPORT_KIND_DATABASE_SERVER mysql":   {take, "part of the project mysql"},
		"IMPORT_KIND_DATABASE_SERVER reports": {leave, "another machine"},
	} {
		s := steps[key]
		text := s.GetBecomes() + " | " + s.GetReason()
		if s.GetAction() != want.action || !strings.Contains(text, want.says) {
			t.Errorf("%s = %v %q, want %v saying %q", key, s.GetAction(), text, want.action, want.says)
		}
	}
	planned := res.Msg.GetChange()
	if planned.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		t.Fatalf("change = %v", planned)
	}
	took := map[string]bool{}
	for _, server := range planned.GetPlan().GetServers() {
		for _, step := range server.GetSteps() {
			if step.GetAdopts() != "" {
				took[step.GetAdopts()] = true
			}
		}
	}
	if !took[mysql] || !took[api] || len(took) != 2 {
		t.Fatalf("the plan takes on %v, want mysql's and api's", took)
	}
	if !slices.ContainsFunc(planned.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "imports_onepanel" && strings.Contains(n.GetMessage(), "OpenResty stays 1Panel's")
	}) {
		t.Errorf("the plan says nothing of the import: %v", planned.GetPlan().GetNotes())
	}
	if s := planned.String(); strings.Contains(s, secretValue) {
		t.Fatalf("the plan shows a value of an environment: %s", s)
	}
	applyConfirmed(t, changeClient(b, b.origin), planned)
	if got := mutations(e, seen); len(got) != 0 {
		t.Errorf("the import asked Docker to %v", got)
	}

	// A token gets what was found, and no plan, as taking on is a person's.
	rw := map[string]string{"Authorization": "Bearer " + createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)}
	if status, _, body := callREST(t, b, http.MethodPost, "/v1/servers/"+id+"/imports:onepanel", rw, `{}`); status != http.StatusOK || strings.Contains(body, `"change"`) {
		t.Errorf("a token's import = %d %s", status, body)
	}
}
