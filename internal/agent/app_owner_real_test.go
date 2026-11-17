// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ownerstep"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/secrets"
)

// The images the catalog snapshot pins (internal/catalog/snapshot/entries).
const (
	giteaImage = "gitea/gitea:28.2.0@sha256:30777c91280d973fa02be48b1ade735bd6859e8f6f7af3b4444d519437852504"
	ghostImage = "ghost:6.72.0@sha256:c0d83a8d45222b3f37a897572000050ab8e6e507d3eee5a35b3ef8345c7293d6"
	mysqlImage = "mysql:8.4.11@sha256:0f51e63e537a1f176423fa03ef559aabe6fa4372a53160a44bc7e75da7d15364"
	n8nImage   = "n8nio/n8n:2.45.5@sha256:9f6d0a5987ab5a297c4b0b38fbf1a22285dd1364551478e93f03d506908fa74c"
	umamiImage = "ghcr.io/umami-software/umami:3.4.1@sha256:e69d1a7932e9dab0fc523edc7be1f3a53f3c261f57856fc2167ae74a8fc022e8"
	pgImage    = "postgres:17.11@sha256:748a18a54d3842d43bc156f0840d78029b8d349e0feca98c12b9c4ce3404a6b3"
	vaultImage = "vaultwarden/server:1.37.5@sha256:4fe17f415f90087377d37c755e0ea3cf189dcc6794e49cdd2a9adbb4937d9e1f"
)

func jsonRequest(port uint32, method, path, body string, headers map[string]string) *resourcev1.OwnerStep {
	return &resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{
		Method: method, Path: path, ContentType: "application/json", Body: body, Headers: headers, Port: port,
	}}}
}

// giteaOwner makes Gitea's owner with a password Gitea makes up and no one keeps,
// then has Gitea make an admin token, which the step captures, sets the owner's own
// password through its API with it, and deletes it, signing in as the owner. So the
// real password is never in any process's arguments (the review's M2), and a second
// try after one that stopped between the user and its password skips making the user
// again, as Gitea refuses a second (the re-check's N6).
func giteaOwner() *resourcev1.OwnerRun {
	return &resourcev1.OwnerRun{
		Service: "web", Email: "owner@example.com", User: "owner", Password: "OWNER_PASSWORD",
		Steps: []*resourcev1.OwnerStep{
			{
				Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{
					User:  "git",
					Words: []string{"sh", "-c", `gitea admin user create --admin --username "$OWNER_USER" --email "$OWNER_EMAIL" --random-password --must-change-password=false`},
				}},
				Unless: &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/api/v1/users/${OWNER_USER}", Port: 3000}},
			},
			{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{
				User:    "git",
				Words:   []string{"sh", "-c", `n="dockwell-$(date +%s%N)"; echo "name=$n"; gitea admin user generate-access-token --username "$OWNER_USER" --token-name "$n" --scopes write:admin --raw`},
				Capture: map[string]string{"TOKEN": `(?m)^([0-9a-f]{40})\s*$`, "TOKEN_NAME": `name=(\S+)`},
			}}},
			jsonRequest(3000, http.MethodPatch, "/api/v1/admin/users/${OWNER_USER}",
				`{"password":"${OWNER_PASSWORD}","login_name":"${OWNER_USER}","source_id":0,"must_change_password":false}`,
				map[string]string{"Authorization": "token ${TOKEN}"}),
			{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{
				Method: http.MethodDelete, Path: "/api/v1/users/${OWNER_USER}/tokens/${TOKEN_NAME}", Port: 3000,
				Headers: map[string]string{"Authorization": "Basic ${DOCKWELL_OWNER_BASIC}"},
			}}},
		},
		Done: &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/api/v1/user", Port: 3000, Headers: map[string]string{"Authorization": "Basic ${DOCKWELL_OWNER_BASIC}"}}},
	}
}

// giteaReset sets a new password for Gitea's owner, as its entry's reset steps do:
// the make's steps after the user, with a token Gitea makes, then deleted signing in
// with the new password (ADR 0105, Rotate).
func giteaReset() *resourcev1.OwnerRun {
	o := giteaOwner()
	o.Steps = o.GetSteps()[1:]
	return o
}

