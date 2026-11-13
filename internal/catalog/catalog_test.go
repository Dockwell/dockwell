// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// What ADR 0029 asks of every entry holds of the ones built in, and each one's
// Compose file goes through what an import takes, with nothing a plan would
// have to warn of.
func TestTheBuiltInEntries(t *testing.T) {
	c, err := catalog.Load("abc123")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range c.All() {
		ids = append(ids, e.GetId())
		t.Run(e.GetId(), func(t *testing.T) {
			if e.GetSource().GetCommit() != "abc123" {
				t.Errorf("its commit = %q, not the build's", e.GetSource().GetCommit())
			}
			if e.GetAppLicense() == "" {
				t.Error("it doesn't say the app's licence")
			}
			given := map[string]string{}
			for _, in := range e.GetInputs() {
				if catalog.Asks(in) && in.GetDefaultValue() == "" {
					given[in.GetName()] = "someone@example.com"
					if catalog.Secret(in) {
						given[in.GetName()] = "typed-by-a-person-xyz"
					}
				}
			}
			filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
			if err != nil {
				t.Fatal(err)
			}
			stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
			if err != nil {
				t.Fatalf("its Compose file: %v", err)
			}
			if len(stack.Exposed) > 0 {
				t.Errorf("its Compose file writes a value in plain sight: %+v", stack.Exposed)
			}
			if w := e.GetWeb().GetService(); w != "" && !slices.ContainsFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == w }) {
				t.Errorf("it opens %s, which it doesn't run", w)
			}
		})
	}
	// The first set: a database with its admin tool, a reverse proxy's
	// replacement, a tunnel, and something that runs AI agents. Then the tools
	// people run beside a database, Coder for development environments, and the
	// apps people self-host most (#221).
	for _, want := range []string{
		"postgres", "mysql", "nginx-proxy-manager", "cloudflared", "t3code",
		"pgadmin", "redis", "mariadb", "coder",
		"uptime-kuma", "n8n", "gitea", "vaultwarden", "nextcloud", "rustfs", "grafana", "umami",
		"plausible", "ghost", "wordpress", "memos", "adminer", "jellyfin", "home-assistant",
		"openclaw", "hermes-agent", "new-api", "lobechat", "sftpgo", "openlist", "gotify", "linkding",
	} {
		if !slices.Contains(ids, want) {
			t.Errorf("the catalog has no %s: %v", want, ids)
		}
	}
}

// Names sort as people read them, whatever their case, so n8n and pgAdmin sit among
// the others rather than after Z.
func TestEntriesSortAsPeopleReadThem(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range c.All() {
		names = append(names, e.GetName())
	}
	if !slices.IsSortedFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) }) {
		t.Errorf("the catalog lists %v", names)
	}
	if i, j := slices.Index(names, "n8n"), slices.Index(names, "Nextcloud"); i < 0 || j < 0 || i > j {
		t.Errorf("n8n comes after Nextcloud, or one is missing: %v", names)
	}
}

// Accents don't move a name past Z, and two of one name keep an order by their IDs.
func TestNamesSortWithAccentsAndIDs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	raw, err := os.ReadFile("snapshot/entries/postgres.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for id, name := range map[string]string{"zeta": "Zeta", "egal": "Égal", "same-b": "Same", "same-a": "Same"} {
		entry := strings.Replace(strings.Replace(string(raw), "id: postgres\n", "id: "+id+"\n", 1), "name: PostgreSQL\n", "name: "+name+"\n", 1)
		if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range c.All() {
		if slices.Contains([]string{"zeta", "egal", "same-b", "same-a"}, e.GetId()) {
			ids = append(ids, e.GetId())
		}
	}
	if want := []string{"egal", "same-a", "same-b", "zeta"}; !slices.Equal(ids, want) {
		t.Errorf("the catalog lists %v, want %v", ids, want)
	}
}

// Each app's licence is one that's had a look: open source, or allowed by name, with
// why. Coder's image is built with its Enterprise code, under Coder's own licence, so
// the catalog's page calls its apps self-hosted rather than open source, and the
// install sheet names the licence. A licence new to these needs a look first.
func TestEveryAppsLicenceHasHadALook(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	open := []string{"AGPL-3.0-only", "AGPL-3.0-or-later", "Apache-2.0", "GPL-2.0-only", "GPL-2.0-or-later", "MIT", "PostgreSQL"}
	allowed := map[string]string{
		// The official image is built from ./enterprise/cmd/coder.
		"coder": "AGPL-3.0-only, with Enterprise code under Coder's own",
		// Source-available, with its .ee files under n8n's Enterprise licence.
		"n8n": "Sustainable Use, with Enterprise code under n8n's own",
		// The unmodified browser-storage image may be self-hosted. Distributing a
		// commercial derivative needs the producer's permission; it isn't Apache-2.0.
		"lobechat": "LobeHub Community License",
	}
	for _, e := range c.All() {
		l := e.GetAppLicense()
		if !slices.Contains(open, l) && allowed[e.GetId()] != l {
			t.Errorf("%s is under %s, which hasn't had a look", e.GetId(), l)
		}
	}
	page, err := os.ReadFile("../../web/src/features/catalog/CatalogPage.tsx")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "Open-source apps") {
		t.Error("the catalog's page says every app is open source, and Coder's image carries code that isn't")
	}
}

// publishedBefore are the first set's entries whose admin tool answers on every
// address. Moving them to loopback would cut off people who use them now, so that
// waits for a migration of its own (#244).
var publishedBefore = []string{"postgres", "mysql", "nginx-proxy-manager"}

