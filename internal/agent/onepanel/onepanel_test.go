// SPDX-License-Identifier: AGPL-3.0-only

package onepanel

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"

	_ "modernc.org/sqlite" // SQLite changes the records a test reads

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/onepanel/onepaneltest"
)

func readOK(t *testing.T, root string) *dockwellv1.OnePanel {
	t.Helper()
	res, err := Read(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if res.GetFound() == nil {
		t.Fatalf("found nothing: %s", res.GetNotHere())
	}
	return res.GetFound()
}

// Everything a plan shows is read, from v2's records and the WAL beside them.
func TestReadsWhat1PanelKeeps(t *testing.T) {
	found := readOK(t, onepaneltest.Make(t, "/opt", false))
	if found.GetVersion() != "v2.4.2" || !found.GetKnownVersion() || found.GetBaseDir() != "/opt" || found.GetRecords() != "/opt/1panel/db/agent.db" {
		t.Fatalf("found %v", found)
	}
	apps := map[string]*dockwellv1.OnePanelApp{}
	for _, a := range found.GetApps() {
		apps[a.GetName()] = a
	}
	if wp := apps["wordpress"]; wp.GetStatus() != "Running" || wp.GetAppKey() != "wordpress" || wp.GetFolder() != "/opt/1panel/apps/wordpress/wordpress" || wp.GetHttpPort() != 40080 {
		t.Errorf("wordpress = %v", wp)
	}
	if mine := apps["mine"]; mine.GetFolder() != "/opt/1panel/apps/local/mine/mine" || mine.GetStore() != "local" {
		t.Errorf("a local app = %v", mine)
	}
	sites := map[string]*dockwellv1.OnePanelWebsite{}
	for _, w := range found.GetWebsites() {
		sites[w.GetAlias()] = w
	}
	blog := sites["blog"]
	if blog.GetFolder() != "/data/www/sites/blog/index/public" || blog.GetRuntime() != "php82" || !blog.GetRewrite() || !blog.GetHttps() ||
		blog.GetCertificate() != "blog.example.com" || len(blog.GetDomains()) != 2 {
		t.Errorf("blog = %v", blog)
	}
	if app := sites["app"]; app.GetProxy() != "http://10.0.0.7:8080" {
		t.Errorf("a proxy's target = %q", app.GetProxy())
	}
	if wp := sites["wp"]; wp.GetApp() != "wordpress" {
		t.Errorf("an app's site = %v", wp)
	}
	if docs := sites["docs"]; docs.GetParent() != "blog" {
		t.Errorf("a subsite = %v", docs)
	}
	if c := found.GetCertificates(); len(c) != 1 || c[0].GetKind() != "autoSSL" || len(c[0].GetDomains()) != 2 || c[0].GetExpires().AsTime().Year() != 2027 {
		t.Errorf("certificates = %v", c)
	}
	if s := found.GetDatabaseServers(); len(s) != 2 || !s[0].GetLocal() || s[0].GetApp() != "mysql" || s[1].GetLocal() {
		t.Errorf("database servers = %v", s)
	}
	if d := found.GetDatabases(); len(d) != 2 || d[0].GetName() != "wp" || d[0].GetUser() != "wp_user" || d[1].GetType() != "postgresql" {
		t.Errorf("databases = %v", d)
	}
	jobs := map[string]*dockwellv1.OnePanelCronJob{}
	for _, j := range found.GetCronJobs() {
		jobs[j.GetName()] = j
	}
	if j := jobs["nightly"]; j.GetScriptLines() != 2 || j.GetContainer() != "mine" || j.GetSchedule() != "0 3 * * *" {
		t.Errorf("a shell job = %v", j)
	}
	if j := jobs["ping"]; j.GetUrlHost() != "hooks.example.com" {
		t.Errorf("a URL job = %v", j)
	}
	if j := jobs["app backup"]; j.GetTarget() != "mysql, wordpress" || j.GetStatus() != "Disable" {
		t.Errorf("an app backup = %v", j)
	}
	if j := jobs["dir backup"]; j.GetTarget() != "/srv/files" {
		t.Errorf("a folder backup = %v", j)
	}
}

// Nothing secret comes back: no password, key, environment, script, command,
// Compose file, or a URL's user, path or query.
func TestNothingSecretComesBack(t *testing.T) {
	found := readOK(t, onepaneltest.Make(t, "/opt", false))
	text := prototext.Format(found)
	for _, s := range onepaneltest.Secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s came back:\n%s", s, text)
		}
	}
}

// 1pctl names where 1Panel is when it's not under /opt, and v1 keeps one database,
// with its sites under OpenResty's own folder.
func TestReadsV1UnderAnotherBase(t *testing.T) {
	found := readOK(t, onepaneltest.Make(t, "/data", true))
	if found.GetVersion() != "v1.10.34-lts" || !found.GetKnownVersion() || found.GetRecords() != "/data/1panel/db/1Panel.db" {
		t.Fatalf("found %v", found)
	}
	for _, w := range found.GetWebsites() {
		if w.GetAlias() == "static" && w.GetFolder() != "/data/1panel/apps/openresty/openresty/www/sites/static/index" {
			t.Errorf("v1's static site = %v", w)
		}
	}
}

// A version this wasn't written from is read the same way, and says so.
func TestAnotherVersionSaysSo(t *testing.T) {
	root := onepaneltest.Make(t, "/opt", false)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "opt", "1panel", "db", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE settings SET value = 'v2.5.0' WHERE key = 'SystemVersion'`); err != nil {
		t.Fatal(err)
	}
	if found := readOK(t, root); found.GetKnownVersion() || found.GetVersion() != "v2.5.0" || len(found.GetApps()) != 4 {
		t.Errorf("found %v", found)
	}
}

// Where 1Panel isn't installed, it says so, and where its records aren't a
// database, that's an error with where.
func TestNo1PanelSaysSo(t *testing.T) {
	res, err := Read(t.Context(), t.TempDir())
	if err != nil || res.GetFound() != nil || !strings.Contains(res.GetNotHere(), "/opt/1panel/db/agent.db") {
		t.Errorf("= %v, %v", res, err)
	}
	root := t.TempDir()
	onepaneltest.WriteFile(t, filepath.Join(root, "opt", "1panel", "db", "agent.db"), "not a database")
	if _, err := Read(t.Context(), root); err == nil || !strings.Contains(err.Error(), "/opt/1panel/db/agent.db") {
		t.Errorf("records that aren't a database: %v", err)
	}
}

func TestOnlyTheOriginOfAProxy(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8080":                     "http://127.0.0.1:8080",
		"https://user:pass@example.com/a?token=x#y": "https://example.com",
		"127.0.0.1:9000":                            "",
		"":                                          "",
	} {
		if got := origin(in); got != want {
			t.Errorf("origin(%q) = %q, want %q", in, got, want)
		}
	}
}