// umamiSignIns signs in to Umami from inside its container with each body it's
// given, each filled from its environment as {{NAME}}, and says yes where the first
// signs in and every other is refused: the owner with their password, then the
// known password for the known user and for the owner's (ADR 0105). It names no
// value, so its words hold none.
const umamiSignIns = `const fill = (s) => s.replace(/\{\{(\w+)\}\}/g, (_, k) => JSON.stringify(process.env[k] || "").slice(1, -1));
const login = (b) => fetch("http://127.0.0.1:3000/api/auth/login", {method: "POST", headers: {"content-type": "application/json"}, body: fill(b)}).then((r) => r.status);
const [own, ...known] = process.argv.slice(1);
Promise.all([login(own), ...known.map(login)]).then(([o, ...k]) => {
  console.log(o + " " + k.join(" "));
  process.exit(o === 200 && k.every((s) => s === 401) ? 0 : 1);
}, (e) => { console.error(e.message); process.exit(1); });`

// umamiOwner makes Umami's owner from the account it starts with, admin with the
// password umami: it signs in with them, then changes the user name and the password
// to the owner's in one request, so no try that stops part-way leaves the known
// password with a name of the owner's. Its done reads the owner signs in, and the
// known password is refused for both names.
func umamiOwner() *resourcev1.OwnerRun {
	login := jsonRequest(3000, http.MethodPost, "/api/auth/login", `{"username":"${KNOWN_USER}","password":"${KNOWN_PASSWORD}"}`, nil)
	login.GetRequest().Capture = map[string]string{"TOKEN": "token", "ADMIN_ID": "user.id"}
	return &resourcev1.OwnerRun{
		Service: "web", Email: "owner@example.com", User: "owner", Password: "OWNER_PASSWORD",
		Steps: []*resourcev1.OwnerStep{
			login,
			jsonRequest(3000, http.MethodPost, "/api/users/${ADMIN_ID}", `{"username":"${OWNER_USER}","password":"${OWNER_PASSWORD}"}`,
				map[string]string{"Authorization": "Bearer ${TOKEN}"}),
		},
		Done: &resourcev1.OwnerCheck{Command: &resourcev1.OwnerCommand{Words: []string{
			"node", "-e", umamiSignIns,
			`{"username":"{{OWNER_USER}}","password":"{{OWNER_PASSWORD}}"}`,
			`{"username":"{{KNOWN_USER}}","password":"{{KNOWN_PASSWORD}}"}`,
			`{"username":"{{OWNER_USER}}","password":"{{KNOWN_PASSWORD}}"}`,
		}}},
	}
}

// vaultSignIn signs in to Vaultwarden's admin page with its admin token, which
// ADMIN_TOKEN holds as an Argon2id PHC, and captures the session's cookie.
func vaultSignIn() *resourcev1.OwnerRequest {
	return &resourcev1.OwnerRequest{
		Method: http.MethodPost, Path: "/admin", ContentType: "application/x-www-form-urlencoded", Body: "token=${ADMIN_TOKEN_PLAIN}", Port: 80,
		Capture: map[string]string{"VW_SESSION": "cookie:VW_ADMIN"},
	}
}

// vaultOwner invites Vaultwarden's owner through its admin API, as its entry does:
// a sign-in with the admin token, then the invite, skipped where the user is there
// already. It's made once done reads the invited user registered (made_when_done),
// as only the person makes their account, from their master password (ADR 0105).
func vaultOwner() *resourcev1.OwnerRun {
	users := func(where string) *resourcev1.OwnerCheck {
		return &resourcev1.OwnerCheck{
			Before:  vaultSignIn(),
			Request: &resourcev1.OwnerRequest{Path: "/admin/users", Port: 80, Headers: map[string]string{"Cookie": "VW_ADMIN=${VW_SESSION}"}},
			Where:   where,
		}
	}
	invite := jsonRequest(80, http.MethodPost, "/admin/invite", `{"email":"${OWNER_EMAIL}"}`, map[string]string{"Cookie": "VW_ADMIN=${VW_SESSION}"})
	invite.Unless = users("[email=${OWNER_EMAIL}].object=user")
	login := &resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Request{Request: vaultSignIn()}}
	return &resourcev1.OwnerRun{
		Service: "web", Email: "owner@example.com", MadeWhenDone: true,
		Steps: []*resourcev1.OwnerStep{login, invite},
		Done:  users("[email=${OWNER_EMAIL}]._status=0"),
	}
}