// An admin tool on a published port answers over plain HTTP on every address. It says
// how to sign in, so the panel shows the sign-in beside where it answers, and says from
// that port that it's plain HTTP: no entry says it in words.
func TestAnAdminToolOnEveryAddressSaysHowToSignIn(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range publishedBefore {
		e, _ := c.Entry(id)
		if e.GetSignIn().GetPasswordInput() == "" {
			t.Errorf("%s doesn't say how to sign in to its admin tool", id)
		}
	}
}

// Every other app answers only on this server until a person adds a domain: an admin
// tool signs in over plain HTTP, an app with a setup page makes its owner of whoever
// opens it first, and Docker's published ports pass a firewall such as ufw by. So every
// port of every service it runs is on loopback alone.
func TestAnAppAnswersOnlyOnThisServerUntilItHasADomain(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.All() {
		if slices.Contains(publishedBefore, e.GetId()) {
			continue
		}
		t.Run(e.GetId(), func(t *testing.T) {
			stack, _ := read(t, e)
			for _, s := range stack.Services {
				for _, p := range s.Spec.GetApp().GetPorts() {
					if p.GetHostIp() != "127.0.0.1" {
						t.Errorf("%s publishes %d on %q, where anyone who reaches the server could take it first", s.Name, p.GetHostPort(), p.GetHostIp())
					}
				}
			}
			// The panel says it answers only on this server, from the port, and offers
			// the ways in: no entry explains a tunnel.
			if steps := e.GetFirstSteps(); strings.Contains(steps, "ssh ") || strings.Contains(steps, "only on this server") {
				t.Errorf("its first steps explain what the panel shows: %q", steps)
			}
		})
	}
}

// firstVisitor are the apps whose first visitor makes its owner or admin. A domain's
// certificate shows in public logs within minutes, where scanners look for new ones, so
// each says so (first_visitor_owns), and the panel says to make the account before a
// domain makes it public.
var firstVisitor = []string{
	"coder", "uptime-kuma", "n8n", "gitea", "vaultwarden",
	"plausible", "ghost", "wordpress", "memos", "jellyfin", "home-assistant",
	"new-api", "openlist",
}

func TestAFirstVisitorAppSaysItsFirstAccountOwnsIt(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.All() {
		if want := slices.Contains(firstVisitor, e.GetId()); e.GetFirstVisitorOwns() != want {
			t.Errorf("%s says its first visitor owns it: %t, want %t", e.GetId(), e.GetFirstVisitorOwns(), want)
		}
	}
	// Umami starts with an account everyone knows, which is a step of its own.
	if e, _ := c.Entry("umami"); !strings.Contains(e.GetFirstSteps(), "the password umami") {
		t.Errorf("umami doesn't say it starts with the password umami: %q", e.GetFirstSteps())
	}
}

// These apps read their password from the environment on first start alone, so a
// rotation leaves the old one signing in. Each marks the secret set once, and says
// where in the app to change it, which the panel shows beside the password. A
// database's own users aren't among them: the agent changes theirs (ADR 0038).
func TestAPasswordReadOnFirstStartIsSetOnce(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"nginx-proxy-manager", "pgadmin", "nextcloud", "grafana", "gotify", "linkding", "sftpgo"} {
		e, _ := c.Entry(id)
		s := e.GetSignIn()
		i := slices.IndexFunc(e.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == s.GetPasswordInput() })
		if i < 0 || !e.GetInputs()[i].GetSetOnce() || s.GetChangedIn() == "" {
			t.Errorf("%s doesn't mark its password set once, and say where it changes: %v", id, s)
		}
	}
}

// A database's users take a new password as their secret changes (ADR 0038), so an
// app beside one that shares that secret keeps signing in, and no entry tells anyone
// to leave it as it is, or to change it by hand.
func TestADatabasesSharedPasswordFollowsItsSecret(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	databases := regexp.MustCompile(`^(postgres|mysql|mariadb):`)
	// An app that keeps the password it installed with in a file of its own, as
	// Nextcloud does in config.php, wouldn't follow its database: its note stays.
	keepsItsOwn := map[string]bool{"nextcloud": true}
	for _, e := range c.All() {
		stack, filled := read(t, e)
		for name, value := range filled.Stands() {
			for _, s := range stack.Services {
				if !keepsItsOwn[e.GetId()] && databases.MatchString(s.Spec.GetApp().GetImage()) && slices.Contains(slices.Collect(maps.Values(s.Spec.GetApp().GetEnv())), value) &&
					strings.Contains(e.GetFirstSteps(), "Leave the "+name+" secret as it is") {
					t.Errorf("%s says to leave its database's %s as it is, which now changes with it", e.GetId(), name)
				}
			}
		}
		if steps := e.GetFirstSteps(); strings.Contains(steps, "ALTER USER") {
			t.Errorf("%s says to change a database's password by hand: %q", e.GetId(), steps)
		}
	}
}

