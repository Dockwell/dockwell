// SPDX-License-Identifier: AGPL-3.0-only

package importapi

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// What an import waits for where Dockwell can't take a thing yet. Each goes once
// its feature is on main.
const (
	waitsForWebsites  = dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_WEBSITES
	waitsForDatabases = dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_DATABASES
	waitsForSchedules = dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_SCHEDULED_TASKS
	waitsForFirewall  = dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_FIREWALL
)

// phpVersions are the PHP versions Dockwell's websites run (ADR 0039).
var phpVersions = []string{"8.2", "8.3", "8.4", "8.5"}

// databaseApps are the store apps that are database servers.
var databaseApps = map[string]bool{"mysql": true, "mariadb": true, "postgresql": true, "redis": true, "mongodb": true}

// Steps says what an import would do with each thing 1Panel keeps, in the order
// the plan takes them: what's taken on, then what's made again, then what stays.
func Steps(found *dockwellv1.OnePanel) []*dockwellv1.ImportStep {
	var steps []*dockwellv1.ImportStep
	add := func(s *dockwellv1.ImportStep) { steps = append(steps, s) }
	for _, a := range found.GetApps() {
		add(appStep(a))
	}
	for _, c := range found.GetComposes() {
		add(&dockwellv1.ImportStep{
			Kind: dockwellv1.ImportKind_IMPORT_KIND_COMPOSE_PROJECT, Name: c.GetName(),
			Summary: "a Compose project made on 1Panel's Containers page, from " + c.GetFile(),
			Action:  dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON,
			Becomes: fmt.Sprintf("a Compose project, %s, taken on as it runs, with no restart", c.GetName()),
			Reason:  "What it binds stays where its Compose file says. Leave it alone in 1Panel afterwards, and change it from Dockwell.",
		})
	}
	// 1Panel runs a container it started on its own with no record of it.
	add(&dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_CONTAINERS, Name: "Other containers",
		Summary: "containers 1Panel started on their own, outside an app or a Compose project",
		Action:  dockwellv1.ImportAction_IMPORT_ACTION_LEAVE,
		Reason: "1Panel keeps no record of a container it started on its own, so this can't list one. " +
			"The server's Containers page lists every container Dockwell didn't make, and takes one on from there.",
	})
	sites := map[string][]*dockwellv1.OnePanelWebsite{}
	for _, w := range found.GetWebsites() {
		if w.GetRuntime() != "" {
			sites[w.GetRuntime()] = append(sites[w.GetRuntime()], w)
		}
	}
	runtimes := map[string]*dockwellv1.OnePanelRuntime{}
	for _, r := range found.GetRuntimes() {
		runtimes[r.GetName()] = r
		add(runtimeStep(r, sites[r.GetName()]))
	}
	for _, s := range found.GetDatabaseServers() {
		add(databaseServerStep(s))
	}
	for _, d := range found.GetDatabases() {
		add(&dockwellv1.ImportStep{
			Kind: dockwellv1.ImportKind_IMPORT_KIND_DATABASE, Name: d.GetName(),
			Summary:  fmt.Sprintf("a %s database on %s, for %s", typeName(d.GetType()), d.GetServer(), d.GetUser()),
			Action:   dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON,
			Becomes:  fmt.Sprintf("a database of %s, with its user %s, whose password becomes a sealed secret", d.GetServer(), d.GetUser()),
			WaitsFor: waitsForDatabases,
		})
	}
	for _, w := range found.GetWebsites() {
		add(websiteStep(w, runtimes))
	}
	for _, c := range found.GetCertificates() {
		add(certificateStep(c))
	}
	for _, j := range found.GetCronJobs() {
		add(cronJobStep(j))
	}
	add(&dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_PANEL, Name: "Firewall", Summary: "the rules 1Panel writes to the server's firewall",
		Action:   dockwellv1.ImportAction_IMPORT_ACTION_LEAVE,
		Reason:   "1Panel writes them to the server's own firewall, ufw or firewalld, so there's nothing to move. Once Dockwell has firewall rules, it reads them where they are.",
		WaitsFor: waitsForFirewall,
	})
	add(panelStep(found))
	return steps
}