// ghostScript asks Ghost from inside its container, on its own loopback, which is
// the only address whose X-Forwarded-Proto Ghost believes, as the address its url
// says, so a Ghost reached at an https address answers rather than redirects (the
// review's M4). Its url is what Ghost checks a request against, and follows the
// app's domain as an address variable (ADR 0103), so it holds where the install
// has no domain, as an address given as an input. It reads every value from its
// environment, and the method, path and body from its arguments, which hold none:
// the body names values as {{NAME}}, which it fills from there. It's the one
// Dockwell/catalog's ghost entry holds.
const ghostScript = `const [m, p, b] = process.argv.slice(1);
const body = b ? b.replace(/\{\{(\w+)\}\}/g, (_, k) => JSON.stringify(process.env[k] || "").slice(1, -1)) : "";
const u = new URL(process.env.url);
const r = require("http").request({host: "127.0.0.1", port: 2368, path: p, method: m, headers: {
  "Host": u.host, "X-Forwarded-Proto": u.protocol.slice(0, -1), "Origin": u.origin, "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body)}},
  res => { let out = ""; res.on("data", d => out += d); res.on("end", () => { console.log(res.statusCode + " " + out); process.exit(res.statusCode < 300 ? 0 : 1); }); });
r.on("error", e => { console.error(e.message); process.exit(1); });
r.end(body);`

func ghostStep(method, path, body string) *resourcev1.OwnerCommand {
	return &resourcev1.OwnerCommand{Words: []string{"node", "-e", ghostScript, method, path, body}}
}

// The steps are ones an entry may hold: no ${NAME} in a command's words, and no
// secret there either.
func TestTheOwnersStepsAreOnesAnEntryMayHold(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]*resourcev1.OwnerRun{"gitea": giteaOwner(), "gitea reset": giteaReset(), "ghost": ghostOwner(), "n8n": n8nOwner(), "umami": umamiOwner(), "vaultwarden": vaultOwner()} {
		if err := ownerstep.Check(o.GetSteps()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if err := ownerstep.CheckSecretWords(o.GetSteps(), []string{"OWNER_PASSWORD"}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if err := ownerstep.CheckCheck(o.GetDone()); err != nil {
			t.Errorf("%s's done: %v", name, err)
		}
	}
}

func ghostOwner() *resourcev1.OwnerRun {
	return &resourcev1.OwnerRun{
		Service: "web", Email: "owner@example.com", User: "owner", Password: "OWNER_PASSWORD", Host: "blog.example.com", Https: true,
		Steps: []*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Command{Command: ghostStep(http.MethodPost, "/ghost/api/admin/authentication/setup/",
			`{"setup":[{"name":"Owner","email":"{{OWNER_EMAIL}}","password":"{{OWNER_PASSWORD}}","blogTitle":"Ghost"}]}`)}}},
		Done: &resourcev1.OwnerCheck{Command: ghostStep(http.MethodGet, "/ghost/api/admin/authentication/setup/", ""), Contains: `"status":true`},
	}
}

func n8nOwner() *resourcev1.OwnerRun {
	return &resourcev1.OwnerRun{
		Service: "web", Email: "owner@example.com", User: "owner", Password: "OWNER_PASSWORD",
		Steps: []*resourcev1.OwnerStep{jsonRequest(5678, http.MethodPost, "/rest/owner/setup",
			`{"email":"${OWNER_EMAIL}","firstName":"Owner","lastName":"Owner","password":"${OWNER_PASSWORD}"}`, nil)},
		Done: &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/rest/settings", Port: 5678}, Contains: `"showSetupOnFirstLoad":false`},
	}
}