// An app that encrypts what it keeps with a key of its own can't read it with another,
// and Gitea and Grafana fall back to one everyone knows when they're given none. Each
// gets a key generated from its first release, marked set once.
func TestAnEncryptionKeyIsGeneratedAndLeftAlone(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for id, key := range map[string]struct{ input, env string }{
		"gitea":   {"SECRET_KEY", "GITEA__security__SECRET_KEY"},
		"grafana": {"GRAFANA_SECRET_KEY", "GF_SECURITY_SECRET_KEY"},
	} {
		e, ok := c.Entry(id)
		if !ok {
			t.Fatalf("the catalog has no %s", id)
		}
		i := slices.IndexFunc(e.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == key.input })
		if i < 0 || !catalog.Secret(e.GetInputs()[i]) || e.GetInputs()[i].GetGenerate() < 40 {
			t.Errorf("%s doesn't generate %s, of 40 characters or more", id, key.input)
		}
		stack, filled := read(t, e)
		if got := stack.Services[0].Spec.GetApp().GetEnv()[key.env]; got == "" || got != filled.Stands()[key.input] || len(filled.Secrets[key.input]) < 40 {
			t.Errorf("%s's %s isn't its generated key", id, key.env)
		}
		if !e.GetInputs()[i].GetSetOnce() {
			t.Errorf("%s doesn't mark %s set once", id, key.input)
		}
	}
}

// An address an app builds its links from follows the port it's given, so one that
// moves past a taken port still links to itself, until a person sets their domain.
func TestAnAppsOwnAddressFollowsItsPort(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for id, env := range map[string]string{"coder": "CODER_ACCESS_URL", "ghost": "url", "plausible": "BASE_URL"} {
		e, ok := c.Entry(id)
		if !ok {
			t.Fatalf("the catalog has no %s", id)
		}
		filled, err := catalog.Fill(e, map[string]string{"PORT": "7444"}, catalog.Suggest(e, nil))
		if err != nil {
			t.Fatal(err)
		}
		stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == e.GetWeb().GetService() })
		if got := stack.Services[i].Spec.GetApp().GetEnv()[env]; got != "http://localhost:7444" {
			t.Errorf("%s's %s is %q on port 7444", id, env, got)
		}
	}
	// Ghost takes its address at install too, in place of its port's.
	e, _ := c.Entry("ghost")
	filled, err := catalog.Fill(e, map[string]string{"PORT": "7444", "URL": "https://blog.example.com"}, catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		t.Fatal(err)
	}
	if got := stack.Services[slices.IndexFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == "ghost" })].Spec.GetApp().GetEnv()["url"]; got != "https://blog.example.com" {
		t.Errorf("Ghost's url is %q when it's given https://blog.example.com", got)
	}
}

// Ghost sends an http request to its https address, out of its container, once its url
// is HTTPS, so its health check asks as the proxy in front does, and follows nothing.
func TestGhostStaysHealthyBehindHTTPS(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := c.Entry("ghost")
	stack, _ := read(t, e)
	i := slices.IndexFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == "ghost" })
	test := strings.Join(stack.Services[i].Spec.GetApp().GetHealthcheck().GetTest(), " ")
	if !strings.Contains(test, "'X-Forwarded-Proto': 'https'") || !strings.Contains(test, "redirect: 'manual'") || !strings.Contains(test, "r.status === 200") {
		t.Errorf("Ghost's health check would follow it out: %s", test)
	}
}

// Home Assistant refuses a request through a proxy it doesn't trust, so on its first
// start it writes a configuration.yaml that trusts the ranges Docker gives its networks,
// as Dockwell's proxy reaches it on one, and its own default beside that. It never
// writes over one that's there.
func TestHomeAssistantTrustsDockwellsProxy(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := c.Entry("home-assistant")
	stack, _ := read(t, e)
	app := stack.Services[0].Spec.GetApp()
	config := app.GetEnv()["HA_STARTING_CONFIGURATION"]
	for _, want := range []string{"default_config:", "use_x_forwarded_for: true", "- 172.16.0.0/12", "- 192.168.0.0/16", "automation: !include automations.yaml"} {
		if !strings.Contains(config, want) {
			t.Errorf("its starting configuration.yaml has no %q: %s", want, config)
		}
	}
	if script := strings.Join(app.GetEntrypoint(), " "); !strings.Contains(script, "if [ ! -e configuration.yaml ]") || !strings.HasSuffix(script, "exec /init") {
		t.Errorf("it writes its configuration as %q", script)
	}
}

// Upstream's backend/setup.js writes the first password in the log, which takes no
// passkey to read, so this entry can't let the sheet call it sealed.
func TestNginxProxyManagersPasswordIsMarkedLogged(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := c.Entry("nginx-proxy-manager")
	for _, in := range e.GetInputs() {
		if want := in.GetName() == "ADMIN_PASSWORD"; in.GetLogged() != want {
			t.Errorf("%s has logged %t, want %t", in.GetName(), in.GetLogged(), want)
		}
	}
}

