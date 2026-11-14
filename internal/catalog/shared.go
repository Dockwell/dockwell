// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/link"
)

// One you run: a database server Dockwell runs already, shared because a person
// chooses it, on which Dockwell makes the app a database and a user of its own, who
// isn't its administrator. Only a server whose administrator's secret no service
// outside it uses is offered; any other is listed with why (ADR 0084).

// A Candidate is a database server Dockwell runs that might meet a need, as the
// control plane knows it.
type Candidate struct {
	ServiceID, Service       string
	Project, Environment     string // by name
	ProjectID, EnvironmentID string
	Engine                   catalogv1.Engine
	Version                  string           // what it says of itself, or its tag's
	Servers                  []string         // the servers it runs on, by ID
	ServerNames              []string         // and by name
	Databases, Users         []string         // what its reading lists, for names that are free
	Sizes                    map[string]int64 // how much each of its databases takes, in bytes, as its reading says
	SharedWith               []string         // the projects whose apps link to it already
	SharedWithIDs            []string         // and by ID
	AdministeredBy           string           // a service outside it that signs in as its administrator, if any
	// Unguarded says it asks no one for a password: a Redis or Valkey that requires
	// none, or a MongoDB without access control. A user of an app's own there would
	// keep no one out, so it isn't shared.
	Unguarded bool
	// Unprefixed says a Redis or Valkey's apps may read and change each other's data: a
	// user on it that can sign in isn't one Dockwell made with a key prefix, or its
	// default asks no password, or its users couldn't be read.
	Unprefixed   bool
	JoinsInPlace bool // every server it runs on has an agent that joins link networks in place
	// PartOf names the app whose catalog install it's part of, as WordPress's own
	// MariaDB is, where that app's upgrades would change it, and Pinnable says the
	// plan that first shares it can pin it apart from the app, as the entry's own
	// service for a need of its name, so the app's upgrades leave it as it runs. One
	// that isn't waits (ADR 0084).
	PartOf   string
	Pinnable bool
}

// KeepsApart reports a Redis or Valkey shared way on which each app keeps to its own
// keys: the need sets a key prefix, and every user there has one.
func (w Way) KeepsApart() bool {
	return w.Existing != nil && keyValue(w.Engine) && keepsPrefix(w.Need) && !w.Existing.Unprefixed
}

// EndsApart is what sharing a Redis or Valkey this way ends: where its apps keep to
// their own keys, an app that keeps to no prefix of its own reaches every one of
// theirs. It's each project whose apps link to it, by name and by ID, and nothing
// where the way keeps a prefix, or its apps don't keep apart now. Choosing it is
// allowed, as a person chose it, with a role in each of those projects (ADR 0084).
func (w Way) EndsApart() (names, ids []string) {
	c := w.Existing
	if c == nil || !keyValue(w.Engine) || c.Unprefixed || keepsPrefix(w.Need) {
		return nil, nil
	}
	return c.SharedWith, c.SharedWithIDs
}

// Choice is how InstallCatalogEntryRequest.needs names a candidate.
func (c Candidate) Choice() string {
	return "service:" + c.Project + "/" + c.Environment + "/" + c.Service
}

// usersOwn reports an engine Dockwell makes an app's own database and user on, or on
// Redis and Valkey an ACL user (ADR 0038, 0084).
func usersOwn(e catalogv1.Engine) bool {
	switch e {
	case catalogv1.Engine_ENGINE_POSTGRESQL, catalogv1.Engine_ENGINE_MYSQL, catalogv1.Engine_ENGINE_MARIADB, catalogv1.Engine_ENGINE_MONGODB,
		catalogv1.Engine_ENGINE_REDIS, catalogv1.Engine_ENGINE_VALKEY:
		return true
	default:
		return false
	}
}