func appStep(a *dockwellv1.OnePanelApp) *dockwellv1.ImportStep {
	store := map[string]string{"local": "your own store", "custom": "a custom store"}[a.GetStore()]
	if store == "" {
		store = "1Panel's store"
	}
	name := a.GetAppName()
	if name == "" {
		name = a.GetAppKey()
	}
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_APP, Name: a.GetName(),
		Summary: fmt.Sprintf("%s %s from %s, %s", name, a.GetVersion(), store, strings.ToLower(a.GetStatus())),
	}
	if a.GetAppKey() == "openresty" {
		s.Action = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
		s.Reason = "It serves 1Panel's websites on ports 80 and 443, on the host's network, which Dockwell's ingress needs. " +
			"It stays until Dockwell has websites and you hand those ports over: then stop it in 1Panel, apply the websites' plan, and check each site. " +
			"The sites are down from the stop until the ingress has its certificates, a minute or so. Starting it again in 1Panel undoes that."
		return s
	}
	s.Action = dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON
	s.Becomes = fmt.Sprintf("a Compose project, %s, taken on as it runs, with no restart", a.GetName())
	s.Reason = fmt.Sprintf("What it binds stays in %s. Leave it alone in 1Panel afterwards, and change its image from Dockwell.", a.GetFolder())
	if databaseApps[a.GetAppKey()] {
		s.Reason += " Its data stays where it is, and its databases come with Dockwell's databases."
	}
	if strings.EqualFold(a.GetStatus(), "Stopped") {
		s.Reason += " It's stopped, and stays stopped."
	}
	return s
}

func runtimeStep(r *dockwellv1.OnePanelRuntime, sites []*dockwellv1.OnePanelWebsite) *dockwellv1.ImportStep {
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_RUNTIME, Name: r.GetName(),
		Summary: fmt.Sprintf("%s %s, %s", runtimeName(r.GetType()), r.GetVersion(), strings.ToLower(r.GetStatus())),
	}
	if r.GetType() != "php" {
		s.Action = dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON
		s.Becomes = fmt.Sprintf("a Compose project, %s, taken on as it runs, with no restart", r.GetName())
		if r.GetCodeFolder() != "" {
			s.Reason = fmt.Sprintf("The code it runs stays in %s.", r.GetCodeFolder())
		}
		return s
	}
	if len(sites) == 0 {
		s.Action = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
		s.Reason = "No website runs on it, so there's nothing to make it for. A PHP website in Dockwell runs its own PHP."
		return s
	}
	version, ok := php(r.GetVersion())
	if !ok {
		s.Action = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
		s.Reason = fmt.Sprintf("Dockwell's websites run PHP %s to %s, so its sites stay on 1Panel until they run on one of those.", phpVersions[0], phpVersions[len(phpVersions)-1])
		return s
	}
	s.Action = dockwellv1.ImportAction_IMPORT_ACTION_MAKE
	s.Becomes = fmt.Sprintf("PHP %s in each website that runs on it, made with the website", version)
	s.Reason = "Dockwell's PHP has the extensions most sites need, such as mysqli, gd and intl, and not pdo_mysql. Check what your sites use."
	s.WaitsFor = waitsForWebsites
	return s
}

func runtimeName(kind string) string {
	if n := map[string]string{"php": "PHP", "node": "Node.js", "java": "Java", "go": "Go", "python": "Python", "dotnet": ".NET"}[kind]; n != "" {
		return n
	}
	return kind
}

// php is the PHP version Dockwell runs a site of version v on: the same minor,
// or false where Dockwell has none that doesn't go back.
func php(v string) (string, bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "", false
	}
	want := parts[0] + "." + parts[1]
	for _, have := range phpVersions {
		if have == want {
			return have, true
		}
	}
	return "", false
}