// Whoever reaches T3 Code gets a shell in it, so it's published on loopback alone, its
// setup key is a secret it uses by name, and both of its folders are named volumes, as
// recreating a container loses an anonymous one.
func TestT3CodeStaysOnThisServer(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Entry("t3code")
	if !ok {
		t.Fatal("the catalog has no t3code")
	}
	filled, err := catalog.Fill(e, nil, catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(filled.Secrets["T3_SETUP_KEY"]) != 32 {
		t.Errorf("its setup key isn't generated: %v", filled.Secrets)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		t.Fatal(err)
	}
	app := stack.Services[0].Spec.GetApp()
	if ports := app.GetPorts(); len(ports) != 1 || ports[0].GetHostIp() != "127.0.0.1" {
		t.Errorf("it publishes %v, want loopback alone", ports)
	}
	if got := app.GetShmSize(); got != "1G" {
		t.Errorf("its Chromium has %q of shared memory, where the recipe asks for 1G", got)
	}
	var targets []string
	for _, m := range app.GetMounts() {
		if m.GetVolume() != "" {
			targets = append(targets, m.GetTarget())
		}
	}
	slices.Sort(targets)
	if !slices.Equal(targets, []string{"/home/t3", "/workspace"}) {
		t.Errorf("its named volumes hold %v", targets)
	}
}

// An entry says what it reaches on its server beyond its own containers exactly when
// its Compose file reaches something, as a person reads that before approving it, and
// an entry that says it reaches what it doesn't would teach them to skim it.
func TestHostAccessIsSaid(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.All() {
		t.Run(e.GetId(), func(t *testing.T) {
			stack, _ := read(t, e)
			var reaches []string
			for _, s := range stack.Services {
				for _, a := range container.ServiceAccess(s.Spec.GetApp()) {
					reaches = append(reaches, s.Name+" reaches "+a.Path)
				}
			}
			switch said := e.GetHostAccess() != ""; {
			case len(reaches) > 0 && !said:
				t.Errorf("it doesn't say what it reaches on its server, where %v", reaches)
			case len(reaches) == 0 && said:
				t.Errorf("it says it reaches %q, and its Compose file reaches nothing", e.GetHostAccess())
			}
		})
	}
}

// Whoever owns Coder controls the server, through Docker's socket, which it reaches as
// root in its container. So, as T3 Code is, it answers only on this server until it has
// a domain, which its first steps say, and its access URL follows the port it's given.
// Its database's password is a whole value, which lib/pq reads as PGPASSWORD, and never
// in a URL.
func TestCoderStaysOnThisServer(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Entry("coder")
	if !ok {
		t.Fatal("the catalog has no coder")
	}
	given := map[string]string{"PORT": "7444"}
	filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == "coder" })
	if i < 0 {
		t.Fatal("it runs no coder service")
	}
	app := stack.Services[i].Spec.GetApp()
	if ports := app.GetPorts(); len(ports) != 1 || ports[0].GetHostIp() != "127.0.0.1" || ports[0].GetHostPort() != 7444 {
		t.Errorf("it publishes %v, want port 7444 on loopback alone", ports)
	}
	if got := app.GetEnv()["CODER_ACCESS_URL"]; got != "http://localhost:7444" {
		t.Errorf("its access URL is %q, which doesn't follow its port", got)
	}
	if !slices.ContainsFunc(container.ServiceAccess(app), func(a resource.Access) bool { return a.Kind == resource.AccessDocker }) {
		t.Error("Coder doesn't reach Docker's socket")
	}
	if app.GetUser() != "root" {
		t.Errorf("Coder runs as %q, which can't reach a socket whose group differs on each server", app.GetUser())
	}
	if !strings.Contains(e.GetHostAccess(), "Whoever owns Coder controls this server") {
		t.Errorf("it doesn't say whoever owns it controls the server: %q", e.GetHostAccess())
	}
	if !slices.ContainsFunc(e.GetNotes(), func(n string) bool { return strings.Contains(n, "leaves the workspace containers it started") }) {
		t.Errorf("it doesn't say removing it leaves its workspaces: %q", e.GetNotes())
	}
	if url := app.GetEnv()["CODER_PG_CONNECTION_URL"]; strings.Contains(url, ":@") || strings.Count(url, ":") > 2 {
		t.Errorf("its database's URL holds a password: %q", url)
	}
	if got := app.GetEnv()["PGPASSWORD"]; got == "" || got != filled.Stands()["POSTGRES_PASSWORD"] {
		t.Error("PGPASSWORD isn't the database's password, as a whole value")
	}
}

// pgAdmin reaches this server as host.docker.internal, for a database it runs outside
// any project, and publishes nothing past loopback, and its notes say so.
func TestPgAdminReachesThisServerByName(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Entry("pgadmin")
	if !ok {
		t.Fatal("the catalog has no pgadmin")
	}
	stack, _ := read(t, e)
	if got := stack.Services[0].Spec.GetApp().GetExtraHosts(); !slices.Equal(got, []string{"host.docker.internal:host-gateway"}) {
		t.Errorf("pgAdmin adds the hosts %v", got)
	}
	if !slices.ContainsFunc(e.GetNotes(), func(n string) bool { return strings.Contains(n, "host.docker.internal") }) {
		t.Errorf("it doesn't say it reaches this server as host.docker.internal: %q", e.GetNotes())
	}
}

// Redis is published on loopback alone, as it has no admin tool of its own to stand in
// front of it, and takes its password on its command line by reference, which it reads
// each time it starts and keeps nowhere. Its database page says what reaches it, as a
// container can't reach this server's loopback.
func TestRedisStaysOnThisServer(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Entry("redis")
	if !ok {
		t.Fatal("the catalog has no redis")
	}
	stack, _ := read(t, e)
	app := stack.Services[0].Spec.GetApp()
	if ports := app.GetPorts(); len(ports) != 1 || ports[0].GetHostIp() != "127.0.0.1" {
		t.Errorf("it publishes %v, want loopback alone", ports)
	}
	if !slices.Contains(app.GetReferences(), "command") || !slices.Contains(app.GetCommand(), "${REDIS_PASSWORD}") {
		t.Errorf("its command %v doesn't use the password by reference", app.GetCommand())
	}
	if steps := e.GetFirstSteps(); steps != "" {
		t.Errorf("its first steps say %q, which its database page shows as its connection", steps)
	}
}

