// SPDX-License-Identifier: AGPL-3.0-only

// Package onepanel reads what 1Panel keeps on a server, from its own records,
// read-only, for an import (ADR 0056). It reads the columns a plan shows and no
// others: never a password, a key, a script, an app's environment, or a URL's path.
package onepanel

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/lazyre"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/sqlitefile"
)

// ctl is 1Panel's own command, which names the folder it's installed under.
const ctl = "/usr/local/bin/1pctl"

// coreTables are the tables every version this was written from has.
var coreTables = []string{"settings", "apps", "app_installs", "runtimes", "websites", "website_domains", "website_ssls", "databases", "cronjobs"}

// tries is how many times a read starts again when 1Panel writes as it reads.
const tries = 5

// known are the versions this was written from: v2.0 to v2.4.2, and v1.10 LTS.
var known = lazyre.Regexp(`^v?(2\.[0-3]\.\d+|2\.4\.[012]|1\.10\.\d+)(-lts)?$`)

// Read reads what 1Panel keeps on the server whose root is root: "/", but for a test.
func Read(ctx context.Context, root string) (*agentv1.ReadOnePanelResult, error) {
	base := baseDir(root)
	dbDir := path.Join(base, "1panel", "db")
	records, v1 := "", false
	for _, name := range []string{"agent.db", "1Panel.db"} {
		if st, err := os.Lstat(filepath.Join(root, dbDir, name)); err == nil && st.Mode().IsRegular() {
			records, v1 = path.Join(dbDir, name), name == "1Panel.db"
			break
		}
	}
	if records == "" {
		return &agentv1.ReadOnePanelResult{NotHere: fmt.Sprintf(
			"there's no %s or %s", path.Join(dbDir, "agent.db"), path.Join(dbDir, "1Panel.db"))}, nil
	}
	at := filepath.Join(root, records)
	return retried(ctx, records,
		func() (*dockwellv1.OnePanel, bool, error) { return read(ctx, root, base, records, v1) },
		func() [3]fileState { return stateOf(at) })
}

// said is a refusal of what the records say, read whole, which reading again
// won't change.
type said struct{ error }

// retryAfter is how long a read waits before it goes again, doubling each time.
var retryAfter = 100 * time.Millisecond

// retried reads with attempt until a read gets through with nothing changed under
// it, up to tries times. A read that fails goes again too: what a stat says can
// miss a write within the same tick of the clock, so a page half written can look
// like a file that held still. Only a read that failed every time, with no change
// seen at all, says what the files hold, as a damaged file does.
func retried(ctx context.Context, records string, attempt func() (*dockwellv1.OnePanel, bool, error), state func() [3]fileState) (*agentv1.ReadOnePanelResult, error) {
	var failed error
	moved := false
	for i := range tries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before := state()
		found, changed, err := attempt()
		if err == nil && !changed {
			return &agentv1.ReadOnePanelResult{Found: found}, nil
		}
		if s := (said{}); errors.As(err, &s) && !changed {
			return nil, s.error
		}
		if changed || errors.Is(err, sqlitefile.ErrWriting) || state() != before {
			moved = true
		}
		if err != nil && !errors.Is(err, sqlitefile.ErrWriting) {
			failed = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryAfter << i):
		}
	}
	if failed != nil && !moved {
		return nil, failed
	}
	return nil, fmt.Errorf("1Panel changed %s each of the %d times the agent read it, so what it read may mix two moments. Try again in a moment", records, tries)
}

// fileState is what a file's stat says of whether it changed.
type fileState struct {
	size, mtime, ctime int64
	ino                uint64
}

// stateOf is the state of a database, its WAL and its journal, so a read that failed can tell
// a file that's damaged from one written as it read.
func stateOf(db string) [3]fileState {
	var out [3]fileState
	for i, p := range []string{db, db + "-wal", db + "-journal"} {
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil {
			out[i] = fileState{size: st.Size, mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano(), ino: st.Ino}
		}
	}
	return out
}

// baseDir is the folder 1Panel is installed under: what 1pctl says, or /opt.
func baseDir(root string) string {
	f, err := os.Open(filepath.Join(root, ctl)) //nolint:gosec // 1Panel's own command, at its fixed path
	if err != nil {
		return "/opt"
	}
	defer f.Close()
	lines := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for lines.Scan() {
		v, ok := strings.CutPrefix(strings.TrimSpace(lines.Text()), "BASE_DIR=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if path.IsAbs(v) && path.Clean(v) == v && !strings.Contains(v, "\x00") {
			return v
		}
	}
	return "/opt"
}