// run starts a container of Dockwell's for env's service, on network where one is
// named.
func run(t *testing.T, client *docker.Client, env, service, image, network string, cfg docker.Config) string {
	t.Helper()
	project := docker.Project(env)
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	cfg.Image = image
	cfg.Labels = map[string]string{docker.LabelProject: project, docker.LabelService: service, docker.LabelNumber: "1", docker.LabelRevision: "1"}
	req := &docker.CreateRequest{Config: cfg}
	if network != "" {
		req.NetworkingConfig = docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{network: {Aliases: []string{service}}}}
	}
	id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, service), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// answers waits for url to answer below 500, for 5 minutes at most.
func answers(t *testing.T, url string) {
	t.Helper()
	until := time.Now().Add(5 * time.Minute)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode < 500 {
				return
			}
		}
		if time.Now().After(until) {
			t.Fatalf("%s didn't answer: %v", url, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func basicStatus(t *testing.T, url, user, password string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	req.SetBasicAuth(user, password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// newPassword is one Gitea, Ghost and n8n each take: letters and digits, with an
// upper-case letter, a lower-case one and a digit, as GENERATE_AS_PASSWORD makes.
func newPassword() string {
	return "Ow9" + strconv.FormatInt(time.Now().UnixNano(), 36) + "Xq7"
}

// Gitea's, Ghost's and n8n's owners are made on the images the catalog pins, by the
// steps their entries give (ADR 0105, slice 1, and the review's M2, M4 and L1):
//   - Gitea's with a password it makes up, which the step captures, then the owner's
//     own set through its API, so no process's arguments hold it;
//   - Ghost's in production, reached at an https address, on MySQL as its entry runs,
//     from inside its container;
//   - n8n's through the agent's own call, sealed as the control plane sends it.
//
// Each owner then signs in with the password Dockwell generated, done reads it made,
// and a second run, which reads done first, makes nothing again.
func TestAnAppsOwnerIsMadeOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	t.Run("gitea", func(t *testing.T) {
		env := dockertest.Scope(t)
		dockertest.Cleanup(t, client, docker.Project(env))
		run(t, client, env, "web", giteaImage, "", docker.Config{Env: []string{"GITEA__database__DB_TYPE=sqlite3", "GITEA__security__INSTALL_LOCK=true"}})
		target, err := healthyContainer(t.Context(), client, env, "web")
		if err != nil {
			t.Fatal(err)
		}
		addr, _ := target.Address(t.Context(), 3000)
		answers(t, "http://"+addr+"/api/healthz")
		password := newPassword()
		values := map[string]string{"OWNER_EMAIL": "owner@example.com", "OWNER_USER": "owner", "OWNER_PASSWORD": password}
		// A first try that made the user and stopped before its password, then a
		// second, which reads done first and finishes.
		partial := giteaOwner()
		partial.Steps = partial.GetSteps()[:1]
		if res := ownerstep.Run(t.Context(), target, partial, values, false); !res.Made {
			t.Fatalf("making Gitea's user alone: %+v", res)
		}
		if res := ownerstep.Run(t.Context(), target, giteaOwner(), values, true); !res.Made {
			t.Fatalf("making Gitea's owner after a try that stopped: %+v", res)
		}
		if got := basicStatus(t, "http://"+addr+"/api/v1/user", "owner", password); got != http.StatusOK {
			t.Errorf("the owner signs in to Gitea with their own password: %d", got)
		}
		if yes, said := ownerstep.Read(t.Context(), target, giteaOwner(), giteaOwner().GetDone(), values); !yes {
			t.Errorf("done doesn't read Gitea's owner made: %s", said)
		}
		if res := ownerstep.Run(t.Context(), target, giteaOwner(), values, true); !res.Made {
			t.Errorf("a second run, reading done first: %+v", res)
		}
		// A new password, by the reset steps: the new one signs in, done reads it, and
		// the one before is refused.
		fresh := maps.Clone(values)
		fresh["OWNER_PASSWORD"] = newPassword() + "n"
		if res := ownerstep.Run(t.Context(), target, giteaReset(), fresh, false); !res.Made {
			t.Fatalf("setting a new password for Gitea's owner: %+v", res)
		}
		if got := basicStatus(t, "http://"+addr+"/api/v1/user", "owner", fresh["OWNER_PASSWORD"]); got != http.StatusOK {
			t.Errorf("the owner signs in to Gitea with the new password: %d", got)
		}
		if got := basicStatus(t, "http://"+addr+"/api/v1/user", "owner", password); got != http.StatusUnauthorized {
			t.Errorf("the password before still signs in: %d", got)
		}
		if yes, _ := ownerstep.Read(t.Context(), target, giteaReset(), giteaReset().GetDone(), fresh); !yes {
			t.Error("done doesn't read the new password signs in")
		}
		if yes, _ := ownerstep.Read(t.Context(), target, giteaReset(), giteaReset().GetDone(), values); yes {
			t.Error("done reads the password before signs in")
		}
	})
	t.Run("ghost", func(t *testing.T) {
		env := dockertest.Scope(t)
		project := docker.Project(env)
		dockertest.Cleanup(t, client, project)
		network := project + "_default"
		if _, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: network, Labels: map[string]string{docker.LabelProject: project}}); err != nil {
			t.Fatal(err)
		}
		run(t, client, env, "db", mysqlImage, network, docker.Config{Env: []string{
			"MYSQL_ROOT_PASSWORD=the-administrators", "MYSQL_DATABASE=ghost", "MYSQL_USER=ghost", "MYSQL_PASSWORD=the-blogs",
		}})
		// The entry's own settings (internal/catalog/snapshot/entries/ghost.yaml), but
		// url: its default, http://localhost:<port>, is where an install without a
		// domain is reached, and the entry writes https://<domain> once one routes
		// (ADR 0103), which is the case a redirect would break. NODE_ENV is the
		// image's own default, said here so the test can't run in development. Staff
		// device verification is off, as the entry has it while it has no mail to send
		// a new device its code (ADR 0105, "Ghost and mail").
		run(t, client, env, "web", ghostImage, network, docker.Config{Env: []string{
			"NODE_ENV=production", "url=https://blog.example.com", "security__staffDeviceVerification=false", "database__client=mysql", "database__connection__host=db",
			"database__connection__user=ghost", "database__connection__password=the-blogs", "database__connection__database=ghost",
		}})
		target, err := healthyContainer(t.Context(), client, env, "web")
		if err != nil {
			t.Fatal(err)
		}
		// Ghost answers once MySQL is up and its tables are made, from inside.
		site := ghostStep(http.MethodGet, "/ghost/api/admin/site/", "")
		for until := time.Now().Add(6 * time.Minute); ; time.Sleep(3 * time.Second) {
			out, code, err := target.Exec(t.Context(), site.GetWords(), "", nil)
			if err == nil && code == 0 {
				break
			}
			if time.Now().After(until) {
				t.Fatalf("Ghost never answered from inside: %d %v %s", code, err, out)
			}
		}
		password := newPassword()
		values := map[string]string{"OWNER_EMAIL": "owner@example.com", "OWNER_PASSWORD": password}
		if res := ownerstep.Run(t.Context(), target, ghostOwner(), values, false); !res.Made {
			t.Fatalf("making Ghost's owner: %+v", res)
		}
		signIn := ghostStep(http.MethodPost, "/ghost/api/admin/session", `{"username":"{{OWNER_EMAIL}}","password":"{{OWNER_PASSWORD}}"}`)
		// A fresh client, with no cookie of a device Ghost knows, signs in with the
		// password Dockwell generated and gets a session, with no code to wait for.
		out, _, err := target.Exec(t.Context(), signIn.GetWords(), "", []string{"OWNER_EMAIL=owner@example.com", "OWNER_PASSWORD=" + password})
		said := string(out)
		if err != nil || !strings.HasPrefix(said, "201 ") {
			t.Errorf("the owner signs in to Ghost at https://blog.example.com: %v %s", err, said)
		}
		if yes, said := ownerstep.Read(t.Context(), target, ghostOwner(), ghostOwner().GetDone(), values); !yes {
			t.Errorf("done doesn't read Ghost's owner made: %s", said)
		}
		if res := ownerstep.Run(t.Context(), target, ghostOwner(), values, true); !res.Made {
			t.Errorf("a second run, reading done first: %+v", res)
		}
	})
	t.Run("umami", func(t *testing.T) {
		env := dockertest.Scope(t)
		project := docker.Project(env)
		dockertest.Cleanup(t, client, project)
		network := project + "_default"
		if _, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: network, Labels: map[string]string{docker.LabelProject: project}}); err != nil {
			t.Fatal(err)
		}
		run(t, client, env, "db", pgImage, network, docker.Config{Env: []string{"POSTGRES_USER=umami", "POSTGRES_DB=umami", "POSTGRES_PASSWORD=the-sites"}})
		run(t, client, env, "web", umamiImage, network, docker.Config{Env: []string{
			"DATABASE_URL=postgresql://umami:the-sites@db:5432/umami", "APP_SECRET=" + newPassword(),
		}})
		target, err := healthyContainer(t.Context(), client, env, "web")
		if err != nil {
			t.Fatal(err)
		}
		// Umami signs in once its migrations ran: a user it doesn't have is refused.
		probe := []string{"node", "-e", umamiSignIns, `{"username":"nobody","password":"nobody-at-all"}`}
		for until := time.Now().Add(6 * time.Minute); ; time.Sleep(3 * time.Second) {
			out, _, err := target.Exec(t.Context(), probe, "", nil)
			if err == nil && strings.HasPrefix(string(out), "401") {
				break
			}
			if time.Now().After(until) {
				t.Fatalf("Umami never answered a sign-in: %v %s", err, out)
			}
		}
		password := newPassword()
		values := map[string]string{"OWNER_EMAIL": "owner@example.com", "OWNER_USER": "owner", "OWNER_PASSWORD": password, "KNOWN_USER": "admin", "KNOWN_PASSWORD": "umami"}
		if yes, _ := ownerstep.Read(t.Context(), target, umamiOwner(), umamiOwner().GetDone(), values); yes {
			t.Fatal("done reads Umami's owner made before its steps ran")
		}
		if res := ownerstep.Run(t.Context(), target, umamiOwner(), values, false); !res.Made {
			t.Fatalf("making Umami's owner: %+v", res)
		}
		if yes, said := ownerstep.Read(t.Context(), target, umamiOwner(), umamiOwner().GetDone(), values); !yes {
			t.Errorf("done doesn't read Umami's owner made, and admin with umami refused: %s", said)
		}
		if res := ownerstep.Run(t.Context(), target, umamiOwner(), values, true); !res.Made {
			t.Errorf("a second run, reading done first: %+v", res)
		}
	})
	t.Run("vaultwarden", func(t *testing.T) {
		env := dockertest.Scope(t)
		dockertest.Cleanup(t, client, docker.Project(env))
		token := newPassword() + newPassword()
		phc, err := catalog.HashOf(token)
		if err != nil {
			t.Fatal(err)
		}
		run(t, client, env, "web", vaultImage, "", docker.Config{Env: []string{
			"ADMIN_TOKEN=" + phc, "SIGNUPS_ALLOWED=false", "ADMIN_RATELIMIT_SECONDS=60", "ADMIN_RATELIMIT_MAX_BURST=30",
		}})
		target, err := healthyContainer(t.Context(), client, env, "web")
		if err != nil {
			t.Fatal(err)
		}
		addr, _ := target.Address(t.Context(), 80)
		answers(t, "http://"+addr+"/alive")
		// An email in mixed case, which Vaultwarden keeps in lower case (the review's M3).
		values := map[string]string{"OWNER_EMAIL": "Owner@Example.com", "ADMIN_TOKEN_PLAIN": token}
		// Vaultwarden's own check takes the token against the PHC Dockwell wrote, and
		// refuses another.
		wrong := maps.Clone(values)
		wrong["ADMIN_TOKEN_PLAIN"] = "not-the-token"
		if res := ownerstep.Run(t.Context(), target, &resourcev1.OwnerRun{Steps: vaultOwner().GetSteps()[:1]}, wrong, false); res.Made {
			t.Fatal("Vaultwarden took a token that isn't its admin token's")
		}
		res := ownerstep.Run(t.Context(), target, vaultOwner(), values, false)
		if res.Made || !res.Waiting {
			t.Fatalf("inviting Vaultwarden's owner: %+v, want waiting for them to register", res)
		}
		if strings.Contains(res.Said, token) {
			t.Error("what the steps said holds the admin token")
		}
		// A second run skips the invite of a user who's there, and still waits.
		if res := ownerstep.Run(t.Context(), target, vaultOwner(), values, true); res.Made || !res.Waiting {
			t.Fatalf("a second run: %+v, want waiting", res)
		}
		// The owner registers as Bitwarden's web vault does, with what stands for the
		// keys it would make from their master password: invited, with no mail, they
		// take a token without one (Vaultwarden's register_verification_email).
		said, _ := vaultRegister(t, addr)
		if said != http.StatusOK {
			t.Fatalf("the invited owner couldn't register: %d", said)
		}
		if yes, said := ownerstep.Read(t.Context(), target, vaultOwner(), vaultOwner().GetDone(), values); !yes {
			t.Errorf("done doesn't read the owner registered: %s", said)
		}
	})
	t.Run("n8n", func(t *testing.T) {
		env := dockertest.Scope(t)
		dockertest.Cleanup(t, client, docker.Project(env))
		run(t, client, env, "web", n8nImage, "", docker.Config{Env: []string{"N8N_LISTEN_ADDRESS=0.0.0.0", "N8N_PORT=5678", "N8N_SECURE_COOKIE=false"}})
		target, err := healthyContainer(t.Context(), client, env, "web")
		if err != nil {
			t.Fatal(err)
		}
		addr, _ := target.Address(t.Context(), 5678)
		answers(t, "http://"+addr+"/healthz/readiness")
		password := newPassword()
		values := map[string]string{"OWNER_EMAIL": "owner@example.com", "OWNER_PASSWORD": password}
		key, err := delivery.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := secrets.SealValues(key.Public(), "srv_1", resource.AppOwnerRef(env), values)
		if err != nil {
			t.Fatal(err)
		}
		c := &calls{cfg: Config{Docker: client}, delivery: key, server: "srv_1"}
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
		defer cancel()
		got, err := c.makeAppOwner(ctx, &agentv1.MakeAppOwnerCall{Environment: env, Service: "web", Owner: n8nOwner(), Values: sealed})
		if err != nil || !got.GetMade() {
			t.Fatalf("making n8n's owner through the agent's call: %v, %v", got, err)
		}
		body, _ := json.Marshal(map[string]string{"emailOrLdapLoginId": "owner@example.com", "password": password})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/rest/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("the owner signs in to n8n: %d", res.StatusCode)
		}
		again, err := c.makeAppOwner(ctx, &agentv1.MakeAppOwnerCall{Environment: env, Service: "web", Owner: n8nOwner(), Values: sealed, CheckFirst: true})
		if err != nil || !again.GetMade() || strings.Contains(again.String(), password) {
			t.Errorf("a second call, reading done first: %v, %v", again, err)
		}
	})
}

// vaultRegister registers owner@example.com at Vaultwarden as an invited user does:
// a token for the address, which an invite without mail gives at once, then the
// account, with what stands for the keys a web vault makes from a master password.
func vaultRegister(t *testing.T, addr string) (int, string) {
	t.Helper()
	post := func(path, body string) (int, []byte) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	status, said := post("/identity/accounts/register/send-verification-email", `{"email":"owner@example.com","name":"Owner"}`)
	var token string
	if status != http.StatusOK || json.Unmarshal(said, &token) != nil || token == "" {
		t.Fatalf("Vaultwarden gave the invited owner no token: %d %s", status, said)
	}
	body, _ := json.Marshal(map[string]any{
		"email": "owner@example.com", "masterPasswordHash": "ZTJlLW1hc3Rlci1wYXNzd29yZC1oYXNoLXN0YW5kLWlu", "emailVerificationToken": token,
		"kdf": 0, "kdfIterations": 600000,
		"userSymmetricKey": "2.AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	})
	status, said = post("/identity/accounts/register/finish", string(body))
	return status, string(said)
}