// What an entry says in words keeps to the design system's text budget (#426): its
// first steps are one short sentence at most, and the rest is a field the panel shows
// as state, or a note of one line, which Check holds to.
func TestFirstStepsKeepToTheTextBudget(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.All() {
		steps := e.GetFirstSteps()
		if len(steps) > 140 || strings.Count(strings.TrimSuffix(steps, "."), ". ") > 0 {
			t.Errorf("%s's first steps are more than one short sentence: %q", e.GetId(), steps)
		}
		for _, in := range e.GetInputs() {
			if len(in.GetHint()) > 80 {
				t.Errorf("%s's %s has a hint of %d characters, past one line: %q", e.GetId(), in.GetName(), len(in.GetHint()), in.GetHint())
			}
		}
	}
}

// Every entry is tried on real Docker in CI: by scripts/docker-e2e.sh, as the first
// five are, or by scripts/catalog-e2e.sh in one of the Catalog on Docker jobs, which
// installs the version before it, checks it answers, upgrades it and removes it. An
// entry no job tries would reach people untried. Its name stays clear of OnRealDocker,
// which the Go on Docker job runs, as this reads files and needs no Docker.
func TestEveryEntryHasACatalogJob(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Apps []string `yaml:"apps"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	tried := map[string]string{}
	for _, apps := range workflow.Jobs["catalog-docker"].Strategy.Matrix.Apps {
		for _, id := range strings.Fields(apps) {
			if was, twice := tried[id]; twice {
				t.Errorf("%s is in two of the Catalog on Docker jobs, %q and %q", id, was, apps)
			}
			tried[id] = apps
		}
	}
	if len(tried) == 0 {
		t.Fatal("ci.yml has no catalog-docker job with a matrix of apps")
	}
	script, err := os.ReadFile("../../scripts/docker-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	// An install it runs, by its helper or the CLI, and not words in a message.
	for _, m := range regexp.MustCompile(`(?m)^\s*catalog_install ([a-z0-9-]+)|"\$bin" catalog install ([a-z0-9-]+)`).FindAllSubmatch(script, -1) {
		tried[string(m[1])+string(m[2])] = "scripts/docker-e2e.sh"
	}
	checks, err := os.ReadFile("../../scripts/catalog-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)declare -A previous=\((.*?)\n\)`).FindSubmatch(checks)
	if block == nil {
		t.Fatal("scripts/catalog-e2e.sh has no table of the version before each app")
	}
	before := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*\[([a-z0-9-]+)\]="\S+ \S+@sha256:[0-9a-f]{64}`).FindAllSubmatch(block[1], -1) {
		before[string(m[1])] = true
	}
	for _, e := range c.All() {
		at, ok := tried[e.GetId()]
		switch {
		case !ok:
			t.Errorf("no CI job tries %s on real Docker. Add it to a Catalog on Docker job's apps in .github/workflows/ci.yml", e.GetId())
		case at == "scripts/docker-e2e.sh":
		case !strings.Contains(string(checks), "\ncheck_"+e.GetId()+"() {"):
			t.Errorf("scripts/catalog-e2e.sh has no check_%s, which says what answering means for it", e.GetId())
		case !before[e.GetId()]:
			t.Errorf("scripts/catalog-e2e.sh has no version before %s to upgrade from, pinned by digest", e.GetId())
		}
		delete(tried, e.GetId())
	}
	for id := range tried {
		if id != "e2e-notes" {
			t.Errorf("CI tries %s, which the catalog doesn't have", id)
		}
	}
}

// read reads an entry's Compose file as an install would, with what a person types
// made up.
func read(t *testing.T, e *catalogv1.Entry) (*compose.Stack, *catalog.Filled) {
	t.Helper()
	given := map[string]string{}
	for _, in := range e.GetInputs() {
		if catalog.Asks(in) && in.GetDefaultValue() == "" {
			given[in.GetName()] = "someone@example.com"
		}
	}
	filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		t.Fatalf("its Compose file: %v", err)
	}
	return stack, filled
}