func databaseServerStep(d *dockwellv1.OnePanelDatabaseServer) *dockwellv1.ImportStep {
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_DATABASE_SERVER, Name: d.GetName(),
		Summary: fmt.Sprintf("%s %s", typeName(d.GetType()), d.GetVersion()),
	}
	if !d.GetLocal() {
		s.Summary += ", on another machine"
		s.Action = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
		s.Reason = "It's on another machine, so there's nothing on this server to take on. Add it from Dockwell's Databases."
		s.WaitsFor = waitsForDatabases
		return s
	}
	s.Action = dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON
	s.Becomes = fmt.Sprintf("taken on with the app %s, with its data where it is", d.GetApp())
	return s
}

func websiteStep(w *dockwellv1.OnePanelWebsite, runtimes map[string]*dockwellv1.OnePanelRuntime) *dockwellv1.ImportStep {
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, Name: w.GetPrimaryDomain(),
		Action: dockwellv1.ImportAction_IMPORT_ACTION_MAKE, WaitsFor: waitsForWebsites,
	}
	var lost []string
	switch w.GetType() {
	case "static":
		s.Summary = "a static site"
		s.Becomes = fmt.Sprintf("a website serving %s, in place", w.GetFolder())
	case "runtime":
		r := runtimes[w.GetRuntime()]
		if r.GetType() != "php" {
			s.Summary = fmt.Sprintf("a site on the %s runtime %s", runtimeName(r.GetType()), w.GetRuntime())
			s.Becomes = fmt.Sprintf("a route to the runtime %s, taken on", w.GetRuntime())
			break
		}
		s.Summary = fmt.Sprintf("a PHP %s site", r.GetVersion())
		version, ok := php(r.GetVersion())
		if !ok {
			s.Action, s.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, 0
			s.Reason = fmt.Sprintf("Dockwell's websites run PHP %s to %s. Move it to one of those in 1Panel, then import it.", phpVersions[0], phpVersions[len(phpVersions)-1])
			return s
		}
		s.Becomes = fmt.Sprintf("a PHP %s website on %s, in place", version, w.GetFolder())
	case "proxy":
		s.Summary = "a reverse proxy"
		target, ok := proxyTarget(w.GetProxy())
		if !ok {
			s.Action, s.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, 0
			s.Reason = "Where it sends can't be read from 1Panel's records. Make it again from Dockwell's Websites."
			return s
		}
		s.Becomes = "a website sending to " + target
	case "deployment":
		s.Summary = "the site of the app " + w.GetApp()
		s.Becomes = fmt.Sprintf("a route to the app %s, taken on", w.GetApp())
	case "subsite":
		s.Summary = "a subsite of " + w.GetParent()
		s.Action, s.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, 0
		s.Reason = "Dockwell's websites have no subsites. Make it a website of its own, at a domain of its own."
		return s
	case "stream":
		s.Summary = "a TCP or UDP stream"
		s.Action, s.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, 0
		s.Reason = "Dockwell has no TCP or UDP proxy for websites. Publish the port from the app it sends to instead."
		return s
	default:
		s.Summary = fmt.Sprintf("a site of a kind 1Panel calls %q", w.GetType())
		s.Action, s.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, 0
		s.Reason = "Dockwell doesn't know what that kind of site is. Make it again from Dockwell's Websites."
		return s
	}
	switch n := len(w.GetDomains()); {
	case n == 2:
		lost = append(lost, "its other domain, "+w.GetDomains()[1].GetDomain())
	case n > 2:
		lost = append(lost, fmt.Sprintf("its %d other domains", n-1))
	}
	if w.GetRewrite() {
		lost = append(lost, "its rewrite rules")
	}
	if len(lost) > 0 {
		s.Reason = "A Dockwell website has no place yet for " + strings.Join(lost, " or ") + ", so they stay in 1Panel's config."
	}
	if strings.EqualFold(w.GetStatus(), "Stopped") {
		s.Reason = strings.TrimSpace(s.Reason + " It's stopped in 1Panel, and a Dockwell website serves it once it's made, so leave it out if it should stay down.")
	}
	return s
}