// read reads the records once, and says whether they changed underneath it.
func read(ctx context.Context, root, base, records string, v1 bool) (*dockwellv1.OnePanel, bool, error) {
	db, err := sqlitefile.Open(filepath.Join(root, records))
	if errors.Is(err, sqlitefile.ErrNotSQLite) {
		return nil, false, said{fmt.Errorf("the agent can't read 1Panel's records, %s: %w", records, err)}
	}
	if err != nil {
		return nil, false, fmt.Errorf("the agent can't read 1Panel's records, %s: %w", records, err)
	}
	defer db.Close()
	r := &reader{ctx: ctx, db: db, base: base, root: root}
	found := &dockwellv1.OnePanel{BaseDir: base, Records: records}
	// What every version has, a version this doesn't know has too, or it isn't
	// read: a table missing reads as empty otherwise, and says nothing of why.
	for _, table := range coreTables {
		if _, ok := db.Table(table); !ok {
			return nil, false, said{fmt.Errorf("1Panel's records, %s, have no %s table, which every version this was written from has, so the agent can't tell how to read them. Update 1Panel, then try again", records, table)}
		}
	}
	settings := r.settings()
	found.Version = settings["SystemVersion"]
	if found.Version == "" {
		return nil, false, said{fmt.Errorf("1Panel's records, %s, say no version, so the agent can't tell how to read them. Update 1Panel, then try again", records)}
	}
	found.KnownVersion = known().MatchString(found.Version)
	if err := r.all(found, settings, v1); err != nil {
		return nil, db.Changed(), fmt.Errorf("the agent can't read 1Panel's records, %s: %w", records, err)
	}
	return found, db.Changed(), nil
}

type reader struct {
	ctx  context.Context //nolint:containedctx // it lives as long as one read
	db   *sqlitefile.DB
	base string
	// root is where the server's file system is, "/" but in tests.
	root string
}

// rows calls each with each row of table, or with none where there's no such table.
func (r *reader) rows(table string, each func(sqlitefile.Row)) error {
	t, ok := r.db.Table(table)
	if !ok {
		return nil
	}
	n := 0
	return r.db.Scan(t, func(row sqlitefile.Row) error {
		n++
		if n%1024 == 0 {
			if err := r.ctx.Err(); err != nil {
				return err
			}
		}
		each(row)
		return nil
	})
}

func (r *reader) settings() map[string]string {
	out := map[string]string{}
	_ = r.rows("settings", func(row sqlitefile.Row) {
		switch k := row.Text("key"); k {
		case "SystemVersion", "WEBSITE_DIR":
			out[k] = row.Text("value")
		}
	})
	return out
}

func (r *reader) all(found *dockwellv1.OnePanel, settings map[string]string, v1 bool) error {
	type app struct{ key, name, store string }
	apps := map[int64]app{}
	if err := r.rows("apps", func(row sqlitefile.Row) {
		apps[row.Int("id")] = app{row.Text("key"), row.Text("name"), row.Text("resource")}
	}); err != nil {
		return err
	}
	installs := map[int64]string{}
	openresty := ""
	if err := r.rows("app_installs", func(row sqlitefile.Row) {
		a := apps[row.Int("app_id")]
		name := row.Text("name")
		installs[row.Int("id")] = name
		if a.key == "openresty" {
			openresty = name
		}
		found.Apps = append(found.Apps, &dockwellv1.OnePanelApp{
			Name: name, AppKey: a.key, AppName: a.name, Version: row.Text("version"), Status: row.Text("status"),
			Store: a.store, Folder: r.appFolder(a.key, a.store, name), Container: row.Text("container_name"),
			HttpPort: port(row.Int("http_port")), HttpsPort: port(row.Int("https_port")),
		})
	}); err != nil {
		return err
	}
	runtimes := map[int64]string{}
	if err := r.rows("runtimes", func(row sqlitefile.Row) {
		name, kind := row.Text("name"), row.Text("type")
		runtimes[row.Int("id")] = name
		found.Runtimes = append(found.Runtimes, &dockwellv1.OnePanelRuntime{
			Name: name, Type: kind, Version: row.Text("version"), Status: row.Text("status"), Image: row.Text("image"),
			Folder: path.Join(r.base, "1panel", "runtime", kind, name), CodeFolder: row.Text("code_dir"),
			Container: row.Text("container_name"),
		})
	}); err != nil {
		return err
	}
	// 1Panel keeps how a certificate was got in provider, and its issuer in type
	// (the common name) and organization. The CA is its ACME account's.
	cas := map[int64]string{}
	if err := r.rows("website_acme_accounts", func(row sqlitefile.Row) {
		cas[row.Int("id")] = row.Text("type")
	}); err != nil {
		return err
	}
	certs := map[int64]string{}
	if err := r.rows("website_ssls", func(row sqlitefile.Row) {
		primary, kind := row.Text("primary_domain"), row.Text("provider")
		certs[row.Int("id")] = primary
		c := &dockwellv1.OnePanelCertificate{
			PrimaryDomain: primary, Domains: list(row.Text("domains")), Kind: kind,
			Issuer: cmp.Or(row.Text("organization"), row.Text("type")), Expires: when(row.Text("expire_date")), AutoRenew: row.Bool("auto_renew"),
		}
		switch kind {
		case "http", "dnsAccount", "dnsManual":
			c.Provider = cas[row.Int("acme_account_id")]
		}
		found.Certificates = append(found.Certificates, c)
	}); err != nil {
		return err
	}
	if err := r.websites(found, settings, v1, openresty, installs, runtimes, certs); err != nil {
		return err
	}
	if err := r.databases(found, installs); err != nil {
		return err
	}
	if err := r.rows("composes", func(row sqlitefile.Row) {
		found.Composes = append(found.Composes, &dockwellv1.OnePanelCompose{Name: row.Text("name"), File: row.Text("path")})
	}); err != nil {
		return err
	}
	return r.cronJobs(found, installs, v1)
}