// The checks the catalog's CI will make, on what would get past a look.
func TestCheckRefuses(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	for name, tc := range map[string]struct {
		edit func(*catalogv1.Entry)
		want string
	}{
		"no licence": {func(e *catalogv1.Entry) { e.License = "" }, "no license"},
		"no source":  {func(e *catalogv1.Entry) { e.Source = nil }, "no source's repository"},
		"a tag alone": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "@sha256:748a18a54d3842d43bc156f0840d78029b8d349e0feca98c12b9c4ce3404a6b3", "", 1)
		}, "isn't pinned by digest"},
		"no release":    {func(e *catalogv1.Entry) { e.Release = 0 }, "no release"},
		"unused input":  {func(e *catalogv1.Entry) { e.Inputs[0].Name = "NOT_USED" }, "doesn't use the input NOT_USED"},
		"shared secret": {func(e *catalogv1.Entry) { e.Inputs[1].DefaultValue = "hunter2" }, "everyone who installs it would share"},
		"a secret inside a longer value": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      URL: postgres://u:${POSTGRES_PASSWORD}@db/x", 1)
		}, "inside a longer value"},
		"a secret inside a label": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n    labels: {key: \"k-${POSTGRES_PASSWORD}\"}", 1)
		}, "inside a longer value"},
		"a secret as all a config holds": {func(e *catalogv1.Entry) {
			e.Compose += "configs:\n  pass:\n    content: ${POSTGRES_PASSWORD}\n"
		}, "into a file a plan shows"},
		"short secret": {func(e *catalogv1.Entry) { e.Inputs[1].Generate = 8 }, "at least 16"},
		"a secret longer than Dockwell generates": {func(e *catalogv1.Entry) { e.Inputs[1].Generate = 1 << 30 }, "more than the 256 Dockwell generates"},
		"a database it doesn't run":               {func(e *catalogv1.Entry) { e.Databases = []string{"cache"} }, "marks cache as a database"},
		"a sign-in with a password that isn't a secret": {func(e *catalogv1.Entry) {
			e.SignIn.PasswordInput = "ADMIN_PORT"
		}, "isn't one of its secret inputs"},
		"a sign-in with a user and an input for one": {func(e *catalogv1.Entry) {
			e.SignIn.UserInput = "ADMIN_PORT"
		}, "names a user and an input"},
		"where a password changes, for one that isn't set once": {func(e *catalogv1.Entry) {
			e.SignIn.ChangedIn = "Profile"
		}, "isn't set once"},
		"a set-once input that isn't a secret": {func(e *catalogv1.Entry) { e.Inputs[0].SetOnce = true }, "only a secret is"},
		"sign-ups on a service it doesn't run": {func(e *catalogv1.Entry) {
			e.Signups = &catalogv1.Signups{Service: "web", Variable: "SIGNUPS", Closed: "false"}
		}, "needs a service it runs"},
		"an address that names more than its domain": {func(e *catalogv1.Entry) {
			e.AddressVariables = []*catalogv1.AddressVariable{{Service: "adminer", Variable: "URL", Value: "https://{domain}:{port}"}}
		}, "names something other than {domain}"},
		"a note of two lines":             {func(e *catalogv1.Entry) { e.Notes = []string{"One.\nTwo."} }, "isn't one line"},
		"a port with a length":            {func(e *catalogv1.Entry) { e.Inputs[0].MinLength = 4 }, "only text and a secret do"},
		"a port of characters":            {func(e *catalogv1.Entry) { e.Inputs[0].Characters = catalogv1.Characters_CHARACTERS_URL_SAFE }, "only text and a secret do"},
		"a length past what it generates": {func(e *catalogv1.Entry) { e.Inputs[1].MinLength = 40 }, "Generate at least 40"},
		"a length beside a form": {func(e *catalogv1.Entry) {
			e.Inputs[1].MinLength, e.Inputs[1].GenerateAs = 16, catalogv1.GenerateAs_GENERATE_AS_HEX
		}, "generate_as says what it takes already"},
		"base64 in a URL": {func(e *catalogv1.Entry) {
			e.Inputs[1].Characters, e.Inputs[1].GenerateAs = catalogv1.Characters_CHARACTERS_URL_SAFE, catalogv1.GenerateAs_GENERATE_AS_BASE64
		}, "which has + / and ="},
	} {
		t.Run(name, func(t *testing.T) {
			e := proto.CloneOf(base)
			tc.edit(e)
			if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check = %v, want %q", err, tc.want)
			}
		})
	}
}