// proxyTarget says where a proxy sends, in Dockwell's words: a port on the
// server, or a URL.
func proxyTarget(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", false
	}
	host, port := u.Hostname(), u.Port()
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		if n, err := strconv.Atoi(port); err == nil && n > 0 {
			return fmt.Sprintf("port %d on the server", n), true
		}
	}
	return origin, true
}

func certificateStep(c *dockwellv1.OnePanelCertificate) *dockwellv1.ImportStep {
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_CERTIFICATE, Name: c.GetPrimaryDomain(),
		Action: dockwellv1.ImportAction_IMPORT_ACTION_LEAVE,
	}
	switch c.GetKind() {
	case "manual":
		s.Summary = "a certificate you uploaded"
		if i := c.GetIssuer(); i != "" {
			s.Summary += ", issued by " + i
		}
	case "selfSigned":
		s.Summary = "a self-signed certificate from 1Panel's own CA"
	case "fromMaster":
		s.Summary = "a certificate the 1Panel that manages this one sends it"
	case "dnsManual":
		s.Summary = "a certificate from " + certificateSource(c) + ", with DNS records set by hand"
	case "dnsAccount":
		s.Summary = "a certificate from " + certificateSource(c) + ", through a DNS provider's account"
	default:
		s.Summary = "a certificate from " + certificateSource(c)
	}
	if end := c.GetExpires(); end != nil {
		// A date in UTC, said so, as a certificate's end is a moment, not a day.
		s.Summary += ", until " + end.AsTime().UTC().Format("2006-01-02 15:04") + " UTC"
	}
	switch c.GetKind() {
	case "manual", "selfSigned", "dnsManual", "fromMaster":
		s.Reason = "Dockwell's ingress gets one from Let's Encrypt in its place when it takes over ports 80 and 443. " +
			"A domain that can't get one, as it isn't on public DNS, goes behind a tunnel or a proxy that ends TLS."
	default:
		s.Reason = "Dockwell's ingress gets its own when it takes over ports 80 and 443. This one stays in 1Panel's records."
	}
	return s
}

// certificateSource names the CA an ACME certificate came from: its account's, or
// its issuer.
func certificateSource(c *dockwellv1.OnePanelCertificate) string {
	if name, ok := map[string]string{"letsencrypt": "Let's Encrypt", "zerossl": "ZeroSSL", "buypass": "Buypass", "google": "Google Trust Services"}[strings.ToLower(c.GetProvider())]; ok {
		return name
	}
	return cmp.Or(c.GetIssuer(), "an ACME server")
}