// appFolder is where an install of the app keyed key keeps its Compose file.
func (r *reader) appFolder(key, store, name string) string {
	if store == "local" {
		return path.Join(r.base, "1panel", "apps", "local", strings.TrimPrefix(key, "local"), name)
	}
	return path.Join(r.base, "1panel", "apps", key, name)
}

func (r *reader) websites(found *dockwellv1.OnePanel, settings map[string]string, v1 bool, openresty string,
	installs, runtimes, certs map[int64]string,
) error {
	sites := settings["WEBSITE_DIR"]
	switch {
	case v1 && openresty != "":
		sites = path.Join(r.base, "1panel", "apps", "openresty", openresty, "www")
	case sites == "":
		sites = path.Join(r.base, "1panel", "www")
	}
	domains := map[int64][]*dockwellv1.OnePanelDomain{}
	if err := r.rows("website_domains", func(row sqlitefile.Row) {
		id := row.Int("website_id")
		domains[id] = append(domains[id], &dockwellv1.OnePanelDomain{
			Domain: row.Text("domain"), Port: port(row.Int("port")), Https: row.Bool("ssl"),
		})
	}); err != nil {
		return err
	}
	aliases := map[int64]string{}
	parents := map[*dockwellv1.OnePanelWebsite]int64{}
	if err := r.rows("websites", func(row sqlitefile.Row) {
		alias := row.Text("alias")
		aliases[row.Int("id")] = alias
		run := row.Text("site_dir")
		if run == "" {
			run = "/"
		}
		w := &dockwellv1.OnePanelWebsite{
			Alias: alias, PrimaryDomain: row.Text("primary_domain"), Type: row.Text("type"), Domains: domains[row.Int("id")],
			Folder: path.Join(sites, "sites", alias, "index", path.Clean("/"+run)), Proxy: origin(row.Text("proxy")),
			Runtime: runtimes[row.Int("runtime_id")], App: installs[row.Int("app_install_id")],
			Certificate: certs[row.Int("website_ssl_id")], Rewrite: strings.TrimSpace(row.Text("rewrite")) != "",
			Https: strings.EqualFold(row.Text("protocol"), "HTTPS"), Status: row.Text("status"),
		}
		parents[w] = row.Int("parent_website_id")
		w.Redirects = r.redirects(path.Join(sites, "sites", alias, "redirect"))
		found.Websites = append(found.Websites, w)
	}); err != nil {
		return err
	}
	for w, id := range parents {
		w.Parent = aliases[id]
	}
	return nil
}

// Bounds on reading a site's redirects: how many files, and how much of each.
const (
	maxRedirects    = 100
	maxRedirectFile = 64 << 10
)