// A secret stands alone: as an environment value in either form, or anywhere in a word
// of a command, an entrypoint or a health check's test, which name it, as the container
// is made with its value written there (ADR 0022, ADR 0085).
func TestCheckTakesASecretThatStandsAlone(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	for name, compose := range map[string]string{
		"a value":                    "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"a list entry":               "- POSTGRES_PASSWORD=${POSTGRES_PASSWORD}",
		"a word":                     "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n    command: [\"serve\", \"${POSTGRES_PASSWORD}\"]",
		"inside a word of a command": "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n    command: [\"serve\", \"--key=${POSTGRES_PASSWORD}\"]",
		"in a command of one string": "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n    command: serve --requirepass ${POSTGRES_PASSWORD}",
		"inside an entrypoint":       "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n    entrypoint: [\"run\", \"-p${POSTGRES_PASSWORD}\"]",
		// A longer name that starts with this one is another secret, not this one inside it.
		"beside one whose name starts with it": "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      OTHER: x${POSTGRES_PASSWORD_OLD}",
	} {
		e := proto.CloneOf(base)
		e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", compose, 1)
		if err := catalog.Check(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseRefusesAFieldTheFormatHasnt(t *testing.T) {
	if _, err := catalog.Parse([]byte("id: x\nnot_a_field: 1\n")); err == nil || !strings.Contains(err.Error(), "not_a_field") {
		t.Errorf("Parse = %v", err)
	}
}

// A port someone took moves on, and two inputs of one entry never land on one.
func TestSuggestMovesPastWhatsTaken(t *testing.T) {
	e := &catalogv1.Entry{Inputs: []*catalogv1.Input{
		{Name: "A", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8080"},
		{Name: "B", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8080"},
		{Name: "C", DefaultValue: "plain"},
		{Name: "D", Type: catalogv1.InputType_INPUT_TYPE_SECRET},
	}}
	got := catalog.Suggest(e, map[uint32]bool{8080: true, 8082: true})
	if got["A"] != "8081" || got["B"] != "8083" || got["C"] != "plain" {
		t.Errorf("Suggest = %v", got)
	}
	if _, ok := got["D"]; ok {
		t.Error("it suggests a secret")
	}
}

// A key an app decodes is generated as it reads one: hex digits, or random bytes in
// base64, as openssl rand writes them.
func TestASecretIsGeneratedAsItsAppReadsIt(t *testing.T) {
	e := &catalogv1.Entry{Name: "Thing", Inputs: []*catalogv1.Input{
		{Name: "HEX", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 33, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_HEX},
		{Name: "B64", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_BASE64},
	}}
	filled, err := catalog.Fill(e, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := filled.Secrets["HEX"]; len(h) != 33 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Errorf("HEX is %q, want 33 hex digits", h)
	}
	if b, err := base64.StdEncoding.DecodeString(filled.Secrets["B64"]); err != nil || len(b) != 32 {
		t.Errorf("B64 is %q, which decodes to %d bytes, %v; want 32", filled.Secrets["B64"], len(b), err)
	}
	e.Inputs = append(e.Inputs, &catalogv1.Input{Name: "TYPED", Type: catalogv1.InputType_INPUT_TYPE_SECRET, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_HEX})
	e.Compose = "services:\n  a:\n    image: x@sha256:" + strings.Repeat("a", 64) + "\n    environment:\n      HEX: ${HEX}\n      B64: ${B64}\n      TYPED: ${TYPED}\n"
	e.Id, e.Summary, e.Category, e.Version, e.Release, e.License = "thing", "A thing", "ai", "1", 1, "MIT"
	e.Source = &catalogv1.Source{Repository: "https://example.com/thing"}
	if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), "TYPED says how to write what it generates, and generates nothing") {
		t.Errorf("Check = %v", err)
	}
}

// A person's own value for a secret Dockwell generates is what the app takes, as its
// input says: long enough, of the characters a URL carries where it goes into one, and
// in the form a key it decodes is written in. A refusal never quotes the value.
func TestAnOwnValueIsWhatTheAppTakes(t *testing.T) {
	for name, tc := range map[string]struct {
		input      *catalogv1.Input
		ok, refuse string
		want       string
	}{
		"the floor":          {&catalogv1.Input{Generate: 32}, "eight ch", "seven c", "at least 8 characters"},
		"its length":         {&catalogv1.Input{Generate: 64, MinLength: 64}, strings.Repeat("k", 64), strings.Repeat("k", 63), "at least 64 characters"},
		"a URL":              {&catalogv1.Input{Generate: 32, Characters: catalogv1.Characters_CHARACTERS_URL_SAFE}, "Abc-12.3_~xyz", "p@ss/w0rd#1", "letters, digits and - . _ ~ alone"},
		"spaces and symbols": {&catalogv1.Input{Generate: 32}, "correct horse $battery 'staple\"", "tab\there!", "printable characters alone"},
		"hex":                {&catalogv1.Input{Generate: 8, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_HEX}, "0aF19bC2", "0aF19bCz", "8 hex digits"},
		"short hex":          {&catalogv1.Input{Generate: 8, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_HEX}, "deadbeef", "dead", "8 hex digits"},
		"base64":             {&catalogv1.Input{Generate: 4, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_BASE64}, "AAECAw==", "AAEC", "4 bytes written in base64"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.input.Name, tc.input.Label, tc.input.Type = "KEY", "Key", catalogv1.InputType_INPUT_TYPE_SECRET
			e := &catalogv1.Entry{Name: "Thing", Inputs: []*catalogv1.Input{tc.input}}
			filled, err := catalog.Fill(e, map[string]string{"KEY": tc.ok}, nil)
			if err != nil || filled.Secrets["KEY"] != tc.ok {
				t.Errorf("Fill(%q) = %v, %v", tc.ok, filled, err)
			}
			_, err = catalog.Fill(e, map[string]string{"KEY": tc.refuse}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Fill(%q) = %v, want %q", tc.refuse, err, tc.want)
			}
			if strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("a refusal quotes the value: %v", err)
			}
		})
	}
}

// Generated secrets differ each time, stay in the secrets, and aren't in what the
// pin keeps. What a person gives is checked by its type, and a refusal never
// quotes a value.
func TestFill(t *testing.T) {
	e := &catalogv1.Entry{Name: "Thing", Inputs: []*catalogv1.Input{
		{Name: "PORT", Label: "Port", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8081"},
		{Name: "EMAIL", Label: "Your email", Type: catalogv1.InputType_INPUT_TYPE_EMAIL},
		{Name: "TOKEN", Label: "Token", Type: catalogv1.InputType_INPUT_TYPE_SECRET},
		{Name: "KEY", Label: "Key", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32},
		{Name: "NOTE", Label: "Note", Optional: true},
	}}
	given := func(over map[string]string) map[string]string {
		out := map[string]string{"EMAIL": "a@example.com", "TOKEN": "t0ken"}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	one, err := catalog.Fill(e, given(nil), catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	two, err := catalog.Fill(e, given(nil), catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Secrets["KEY"]) != 32 || one.Secrets["KEY"] == two.Secrets["KEY"] {
		t.Errorf("generated %q and %q", one.Secrets["KEY"], two.Secrets["KEY"])
	}
	if _, ok := one.Plain["KEY"]; ok || one.Plain["TOKEN"] != "" {
		t.Errorf("the pin would keep a secret: %v", one.Plain)
	}
	if one.Variables["PORT"] != "8081" || one.Plain["PORT"] != "8081" || one.Secrets["TOKEN"] != "t0ken" {
		t.Errorf("Fill = %+v", one)
	}
	// What the file reads for a secret is a stand-in, which a service then uses by
	// reference, so a typed value can't be mistaken for another that's the same.
	for name, secret := range one.Secrets {
		if stand := one.Variables[name]; len(stand) != 32 || stand == secret || one.Stands()[name] != stand {
			t.Errorf("%s reads as %q in the file, beside the secret %q", name, stand, secret)
		}
	}
	// A person may give their own value for a secret Dockwell would generate, which is
	// kept as it is, and sealed as a generated one is.
	mine, err := catalog.Fill(e, given(map[string]string{"KEY": "my own key"}), catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	if mine.Secrets["KEY"] != "my own key" || mine.Variables["KEY"] == "my own key" || mine.Plain["KEY"] != "" {
		t.Errorf("an own value fills %+v", mine)
	}
	for name, tc := range map[string]struct {
		over map[string]string
		want string
	}{
		"bad port":   {map[string]string{"PORT": "99999"}, "Port takes a port"},
		"not a port": {map[string]string{"PORT": "web"}, "Port takes a port"},
		"bad email":  {map[string]string{"EMAIL": "not an address"}, "Your email takes an email address"},
		"no value":   {map[string]string{"TOKEN": " "}, "Token needs a value"},
		"two lines":  {map[string]string{"NOTE": "a\nb"}, "Note takes one line"},
		"no such":    {map[string]string{"OTHER": "x"}, "takes no input OTHER"},
		"short own":  {map[string]string{"KEY": "mine"}, "Key takes at least 8 characters"},
		"control":    {map[string]string{"KEY": "my\x00own key"}, "Key takes printable characters alone"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := catalog.Fill(e, given(tc.over), catalog.Suggest(e, nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Fill = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "t0ken") {
				t.Errorf("a refusal quotes a value: %v", err)
			}
		})
	}
}

// An upgrade gives what the install was given, a default to what's new, and reads no
// secret: one that's held stands for itself, and only a new one is made.
func TestCarry(t *testing.T) {
	e := &catalogv1.Entry{Name: "Thing", Inputs: []*catalogv1.Input{
		{Name: "PORT", Label: "Port", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8081"},
		{Name: "ADDED", Label: "Added", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "9000"},
		{Name: "KEPT", Label: "Kept", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32},
		{Name: "NEW", Label: "New", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32},
		{Name: "TYPED", Label: "Typed", Type: catalogv1.InputType_INPUT_TYPE_SECRET},
	}}
	pin := &resourcev1.CatalogPin{Values: map[string]string{"PORT": "8090", "GONE": "x"}}
	filled, err := catalog.Carry(e, pin, nil, catalog.Suggest(e, nil), map[string]bool{"KEPT": true, "TYPED": true})
	if err != nil {
		t.Fatal(err)
	}
	if filled.Variables["PORT"] != "8090" || filled.Variables["ADDED"] != "9000" {
		t.Errorf("Carry gives %v", filled.Variables)
	}
	if _, ok := filled.Plain["GONE"]; ok {
		t.Errorf("the pin keeps an input the entry dropped: %v", filled.Plain)
	}
	if _, ok := filled.Secrets["KEPT"]; ok || filled.Held["KEPT"] == "" || filled.Held["TYPED"] == "" {
		t.Errorf("a held secret reads %+v", filled)
	}
	if len(filled.Secrets["NEW"]) != 32 {
		t.Errorf("a new secret reads %q", filled.Secrets["NEW"])
	}
	if got := filled.Stands(); len(got) != 3 || got["NEW"] == "" || got["KEPT"] != filled.Held["KEPT"] {
		t.Errorf("Stands = %v", got)
	}
	if _, err := catalog.Carry(e, pin, nil, catalog.Suggest(e, nil), map[string]bool{"KEPT": true}); err == nil || !strings.Contains(err.Error(), "Typed needs a value") {
		t.Errorf("a secret this release adds with no value = %v", err)
	}
	// What Dockwell generated an upgrade keeps, and a secret the release adds may take
	// a person's own value, as an install's may.
	held := map[string]bool{"KEPT": true, "TYPED": true}
	if _, err := catalog.Carry(e, pin, map[string]string{"KEPT": "a new value"}, catalog.Suggest(e, nil), held); err == nil || !strings.Contains(err.Error(), "an upgrade keeps the KEPT") {
		t.Errorf("a new value for a generated secret it has = %v", err)
	}
	if filled, err := catalog.Carry(e, pin, map[string]string{"NEW": "my own value"}, catalog.Suggest(e, nil), held); err != nil || filled.Secrets["NEW"] != "my own value" {
		t.Errorf("an own value for a secret the release adds = %v, %v", filled, err)
	}
}

// An install's old file reads again from its pin alone: what it kept, and a stand-in
// for a secret, which it never kept.
func TestGivenReadsAPinBack(t *testing.T) {
	variables, stands, err := catalog.Given(&resourcev1.CatalogPin{
		File:   []byte("a: ${PORT}\nb: ${PASSWORD}\nc: ${PASSWORD}\n"),
		Values: map[string]string{"PORT": "8081", "UNUSED": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if variables["PORT"] != "8081" || len(variables) != 2 || len(stands) != 1 {
		t.Errorf("variables %v, stands %v", variables, stands)
	}
	if s := stands["PASSWORD"]; len(s) != 32 || variables["PASSWORD"] != s {
		t.Errorf("the secret's stand-in is %q", s)
	}
}

// A directory of entries takes over from one built in, and adds to the rest.
func TestADirectoryOfEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	built, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := built.Entry("postgres")
	// A release of its own, in the entry's own words.
	raw, err := os.ReadFile("snapshot/entries/postgres.yaml")
	if err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(raw), fmt.Sprintf("release: %d\n", mine.GetRelease()), "release: 9\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "postgres.yaml"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.Entry("postgres")
	if got.GetRelease() != 9 || len(c.All()) != len(built.All()) || got.GetName() != mine.GetName() {
		t.Errorf("Load gives release %d of %d entries", got.GetRelease(), len(c.All()))
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("id: broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load(""); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("a broken entry in the directory = %v", err)
	}
}