// existing lists the candidates for a need, the usable first, each with why it can't
// meet the need where it can't. on is the server the app goes on, by ID, or empty
// where the app can go wherever the candidate is.
func existing(n *catalogv1.Need, app string, candidates []Candidate, on string) []Way {
	var usable, not []Way
	for i := range candidates {
		c := &candidates[i]
		a := acceptOf(n, c.Engine)
		if a == nil {
			continue
		}
		w := Way{Need: n, Engine: c.Engine, Version: c.Version, Existing: c}
		switch {
		case !usersOwn(c.Engine):
			w.Why = fmt.Sprintf("Dockwell can't make an app its own user on %s yet", EngineName(c.Engine))
		case c.Engine == catalogv1.Engine_ENGINE_REDIS && len(link.Numbers(c.Version)) > 0 && link.Numbers(c.Version)[0] < 6:
			w.Why = fmt.Sprintf("Redis %s keeps one password, so an app can't have a user of its own on it", c.Version)
		case c.Engine == catalogv1.Engine_ENGINE_REDIS && len(link.Numbers(c.Version)) > 1 && link.Numbers(c.Version)[0] == 6 && link.Numbers(c.Version)[1] < 2:
			w.Why = fmt.Sprintf("Redis %s has no rules for channels, so its apps would read each other's messages. It takes 6.2 or later", c.Version)
		case c.Unguarded && keyValue(c.Engine):
			w.Why = "Asks no one for a password, so every app on it is its administrator"
		case c.Unguarded:
			w.Why = "Runs without access control, so every app on it reaches every other's data"
		case keyValue(c.Engine) && !sendsUser(n):
			// One that sends no user name would sign in as the server's administrator.
			w.Why = app + " sends a password alone, so it would sign in as the server's administrator"
		case len(link.Numbers(c.Version)) > 0 && !link.Takes(a.GetVersions(), c.Version):
			w.Why = fmt.Sprintf("Runs %s %s; %s takes %s", EngineName(c.Engine), c.Version, app, versionWords(a.GetVersions()))
		case c.AdministeredBy != "":
			w.Why = c.AdministeredBy + " signs in to it as its administrator"
		case c.PartOf != "" && !c.Pinnable:
			w.Why = fmt.Sprintf("Part of %s's install, whose upgrades would change it", c.PartOf)
		case len(c.Servers) != 1:
			w.Why = "Runs on more than one server"
		case on != "" && c.Servers[0] != on:
			w.Why = "Runs on another server"
		case !c.JoinsInPlace:
			w.Why = "Its server's agent needs updating first"
		}
		if w.Why == "" {
			usable = append(usable, w)
		} else {
			not = append(not, w)
		}
	}
	return append(usable, not...)
}

// versionWords says a range of versions as a person reads it: ">=10.6" as "10.6 or later".
func versionWords(versions string) string {
	var out []string
	for _, c := range strings.Fields(versions) {
		switch {
		case strings.HasPrefix(c, ">="):
			out = append(out, c[2:]+" or later")
		case strings.HasPrefix(c, "<="):
			out = append(out, c[2:]+" or earlier")
		case strings.HasPrefix(c, ">"):
			out = append(out, "later than "+c[1:])
		case strings.HasPrefix(c, "<"):
			out = append(out, "before "+c[1:])
		default:
			out = append(out, strings.TrimPrefix(c, "="))
		}
	}
	return strings.Join(out, ", ")
}

// freeName is name, or where the server has it, the next it hasn't: wordpress_2.
func freeName(name string, taken []string) string {
	if !slices.Contains(taken, name) {
		return name
	}
	for i := 2; ; i++ {
		next := name + "_" + strconv.Itoa(i)
		if len(next) > 32 {
			next = name[:32-len(strconv.Itoa(i))-1] + "_" + strconv.Itoa(i)
		}
		if !slices.Contains(taken, next) {
			return next
		}
	}
}