var (
	// domainRedirect is 1Panel's redirect of a domain: an if on the host, which
	// returns a status and where to.
	domainRedirect = lazyre.Regexp(`if\s*\(\$host\s*~\s*'\^([^']+)'\)\s*\{\s*return\s+(30[12])\s+([^;\s]+)\s*;`)
	// pathRedirect is 1Panel's redirect of a path: a rewrite of everything under it.
	pathRedirect = lazyre.Regexp(`rewrite\s+\^(\S*?)\(\.\*\)\s+(\S+)\s+(permanent|redirect)\s*;`)
	// hostName is a domain, or a wildcard of one, as a redirect's host reads once
	// 1Panel's pattern is undone.
	hostName = lazyre.Regexp(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// hostOf undoes the pattern 1Panel matches a redirect's host with, as its own
// parseRedirectHostPattern does: '^www.example.com' up to 2.2.0, and from 2.2.5
// '^www\.example\.com$', with '[^.]+' for a wildcard's label. It's empty for a
// pattern that doesn't read back as a domain.
func hostOf(pattern string) string {
	host := strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	host = strings.ReplaceAll(host, `[^.]+`, "*")
	host = strings.ToLower(strings.ReplaceAll(host, `\.`, "."))
	if !hostName().MatchString(host) {
		return ""
	}
	return host
}

// redirects reads a site's redirects as 1Panel writes them (ADR 0070): each a file in
// its redirect folder, name.conf while it's on and name.bak once it's off, holding one
// of three shapes 1Panel writes. A file in another shape reads as one with no type, and
// a folder that isn't there as none.
func (r *reader) redirects(dir string) []*dockwellv1.OnePanelRedirect {
	entries, err := os.ReadDir(filepath.Join(r.root, dir))
	if err != nil {
		return nil
	}
	var out []*dockwellv1.OnePanelRedirect
	for _, e := range entries {
		name, on := strings.CutSuffix(e.Name(), ".conf")
		if !on {
			var off bool
			if name, off = strings.CutSuffix(e.Name(), ".bak"); !off {
				continue
			}
		}
		if !e.Type().IsRegular() || len(out) == maxRedirects {
			continue
		}
		text, err := readSome(filepath.Join(r.root, dir, e.Name()), maxRedirectFile)
		if err != nil {
			continue
		}
		out = append(out, parseRedirect(name, on, text))
	}
	return out
}

// parseRedirect reads one redirect file's text in the shapes 1Panel writes it.
func parseRedirect(name string, on bool, text string) *dockwellv1.OnePanelRedirect {
	out := &dockwellv1.OnePanelRedirect{Name: name, Enabled: on}
	if found := domainRedirect().FindAllStringSubmatch(text, -1); len(found) > 0 {
		var domains []string
		for _, f := range found {
			host := hostOf(f[1])
			if host == "" {
				// A host in a shape 1Panel doesn't write: what it is can't be said.
				return out
			}
			domains = append(domains, host)
		}
		out.Type, out.Temporary, out.Domains = "domain", found[0][2] == "302", domains
		out.Target, out.KeepPath = strings.CutSuffix(found[0][3], "$request_uri")
		return out
	}
	if found := pathRedirect().FindStringSubmatch(text); found != nil {
		out.Type, out.Path, out.Temporary = "path", found[1], found[3] == "redirect"
		if target, kept := strings.CutSuffix(found[2], "$1"); kept {
			out.Target, out.KeepPath = target, true
		} else {
			out.Target = strings.TrimSuffix(found[2], "?")
		}
		return out
	}
	if strings.Contains(text, "error_page 404") {
		out.Type = "404"
	}
	return out
}

// readSome reads up to limit bytes of a regular file.
func readSome(p string, limit int64) (string, error) {
	f, err := os.Open(p) //nolint:gosec // a file of 1Panel's own, under its site's folder
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit))
	return string(b), err
}

