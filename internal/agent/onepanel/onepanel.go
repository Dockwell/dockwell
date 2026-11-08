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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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
var known = regexp.MustCompile(`^v?(2\.[0-3]\.\d+|2\.4\.[012]|1\.10\.\d+)(-lts)?$`)

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
			"1Panel isn't installed here: there's no %s or %s", path.Join(dbDir, "agent.db"), path.Join(dbDir, "1Panel.db"))}, nil
	}
	last := fmt.Errorf("1Panel changed %s each of the %d times the agent read it, so what it read may mix two moments. Try again in a moment", records, tries)
	for i := range tries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before := stateOf(filepath.Join(root, records))
		found, changed, err := read(ctx, root, base, records, v1)
		switch {
		case err == nil && !changed:
			return &agentv1.ReadOnePanelResult{Found: found}, nil
		// A read that failed on files that held still failed on what they hold. One
		// whose files changed under it may have read a page half written, so it
		// goes again, as one that read through a change does.
		case err != nil && !changed && !errors.Is(err, sqlitefile.ErrWriting) && stateOf(filepath.Join(root, records)) == before:
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(100<<i) * time.Millisecond):
		}
	}
	return nil, last
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
	if err != nil {
		return nil, false, fmt.Errorf("the agent can't read 1Panel's records, %s: %w", records, err)
	}
	defer db.Close()
	r := &reader{ctx: ctx, db: db, base: base}
	found := &dockwellv1.OnePanel{BaseDir: base, Records: records}
	// What every version has, a version this doesn't know has too, or it isn't
	// read: a table missing reads as empty otherwise, and says nothing of why.
	for _, table := range coreTables {
		if _, ok := db.Table(table); !ok {
			return nil, false, fmt.Errorf("1Panel's records, %s, have no %s table, which every version this was written from has, so the agent can't tell how to read them. Update 1Panel, then try again", records, table)
		}
	}
	settings := r.settings()
	found.Version = settings["SystemVersion"]
	if found.Version == "" {
		return nil, false, fmt.Errorf("1Panel's records, %s, say no version, so the agent can't tell how to read them. Update 1Panel, then try again", records)
	}
	found.KnownVersion = known.MatchString(found.Version)
	if err := r.all(found, settings, v1); err != nil {
		return nil, db.Changed(), fmt.Errorf("the agent can't read 1Panel's records, %s: %w", records, err)
	}
	return found, db.Changed(), nil
}

type reader struct {
	ctx  context.Context //nolint:containedctx // it lives as long as one read
	db   *sqlitefile.DB
	base string
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
		found.Websites = append(found.Websites, w)
	}); err != nil {
		return err
	}
	for w, id := range parents {
		w.Parent = aliases[id]
	}
	return nil
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
