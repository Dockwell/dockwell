// SPDX-License-Identifier: AGPL-3.0-only

package onepanel

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	// How it was got is 1Panel's provider, its CA its ACME account's, and its
	// issuer its organization.
	if c := found.GetCertificates(); len(c) != 2 || c[0].GetKind() != "http" || c[0].GetProvider() != "letsencrypt" ||
		c[0].GetIssuer() != "Let's Encrypt" || len(c[0].GetDomains()) != 2 || c[0].GetExpires().AsTime().Year() != 2027 ||
		c[1].GetKind() != "selfSigned" || c[1].GetProvider() != "" || c[1].GetIssuer() != "1Panel CA" {
		t.Errorf("certificates = %v", c)
	}
	if s := found.GetDatabaseServers(); len(s) != 2 || !s[0].GetLocal() || s[0].GetApp() != "mysql" || s[1].GetLocal() {
		t.Errorf("database servers = %v", s)
	}
	if d := found.GetDatabases(); len(d) != 2 || d[0].GetName() != "wp" || d[0].GetUser() != "wp_user" || d[1].GetType() != "postgresql" {
		t.Errorf("databases = %v", d)
	}
	if c := found.GetComposes(); len(c) != 1 || c[0].GetName() != "stack" || c[0].GetFile() != "/opt/1panel/docker/compose/stack/docker-compose.yml" {
		t.Errorf("Compose projects = %v", c)
	}
	jobs := map[string]*dockwellv1.OnePanelCronJob{}
	for _, j := range found.GetCronJobs() {
		jobs[j.GetName()] = j
	}
	if j := jobs["nightly"]; j.GetScriptLines() != 2 || j.GetContainer() != "mine" || j.GetSchedule() != "0 3 * * *" ||
		j.GetScriptFrom() != "input" || j.GetInterpreter() != "sh" || j.GetUser() != "www-data" {
		t.Errorf("a shell job = %v", j)
	}
	// One from 1Panel's library runs what's there, and its schedules are a line each.
	if j := jobs["report"]; j.GetScriptFrom() != "library" || j.GetScriptName() != "disk report" || j.GetScriptLines() != 2 ||
		j.GetInterpreter() != "python3" || j.GetSchedule() != "0 8 * * 1\n0 8 * * 4" {
		t.Errorf("a library job = %v", j)
	}
	if j := jobs["rotate"]; j.GetScriptFrom() != "select" || j.GetScriptPath() != "/srv/scripts/rotate.sh" || j.GetScriptLines() != 0 || j.GetInterpreter() != "bash" {
		t.Errorf("a job running a file = %v", j)
	}
	// A backup's targets are IDs in 1Panel, and names here.
	if j := jobs["site backup"]; j.GetTarget() != "blog.example.com, static.example.com" {
		t.Errorf("a website backup = %v", j)
	}
	if j := jobs["db backup"]; j.GetTarget() != "wp" || j.GetDatabaseType() != "mysql" {
		t.Errorf("a database backup = %v", j)
	}
	if j := jobs["every db"]; j.GetTarget() != "all" || j.GetDatabaseType() != "postgresql" {
		t.Errorf("a backup of every database = %v", j)
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
	// One that says no version, or lacks a table every version has, isn't read as
	// if it were empty.
	for _, change := range []string{`UPDATE settings SET value = '' WHERE key = 'SystemVersion'`, `DROP TABLE cronjobs`} {
		if _, err := db.Exec(change); err != nil {
			t.Fatal(err)
		}
		if res, err := Read(t.Context(), root); err == nil || !strings.Contains(err.Error(), "can't tell how to read them") {
			t.Errorf("after %s: %v, %v", change, res, err)
		}
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

// A 1Panel that writes as the agent reads, checkpointing often, never reads as a
// damaged database: a read that fails on a page written under it goes again, and
// one that can't get a still moment says that's why. What does come back is whole.
func TestAReadDuringWritesGoesAgain(t *testing.T) {
	root := onepaneltest.Make(t, "/opt", false)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "opt", "1panel", "db", "agent.db")+
		"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(4)&_pragma=busy_timeout(5000)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS `task_logs` (`id` integer PRIMARY KEY AUTOINCREMENT, `body` text)"); err != nil {
		t.Fatal(err)
	}
	// Enough rows that a read takes a while, as a server's records do.
	for i := range 300 {
		if _, err := db.Exec("INSERT INTO app_installs (name, app_id, app_detail_id, version, env, status, container_name, service_name) VALUES (?, 3, 3, '1', ?, 'Running', ?, 'x')",
			"extra"+strconv.Itoa(i), strings.Repeat("e", 5000), "c"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	want := len(readOK(t, root).GetApps())
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; !stop.Load(); i++ {
			st := []string{"Running", "Stopped"}[i%2]
			_, _ = db.Exec("UPDATE app_installs SET status = ?, env = ? WHERE id % 3 = ?", st, strings.Repeat(st, 1000+i%500), i%3)
			_, _ = db.Exec("INSERT INTO task_logs (body) VALUES (?)", strings.Repeat(strconv.Itoa(i), 3000))
			if i%50 == 0 {
				_, _ = db.Exec("DELETE FROM task_logs WHERE id % 3 = 0")
			}
		}
	}()
	defer func() { stop.Store(true); <-done }()
	ok := 0
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		res, err := Read(t.Context(), root)
		if err != nil {
			if !strings.Contains(err.Error(), "1Panel changed") {
				t.Fatalf("a read during writes failed as if the records were damaged: %v", err)
			}
			continue
		}
		if got := len(res.GetFound().GetApps()); got != want {
			t.Fatalf("a read during writes found %d apps, not %d", got, want)
		}
		ok++
	}
	// A writer this busy may leave no still moment at all: what matters is that no
	// read says the records are damaged, or returns what isn't whole.
	t.Logf("%d reads got through", ok)
}

// A read that fails goes again, even where a stat saw nothing change, as a write
// within the same tick of the clock leaves the same mtime. Only one that fails every
// time with nothing seen moving says what the files hold, and one that sees a
// change every time says so.
func TestAFailedReadGoesAgain(t *testing.T) {
	was := retryAfter
	retryAfter = time.Millisecond
	t.Cleanup(func() { retryAfter = was })
	still := func() [3]fileState { return [3]fileState{{size: 1}} }
	half := errors.New("sqlite_schema's page 1 is of kind 0x0, not a table's")

	n := 0
	res, err := retried(t.Context(), "agent.db", func() (*dockwellv1.OnePanel, bool, error) {
		n++
		if n < 3 {
			return nil, false, half
		}
		return &dockwellv1.OnePanel{Version: "v2.4.2"}, false, nil
	}, still)
	if err != nil || res.GetFound().GetVersion() != "v2.4.2" || n != 3 {
		t.Errorf("after two failed reads = %v, %v, %d reads", res, err, n)
	}

	n = 0
	if _, err := retried(t.Context(), "agent.db", func() (*dockwellv1.OnePanel, bool, error) { n++; return nil, false, half }, still); !errors.Is(err, half) || n != tries {
		t.Errorf("a damaged file = %v after %d reads", err, n)
	}
	if _, err := retried(t.Context(), "agent.db", func() (*dockwellv1.OnePanel, bool, error) { return nil, true, nil }, still); err == nil || !strings.Contains(err.Error(), "1Panel changed") {
		t.Errorf("a file that changes every time = %v", err)
	}
}