func cronJobStep(j *dockwellv1.OnePanelCronJob) *dockwellv1.ImportStep {
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_CRON_JOB, Name: j.GetName(),
		Action: dockwellv1.ImportAction_IMPORT_ACTION_MAKE, WaitsFor: waitsForSchedules,
	}
	schedule := strings.Join(strings.Fields(strings.ReplaceAll(j.GetSchedule(), "\n", ", ")), " ")
	off := !strings.EqualFold(j.GetStatus(), "Enable")
	leave := func(summary, reason string) *dockwellv1.ImportStep {
		s.Summary, s.Action, s.WaitsFor, s.Reason = summary, dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, 0, reason
		return s
	}
	switch j.GetType() {
	case "shell":
		run := j.GetInterpreter()
		switch j.GetScriptFrom() {
		case "library":
			// 1Panel can't run one whose script its library no longer has, either.
			if j.GetScriptName() == "" {
				return leave("a script from 1Panel's library, on "+schedule,
					"1Panel's library no longer has its script, so 1Panel can't run it either. Make it again from Dockwell's scheduled tasks.")
			}
			s.Summary = fmt.Sprintf("the script %s from 1Panel's library, %s, run by %s", j.GetScriptName(), lines(j.GetScriptLines()), run)
			s.Becomes = "a scheduled task running the same script"
		case "select":
			s.Summary = fmt.Sprintf("the script %s on the server, run by %s", j.GetScriptPath(), run)
			s.Becomes = "a scheduled task running " + j.GetScriptPath()
		default:
			s.Summary = fmt.Sprintf("a %s script of %s", run, lines(j.GetScriptLines()))
			s.Becomes = "a scheduled task running the same script"
		}
		if c := j.GetContainer(); c != "" {
			s.Summary += ", in the container " + c
			s.Becomes += ", in the container " + c
		}
		if u := j.GetUser(); u != "" {
			s.Summary += ", as " + u
			s.Becomes += ", as " + u
		}
		s.Summary += ", on " + schedule
	case "curl":
		s.Summary = fmt.Sprintf("a call to %s, on %s", j.GetUrlHost(), schedule)
		s.Becomes = "a scheduled task calling the same URL"
	case "app", "website", "database", "directory":
		s.Summary = fmt.Sprintf("a backup of %s, on %s", backupOf(j), schedule)
		s.Becomes = "a scheduled backup of the same, to Dockwell's backups"
	case "snapshot":
		return leave("a snapshot of 1Panel", "A snapshot of 1Panel itself is 1Panel's.")
	case "ntp":
		return leave("setting the clock", "The server's own time service keeps its clock.")
	case "cutWebsiteLog":
		return leave("cutting websites' logs", "Docker rotates the logs of Dockwell's websites.")
	case "clean", "cleanLog", "log":
		return leave("cleaning 1Panel's own files", "It cleans what 1Panel keeps, which Dockwell doesn't use.")
	case "syncIpGroup":
		return leave("syncing 1Panel's IP groups", "1Panel's IP groups are its own.")
	default:
		return leave(fmt.Sprintf("a job 1Panel calls %q", j.GetType()), "Dockwell doesn't know what that kind of job does. Make it again from Dockwell's scheduled tasks.")
	}
	if off {
		s.Reason = "It's off in 1Panel, and comes off."
	}
	return s
}

// typeName names a kind of database as its makers write it: "mysql" is MySQL.
func typeName(t string) string {
	if name := databaseTypes[t]; name != "" {
		return name
	}
	return t
}

// lines says how many lines a script has.
func lines(n uint32) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// databaseTypes names the kinds of database 1Panel backs up.
var databaseTypes = map[string]string{
	"mysql": "MySQL", "mariadb": "MariaDB", "mysql-cluster": "MySQL cluster", "postgresql": "PostgreSQL",
	"postgresql-cluster": "PostgreSQL cluster", "mongodb": "MongoDB",
}

// backupOf says what a backup job backs up: "the website blog.example.com",
// "the apps mysql, wordpress", "every PostgreSQL database".
func backupOf(j *dockwellv1.OnePanelCronJob) string {
	nouns := map[string][2]string{
		"app": {"app", "apps"}, "website": {"website", "websites"}, "database": {"database", "databases"}, "directory": {"folder", "folders"},
	}[j.GetType()]
	one, many := nouns[0], nouns[1]
	if t := typeName(j.GetDatabaseType()); t != "" {
		one, many = t+" "+one, t+" "+many
	}
	switch target := j.GetTarget(); {
	case target == "all":
		return "every " + one
	case target == "":
		return "the " + many + " it names, which 1Panel no longer has"
	case strings.Contains(target, ","):
		return "the " + many + " " + target
	default:
		return "the " + one + " " + target
	}
}

func panelStep(found *dockwellv1.OnePanel) *dockwellv1.ImportStep {
	s := &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_PANEL, Name: "1Panel",
		Summary: fmt.Sprintf("1Panel %s, under %s", found.GetVersion(), found.GetBaseDir()),
		Action:  dockwellv1.ImportAction_IMPORT_ACTION_LEAVE,
		Reason: "Its login, settings, users, backup accounts and snapshots are its own. " +
			"It goes on running until you uninstall it, which waits until what's imported runs as you want.",
	}
	if !found.GetKnownVersion() {
		s.Reason += fmt.Sprintf(" This import was written from 1Panel v2.0 to v2.4.2 and v1.10, and %s is read the same way, by its records' column names, so check what it found.", found.GetVersion())
	}
	return s
}