func (r *reader) databases(found *dockwellv1.OnePanel, installs map[int64]string) error {
	kinds := map[string]string{}
	if err := r.rows("databases", func(row sqlitefile.Row) {
		name, kind := row.Text("name"), row.Text("type")
		kinds[name] = kind
		found.DatabaseServers = append(found.DatabaseServers, &dockwellv1.OnePanelDatabaseServer{
			Name: name, Type: kind, Version: row.Text("version"), Local: row.Text("from") == "local",
			App: installs[row.Int("app_install_id")], Port: port(row.Int("port")),
		})
	}); err != nil {
		return err
	}
	for _, table := range []struct{ name, server, kind string }{
		{"database_mysqls", "mysql_name", "mysql"},
		{"database_postgresqls", "postgresql_name", "postgresql"},
	} {
		if err := r.rows(table.name, func(row sqlitefile.Row) {
			if row.Bool("is_delete") {
				return
			}
			server := row.Text(table.server)
			kind := kinds[server]
			if kind == "" {
				kind = table.kind
			}
			found.Databases = append(found.Databases, &dockwellv1.OnePanelDatabase{
				Name: row.Text("name"), Server: server, Type: kind, User: row.Text("username"), Format: row.Text("format"),
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *reader) cronJobs(found *dockwellv1.OnePanel, installs map[int64]string, v1 bool) error {
	// A backup job keeps what it backs up by ID, so the names come from where each
	// kind is kept.
	names := func(table, column string) map[int64]string {
		out := map[int64]string{}
		_ = r.rows(table, func(row sqlitefile.Row) { out[row.Int("id")] = row.Text(column) })
		return out
	}
	sites := names("websites", "primary_domain")
	databases := map[string]map[int64]string{
		"mysql": names("database_mysqls", "name"), "postgresql": names("database_postgresqls", "name"),
		"mongodb": names("database_mongodbs", "name"),
	}
	databases["mariadb"], databases["mysql-cluster"] = databases["mysql"], databases["mysql"]
	databases["postgresql-cluster"] = databases["postgresql"]
	library := map[int64][2]string{}
	_ = r.rows("script_libraries", func(row sqlitefile.Row) {
		library[row.Int("id")] = [2]string{row.Text("name"), row.Text("script")}
	})
	return r.rows("cronjobs", func(row sqlitefile.Row) {
		job := &dockwellv1.OnePanelCronJob{
			Name: row.Text("name"), Type: row.Text("type"), Schedule: schedules(row.Text("spec"), v1), Status: row.Text("status"),
			Container: row.Text("container_name"),
		}
		switch job.Type {
		case "shell":
			r.shell(job, row, library)
		case "curl":
			if u, err := url.Parse(strings.TrimSpace(row.Text("url"))); err == nil {
				job.UrlHost = u.Hostname()
			}
		case "website":
			job.Target = targets(row.Text("website"), sites)
		case "database":
			job.DatabaseType = row.Text("db_type")
			job.Target = targets(row.Text("db_name"), databases[job.DatabaseType])
		case "directory":
			job.Target = row.Text("source_dir")
		case "app":
			job.Target = targets(row.Text("app_id"), installs)
		}
		found.CronJobs = append(found.CronJobs, job)
	})
}

// shell reads where a shell job's script is, what runs it and as whom, and how
// long it is, but never the script.
func (r *reader) shell(job *dockwellv1.OnePanelCronJob, row sqlitefile.Row, library map[int64][2]string) {
	job.ScriptFrom = cmp.Or(row.Text("script_mode"), "input")
	script := row.Text("script")
	switch job.ScriptFrom {
	case "library":
		named := library[row.Int("script_id")]
		job.ScriptName, script = named[0], named[1]
	case "select":
		job.ScriptPath, script = strings.TrimSpace(script), ""
	}
	if script != "" {
		job.ScriptLines = uint32(min(bytes.Count([]byte(strings.TrimRight(script, "\n")), []byte("\n"))+1, 1<<20)) //nolint:gosec // bounded
	}
	// What 1Panel runs it with: the command in a container, sh where none's
	// given, and the executor on the server, bash where none's given. Either
	// runs as its user where it names one, with sudo on the server.
	if job.Container != "" {
		job.Interpreter = cmp.Or(row.Text("command"), "sh")
	} else {
		job.Interpreter = cmp.Or(row.Text("executor"), "bash")
	}
	job.User = row.Text("user")
}

// schedules is a job's schedules, a line each: 1Panel joins several with "&&",
// and 1.10 with ",".
func schedules(spec string, v1 bool) string {
	sep := "&&"
	if v1 {
		sep = ","
	}
	var out []string
	for s := range strings.SplitSeq(spec, sep) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// targets names what a backup acts on: 1Panel keeps IDs, comma separated, or
// "all".
func targets(ids string, installs map[int64]string) string {
	if strings.TrimSpace(ids) == "all" {
		return "all"
	}
	var names []string
	for id := range strings.SplitSeq(ids, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil {
			continue
		}
		if name := installs[n]; name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// origin is where a proxy sends, as a scheme, a host and a port: no path, user
// or query, which can hold a password.
func origin(proxy string) string {
	u, err := url.Parse(strings.TrimSpace(proxy))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// list reads 1Panel's comma-separated domains.
func list(s string) []string {
	var out []string
	for d := range strings.SplitSeq(s, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// port is a port number, or 0 where it isn't one.
func port(n int64) uint32 {
	if n <= 0 || n > 65535 {
		return 0
	}
	return uint32(n)
}

// when reads a time as GORM writes it to SQLite, or nothing.
func when(s string) *timestamppb.Timestamp {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Year() <= 1 {
				return nil
			}
			return timestamppb.New(t)
		}
	}
	return nil
}