// sqlName is a project's name as a database's or a user's: lowercase letters, digits
// and underscores, starting with a letter, at most 32.
func sqlName(project string) string {
	b := []byte(strings.ToLower(strings.ReplaceAll(project, "-", "_")))
	out := b[:0]
	for _, c := range b {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			out = append(out, c)
		}
	}
	s := string(out)
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "app_" + s
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// SharedNames are the database and user an app is given on a server it shares: the
// entry's, or on a server other apps use already, the app's project's, so two
// WordPresses on one server don't clash, and the next free where one is taken.
func SharedNames(n *catalogv1.Need, project string, c Candidate) (database, user string) {
	database = databaseOf(n)
	user = cmpOr(n.GetUser(), database)
	if len(c.SharedWith) > 0 || project != "" && (slices.Contains(c.Databases, database) || slices.Contains(c.Users, user)) {
		database, user = sqlName(project), sqlName(project)
	}
	// A database or a user its server keeps for itself is never an app's.
	databases := slices.Concat(c.Databases, keptDatabases)
	users := slices.Concat(c.Users, keptUsers)
	return freeName(database, databases), freeName(user, users)
}

// keptDatabases and keptUsers are what a server keeps for itself, on any engine:
// never an app's on a server it shares (ADR 0084).
var (
	keptDatabases = []string{"postgres", "template0", "template1", "mysql", "information_schema", "performance_schema", "sys", "admin", "config", "local"}
	keptUsers     = []string{"default", "postgres", "root"}
)

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// NeedOf is the need of an install's pin that its entry's own service, of engine,
// meets: the need of the service's own name; one an earlier share pinned apart as
// that service; or else the one need of that engine its entry's own service meets.
// Two such needs can't be told apart, so neither is found (ADR 0084).
func NeedOf(pin *resourcev1.CatalogPin, service string, engine catalogv1.Engine) (string, bool) {
	needs := pin.GetNeeds()
	if n, ok := needs[service]; ok {
		return service, n.GetEnvironment() == ""
	}
	var own []string
	for _, key := range slices.Sorted(maps.Keys(needs)) {
		n := needs[key]
		switch {
		case n.GetEnvironment() != "":
		case n.GetService() == service && len(n.GetFile()) > 0 && n.GetEntry() == engines[engine].entry:
			return key, true
		case n.GetEntry() == "" && len(n.GetFile()) == 0 && catalogv1.Engine(n.GetEngine()) == engine:
			own = append(own, key)
		}
	}
	if len(own) != 1 {
		return "", false
	}
	return own[0], true
}

// PinApart is how an install's pin keeps its entry's own service for need once a
// person first shares it, as it runs in file, the file it ran: the service, under its
// own name, with the volumes it mounts, and the catalog's entry for its engine, so
// the app's upgrades leave it as it runs (ADR 0084). It refuses a service the file
// hasn't got.
func PinApart(file, need string, was *resourcev1.PinnedNeed, version string) (*resourcev1.PinnedNeed, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return nil, errors.New("the file the install ran isn't YAML")
	}
	root := doc.Content[0]
	svc := nodeGet(nodeGet(root, "services"), need)
	if svc == nil {
		return nil, fmt.Errorf("the file the install ran has no service %s", need)
	}
	out := &yaml.Node{Kind: yaml.MappingNode}
	services := &yaml.Node{Kind: yaml.MappingNode}
	services.Content = append(services.Content, scalar(need), svc)
	out.Content = append(out.Content, scalar("services"), services)
	if named := namedVolumes(svc); len(named) > 0 {
		volumes := &yaml.Node{Kind: yaml.MappingNode}
		from := nodeGet(root, "volumes")
		for _, v := range named {
			spec := nodeGet(from, v)
			if spec == nil {
				spec = &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			}
			volumes.Content = append(volumes.Content, scalar(v), spec)
		}
		out.Content = append(out.Content, scalar("volumes"), volumes)
	}
	text, err := encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{out}})
	if err != nil {
		return nil, err
	}
	pinned := proto.CloneOf(was)
	pinned.Entry, pinned.Service, pinned.File = engines[catalogv1.Engine(was.GetEngine())].entry, need, []byte(text)
	if version != "" {
		pinned.Version = version
	}
	return pinned, nil
}

// ServiceNames are a Compose file's services, by name, sorted.
func ServiceNames(file string) ([]string, error) {
	services, err := composeServices(file)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(services)), nil
}
