// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/link"
)

// A need is something an app reads from outside its own containers, such as its
// database, by the names it sets in the app's environment. An install meets each,
// and records how in its pin, so an upgrade reads the file as it did. See
// docs/adr/0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md.
//
// A need is met by a new one beside the app: the entry's own service, or the
// catalog's entry for another engine the need accepts, where that engine's image makes
// the app a user who isn't its administrator (MySQL and MariaDB), or where the plan
// makes one (PostgreSQL), or where the server is the app's alone and it signs in with
// the server's password (Redis); or a server Dockwell runs already, shared because a
// person chose it. MongoDB from the catalog waits for users Dockwell makes on it.
// A URL is a value Dockwell writes from the need's parts (Written).

// engine is what the catalog knows of an engine a need can name.
type engine struct {
	// word is how InstallCatalogEntryRequest.needs names it, such as "mysql".
	word string
	// name is what people call it.
	name string
	// entry is the catalog entry a new one beside an app comes from, where its image
	// makes the app's user, with its database, from first-start variables, and that
	// user isn't its administrator. Empty where none comes from the catalog yet.
	entry string
	// database, user and password are those first-start variables.
	database, user, password string
	// planned says the image makes only an administrator, so the app's user, with its
	// database, is a database user the install's plan makes once the server is up,
	// and the administrator's password stays the server's own (ADR 0084).
	planned bool
	// alone names the entry's input that holds the server's one password, which an
	// install from before an app had a user of its own on its new Redis gave the app,
	// as the need's: its pin keeps the server so, and plans it no user (ADR 0084).
	alone string
}

var engines = map[catalogv1.Engine]engine{
	catalogv1.Engine_ENGINE_POSTGRESQL: {word: "postgresql", name: "PostgreSQL", entry: "postgres", planned: true},
	catalogv1.Engine_ENGINE_MYSQL: {
		word: "mysql", name: "MySQL", entry: "mysql",
		database: "MYSQL_DATABASE", user: "MYSQL_USER", password: "MYSQL_PASSWORD",
	},
	catalogv1.Engine_ENGINE_MARIADB: {
		word: "mariadb", name: "MariaDB", entry: "mariadb",
		database: "MARIADB_DATABASE", user: "MARIADB_USER", password: "MARIADB_PASSWORD",
	},
	catalogv1.Engine_ENGINE_REDIS:      {word: "redis", name: "Redis", entry: "redis", planned: true, alone: "REDIS_PASSWORD"},
	catalogv1.Engine_ENGINE_VALKEY:     {word: "valkey", name: "Valkey"},
	catalogv1.Engine_ENGINE_MONGODB:    {word: "mongodb", name: "MongoDB", entry: "mongo", planned: true},
	catalogv1.Engine_ENGINE_CLICKHOUSE: {word: "clickhouse", name: "ClickHouse"},
}

// EngineName is what people call an engine, such as "MariaDB".
func EngineName(e catalogv1.Engine) string { return engines[e].name }

// EngineWord is how InstallCatalogEntryRequest.needs names an engine, such as "mariadb".
func EngineWord(e catalogv1.Engine) string { return engines[e].word }

// EngineOfImage is the engine an image runs, by its official image, or
// ENGINE_UNSPECIFIED for one Dockwell doesn't know as a database.
func EngineOfImage(image string) catalogv1.Engine {
	if e := database.Recognise(image); e != nil {
		// The API's engines and the format's share their numbers.
		return catalogv1.Engine(e.Kind)
	}
	return catalogv1.Engine_ENGINE_UNSPECIFIED
}

// port is what an engine listens on unless its command says otherwise.
func port(e catalogv1.Engine) uint32 {
	if d := database.Of(dockwellv1.DatabaseEngine(e)); d != nil {
		return d.Port
	}
	return 0
}

// databaseName is what every engine takes as a database's or a user's name as it is,
// unquoted (ADR 0038).
var databaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// placeholder is a part a template names, such as {password}.
var placeholder = regexp.MustCompile(`\{([a-z_]*)\}`)

var templateParts = map[string]bool{"host": true, "port": true, "native_port": true, "address": true, "database": true, "user": true, "password": true, "tls": true}

// maxNeedLabel is how long a need's label may be: a word or two, on one line.
const maxNeedLabel = 40

// checkNeeds refuses needs an install couldn't meet as they say.
func checkNeeds(e *catalogv1.Entry, cf *composeFile) error {
	if len(e.GetNeeds()) == 0 {
		return nil
	}
	services := cf.services
	inputs := map[string]bool{}
	for _, in := range e.GetInputs() {
		inputs[in.GetName()] = true
	}
	companions := map[string]bool{}
	for _, c := range e.GetCompanions() {
		companions[c.GetService()] = true
	}
	names, sets := map[string]bool{}, map[string]string{}
	for _, n := range e.GetNeeds() {
		name := n.GetName()
		if err := resource.CheckLabel("need", name, "db"); err != nil {
			return err
		}
		if names[name] {
			return fmt.Errorf("two needs are named %s. Rename one", name)
		}
		names[name] = true
		label := strings.TrimSpace(n.GetLabel())
		switch {
		case label == "" || strings.ContainsAny(label, "\r\n"):
			return fmt.Errorf("the need %s needs a label on one line, such as Database", name)
		case len([]rune(label)) > maxNeedLabel:
			return fmt.Errorf("the need %s's label is %d characters, and takes at most %d: a word or two", name, len([]rune(label)), maxNeedLabel)
		case len(n.GetSets()) == 0:
			return fmt.Errorf("the need %s sets nothing for the app to read. Add a name to sets, such as DB_HOST", name)
		}
		if n.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED {
			if err := checkKindNeed(n, services); err != nil {
				return err
			}
		} else if err := checkDatabaseNeed(e, n, services, companions); err != nil {
			return err
		}
		if err := checkOptional(n); err != nil {
			return err
		}
		password := ""
		for _, s := range n.GetSets() {
			if err := checkSet(e, n, s, inputs, sets, cf); err != nil {
				return err
			}
			sets[s.GetName()] = name
			if s.GetPart() == catalogv1.Part_PART_PASSWORD {
				password = s.GetName()
			}
		}
		if n.GetSetOnce() && password == "" {
			return fmt.Errorf("the need %s says its password is set once, and sets no password. Take set_once away, or add a name with PART_PASSWORD", name)
		}
	}
	return nil
}

// checkDatabaseNeed refuses a database need an install couldn't meet as it says:
// engines Dockwell doesn't know, versions that don't read, and a database or user
// a server keeps for itself.
func checkDatabaseNeed(e *catalogv1.Entry, n *catalogv1.Need, services map[string]any, companions map[string]bool) error {
	name := n.GetName()
	switch {
	case len(n.GetAccepts()) == 0:
		return fmt.Errorf("the need %s names no engine that meets it. Add one to accepts, such as ENGINE_MYSQL", name)
	case n.GetBucket() != "" || n.GetPresignLifetime() != "":
		return fmt.Errorf("the need %s is a database's, and names a bucket or a signed URL's lifetime, which only S3 storage takes. Take them out, or set its kind", name)
	}
	seen := map[catalogv1.Engine]bool{}
	for _, a := range n.GetAccepts() {
		if a.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED || len(a.GetFeatures()) > 0 {
			return fmt.Errorf("the need %s is a database's, and accepts a kind or features, which a database need names by engine. Name its engines, such as ENGINE_MYSQL", name)
		}
		if _, ok := engines[a.GetEngine()]; !ok {
			return fmt.Errorf("the need %s names an engine Dockwell doesn't know. Name one of %s", name, engineList())
		}
		if seen[a.GetEngine()] {
			return fmt.Errorf("the need %s names %s twice. Keep one, with every version the app takes", name, EngineName(a.GetEngine()))
		}
		seen[a.GetEngine()] = true
		if err := link.CheckVersions(a.GetVersions()); err != nil {
			return fmt.Errorf("the need %s: %w", name, err)
		}
	}
	for field, v := range map[string]string{"database": n.GetDatabase(), "user": n.GetUser()} {
		if v != "" && !databaseName.MatchString(v) {
			return fmt.Errorf("the need %s's %s %q needs lowercase letters, digits and underscores, starting with a letter, at most 32", name, field, v)
		}
	}
	// What an engine keeps for itself, such as its administrator, is never an app's.
	db := databaseOf(n)
	user := cmp.Or(n.GetUser(), db)
	switch {
	case reservedUser(user):
		return fmt.Errorf("the need %s's user would be %s, which a database server keeps for itself. Give the need a user of its own, such as %s", name, user, strings.ReplaceAll(e.GetId(), "-", "_"))
	case reservedDatabases[db]:
		return fmt.Errorf("the need %s's database would be %s, which a database server keeps for itself. Give the need a database of its own, such as %s", name, db, strings.ReplaceAll(e.GetId(), "-", "_"))
	}
	return checkNeedService(e, n, services, companions)
}

// bucketName is a bucket's name as every S3 service takes it.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// maxPresign is the longest a signed URL lasts on AWS, and so on any S3 service.
const maxPresign = 7 * 24 * time.Hour

// kindParts are the parts each kind's need may set, beside PART_MET.
var kindParts = map[catalogv1.Kind]map[catalogv1.Part]bool{
	catalogv1.Kind_KIND_S3: {
		catalogv1.Part_PART_ENDPOINT: true, catalogv1.Part_PART_REGION: true, catalogv1.Part_PART_BUCKET: true,
		catalogv1.Part_PART_ACCESS_KEY_ID: true, catalogv1.Part_PART_SECRET_KEY: true, catalogv1.Part_PART_PATH_STYLE: true,
		catalogv1.Part_PART_PUBLIC_URL: true, catalogv1.Part_PART_KEY_PREFIX: true, catalogv1.Part_PART_URL: true,
	},
	catalogv1.Kind_KIND_SMTP: {
		catalogv1.Part_PART_HOST: true, catalogv1.Part_PART_PORT: true, catalogv1.Part_PART_USER: true, catalogv1.Part_PART_PASSWORD: true,
		catalogv1.Part_PART_URL: true, catalogv1.Part_PART_SECURITY: true, catalogv1.Part_PART_SENDER: true, catalogv1.Part_PART_SENDER_NAME: true,
	},
}

// smtpOnly are the parts only a mail server has, which a database need can't set.
var smtpOnly = map[catalogv1.Part]bool{
	catalogv1.Part_PART_SECURITY: true, catalogv1.Part_PART_SENDER: true, catalogv1.Part_PART_SENDER_NAME: true,
}

// s3Features are the features an S3 need may ask for.
var s3Features = map[catalogv1.Feature]bool{
	catalogv1.Feature_FEATURE_PRESIGNED_URLS: true, catalogv1.Feature_FEATURE_VIRTUAL_HOST: true, catalogv1.Feature_FEATURE_PATH_STYLE: true,
	catalogv1.Feature_FEATURE_MULTIPART: true, catalogv1.Feature_FEATURE_BATCH_DELETE: true, catalogv1.Feature_FEATURE_CORS: true,
	catalogv1.Feature_FEATURE_PUBLIC_READ: true, catalogv1.Feature_FEATURE_OBJECT_ACL: true, catalogv1.Feature_FEATURE_PRESIGNED_POST: true,
	catalogv1.Feature_FEATURE_SDK_CHECKSUMS: true,
}

// checkKindNeed refuses a need of a kind beyond the databases that an install
// couldn't meet as it says (ADR 0099): S3 storage takes its kind with features, a
// bucket's name, a signed URL's lifetime, and the parts that sign in to it.
func checkKindNeed(n *catalogv1.Need, services map[string]any) error {
	name, kind := n.GetName(), n.GetKind()
	switch kind {
	case catalogv1.Kind_KIND_SMTP:
		return checkMailNeed(n, services)
	case catalogv1.Kind_KIND_S3:
	default:
		return fmt.Errorf("the need %s is for %s, which Dockwell doesn't meet yet. Leave it to inputs for now", name, cmp.Or(catalogv1.Kind_name[int32(kind)], kind.String()))
	}
	switch {
	case n.GetDatabase() != "" || n.GetUser() != "" || n.GetSetOnce():
		return fmt.Errorf("the need %s is for S3 storage, and names a database, a user or set_once, which only a database need takes. Take them out", name)
	case n.GetBucket() != "" && (!bucketName.MatchString(n.GetBucket()) || strings.Contains(n.GetBucket(), "..")):
		return fmt.Errorf("the need %s's bucket %q needs 3 to 63 lowercase letters, digits, dots and dashes, starting and ending with a letter or digit", name, n.GetBucket())
	}
	if v := n.GetPresignLifetime(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > maxPresign {
			return fmt.Errorf("the need %s's signed URLs last %q, which needs a time from 1s to 168h, such as 1h", name, v)
		}
	}
	seen := map[catalogv1.Feature]bool{}
	for _, a := range n.GetAccepts() {
		if a.GetEngine() != catalogv1.Engine_ENGINE_UNSPECIFIED || a.GetVersions() != "" || a.GetKind() != kind {
			return fmt.Errorf("the need %s is for S3 storage, and accepts what isn't: give accepts the kind KIND_S3 with the features the app needs, or leave accepts out for any", name)
		}
		for _, f := range a.GetFeatures() {
			switch {
			case !s3Features[f]:
				return fmt.Errorf("the need %s asks for a feature S3 storage doesn't have, %s", name, f)
			case seen[f]:
				return fmt.Errorf("the need %s asks for %s twice. Keep one", name, f)
			}
			seen[f] = true
		}
	}
	parts := map[catalogv1.Part]bool{}
	for _, s := range n.GetSets() {
		parts[s.GetPart()] = true
	}
	reaches := parts[catalogv1.Part_PART_URL] ||
		parts[catalogv1.Part_PART_ENDPOINT] && parts[catalogv1.Part_PART_BUCKET] && parts[catalogv1.Part_PART_ACCESS_KEY_ID] && parts[catalogv1.Part_PART_SECRET_KEY]
	if !reaches {
		return fmt.Errorf("the need %s is for S3 storage, and sets too little to reach a bucket. Set its endpoint, bucket, access key ID and secret key, or a URL", name)
	}
	if _, clash := services[name]; clash && name != n.GetService() {
		return fmt.Errorf("the need %s brings in a service of its name when it's met otherwise, and its Compose file has a service called %s already. Rename the need, or name %s as its service", name, name, name)
	}
	// S3 storage beside the app, its own or the catalog's, is slice 5's (ADR 0099):
	// until then an entry that brings one keeps it as a service of its own.
	if n.GetService() != "" {
		return fmt.Errorf("the need %s is met by %s, its own S3 storage, which Dockwell can't run as a need's yet. Leave service out, and keep %s as a service of the app's", name, n.GetService(), n.GetService())
	}
	return nil
}

// checkMailNeed refuses a need of a mail server an install couldn't meet as it says
// (ADR 0099): it takes its kind alone, sets where to connect and how it's secured,
// or a URL, and signs in with a user and a password together. It's met by a
// connection, so it brings no service of its own.
func checkMailNeed(n *catalogv1.Need, services map[string]any) error {
	name := n.GetName()
	switch {
	case n.GetDatabase() != "" || n.GetUser() != "" || n.GetSetOnce():
		return fmt.Errorf("the need %s is for a mail server, and names a database, a user or set_once, which only a database need takes. Take them out", name)
	case n.GetBucket() != "" || n.GetPresignLifetime() != "":
		return fmt.Errorf("the need %s is for a mail server, and names a bucket or how long signed URLs last, which only S3 storage takes. Take them out", name)
	case n.GetService() != "":
		return fmt.Errorf("the need %s is met by %s, its own mail server, which Dockwell can't run as a need's yet. Leave service out, and keep %s as a service of the app's", name, n.GetService(), n.GetService())
	}
	for _, a := range n.GetAccepts() {
		if a.GetEngine() != catalogv1.Engine_ENGINE_UNSPECIFIED || a.GetVersions() != "" || a.GetKind() != catalogv1.Kind_KIND_SMTP || len(a.GetFeatures()) > 0 {
			return fmt.Errorf("the need %s is for a mail server, and accepts what isn't: give accepts the kind KIND_SMTP alone, or leave accepts out", name)
		}
	}
	parts := map[catalogv1.Part]bool{}
	for _, s := range n.GetSets() {
		parts[s.GetPart()] = true
	}
	switch {
	case !parts[catalogv1.Part_PART_URL] && (!parts[catalogv1.Part_PART_HOST] || !parts[catalogv1.Part_PART_PORT]):
		return fmt.Errorf("the need %s is for a mail server, and sets too little to reach one. Set its host, port and security, or a URL", name)
	case parts[catalogv1.Part_PART_HOST] && !parts[catalogv1.Part_PART_SECURITY]:
		// An app told nothing of TLS may send its password before STARTTLS, or with
		// none where a server between strips it (ADR 0099).
		return fmt.Errorf("the need %s sets a mail server's host, and not how the app secures it. Set PART_SECURITY under each name the app reads it by, as SMTP_SECURE and SMTP_REQUIRE_TLS", name)
	case parts[catalogv1.Part_PART_USER] != parts[catalogv1.Part_PART_PASSWORD]:
		return fmt.Errorf("the need %s signs in to a mail server with a user and a password, and sets one of them. Set both, or neither", name)
	}
	if _, clash := services[name]; clash {
		return fmt.Errorf("the need %s brings in a service of its name when it's met otherwise, and its Compose file has a service called %s already. Rename the need", name, name)
	}
	return nil
}

// SecurityWhy is why a mail server's need can't take a connection secured as
// security is ("starttls", "tls" or "none"), or "" where it can: the app's words for
// it must tell it apart from each other security, under one name or another, or the
// app could take one for another, as an app told only to use TLS from the start
// upgrades to STARTTLS only if the server offers it, and a server between can strip
// that offer (ADR 0099).
func SecurityWhy(n *catalogv1.Need, security string) string {
	var sets []*catalogv1.NeedSet
	for _, s := range n.GetSets() {
		if s.GetPart() == catalogv1.Part_PART_SECURITY {
			sets = append(sets, s)
		}
	}
	if len(sets) == 0 {
		// A need of a URL alone says its security in the URL's scheme, and no app
		// reads smtp:// as requiring STARTTLS.
		if security == "starttls" {
			return "the app can't be told to require STARTTLS"
		}
		return ""
	}
	for _, other := range []string{"starttls", "tls", "none"} {
		if other == security {
			continue
		}
		if !slices.ContainsFunc(sets, func(s *catalogv1.NeedSet) bool { return fixedWord(s, security) != fixedWord(s, other) }) {
			return map[string]string{
				"starttls": "the app can't be told to require STARTTLS",
				"tls":      "the app can't be told to use TLS from the start",
				"none":     "the app can't be told to go without TLS",
			}[security]
		}
	}
	return ""
}

// checkOptional refuses what only an optional need says on one that isn't, and an
// optional need that says nothing of being met where it's set unmet.
func checkOptional(n *catalogv1.Need) error {
	name := n.GetName()
	met := 0
	for _, s := range n.GetSets() {
		switch {
		case s.GetPart() == catalogv1.Part_PART_MET:
			met++
			if !n.GetOptional() {
				return fmt.Errorf("the need %s says whether it's met in %s, and isn't optional, so it's always met. Take %s out, or make the need optional", name, s.GetName(), s.GetName())
			}
			if s.GetEmptyWhenUnmet() {
				return fmt.Errorf("the need %s sets %s, which says whether it's met and is set while unmet already. Take empty_when_unmet away", name, s.GetName())
			}
		case s.GetEmptyWhenUnmet() && !n.GetOptional():
			return fmt.Errorf("the need %s sets %s empty while unmet, and isn't optional, so it's never unmet. Take empty_when_unmet away, or make the need optional", name, s.GetName())
		}
	}
	if met > 1 {
		return fmt.Errorf("the need %s says whether it's met in %d names. Keep one", name, met)
	}
	return nil
}

// reservedUser reports a user an engine keeps for itself: its administrator, as root,
// postgres and Redis's default are, or one in PostgreSQL's pg_ names.
func reservedUser(user string) bool {
	switch user {
	case "root", "postgres", "mysql", "mariadb", "default", "admin":
		return true
	}
	return strings.HasPrefix(user, "pg_")
}

// reservedDatabases are databases an engine keeps for itself.
var reservedDatabases = map[string]bool{
	"mysql": true, "information_schema": true, "performance_schema": true, "sys": true,
	"postgres": true, "template0": true, "template1": true, "admin": true, "local": true, "config": true,
}

// checkNeedService refuses a need's own service that couldn't stand for a new one,
// and a service that would clash with the one a new need brings in.
func checkNeedService(e *catalogv1.Entry, n *catalogv1.Need, services map[string]any, companions map[string]bool) error {
	name, own := n.GetName(), n.GetService()
	if _, clash := services[name]; clash && name != own {
		return fmt.Errorf("the need %s brings in a service of its name when it's met otherwise, and its Compose file has a service called %s already. Rename the need, or name %s as its service", name, name, name)
	}
	if own == "" {
		return nil
	}
	svc, ok := services[own]
	switch {
	case !ok:
		return fmt.Errorf("the need %s is met by the service %s, which its Compose file doesn't run", name, own)
	case !slices.Contains(e.GetDatabases(), own):
		return fmt.Errorf("the need %s is met by %s, which isn't one of the entry's databases. Add it to databases", name, own)
	case companions[own]:
		return fmt.Errorf("the need %s is met by %s, which is a companion too. A need's service isn't one", name, own)
	}
	image, _ := mapGet(svc, "image").(string)
	eng := EngineOfImage(image)
	accept := acceptOf(n, eng)
	if accept == nil {
		return fmt.Errorf("the need %s is met by %s, which runs %s, an engine the need doesn't accept", name, own, cmp.Or(EngineName(eng), "an image Dockwell doesn't know as a database"))
	}
	if v := database.Version(image); len(link.Numbers(v)) > 0 && !link.Takes(accept.GetVersions(), v) {
		return fmt.Errorf("the need %s is met by %s, whose %s %s isn't one the need takes (%s)", name, own, EngineName(eng), v, accept.GetVersions())
	}
	for _, s := range n.GetSets() {
		// What its new database is made with: the database, the user and the password,
		// but a Redis's database, which is a number the app picks.
		switch s.GetPart() {
		case catalogv1.Part_PART_DATABASE, catalogv1.Part_PART_USER:
			if eng == catalogv1.Engine_ENGINE_REDIS || eng == catalogv1.Engine_ENGINE_VALKEY {
				continue
			}
		case catalogv1.Part_PART_PASSWORD:
		default:
			continue
		}
		if !uses(svc, s.GetName()) {
			return fmt.Errorf("the need %s's service %s doesn't read %s, so a new one wouldn't be made with what the app signs in with. Use ${%s} in its environment", name, own, s.GetName(), s.GetName())
		}
	}
	for _, c := range e.GetCompanions() {
		if dependsOn(services[c.GetService()], own) {
			return fmt.Errorf("the companion %s depends on %s, which isn't there when the need %s is met otherwise. Take the dependency away", c.GetService(), own, name)
		}
	}
	return nil
}

// checkSet refuses a name a need sets that the app couldn't read as it's set.
func checkSet(e *catalogv1.Entry, n *catalogv1.Need, s *catalogv1.NeedSet, inputs map[string]bool, sets map[string]string, cf *composeFile) error {
	need, name := n.GetName(), s.GetName()
	part := s.GetPart()
	tls := s.GetTls()
	switch {
	case !inputName.MatchString(name):
		return fmt.Errorf("the need %s sets %q, which needs a name a Compose file can use, in capitals, such as DB_HOST", need, name)
	case inputs[name]:
		return fmt.Errorf("the need %s sets %s, which is an input too. Rename one", need, name)
	case sets[name] != "":
		return fmt.Errorf("%s is set by two needs, %s and %s. Rename one", name, sets[name], need)
	case part == catalogv1.Part_PART_UNSPECIFIED || catalogv1.Part_name[int32(part)] == "":
		return fmt.Errorf("the need %s sets %s, and needs to say what it holds, such as PART_HOST", need, name)
	case part != catalogv1.Part_PART_MET && n.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED && !kindParts[n.GetKind()][part]:
		return fmt.Errorf("the need %s sets %s as %s, which isn't a part of what it needs. Use one of its own, such as PART_ENDPOINT", need, name, part)
	case n.GetKind() == catalogv1.Kind_KIND_UNSPECIFIED && part != catalogv1.Part_PART_KEY_PREFIX && part != catalogv1.Part_PART_URL && kindParts[catalogv1.Kind_KIND_S3][part]:
		return fmt.Errorf("the need %s sets %s as %s, which only S3 storage has. Set the need's kind, or use a database's part", need, name, part)
	case n.GetKind() == catalogv1.Kind_KIND_UNSPECIFIED && smtpOnly[part]:
		return fmt.Errorf("the need %s sets %s as %s, which only a mail server has. Set the need's kind to KIND_SMTP, or use a database's part", need, name, part)
	case s.GetTemplate() != "" && part != catalogv1.Part_PART_URL:
		return fmt.Errorf("the need %s writes %s from a template, which only a URL takes", need, name)
	case s.GetTemplate() != "" && n.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED:
		return fmt.Errorf("the need %s writes %s from a template, which only a database's URL takes for now. Take the template out for the kind's own form", need, name)
	case (part == catalogv1.Part_PART_NATIVE_PORT || strings.Contains(s.GetTemplate(), "{native_port}")) &&
		(len(n.GetAccepts()) != 1 || n.GetAccepts()[0].GetEngine() != catalogv1.Engine_ENGINE_CLICKHOUSE):
		return fmt.Errorf("the need %s sets a native port, which only ClickHouse has beside its HTTP port", need)
	case part == catalogv1.Part_PART_KEY_PREFIX && n.GetKind() == catalogv1.Kind_KIND_UNSPECIFIED && !slices.ContainsFunc(n.GetAccepts(), func(a *catalogv1.NeedAccept) bool { return keyValue(a.GetEngine()) }):
		return fmt.Errorf("the need %s sets %s as a key prefix, which only Redis and Valkey keep. Take it out, or accept one of them", need, name)
	case !regexp.MustCompile(`\$\{` + regexp.QuoteMeta(name) + `(\}|:?[-?+])`).MatchString(e.GetCompose()):
		return fmt.Errorf("its Compose file doesn't use %s, which the need %s sets. Use it as ${%s}, or take it out of sets", name, need, name)
	}
	usesTLS := part == catalogv1.Part_PART_TLS || strings.Contains(s.GetTemplate(), "{tls}")
	switch {
	case usesTLS && (oneWord(tls.GetOn()) != nil || oneWord(tls.GetOff()) != nil):
		return fmt.Errorf("the need %s sets %s, which says whether to use TLS, and needs the app's words for on and off, such as require and disable", need, name)
	case !usesTLS && tls != nil:
		return fmt.Errorf("the need %s gives %s words for TLS, which only a TLS part or a template with {tls} reads. Take tls away", need, name)
	}
	if err := checkSetWords(n, s); err != nil {
		return err
	}
	for _, m := range placeholder.FindAllStringSubmatch(s.GetTemplate(), -1) {
		if !templateParts[m[1]] {
			return fmt.Errorf("the need %s's template for %s names {%s}, which isn't a part. Use {host}, {port}, {address}, {database}, {user}, {password}, {tls} or {native_port}", need, name, m[1])
		}
	}
	if SecretPart(s) {
		return standsAlone(cf, name, false)
	}
	return plainAlone(cf, name)
}

// valueWords are the values of each part with a fixed set of them that the app has
// a word for, and whether that word may be empty, which leaves the name out.
var valueWords = map[catalogv1.Part]map[string]bool{
	catalogv1.Part_PART_MET:        {"met": false, "unmet": true},
	catalogv1.Part_PART_PATH_STYLE: {"on": false, "off": false},
	catalogv1.Part_PART_SECURITY:   {"starttls": false, "tls": false, "none": false},
}

// checkSetWords refuses a part with a fixed set of values without the app's word for
// each, as an engine part's engines or PART_MET's met and unmet, and words on any
// other part, as TLS's are checked.
func checkSetWords(n *catalogv1.Need, s *catalogv1.NeedSet) error {
	need, name := n.GetName(), s.GetName()
	values, fixed := valueWords[s.GetPart()]
	switch {
	case fixed:
		said := map[string]bool{}
		for _, w := range s.GetWords() {
			v := w.GetValue()
			empty, ok := values[v]
			switch {
			case w.GetEngine() != catalogv1.Engine_ENGINE_UNSPECIFIED || !ok:
				return fmt.Errorf("the need %s gives %s a word for %q, which it doesn't take. Give one for each of %s", need, name, cmp.Or(v, w.GetEngine().String()), strings.Join(slices.Sorted(maps.Keys(values)), " and "))
			case said[v]:
				return fmt.Errorf("the need %s gives %s two words for %s. Keep one", need, name, v)
			case (!empty || w.GetWord() != "") && oneWord(w.GetWord()) != nil:
				return fmt.Errorf("the need %s gives %s %q for %s, which needs to be one word the app reads", need, name, w.GetWord(), v)
			}
			said[v] = true
		}
		for v := range values {
			if !said[v] {
				return fmt.Errorf("the need %s sets %s and has no word for %s. Give the app's word for each of %s in words", need, name, v, strings.Join(slices.Sorted(maps.Keys(values)), " and "))
			}
		}
		return nil
	case s.GetPart() != catalogv1.Part_PART_ENGINE:
		if len(s.GetWords()) > 0 {
			return fmt.Errorf("the need %s gives %s words, which only an engine part, PART_MET, PART_PATH_STYLE and PART_SECURITY read. Take words away", need, name)
		}
		return nil
	}
	for _, w := range s.GetWords() {
		if w.GetValue() != "" {
			return fmt.Errorf("the need %s gives %s a word for %q, and an engine part's words are for engines", need, name, w.GetValue())
		}
	}
	said := map[catalogv1.Engine]bool{}
	for _, w := range s.GetWords() {
		e := w.GetEngine()
		switch {
		case acceptOf(n, e) == nil:
			return fmt.Errorf("the need %s gives %s a word for %s, which it doesn't accept. Take it out, or accept %s", need, name, cmp.Or(EngineName(e), e.String()), cmp.Or(EngineName(e), e.String()))
		case said[e]:
			return fmt.Errorf("the need %s gives %s two words for %s. Keep one", need, name, EngineName(e))
		case oneWord(w.GetWord()) != nil:
			return fmt.Errorf("the need %s gives %s %q for %s, which needs to be one word the app reads, such as postgres", need, name, w.GetWord(), EngineName(e))
		}
		said[e] = true
	}
	for _, a := range n.GetAccepts() {
		if !said[a.GetEngine()] {
			return fmt.Errorf("the need %s sets %s as the engine, and has no word for %s, which it accepts. Give the app's word for it in words", need, name, EngineName(a.GetEngine()))
		}
	}
	return nil
}

// EngineWordOf is the app's word for engine e, as an engine part writes it.
func EngineWordOf(s *catalogv1.NeedSet, e catalogv1.Engine) string {
	for _, w := range s.GetWords() {
		if w.GetEngine() == e {
			return w.GetWord()
		}
	}
	return ""
}

// oneWord refuses what isn't a word an app reads, on one line with no spaces.
func oneWord(w string) error {
	if w == "" || strings.ContainsAny(w, " \t\r\n") || len(w) > 64 {
		return errors.New("not a word")
	}
	return nil
}

// SecretPart reports a part that holds the password, alone or in a URL, which is
// kept as a secret.
func SecretPart(s *catalogv1.NeedSet) bool {
	switch s.GetPart() {
	case catalogv1.Part_PART_PASSWORD, catalogv1.Part_PART_SECRET_KEY:
		return true
	case catalogv1.Part_PART_URL:
		return s.GetTemplate() == "" || strings.Contains(s.GetTemplate(), "{password}")
	default:
		return false
	}
}

// plainAlone refuses a plain value a need sets that's used anywhere but as a whole
// value of a service's environment, as the app's services use it by reference there,
// and a reference stands only for a whole value.
func plainAlone(cf *composeFile, name string) error {
	services := cf.services
	usesOf := regexp.MustCompile(`\$\{` + regexp.QuoteMeta(name) + `(\}|:?[-?+])`)
	whole := "${" + name + "}"
	all := 0
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			all += len(usesOf.FindAllStringIndex(v, -1))
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(cf.tree)
	inEnv := 0
	for _, svc := range services {
		switch env := mapGet(svc, "environment").(type) {
		case map[string]any:
			for _, v := range env {
				if s, ok := v.(string); ok && s == whole {
					inEnv++
				}
			}
		case []any:
			for _, v := range env {
				if s, ok := v.(string); ok {
					if _, value, ok := strings.Cut(s, "="); ok && value == whole {
						inEnv++
					}
				}
			}
		}
	}
	if all != inEnv {
		return fmt.Errorf("%s is used outside a whole value of a service's environment, which the need's value can't reach. Give the app it as a value of its own, %s", name, whole)
	}
	return nil
}

// uses reports whether any string in a service names ${name}.
func uses(svc any, name string) bool {
	re := regexp.MustCompile(`\$\{` + regexp.QuoteMeta(name) + `(\}|:?[-?+])`)
	found := false
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			found = found || re.MatchString(v)
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(svc)
	return found
}

func mapGet(v any, key string) any {
	m, _ := v.(map[string]any)
	return m[key]
}

func acceptOf(n *catalogv1.Need, e catalogv1.Engine) *catalogv1.NeedAccept {
	for _, a := range n.GetAccepts() {
		if a.GetEngine() == e && e != catalogv1.Engine_ENGINE_UNSPECIFIED {
			return a
		}
	}
	return nil
}

func engineList() string {
	var out []string
	for e := range engines {
		out = append(out, catalogv1.Engine_name[int32(e)])
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// A Way is how an install meets one need: a new one beside the app, of an engine,
// from the entry's own service or the catalog's entry for the engine.
type Way struct {
	// Port and NativePort override the default database link endpoints when known.
	Port, NativePort uint32
	Need             *catalogv1.Need
	Engine           catalogv1.Engine
	Version          string
	// Unset leaves an optional need unmet, as the app runs without it: the default
	// for one, which is never met new unless a person asks (ADR 0099).
	Unset bool
	// UpgradeRelease is, for a server Why says another app's install keeps, the
	// release of that install's entry it would upgrade to for its server to be shared,
	// and UpgradeApp the app's name.
	UpgradeRelease uint32
	UpgradeApp     string
	// From is the catalog entry a new one comes from, nil for the entry's own
	// service, and FromCatalog the source it's in.
	From        *catalogv1.Entry
	FromCatalog string
	// pin is how the install met it, for an upgrade that keeps it so, with the
	// service it added as the pin kept it.
	pin *resourcev1.PinnedNeed
	// Existing is a server Dockwell runs already that meets it, shared because a
	// person chose it, and Why says why it can't, where it can't. Database and User
	// are what's made for the app there (SharedNames), and Password the secret there
	// that holds the user's password.
	Existing                 *Candidate
	Why                      string
	Database, User, Password string
	// Alias is the name a server it shares answers to on its link network, where the
	// need's own is taken beside the app, as by the database that met it before.
	Alias string
	// CopyFrom is a database the user made on the server it shares starts as a copy
	// of, as Use another database copies the app's across.
	CopyFrom *resourcev1.DatabaseCopy
	// Connection is a connection that meets a need of its kind, through a link named
	// for the need (ADR 0099).
	Connection *ConnectionWay
	// Own says the app's own values meet the need, as a Disconnect left them: the
	// install keeps them, and links to nothing (ADR 0099).
	Own bool
}

// ConnectionWay is a connection a need may be met by: one the app's project may use, of
// the need's kind (ADR 0099). Its values reach the app through a link named for the
// need, read from the connection, never held by the install.
type ConnectionWay struct {
	// Name is the connection's, and Label what the panel calls it.
	Name, Label string
	// Kind is what it reaches, as the need's kind: S3 storage unless it says.
	Kind catalogv1.Kind
	// Security is how a mail server's is secured, as a need's words name it:
	// starttls, tls or none. Sender is who its mail comes from, and SenderName the
	// name, unless a link says its app's own.
	Security, Sender, SenderName string
	// PathStyle says the connection puts the bucket in the path, which a need's
	// path-style set says in the app's word.
	PathStyle bool
	// Prefix is where the app's objects go in the bucket, such as "outline/".
	Prefix string
	// Named says the connection is named for the app's environment, as
	// "uploads-production" is, which makes it the default among several.
	Named bool
	// Widens says it doesn't reach the project an install makes yet, as one an admin
	// added from the install sheet: the install's plan gives it that project (ADR
	// 0099). Only an install that asks for it by name is offered it.
	Widens bool
	// Serves says a link of another app reads it already, or an app's own values name
	// its bucket, as a Disconnect leaves them. A connection serves one app unless it's
	// shared (ADR 0099), so it's then no way to meet a need, but an install it meets
	// already keeps it.
	Serves bool
	// Shared says an install admin marked it as serving more than one app, each under
	// a prefix of its own: a need that keeps to a prefix may take it though it serves
	// another app already.
	Shared bool
	// Apart says every other app it serves keeps to a prefix Dockwell knows, through a
	// link: none keeps its files at the bucket's root, and no app's own values name it.
	// A shared connection is a way beside them only then.
	Apart bool
}

// keepsToPrefix says whether an app keeps its files under a prefix of the bucket it's
// given, as a need that sets one does.
func keepsToPrefix(n *catalogv1.Need) bool {
	return slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_KEY_PREFIX })
}

// servesWhy is why a connection another app uses already can't meet a need, or ""
// where it can: a shared one, for an app that keeps to a prefix of the bucket, beside
// apps that each keep to theirs. It names no other app, which may be another
// project's.
func servesWhy(n *catalogv1.Need, c *ConnectionWay) string {
	switch {
	case !c.Serves:
		return ""
	case n.GetKind() == catalogv1.Kind_KIND_SMTP && !c.Shared:
		// A mail server keeps no files, so a shared one has nothing to keep apart.
		return "it serves another app already, and isn't shared"
	case n.GetKind() == catalogv1.Kind_KIND_SMTP:
		return ""
	case !keepsToPrefix(n):
		return "it serves another app already, and this app can't keep to a prefix of the bucket, so their files would mix"
	case !c.Shared:
		return "it serves another app already, and isn't shared"
	case !c.Apart:
		return "another app's link doesn't say which prefix its app keeps to, so nothing keeps their files apart"
	}
	return ""
}

// ChoiceConnection starts the choice of a connection, as "connection:uploads".
const ChoiceConnection = "connection:"

// ChoiceOwn is the way of a need the app's own values meet, as a Disconnect left
// them. Only a pin gives it.
const ChoiceOwn = "own"

// Choice is how InstallCatalogEntryRequest.needs names a way, such as "new:mysql",
// or "service:data/production/db" for a server Dockwell runs already.
func (w Way) Choice() string {
	if w.Existing != nil {
		return w.Existing.Choice()
	}
	if w.Unset {
		return ChoiceUnset
	}
	if w.Connection != nil {
		return ChoiceConnection + w.Connection.Name
	}
	if w.Own {
		return ChoiceOwn
	}
	return "new:" + EngineWord(w.Engine)
}

// ChoiceUnset is the choice that leaves an optional need unmet.
const ChoiceUnset = "unset"

// Shares reports a way that meets its need by a server Dockwell runs already.
func (w Way) Shares() bool { return w.Existing != nil }

// Service is the service that meets the need: the entry's own, or the one a new one
// adds under the need's name, which is the need's name too where the entry brings
// none.
func (w Way) Service() string {
	if own := w.Need.GetService(); own != "" && w.From == nil && len(w.pin.GetFile()) == 0 && w.Existing == nil {
		return own
	}
	// The entry's own server, pinned apart as a person first shared it, keeps its name.
	if apart := w.pin.GetService(); apart != "" && w.From == nil && hasService(string(w.pin.GetFile()), apart) {
		return apart
	}
	// A server shared with the app answers on its link network to the need's name, or
	// its alias where that's taken.
	if w.Existing != nil && w.Alias != "" {
		return w.Alias
	}
	return w.Need.GetName()
}

// KeyValue reports Redis or Valkey, whose app's user is an ACL user, with no database
// of its own.
func KeyValue(e catalogv1.Engine) bool { return keyValue(e) }

// keyValue is KeyValue.
func keyValue(e catalogv1.Engine) bool {
	return e == catalogv1.Engine_ENGINE_REDIS || e == catalogv1.Engine_ENGINE_VALKEY
}

// signsInAlone reports a pinned server whose one password is the app's, as a new
// Redis beside an app was before the app had a user of its own on it, or is where its
// app sends no user name: it stays so, and the plan makes it no user.
func (w Way) signsInAlone(password string) bool {
	return w.From == nil && engines[w.Engine].alone != "" && strings.Contains(string(w.pin.GetFile()), "${"+password+"}")
}

// sendsUser reports a need that gives its app a user name to sign in with: a user
// part, or a URL whose form has one.
func sendsUser(n *catalogv1.Need) bool {
	return slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool {
		return s.GetPart() == catalogv1.Part_PART_USER ||
			s.GetPart() == catalogv1.Part_PART_URL && (s.GetTemplate() == "" || strings.Contains(s.GetTemplate(), "{user}"))
	})
}

// keepsPrefix reports a need that sets a key prefix its app keeps to.
func keepsPrefix(n *catalogv1.Need) bool {
	return slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_KEY_PREFIX })
}

// KeepsApart reports a need whose app keeps to its own keys on a Redis or Valkey it
// shares, as it sets a key prefix (ADR 0084).
func KeepsApart(n *catalogv1.Need) bool { return keepsPrefix(n) }

// plansUser reports a new server beside the app whose image makes only its
// administrator, on which the plan makes the app a user of its own. A Redis or Valkey
// gets one only where the app sends a user name: an app that sends a password alone
// signs in as the server's default user, so the server's one password is the app's,
// and the server is the app's alone (ADR 0084).
func (w Way) plansUser(password string) bool {
	if !engines[w.Engine].planned || w.signsInAlone(password) {
		return false
	}
	return !keyValue(w.Engine) || sendsUser(w.Need)
}

// makesUser reports a way on which the install's plan makes the app's user: a new
// server from the catalog, or one its pin keeps, that plansUser. The entry's own
// service makes its users itself, as its image does, or has the app sign in with its
// one password.
func (w Way) makesUser(password string) bool {
	return w.plansUser(password) && w.Existing == nil && (w.From != nil || len(w.pin.GetFile()) > 0) && !w.imageMakesUser()
}

// imageMakesUser reports a pinned server whose file has its image make the app's user
// from the need's names, as an entry's own server pinned apart does: the app signs in
// as that user, which Dockwell didn't make, so the plan makes it none, which its
// server would refuse as one it has already.
func (w Way) imageMakesUser() bool {
	if w.From != nil {
		return false
	}
	file := string(w.pin.GetFile())
	return slices.ContainsFunc(w.Need.GetSets(), func(s *catalogv1.NeedSet) bool {
		return s.GetPart() == catalogv1.Part_PART_USER && strings.Contains(file, "${"+s.GetName()+"}")
	})
}

// Lookup finds a catalog entry by its ID, with the source it's in.
type Lookup func(id string) (e *catalogv1.Entry, source string, ok bool)

// Ways lists how each of an entry's needs can be met by a new one, by need: the
// entry's own service first, then each other engine the need accepts, in its order,
// that the catalog brings an entry for at a version the need takes. The first is the
// default.
func Ways(e *catalogv1.Entry, lookup Lookup) map[string][]Way {
	return WaysWith(e, lookup, nil, "")
}

// WaysWith lists how each need can be met, as Ways does, and after the new ones the
// servers Dockwell runs already that take it, the usable first, each with why it
// can't where it can't. on is the server the app goes on, by ID, or empty where it
// goes with what it shares.
func WaysWith(e *catalogv1.Entry, lookup Lookup, candidates []Candidate, on string) map[string][]Way {
	out := waysNew(e, lookup)
	for _, n := range e.GetNeeds() {
		out[n.GetName()] = append(out[n.GetName()], existing(n, e.GetName(), candidates, on, lookup)...)
	}
	return out
}

// WithConnections adds to ways, after the others, each connection that may meet
// each of the entry's needs of a kind beyond the databases (ADR 0099), with why it
// can't where it can't.
func WithConnections(e *catalogv1.Entry, ways map[string][]Way, conns []ConnectionWay) map[string][]Way {
	for _, n := range e.GetNeeds() {
		if n.GetKind() != catalogv1.Kind_KIND_S3 && n.GetKind() != catalogv1.Kind_KIND_SMTP {
			continue
		}
		for i := range conns {
			c := conns[i]
			if cmp.Or(c.Kind, catalogv1.Kind_KIND_S3) != n.GetKind() {
				continue
			}
			w := Way{Need: n, Connection: &c}
			switch {
			case n.GetKind() == catalogv1.Kind_KIND_SMTP:
				w.Why = cmp.Or(SecurityWhy(n, c.Security), servesWhy(n, &c))
			case slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_URL }):
				w.Why = "the app reads its storage as one URL, which a connection can't give yet"
			default:
				w.Why = servesWhy(n, &c)
			}
			ways[n.GetName()] = append(ways[n.GetName()], w)
		}
	}
	return ways
}

func waysNew(e *catalogv1.Entry, lookup Lookup) map[string][]Way {
	out := map[string][]Way{}
	services, _ := composeServices(e.GetCompose())
	for _, n := range e.GetNeeds() {
		var ways []Way
		if n.GetOptional() {
			ways = append(ways, Way{Need: n, Unset: true})
		}
		// A kind beyond the databases is met by a connection, which a person makes
		// (ADR 0099); nothing new beside the app yet.
		if n.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED {
			out[n.GetName()] = ways
			continue
		}
		ownEngine := catalogv1.Engine_ENGINE_UNSPECIFIED
		if own := n.GetService(); own != "" {
			image, _ := mapGet(services[own], "image").(string)
			ownEngine = EngineOfImage(image)
			ways = append(ways, Way{Need: n, Engine: ownEngine, Version: database.Version(image)})
		}
		for _, a := range n.GetAccepts() {
			info := engines[a.GetEngine()]
			if a.GetEngine() == ownEngine || info.entry == "" || lookup == nil {
				continue
			}
			from, source, ok := lookup(info.entry)
			if !ok || !link.Takes(a.GetVersions(), from.GetVersion()) {
				continue
			}
			if _, err := databaseService(from, a.GetEngine()); err != nil {
				continue
			}
			ways = append(ways, Way{Need: n, Engine: a.GetEngine(), Version: from.GetVersion(), From: from, FromCatalog: source})
		}
		out[n.GetName()] = ways
	}
	return out
}

// Meet says how an install meets each of its entry's needs: as asked, by need, with
// "new" for the first way or "new:<engine>" for another; or else as the install's pin
// says, for an upgrade; or else by the first way. It refuses a need it can't meet,
// and a choice for no need.
func Meet(e *catalogv1.Entry, ways map[string][]Way, asked map[string]string, pinned map[string]*resourcev1.PinnedNeed) (map[string]Way, error) {
	out := map[string]Way{}
	for _, name := range slices.Sorted(maps.Keys(asked)) {
		if !slices.ContainsFunc(e.GetNeeds(), func(n *catalogv1.Need) bool { return n.GetName() == name }) {
			return nil, fmt.Errorf("%s has no need %s. %s", e.GetName(), name, needList(e))
		}
	}
	for _, n := range e.GetNeeds() {
		name := n.GetName()
		options := ways[name]
		if p := pinned[name]; p != nil && asked[name] == "" {
			w, err := pinnedWay(n, p, options)
			if err != nil {
				return nil, err
			}
			out[name] = w
			continue
		}
		choice := asked[name]
		// New is the default, always: a server is shared only because a person chose it,
		// and a connection where it's the only one the project may use (ADR 0099).
		first := slices.IndexFunc(options, func(w Way) bool { return !w.Shares() && w.Connection == nil })
		if first < 0 && choice == "" {
			if only := onlyConnection(options); only >= 0 {
				out[name] = options[only]
				continue
			}
		}
		switch {
		case (choice == "" || choice == "new") && first < 0:
			if n.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED {
				// Each says what happened, what that means, and the way out (the slice 1
				// review, L2).
				usable := choicesOf(options)
				switch {
				case len(usable) > 1 && choice == "":
					return nil, fmt.Errorf("%s needs %s, and more than one connection may meet it, so nothing was planned. Pick one: %s", e.GetName(), strings.ToLower(n.GetLabel()), strings.Join(usable, " or "))
				case len(usable) > 0:
					return nil, fmt.Errorf("%s needs %s, %s, which Dockwell can't make beside it yet, so nothing was planned. Pick a connection that meets it: %s", e.GetName(), strings.ToLower(n.GetLabel()), acceptWords(n), strings.Join(usable, " or "))
				}
				return nil, fmt.Errorf("%s needs %s, %s, which Dockwell can't make beside it yet, so nothing was planned. Add a connection on Connections that this project may use, or install an app that keeps it optional", e.GetName(), strings.ToLower(n.GetLabel()), acceptWords(n))
			}
			return nil, fmt.Errorf("%s needs %s, and nothing here can make a new one yet: %s. Choose one you run, if one takes it", e.GetName(), strings.ToLower(n.GetLabel()), acceptWords(n))
		case choice == "":
			out[name] = options[first]
		case choice == "new":
			// New is something new, never an optional need left unset.
			i := slices.IndexFunc(options, func(w Way) bool { return !w.Shares() && !w.Unset })
			if i < 0 {
				return nil, fmt.Errorf("%s's need %s takes %s, not %q: nothing here can make a new one yet", e.GetName(), name, strings.Join(choicesOf(options), " or "), choice)
			}
			out[name] = options[i]
		default:
			i := slices.IndexFunc(options, func(w Way) bool { return w.Choice() == choice })
			if i >= 0 && options[i].Why != "" && options[i].Connection != nil {
				// A connection's why is a state, as the sheet shows it beside Add a
				// connection; a refusal adds the way out.
				return nil, fmt.Errorf("the connection %s can't meet %s's %s: %s, so nothing was planned. Pick another, or add a connection of its own on Connections", options[i].Connection.Name, e.GetName(), strings.ToLower(n.GetLabel()), options[i].Why)
			}
			if i >= 0 && options[i].Why != "" {
				return nil, fmt.Errorf("%s can't meet %s's %s: %s", strings.TrimPrefix(choice, "service:"), e.GetName(), strings.ToLower(n.GetLabel()), options[i].Why)
			}
			if c, ok := strings.CutPrefix(choice, ChoiceConnection); ok && i < 0 {
				// One the project can't use reads as not there, as a name can't tell
				// which (slice 2 review, H2).
				return nil, fmt.Errorf("the connection %s doesn't reach %s's project, so nothing was planned. Let every project use it on Connections, or add one from the install sheet, which reaches the project the install makes", c, e.GetName())
			}
			if i < 0 {
				return nil, fmt.Errorf("%s's need %s takes %s, not %q", e.GetName(), name, strings.Join(choicesOf(options), " or "), choice)
			}
			out[name] = options[i]
		}
	}
	return out, nil
}

// onlyConnection is the connection a need is met by unless a person picks another,
// by its index in options: the only one that can meet it, or else the only one of
// those named for the app's environment (ADR 0099). It's -1 where there's no such one.
func onlyConnection(options []Way) int {
	var usable, named []int
	for i := range options {
		if c := options[i].Connection; c != nil && options[i].Why == "" {
			usable = append(usable, i)
			if c.Named {
				named = append(named, i)
			}
		}
	}
	switch {
	case len(usable) == 1:
		return usable[0]
	case len(named) == 1:
		return named[0]
	}
	return -1
}

// choicesOf are the choices of the ways that can meet a need.
func choicesOf(options []Way) []string {
	var out []string
	for i := range options {
		if options[i].Why == "" {
			out = append(out, options[i].Choice())
		}
	}
	return out
}

// pinnedWay is the way an install met a need, as its pin says: the entry's own
// service, or the service it added, as it was. It refuses a release that doesn't take
// what runs: another engine, a version outside the release's, or its own service on
// another engine, whose data the one that runs keeps in a form the other can't read.
func pinnedWay(n *catalogv1.Need, p *resourcev1.PinnedNeed, options []Way) (Way, error) {
	eng := catalogv1.Engine(p.GetEngine())
	runs := strings.TrimSpace(EngineName(eng) + " " + p.GetVersion())
	what := strings.ToLower(cmp.Or(n.GetLabel(), n.GetName()))
	if p.GetUnset() {
		if !n.GetOptional() {
			return Way{}, fmt.Errorf("this release of the app needs its %s, which the install runs without, so it stays on its release. Set up its %s first", what, what)
		}
		return Way{Need: n, Unset: true, pin: p}, nil
	}
	if p.GetOwn() {
		// Disconnected, the app keeps its values: no release links it again unasked.
		if n.GetKind() == catalogv1.Kind_KIND_UNSPECIFIED {
			return Way{}, fmt.Errorf("this release of the app needs a database for its %s, which the install meets with values of its own, so it stays on its release", what)
		}
		return Way{Need: n, Own: true, pin: p}, nil
	}
	if name := p.GetConnection(); name != "" {
		// The connection it reads stays, as the project may still use it.
		i := slices.IndexFunc(options, func(w Way) bool { return w.Connection != nil && w.Connection.Name == name })
		switch {
		case n.GetKind() == catalogv1.Kind_KIND_UNSPECIFIED:
			return Way{}, fmt.Errorf("this release of the app needs a database for its %s, which the install meets with the connection %s, so it stays on its release", what, name)
		case i < 0:
			return Way{}, fmt.Errorf("the install meets its %s with the connection %s, which this project can't use any more, so it stays on its release. Let the project use %s, or meet the need another way", what, name, name)
		// Another app that reads the connection since doesn't move this one off it.
		case options[i].Why != "" && options[i].Why != servesWhy(n, options[i].Connection):
			return Way{}, fmt.Errorf("this release of the app can't keep its %s on the connection %s: %s, so it stays on its release", what, name, options[i].Why)
		}
		w := options[i]
		c := *w.Connection
		c.Prefix = p.GetPrefix()
		w.Connection, w.pin, w.Why = &c, p, ""
		return w, nil
	}
	if eng != catalogv1.Engine_ENGINE_UNSPECIFIED {
		a := acceptOf(n, eng)
		switch {
		case a == nil:
			return Way{}, fmt.Errorf("this release of the app takes %s for its %s, and the install runs %s, so it stays on its release", acceptWords(n), what, runs)
		case len(link.Numbers(p.GetVersion())) > 0 && !link.Takes(a.GetVersions(), p.GetVersion()):
			return Way{}, fmt.Errorf("this release of the app takes %s for its %s, and the install runs %s, so it stays on its release", acceptWords(n), what, runs)
		}
	}
	if p.GetEnvironment() != "" {
		// A server it shares stays as it was; the link says the rest.
		return Way{
			Need: n, Engine: eng, Version: p.GetVersion(), pin: p,
			Existing: &Candidate{EnvironmentID: p.GetEnvironment(), Service: p.GetMeets()},
			Database: p.GetDatabase(), User: p.GetUser(), Password: p.GetPassword(),
		}, nil
	}
	if len(p.GetFile()) == 0 {
		for i := range options {
			w := &options[i]
			if w.From != nil || w.Existing != nil {
				continue
			}
			if eng != catalogv1.Engine_ENGINE_UNSPECIFIED && w.Engine != eng {
				return Way{}, fmt.Errorf("this release of the app runs %s for its %s, where the install runs %s, which keeps its data in a form %s can't read, so it stays on its release", EngineName(w.Engine), what, runs, EngineName(w.Engine))
			}
			return *w, nil
		}
		return Way{}, fmt.Errorf("this release of the app brings no service for %s, which the install ran its own of, so it stays on its release", what)
	}
	return Way{Need: n, Engine: eng, Version: p.GetVersion(), pin: p}, nil
}

func needList(e *catalogv1.Entry) string {
	var names []string
	for _, n := range e.GetNeeds() {
		names = append(names, n.GetName())
	}
	if len(names) == 0 {
		return "It needs nothing."
	}
	return "It needs " + strings.Join(names, ", ") + "."
}

func acceptWords(n *catalogv1.Need) string {
	if n.GetKind() != catalogv1.Kind_KIND_UNSPECIFIED {
		return kindWords[n.GetKind()]
	}
	var out []string
	for _, a := range n.GetAccepts() {
		w := EngineName(a.GetEngine())
		if a.GetVersions() != "" {
			w += " " + a.GetVersions()
		}
		out = append(out, w)
	}
	return strings.Join(out, " or ")
}

// NeedFeatures are what a need asks of what meets it, such as presigned URLs, from
// each of its accepts, each once, in order.
func NeedFeatures(n *catalogv1.Need) []catalogv1.Feature {
	var out []catalogv1.Feature
	for _, a := range n.GetAccepts() {
		out = append(out, a.GetFeatures()...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// FeatureWords say what features ask of storage, as "signed URLs and uploads in
// parts".
func FeatureWords(fs []catalogv1.Feature) string {
	words := make([]string, 0, len(fs))
	for _, f := range fs {
		words = append(words, cmp.Or(featureWords[f], strings.ToLower(strings.TrimPrefix(f.String(), "FEATURE_"))))
	}
	return strings.Join(words, " and ")
}

var featureWords = map[catalogv1.Feature]string{
	catalogv1.Feature_FEATURE_PRESIGNED_URLS: "signed URLs",
	catalogv1.Feature_FEATURE_VIRTUAL_HOST:   "the bucket in the host name",
	catalogv1.Feature_FEATURE_PATH_STYLE:     "the bucket in the path",
	catalogv1.Feature_FEATURE_MULTIPART:      "uploads in parts",
	catalogv1.Feature_FEATURE_BATCH_DELETE:   "deleting many objects at once",
	catalogv1.Feature_FEATURE_CORS:           "uploads straight from browsers",
	catalogv1.Feature_FEATURE_PUBLIC_READ:    "reading objects at its public address",
	catalogv1.Feature_FEATURE_OBJECT_ACL:     "an ACL on each object",
	catalogv1.Feature_FEATURE_PRESIGNED_POST: "form uploads from browsers",
	catalogv1.Feature_FEATURE_SDK_CHECKSUMS:  "the checksums AWS's SDKs send",
}

// kindWords say what a need of each kind beyond the databases takes.
var kindWords = map[catalogv1.Kind]string{
	catalogv1.Kind_KIND_S3:   "S3 storage",
	catalogv1.Kind_KIND_SMTP: "a mail server",
}

// databaseService is an engine entry's service that runs the database, by name, and
// its YAML: the one its databases names, or else the first whose image runs it.
func databaseService(from *catalogv1.Entry, eng catalogv1.Engine) (string, error) {
	services, err := composeServices(from.GetCompose())
	if err != nil {
		return "", err
	}
	names := from.GetDatabases()
	if len(names) == 0 {
		names = slices.Sorted(maps.Keys(services))
	}
	for _, name := range names {
		image, _ := mapGet(services[name], "image").(string)
		if EngineOfImage(image) == eng {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s runs no %s", from.GetName(), EngineName(eng))
}

// Needed is an entry as an install that meets its needs so runs it: its Compose file
// with each new service from the catalog in place of the entry's own, and its inputs,
// with a value for each name a need sets. Those inputs are the install's own: a
// person gives none of them, and Fill gives each its value.
type Needed struct {
	Entry *catalogv1.Entry
	// Plain are the names needs set that hold no password, which the app's services
	// use as the environment's variables, by reference (Filled.Link).
	Plain []string
	// Pinned is how each need is met, for the install's pin.
	Pinned map[string]*resourcev1.PinnedNeed
	// Links are each need, met, for the plan.
	Links map[string]*resourcev1.Link
	// Shared are the needs met by a server Dockwell runs already, whose database,
	// user and network the plan makes too.
	Shared []Way
	// Users are the database users the plan makes on a new server beside the app,
	// whose image makes only its administrator, by name, each with its password in
	// the need's secret in the app's environment.
	Users map[string]*resourcev1.DatabaseUser
	// Written are the URLs needs set, each a secret of the app's environment that
	// Dockwell writes from the need's parts there, by name. A secret, as a URL most
	// often holds the password, and one kind of value for every URL reads the same
	// through an upgrade. Fill gives each a value of no meaning, which the install
	// drops.
	Written map[string]*resourcev1.ValueTemplate
	// Inside are the names in Written that hold a value of a service's environment
	// with a secret inside it, whose templates Fix writes the plain values into
	// (#570).
	Inside map[string]bool
	// InsideKeys are those names by the value they hold, by InsideKey.
	InsideKeys map[string]string
	// Taken are the values, by InsideKey, whose secret what runs names them by the
	// release takes for an input or a need's URL of its own, with that secret's name
	// (NeedAs). Each takes a new name, and an upgrade refuses where the secret it
	// leaves holds what a person gave it (#570's check 2, M1).
	Taken map[string]string
	// Read are the names needs met by a connection set, each read from the
	// connection through the link named for its need, by name (ADR 0099). Fill gives
	// each a value of no meaning, which the install drops.
	Read map[string]ReadFrom
	// Reaches are the connections needs are met by that don't reach the project the
	// install makes yet, by name: its plan gives each that project (ADR 0099).
	Reaches []string
	// Kept are the names a need the app's own values meet sets, each with whether
	// it's a secret: the environment holds each as a Disconnect left it, so the
	// install writes none of them. Fill gives each a value of no meaning, which the
	// install drops.
	Kept map[string]bool
}

// ReadFrom is a name a link to a connection sets: the connection, the link, the
// part it holds, and whether it's a secret, as a key is.
type ReadFrom struct {
	Connection, Link string
	Part             resourcev1.LinkPart
	Secret           bool
}

// connectedNeed is what a need met by a connection adds: a link to the connection,
// named for the need, which sets each name it reads; an input of no meaning for
// each such name, which the install drops for the value the link reads, a key as a
// secret; the app's words for whether it's met and where the bucket goes, and the
// prefix its objects keep to, as plain values. It returns the need's pin and its
// plain names.
func connectedNeed(n *catalogv1.Need, c ConnectionWay, out *Needed, adds *[]*catalogv1.Input) (*resourcev1.PinnedNeed, []string, error) {
	if n.GetKind() == catalogv1.Kind_KIND_SMTP {
		return mailNeed(n, c, out, adds)
	}
	l := &resourcev1.Link{Label: cmp.Or(n.GetLabel(), "Storage"), Connection: c.Name, Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: c.Prefix}
	// What the app asks of the bucket goes with the link, so a check of it tries each
	// (the slice 1 review, M4). The two enums share their numbers.
	for _, f := range NeedFeatures(n) {
		l.Features = append(l.Features, resourcev1.LinkFeature(f))
	}
	var plain []string
	literal := func(name, value string) {
		*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: value, Optional: true})
		plain = append(plain, name)
	}
	for _, s := range n.GetSets() {
		name, part := s.GetName(), s.GetPart()
		switch part {
		case catalogv1.Part_PART_MET:
			literal(name, fixedWord(s, "met"))
		case catalogv1.Part_PART_PATH_STYLE:
			word := fixedWord(s, "off")
			if c.PathStyle {
				word = fixedWord(s, "on")
			}
			literal(name, word)
		case catalogv1.Part_PART_KEY_PREFIX:
			// The app keeps to the prefix, which the link records by the name that
			// tells it, so a shared connection may serve another app beside it. The
			// value is read from the link, which owns it (the re-check's N3).
			literal(name, "")
			out.Read[name] = ReadFrom{Connection: c.Name, Link: n.GetName(), Part: resourcev1.LinkPart_LINK_PART_KEY_PREFIX}
			l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: name, Part: resourcev1.LinkPart_LINK_PART_KEY_PREFIX})
		case catalogv1.Part_PART_ENDPOINT, catalogv1.Part_PART_REGION, catalogv1.Part_PART_BUCKET, catalogv1.Part_PART_PUBLIC_URL:
			literal(name, "")
			out.Read[name] = ReadFrom{Connection: c.Name, Link: n.GetName(), Part: resourcev1.LinkPart(part)}
			l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: name, Part: resourcev1.LinkPart(part)})
		case catalogv1.Part_PART_ACCESS_KEY_ID, catalogv1.Part_PART_SECRET_KEY:
			// A key goes only as a secret, which no plan shows.
			*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
			out.Read[name] = ReadFrom{Connection: c.Name, Link: n.GetName(), Part: resourcev1.LinkPart(part), Secret: true}
			l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: name, Part: resourcev1.LinkPart(part)})
		default:
			return nil, nil, fmt.Errorf("the need %s sets %s as %s, which a connection can't meet yet", n.GetName(), name, part)
		}
	}
	out.Links[n.GetName()] = l
	if c.Widens && !slices.Contains(out.Reaches, c.Name) {
		out.Reaches = append(out.Reaches, c.Name)
	}
	return &resourcev1.PinnedNeed{Connection: c.Name, Prefix: c.Prefix, Plain: plain}, plain, nil
}

// mailNeed is connectedNeed for a mail server: a link to the connection, named for
// the need, which sets each name it reads, a user and a password as secrets; the
// app's word for whether it's met and for the connection's security as plain values.
// The sender is the connection's until the link says one of its app's own, which an
// install keeps (ADR 0099).
func mailNeed(n *catalogv1.Need, c ConnectionWay, out *Needed, adds *[]*catalogv1.Input) (*resourcev1.PinnedNeed, []string, error) {
	l := &resourcev1.Link{Label: cmp.Or(n.GetLabel(), "Mail"), Connection: c.Name, Kind: resourcev1.LinkKind_LINK_KIND_SMTP}
	var plain []string
	literal := func(name, value string) {
		*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: value, Optional: true})
		plain = append(plain, name)
	}
	for _, s := range n.GetSets() {
		name, part := s.GetName(), s.GetPart()
		switch part {
		case catalogv1.Part_PART_MET:
			literal(name, fixedWord(s, "met"))
		case catalogv1.Part_PART_SECURITY:
			literal(name, fixedWord(s, c.Security))
			l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: name, Part: resourcev1.LinkPart_LINK_PART_SECURITY})
		case catalogv1.Part_PART_HOST, catalogv1.Part_PART_PORT, catalogv1.Part_PART_SENDER, catalogv1.Part_PART_SENDER_NAME:
			literal(name, "")
			out.Read[name] = ReadFrom{Connection: c.Name, Link: n.GetName(), Part: resourcev1.LinkPart(part)}
			l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: name, Part: resourcev1.LinkPart(part)})
		case catalogv1.Part_PART_USER, catalogv1.Part_PART_PASSWORD, catalogv1.Part_PART_URL:
			// Who signs in goes only as a secret, which no plan shows, as a provider's
			// user is often its key's ID.
			*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
			out.Read[name] = ReadFrom{Connection: c.Name, Link: n.GetName(), Part: resourcev1.LinkPart(part), Secret: true}
			l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: name, Part: resourcev1.LinkPart(part)})
		default:
			return nil, nil, fmt.Errorf("the need %s sets %s as %s, which a connection can't meet yet", n.GetName(), name, part)
		}
	}
	out.Links[n.GetName()] = l
	if c.Widens && !slices.Contains(out.Reaches, c.Name) {
		out.Reaches = append(out.Reaches, c.Name)
	}
	return &resourcev1.PinnedNeed{Connection: c.Name, Plain: plain}, plain, nil
}

// ownNeed is what a need the app's own values meet adds, as a Disconnect left them:
// the app's word for met, and its path style and prefix as the pin kept them, as
// plain values; and an input of no meaning for each name a connection gave it,
// which the install drops, so it writes none of them. A name the pin leaves out
// stays out. It returns the need's plain names.
func ownNeed(n *catalogv1.Need, p *resourcev1.PinnedNeed, out *Needed, adds *[]*catalogv1.Input) []string {
	var plain []string
	literal := func(name, value string) {
		*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: value, Optional: true})
		plain = append(plain, name)
	}
	for _, s := range n.GetSets() {
		name := s.GetName()
		if slices.Contains(p.GetLeftOut(), name) {
			continue
		}
		switch s.GetPart() {
		case catalogv1.Part_PART_MET:
			literal(name, fixedWord(s, "met"))
		case catalogv1.Part_PART_PATH_STYLE, catalogv1.Part_PART_KEY_PREFIX, catalogv1.Part_PART_SECURITY:
			literal(name, "")
		case catalogv1.Part_PART_ACCESS_KEY_ID, catalogv1.Part_PART_SECRET_KEY:
			*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
			out.Kept[name] = true
		case catalogv1.Part_PART_USER, catalogv1.Part_PART_PASSWORD, catalogv1.Part_PART_URL:
			if n.GetKind() != catalogv1.Kind_KIND_SMTP {
				literal(name, "")
				out.Kept[name] = false
				continue
			}
			*adds = append(*adds, &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
			out.Kept[name] = true
		default:
			literal(name, "")
			out.Kept[name] = false
		}
	}
	return plain
}

// fixedWord is a set's word for one of its part's fixed values, such as "met".
func fixedWord(s *catalogv1.NeedSet, value string) string {
	for _, w := range s.GetWords() {
		if w.GetValue() == value {
			return w.GetWord()
		}
	}
	return ""
}

// Need is the entry as an install that meets its needs by met runs it. An entry with
// no needs is as it is.
func Need(e *catalogv1.Entry, met map[string]Way) (*Needed, error) {
	return NeedAs(e, met, nil)
}

// NeedAs is Need for an install that runs already: each value with a secret inside
// keeps the secret running names it by, by InsideKey, so a release that names it
// otherwise, as when a service it adds before takes the name, never moves the value
// to another secret, or a person's own value to another service (#570's recheck,
// N1). A value it adds takes a name none of running has.
func NeedAs(e *catalogv1.Entry, met map[string]Way, running map[string]string) (*Needed, error) {
	out := &Needed{
		Entry: e, Pinned: map[string]*resourcev1.PinnedNeed{}, Links: map[string]*resourcev1.Link{}, Users: map[string]*resourcev1.DatabaseUser{},
		Written: map[string]*resourcev1.ValueTemplate{}, Read: map[string]ReadFrom{}, Kept: map[string]bool{},
	}
	if len(e.GetNeeds()) == 0 {
		if err := inside(out, running); err != nil {
			return out, err
		}
		return out, loopback(out)
	}
	file := e.GetCompose()
	inputs := slices.Clone(e.GetInputs())
	var adds []*catalogv1.Input
	for _, n := range e.GetNeeds() {
		w, ok := met[n.GetName()]
		if !ok {
			return nil, fmt.Errorf("nothing says how %s's need %s is met", e.GetName(), n.GetName())
		}
		if w.Unset {
			var err error
			var plain []string
			if file, plain, err = unsetNeed(file, n, &adds); err != nil {
				return nil, err
			}
			out.Plain = append(out.Plain, plain...)
			out.Pinned[n.GetName()] = &resourcev1.PinnedNeed{Unset: true, Service: n.GetService(), Plain: plain, LeftOut: leftOut(n)}
			continue
		}
		if w.Own {
			var err error
			if file, err = withoutNames(file, w.pin.GetLeftOut()); err != nil {
				return nil, err
			}
			plain := ownNeed(n, w.pin, out, &adds)
			out.Pinned[n.GetName()] = &resourcev1.PinnedNeed{Own: true, Prefix: w.pin.GetPrefix(), Plain: plain, LeftOut: w.pin.GetLeftOut()}
			out.Plain = append(out.Plain, plain...)
			continue
		}
		if w.Connection != nil {
			pinned, plain, err := connectedNeed(n, *w.Connection, out, &adds)
			if err != nil {
				return nil, err
			}
			out.Pinned[n.GetName()] = pinned
			out.Plain = append(out.Plain, plain...)
			continue
		}
		password := passwordName(n)
		pinned := &resourcev1.PinnedNeed{Engine: resourcev1.LinkEngine(w.Engine), Version: w.Version}
		if w.Existing != nil {
			// Its own service goes, and nothing waits for it: the server it shares is
			// in another project.
			var err error
			if file, err = withoutService(file, n.GetService()); err != nil {
				return nil, err
			}
			pinned.Service, pinned.Environment, pinned.Meets = n.GetService(), w.Existing.EnvironmentID, w.Existing.Service
			pinned.Database, pinned.User, pinned.Password = w.Database, w.User, w.Password
			out.Shared = append(out.Shared, w)
		} else if w.From != nil || len(w.pin.GetFile()) > 0 {
			added, more, err := w.service(password)
			if err != nil {
				return nil, err
			}
			if w.From != nil {
				// It's reached where its readers are, on the networks the entry's own was
				// on, or else each one a service that reads its names is on.
				if added, err = onNetworksOf(added, file, n); err != nil {
					return nil, err
				}
			}
			if file, err = replaceService(file, n.GetService(), w.Service(), added); err != nil {
				return nil, err
			}
			// An input of the app's own of a name the server keeps its secret in would
			// give the app that secret, where another of its services reads it.
			parsed, err := composeServices(file)
			if err != nil {
				return nil, err
			}
			for _, m := range more {
				if !slices.ContainsFunc(inputs, func(in *catalogv1.Input) bool { return in.GetName() == m.GetName() }) {
					continue
				}
				for name, svc := range parsed {
					if name != w.Service() && uses(svc, m.GetName()) {
						return nil, fmt.Errorf("%s has an input %s, which the need %s's %s keeps its own secret in, and %s reads it. Rename the input", e.GetName(), m.GetName(), n.GetName(), EngineName(w.Engine), name)
					}
				}
			}
			if file, err = waitFor(file, n, w.Service()); err != nil {
				return nil, err
			}
			adds = append(adds, more...)
			if w.From != nil {
				pinned.Entry, pinned.Release, pinned.Catalog = w.From.GetId(), w.From.GetRelease(), w.FromCatalog
				pinned.Service, pinned.File = n.GetService(), []byte(added)
			} else {
				pinned = proto.CloneOf(w.pin)
			}
		}
		out.Pinned[n.GetName()] = pinned
		pinned.Plain = nil
		for _, s := range n.GetSets() {
			if !SecretPart(s) {
				pinned.Plain = append(pinned.Plain, s.GetName())
			}
		}
		values := needValues(n, w)
		if w.Existing != nil {
			values[catalogv1.Part_PART_DATABASE], values[catalogv1.Part_PART_USER] = w.Database, w.User
			if keyValue(w.Engine) {
				values[catalogv1.Part_PART_KEY_PREFIX] = w.User + ":"
			}
		}
		if w.makesUser(password) {
			user, db := values[catalogv1.Part_PART_USER], values[catalogv1.Part_PART_DATABASE]
			if _, taken := out.Users[user]; taken {
				return nil, fmt.Errorf("two of %s's needs name the user %s, which one environment has once. Give each need a user of its own", e.GetName(), user)
			}
			// It lives in the server it's made in, under the name the need's server
			// answers to, which a server pinned apart keeps.
			u := &resourcev1.DatabaseUser{Service: w.Service(), Password: password}
			if keyValue(w.Engine) && keepsPrefix(n) {
				u.KeyPrefix = values[catalogv1.Part_PART_KEY_PREFIX]
			}
			if db != user && !keyValue(w.Engine) {
				u.DatabaseName = db
			}
			out.Users[user] = u
		}
		for _, s := range n.GetSets() {
			if s.GetPart() == catalogv1.Part_PART_URL {
				t, parts, err := urlTemplate(e, n, w, s, values)
				if err != nil {
					return nil, err
				}
				for _, p := range parts {
					adds = append(adds, &catalogv1.Input{Name: p.name, Label: p.name, Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: p.value, Optional: true})
					if !slices.Contains(out.Plain, p.name) {
						out.Plain = append(out.Plain, p.name)
					}
				}
				// A value Fill makes and the install drops: the URL is written from the
				// parts, so no one gives it, and no secret of the app's holds it.
				adds = append(adds, &catalogv1.Input{Name: s.GetName(), Label: s.GetName(), Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
				out.Written[s.GetName()] = t
				continue
			}
			if SecretPart(s) {
				continue
			}
			value := values[s.GetPart()]
			if s.GetPart() == catalogv1.Part_PART_ENGINE {
				value = EngineWordOf(s, w.Engine)
			}
			// A part may be empty, as a user is on an engine with none, so it's optional.
			adds = append(adds, &catalogv1.Input{Name: s.GetName(), Label: s.GetName(), Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: value, Optional: true})
			out.Plain = append(out.Plain, s.GetName())
		}
		adds = append(adds, &catalogv1.Input{Name: password, Label: password, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32, SetOnce: n.GetSetOnce()})
		out.Links[n.GetName()] = linkOf(n, w, values)
		if w.Existing != nil {
			l := out.Links[n.GetName()]
			l.MeetsEnvironment, l.Service = w.Existing.EnvironmentID, w.Existing.Service
		}
	}
	// What only a service left out used goes with it, as a companion's inputs do. What
	// the owner's steps name is no service's, and stays, as their file needn't name it
	// (ADR 0105).
	owned := OwnerInputs(e)
	inputs = slices.DeleteFunc(inputs, func(in *catalogv1.Input) bool { return !usesInput(file, in.GetName()) && !owned[in.GetName()] })
	for _, add := range adds {
		if slices.ContainsFunc(inputs, func(in *catalogv1.Input) bool { return in.GetName() == add.GetName() }) {
			continue
		}
		inputs = append(inputs, add)
	}
	needed := proto.CloneOf(e)
	needed.Compose, needed.Inputs = file, inputs
	out.Entry = needed
	if err := inside(out, running); err != nil {
		return out, err
	}
	return out, loopback(out)
}

// PinnedPlain are the names an install's needs set that hold no password, as it ran,
// or else as entry names them, for a pin from before it kept them.
func PinnedPlain(pinned map[string]*resourcev1.PinnedNeed, entry *catalogv1.Entry) []string {
	var out []string
	kept := false
	for _, name := range slices.Sorted(maps.Keys(pinned)) {
		kept = kept || pinned[name].Plain != nil
		out = append(out, pinned[name].GetPlain()...)
	}
	if !kept {
		return PlainNames(entry)
	}
	return out
}

// withoutService takes the entry's own service out of a file, where it has one, with
// every depends_on that names it.
func withoutService(file, own string) (string, error) {
	if own == "" {
		return file, nil
	}
	file, err := Without(file, []string{own})
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", errors.New("its Compose file isn't YAML")
	}
	services := nodeGet(doc.Content[0], "services")
	for i := 1; services != nil && i < len(services.Content); i += 2 {
		if deps := nodeGet(services.Content[i], "depends_on"); deps != nil {
			dropDependency(services.Content[i], deps, own)
		}
	}
	return encode(&doc)
}

// MetFile is an install's file as it ran, from its pin: the entry's file with each
// service a need added in place of the entry's own.
func MetFile(file string, pinned map[string]*resourcev1.PinnedNeed) (string, error) {
	for _, name := range slices.Sorted(maps.Keys(pinned)) {
		p := pinned[name]
		if p.GetUnset() {
			var err error
			if file, err = withoutService(file, p.GetService()); err != nil {
				return "", err
			}
			if file, err = withoutNames(file, p.GetLeftOut()); err != nil {
				return "", err
			}
			continue
		}
		if p.GetOwn() {
			var err error
			if file, err = withoutNames(file, p.GetLeftOut()); err != nil {
				return "", err
			}
			continue
		}
		if p.GetEnvironment() != "" {
			var err error
			if file, err = withoutService(file, p.GetService()); err != nil {
				return "", err
			}
			continue
		}
		if len(p.GetFile()) == 0 {
			continue
		}
		// A server pinned apart keeps its own name; one the catalog added has the need's.
		as := name
		if p.GetService() != "" && p.GetService() != name && hasService(string(p.GetFile()), p.GetService()) {
			as = p.GetService()
		}
		var err error
		if file, err = replaceService(file, p.GetService(), as, string(p.GetFile())); err != nil {
			return "", err
		}
	}
	return file, nil
}

// unsetNeed is the file an install that leaves an optional need unset runs: its
// own service out, the names it sets left out of the app's environment, but its
// PART_MET set to the app's word for unmet, and those empty_when_unmet set empty,
// each a plain value adds gains, which it returns by name (ADR 0099).
func unsetNeed(file string, n *catalogv1.Need, adds *[]*catalogv1.Input) (string, []string, error) {
	file, err := withoutService(file, n.GetService())
	if err != nil {
		return "", nil, err
	}
	var plain []string
	for _, s := range n.GetSets() {
		value, kept := unsetValue(s)
		if !kept {
			continue
		}
		*adds = append(*adds, &catalogv1.Input{Name: s.GetName(), Label: s.GetName(), Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: value, Optional: true})
		plain = append(plain, s.GetName())
	}
	file, err = withoutNames(file, leftOut(n))
	return file, plain, err
}

// unsetValue is what a set holds while its need is unset, and whether it's there
// at all: PART_MET's word for unmet, unless that's empty, and empty for one
// empty_when_unmet.
func unsetValue(s *catalogv1.NeedSet) (string, bool) {
	if s.GetPart() == catalogv1.Part_PART_MET {
		for _, w := range s.GetWords() {
			if w.GetValue() == "unmet" {
				return w.GetWord(), w.GetWord() != ""
			}
		}
		return "", false
	}
	return "", s.GetEmptyWhenUnmet()
}

// leftOut are the names an unset need leaves out of the app's environment.
func leftOut(n *catalogv1.Need) []string {
	var out []string
	for _, s := range n.GetSets() {
		if _, kept := unsetValue(s); !kept {
			out = append(out, s.GetName())
		}
	}
	return out
}

// withoutNames takes out of each service's environment the entries whose whole
// value is one of names, as the check has each name a need sets used.
func withoutNames(file string, names []string) (string, error) {
	if len(names) == 0 {
		return file, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", errors.New("its Compose file isn't YAML")
	}
	whole := map[string]bool{}
	for _, name := range names {
		whole["${"+name+"}"] = true
	}
	services := nodeGet(doc.Content[0], "services")
	for i := 1; services != nil && i < len(services.Content); i += 2 {
		env := nodeGet(services.Content[i], "environment")
		if env == nil {
			continue
		}
		switch env.Kind {
		case yaml.SequenceNode:
			env.Content = slices.DeleteFunc(env.Content, func(n *yaml.Node) bool {
				_, v, ok := strings.Cut(n.Value, "=")
				return ok && whole[v]
			})
		case yaml.MappingNode:
			kept := env.Content[:0]
			for j := 0; j+1 < len(env.Content); j += 2 {
				if !whole[env.Content[j+1].Value] {
					kept = append(kept, env.Content[j], env.Content[j+1])
				}
			}
			env.Content = kept
		default:
		}
	}
	return encode(&doc)
}

// hasService reports a Compose file with a service of that name.
func hasService(file, name string) bool {
	services, err := composeServices(file)
	_, ok := services[name]
	return err == nil && ok
}

// PasswordName is the name a need's password is kept as in the app's environment:
// the one it sets, or one named for the need where it sets none, such as DB_PASSWORD.
func PasswordName(n *catalogv1.Need) string { return passwordName(n) }

// passwordName is PasswordName.
func passwordName(n *catalogv1.Need) string {
	for _, s := range n.GetSets() {
		if s.GetPart() == catalogv1.Part_PART_PASSWORD {
			return s.GetName()
		}
	}
	return strings.ToUpper(strings.ReplaceAll(n.GetName(), "-", "_")) + "_PASSWORD"
}

// databaseOf is the database a need is given: its own, or its name's.
func databaseOf(n *catalogv1.Need) string {
	return cmp.Or(n.GetDatabase(), strings.ReplaceAll(n.GetName(), "-", "_"))
}

// needValues are the values of a need's plain parts, met by w.
func needValues(n *catalogv1.Need, w Way) map[catalogv1.Part]string {
	host := w.Service()
	listen := w.Port
	if listen == 0 {
		listen = port(w.Engine)
	}
	p := strconv.FormatUint(uint64(listen), 10)
	db := databaseOf(n)
	user := cmp.Or(n.GetUser(), db)
	if keyValue(w.Engine) {
		// Its first numbered database, and the app's ACL user, named for the need,
		// but on a server whose one password is the app's, as an install's from
		// before had it.
		db, user = "0", cmp.Or(n.GetUser(), strings.ReplaceAll(n.GetName(), "-", "_"))
		if w.Existing == nil && !w.makesUser(passwordName(n)) {
			user = ""
		}
	}
	// The keys an app keeps to on a Redis or Valkey: its user's, or on a server that's
	// its alone, its need's name.
	prefix := ""
	if keyValue(w.Engine) {
		prefix = cmp.Or(user, strings.ReplaceAll(n.GetName(), "-", "_")) + ":"
	}
	tls := ""
	for _, s := range n.GetSets() {
		if s.GetPart() == catalogv1.Part_PART_TLS {
			// A new one beside the app is reached on the project's own network.
			tls = s.GetTls().GetOff()
		}
	}
	values := map[catalogv1.Part]string{
		catalogv1.Part_PART_HOST:       host,
		catalogv1.Part_PART_PORT:       p,
		catalogv1.Part_PART_ADDRESS:    host + ":" + p,
		catalogv1.Part_PART_DATABASE:   db,
		catalogv1.Part_PART_USER:       user,
		catalogv1.Part_PART_TLS:        tls,
		catalogv1.Part_PART_KEY_PREFIX: prefix,
	}
	if w.Engine == catalogv1.Engine_ENGINE_CLICKHOUSE {
		native := w.NativePort
		if native == 0 {
			native = port(w.Engine)
		}
		values[catalogv1.Part_PART_NATIVE_PORT] = strconv.FormatUint(uint64(native), 10)
	}
	return values
}

// urls are each engine's own form of a URL, for a URL part with no template.
var urls = map[catalogv1.Engine]string{
	catalogv1.Engine_ENGINE_POSTGRESQL: "postgres://{user}:{password}@{host}:{port}/{database}",
	catalogv1.Engine_ENGINE_MYSQL:      "mysql://{user}:{password}@{host}:{port}/{database}",
	catalogv1.Engine_ENGINE_MARIADB:    "mysql://{user}:{password}@{host}:{port}/{database}",
	catalogv1.Engine_ENGINE_REDIS:      "redis://{user}:{password}@{host}:{port}/{database}",
	catalogv1.Engine_ENGINE_VALKEY:     "redis://{user}:{password}@{host}:{port}/{database}",
	catalogv1.Engine_ENGINE_MONGODB:    "mongodb://{user}:{password}@{host}:{port}/{database}",
	catalogv1.Engine_ENGINE_CLICKHOUSE: "clickhouse://{user}:{password}@{host}:{port}/{database}",
}

// partName is a name of the app's environment a URL's part is kept as: the need's
// own where it sets that part, or else one named for the need, such as DB_HOST, and
// value what it holds, for a name the install adds.
type partName struct{ name, value string }

// urlTemplate is a URL part as the template Dockwell writes it from, of the
// environment's names, each part encoded for where it stands, and the names it adds
// for parts the need doesn't set (ADR 0084).
func urlTemplate(e *catalogv1.Entry, n *catalogv1.Need, w Way, s *catalogv1.NeedSet, values map[catalogv1.Part]string) (*resourcev1.ValueTemplate, []partName, error) {
	text := cmp.Or(s.GetTemplate(), urls[w.Engine])
	if text == "" {
		return nil, nil, fmt.Errorf("%s writes %s as a URL, and %s has no URL of its own. Give the need a template", e.GetName(), s.GetName(), EngineName(w.Engine))
	}
	prefix := strings.ToUpper(strings.ReplaceAll(n.GetName(), "-", "_")) + "_"
	taken := map[string]bool{}
	for _, in := range e.GetInputs() {
		taken[in.GetName()] = true
	}
	for _, other := range e.GetNeeds() {
		for _, set := range other.GetSets() {
			taken[set.GetName()] = true
		}
	}
	var added []partName
	nameOf := func(part catalogv1.Part) (string, error) {
		if part == catalogv1.Part_PART_PASSWORD {
			return passwordName(n), nil
		}
		for _, set := range n.GetSets() {
			if set.GetPart() == part {
				return set.GetName(), nil
			}
		}
		word := strings.TrimPrefix(catalogv1.Part_name[int32(part)], "PART_")
		name := prefix + word
		if taken[name] && !slices.ContainsFunc(added, func(p partName) bool { return p.name == name }) {
			if name = prefix + "LINK_" + word; taken[name] {
				return "", fmt.Errorf("%s has %s and %s already, which %s's URL %s would keep its %s in. Rename one", e.GetName(), prefix+word, name, n.GetName(), s.GetName(), strings.ToLower(word))
			}
		}
		if !slices.ContainsFunc(added, func(p partName) bool { return p.name == name }) {
			value := values[part]
			if part == catalogv1.Part_PART_TLS {
				// A new one beside the app, or one shared on a link network, takes none.
				value = s.GetTls().GetOff()
			}
			added = append(added, partName{name: name, value: value})
		}
		return name, nil
	}
	// Where each part stands: the user and password before the host's @, the database
	// in the path, anything after ? in the query.
	authority, userinfo, query := 0, -1, strings.IndexByte(text, '?')
	if i := strings.Index(text, "://"); i >= 0 {
		authority = i + 3
	}
	end := len(text)
	if i := strings.IndexAny(text[authority:], "/?#"); i >= 0 {
		end = authority + i
	}
	if i := strings.LastIndexByte(text[authority:end], '@'); i >= 0 {
		userinfo = authority + i
	}
	encodingAt := func(at int) resourcev1.Encoding {
		switch {
		case at < userinfo:
			return resourcev1.Encoding_ENCODING_USERINFO
		case query >= 0 && at > query:
			return resourcev1.Encoding_ENCODING_QUERY
		case at >= end:
			return resourcev1.Encoding_ENCODING_PATH
		default:
			return resourcev1.Encoding_ENCODING_UNSPECIFIED
		}
	}
	t := &resourcev1.ValueTemplate{}
	var b strings.Builder
	last := 0
	for _, m := range placeholder.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(strings.ReplaceAll(text[last:m[0]], "$", "$$"))
		last = m[1]
		word := text[m[2]:m[3]]
		parts := []catalogv1.Part{catalogv1.Part(catalogv1.Part_value["PART_"+strings.ToUpper(word)])}
		if word == "address" {
			parts = []catalogv1.Part{catalogv1.Part_PART_HOST, catalogv1.Part_PART_PORT}
		}
		for i, part := range parts {
			if i > 0 {
				b.WriteByte(':')
			}
			name, err := nameOf(part)
			if err != nil {
				return nil, nil, err
			}
			b.WriteString("${" + name + "}")
			if !slices.ContainsFunc(t.GetNames(), func(x *resourcev1.TemplateName) bool { return x.GetName() == name }) {
				t.Names = append(t.Names, &resourcev1.TemplateName{Name: name, Secret: part == catalogv1.Part_PART_PASSWORD, Encoding: encodingAt(m[0])})
			}
		}
	}
	b.WriteString(strings.ReplaceAll(text[last:], "$", "$$"))
	t.Text = b.String()
	return t, added, nil
}

// linkOf is a need, met by w, as its link says it.
func linkOf(n *catalogv1.Need, w Way, values map[catalogv1.Part]string) *resourcev1.Link {
	l := &resourcev1.Link{
		Label: n.GetLabel(), Service: w.Service(), Engine: resourcev1.LinkEngine(w.Engine), Version: w.Version,
		DatabaseName: values[catalogv1.Part_PART_DATABASE], User: values[catalogv1.Part_PART_USER], SetOnce: n.GetSetOnce(),
	}
	// A Redis or Valkey has numbered databases, not named ones; its app's user is the
	// link's, whose password a value from another environment reads.
	if keyValue(w.Engine) {
		l.DatabaseName = ""
	}
	for _, a := range n.GetAccepts() {
		l.Accepts = append(l.Accepts, &resourcev1.LinkAccept{Engine: resourcev1.LinkEngine(a.GetEngine()), Versions: a.GetVersions()})
	}
	for _, s := range n.GetSets() {
		l.Sets = append(l.Sets, &resourcev1.LinkSet{
			Name: s.GetName(), Part: resourcev1.LinkPart(s.GetPart()), Template: s.GetTemplate(),
			TlsOn: s.GetTls().GetOn(), TlsOff: s.GetTls().GetOff(),
		})
	}
	return l
}

// service is the service a way adds under its need's name, as YAML with its volumes,
// and the inputs it uses: what its pin kept, or the catalog entry's database service,
// without its ports, with its volumes and inputs named for the need, and with its
// image making the app's database and user from the need's names.
func (w Way) service(password string) (string, []*catalogv1.Input, error) {
	n := w.Need
	prefix := strings.ToUpper(strings.ReplaceAll(n.GetName(), "-", "_")) + "_"
	if w.From == nil {
		// What the pin kept names its inputs as the install did: each is a secret the
		// install has, which an upgrade holds as it is.
		file := string(w.pin.GetFile())
		var more []*catalogv1.Input
		for _, m := range usedNames.FindAllStringSubmatch(file, -1) {
			if m[1] == password || slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetName() == m[1] }) {
				continue
			}
			more = append(more, &catalogv1.Input{Name: m[1], Label: m[1], Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
		}
		return file, more, nil
	}
	info := engines[w.Engine]
	name, err := databaseService(w.From, w.Engine)
	if err != nil {
		return "", nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(w.From.GetCompose()), &doc); err != nil || len(doc.Content) != 1 {
		return "", nil, fmt.Errorf("%s's Compose file isn't YAML", w.From.GetName())
	}
	root := doc.Content[0]
	svc := nodeGet(nodeGet(root, "services"), name)
	if svc == nil || svc.Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("%s has no service %s", w.From.GetName(), name)
	}
	// It publishes nothing, and waits for nothing of its entry's.
	svc.Content = deleteKeys(svc.Content, "ports", "depends_on", "container_name")
	// Its volumes are named for the need, beside the app's own.
	renamed := map[string]string{}
	for _, v := range namedVolumes(svc) {
		renamed[v] = n.GetName() + "-" + v
	}
	renameVolumes(svc, renamed)
	// Its inputs are named for the need too, and any it uses must be one it generates.
	var more []*catalogv1.Input
	var bad error
	walkStrings(svc, func(s *yaml.Node) {
		s.Value = usedNames.ReplaceAllStringFunc(s.Value, func(m string) string {
			in := m[2:]
			if in == info.alone && !w.plansUser(password) {
				// The server's one password is the app's, as the need's.
				return "${" + password
			}
			i := slices.IndexFunc(w.From.GetInputs(), func(x *catalogv1.Input) bool { return x.GetName() == in })
			if i < 0 || w.From.GetInputs()[i].GetGenerate() == 0 {
				bad = fmt.Errorf("%s's %s uses %s, which a new one beside an app can't ask for", w.From.GetName(), name, in)
				return m
			}
			x := w.From.GetInputs()[i]
			if !slices.ContainsFunc(more, func(y *catalogv1.Input) bool { return y.GetName() == prefix+in }) {
				more = append(more, &catalogv1.Input{Name: prefix + in, Label: x.GetLabel(), Type: x.GetType(), Generate: x.GetGenerate(), GenerateAs: x.GetGenerateAs(), Characters: x.GetCharacters()})
			}
			return "${" + prefix + in
		})
	})
	if bad != nil {
		return "", nil, bad
	}
	// The image makes the app's database and user from the need's names, or its
	// values where the need sets none, but where the plan makes them, or the app
	// signs in with the server's one password.
	if !info.planned && info.alone == "" {
		env := map[string]string{info.password: "${" + password + "}"}
		values := needValues(n, w)
		for _, part := range []struct {
			variable string
			part     catalogv1.Part
		}{{info.database, catalogv1.Part_PART_DATABASE}, {info.user, catalogv1.Part_PART_USER}} {
			env[part.variable] = values[part.part]
			for _, s := range n.GetSets() {
				if s.GetPart() == part.part {
					env[part.variable] = "${" + s.GetName() + "}"
				}
			}
		}
		setEnvironment(svc, env)
	}
	// What it adds: the service, under the need's name, and the volumes it names.
	out := &yaml.Node{Kind: yaml.MappingNode}
	services := &yaml.Node{Kind: yaml.MappingNode}
	services.Content = append(services.Content, scalar(n.GetName()), svc)
	out.Content = append(out.Content, scalar("services"), services)
	if len(renamed) > 0 {
		volumes := &yaml.Node{Kind: yaml.MappingNode}
		from := nodeGet(root, "volumes")
		for _, old := range slices.Sorted(maps.Keys(renamed)) {
			spec := nodeGet(from, old)
			if spec == nil {
				spec = &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			}
			volumes.Content = append(volumes.Content, scalar(renamed[old]), spec)
		}
		out.Content = append(out.Content, scalar("volumes"), volumes)
	}
	text, err := encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{out}})
	return text, more, err
}

// replaceService takes the entry's own service out of a file, where it has one, and
// puts in what added gives, under name, with its volumes. A service that waited for
// the entry's own waits for the one added, when it has the same name, and else for
// nothing.
func replaceService(file, own, name, added string) (string, error) {
	var err error
	if own != "" {
		if file, err = Without(file, []string{own}); err != nil {
			return "", err
		}
	}
	var doc, more yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", errors.New("its Compose file isn't YAML")
	}
	if err := yaml.Unmarshal([]byte(added), &more); err != nil || len(more.Content) != 1 {
		return "", errors.New("the service a need adds isn't YAML")
	}
	root, add := doc.Content[0], more.Content[0]
	services := nodeGet(root, "services")
	if own != "" && own != name {
		condition := "service_started"
		if nodeGet(nodeGet(nodeGet(add, "services"), name), "healthcheck") != nil {
			condition = "service_healthy"
		}
		for i := 1; i < len(services.Content); i += 2 {
			if deps := nodeGet(services.Content[i], "depends_on"); deps != nil {
				moveDependency(deps, own, name, condition)
			}
		}
	}
	for _, key := range []string{"services", "volumes"} {
		from := nodeGet(add, key)
		if from == nil {
			continue
		}
		into := nodeGet(root, key)
		if into == nil {
			into = &yaml.Node{Kind: yaml.MappingNode}
			root.Content = append(root.Content, scalar(key), into)
		}
		for i := 0; i+1 < len(from.Content); i += 2 {
			if nodeGet(into, from.Content[i].Value) != nil {
				return "", fmt.Errorf("its Compose file has a %s called %s already, which a need's new one would take", strings.TrimSuffix(key, "s"), from.Content[i].Value)
			}
			into.Content = append(into.Content, from.Content[i], from.Content[i+1])
		}
	}
	return encode(&doc)
}

// waitFor has each service that reads a need's names wait for the new one beside it,
// which the need's name names, healthy where it says how it's healthy: an entry from
// another store that brought none waits for nothing, and its app would start first,
// before the user the plan makes for it. Only the order a change makes them in reads
// it, so a file without it, as MetFile's, runs the same.
func waitFor(file string, n *catalogv1.Need, as string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", errors.New("its Compose file isn't YAML")
	}
	services := nodeGet(doc.Content[0], "services")
	added := nodeGet(services, as)
	if added == nil {
		return file, nil
	}
	condition := "service_started"
	if nodeGet(added, "healthcheck") != nil {
		condition = "service_healthy"
	}
	parsed, err := composeServices(file)
	if err != nil {
		return "", err
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, svc := services.Content[i].Value, services.Content[i+1]
		if name == as || svc.Kind != yaml.MappingNode ||
			!slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return uses(parsed[name], s.GetName()) }) {
			continue
		}
		deps := nodeGet(svc, "depends_on")
		switch {
		case deps == nil:
			deps = &yaml.Node{Kind: yaml.MappingNode}
			svc.Content = append(svc.Content, scalar("depends_on"), deps)
		case deps.Kind == yaml.SequenceNode:
			if slices.ContainsFunc(deps.Content, func(d *yaml.Node) bool { return d.Value == as }) {
				continue
			}
			deps.Content = append(deps.Content, scalar(as))
			continue
		case nodeGet(deps, as) != nil:
			continue
		}
		wait := &yaml.Node{Kind: yaml.MappingNode}
		wait.Content = append(wait.Content, scalar("condition"), scalar(condition))
		deps.Content = append(deps.Content, scalar(as), wait)
	}
	return encode(&doc)
}

// dropDependency takes a service out of what another depends on, in either form, and
// the key itself once it names nothing.
func dropDependency(svc, deps *yaml.Node, name string) {
	switch deps.Kind {
	case yaml.SequenceNode:
		deps.Content = slices.DeleteFunc(deps.Content, func(n *yaml.Node) bool { return n.Value == name })
	case yaml.MappingNode:
		deps.Content = deleteKeys(deps.Content, name)
	default:
	}
	if len(deps.Content) == 0 {
		svc.Content = deleteKeys(svc.Content, "depends_on")
	}
}

// moveDependency has what waited for one service wait for another instead, as it
// starts or once it's healthy, writing what it waits for as a map.
func moveDependency(deps *yaml.Node, from, to, condition string) {
	waits := false
	switch deps.Kind {
	case yaml.SequenceNode:
		waits = slices.ContainsFunc(deps.Content, func(n *yaml.Node) bool { return n.Value == from })
		if !waits {
			return
		}
		var mapped []*yaml.Node
		for _, d := range deps.Content {
			if d.Value == from {
				continue
			}
			started := &yaml.Node{Kind: yaml.MappingNode}
			started.Content = append(started.Content, scalar("condition"), scalar("service_started"))
			mapped = append(mapped, scalar(d.Value), started)
		}
		deps.Kind, deps.Tag, deps.Style, deps.Content = yaml.MappingNode, "!!map", 0, mapped
	case yaml.MappingNode:
		waits = nodeGet(deps, from) != nil
		deps.Content = deleteKeys(deps.Content, from)
	default:
	}
	if !waits || nodeGet(deps, to) != nil {
		return
	}
	wait := &yaml.Node{Kind: yaml.MappingNode}
	wait.Content = append(wait.Content, scalar("condition"), scalar(condition))
	deps.Content = append(deps.Content, scalar(to), wait)
}

// onNetworksOf has the service a need adds join the networks the entry's own service
// for it was on, with its aliases there, or else each network a service that reads
// the need's names is on, the default one for a service that names none. One that
// joins only the default network, as most apps' do, is left as it is.
func onNetworksOf(added, file string, n *catalogv1.Need) (string, error) {
	var doc, more yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", errors.New("its Compose file isn't YAML")
	}
	if err := yaml.Unmarshal([]byte(added), &more); err != nil || len(more.Content) != 1 {
		return "", errors.New("the service a need adds isn't YAML")
	}
	svc := nodeGet(nodeGet(more.Content[0], "services"), n.GetName())
	if svc == nil {
		return added, nil
	}
	services := nodeGet(doc.Content[0], "services")
	var joins *yaml.Node
	if own := nodeGet(services, n.GetService()); own != nil && n.GetService() != "" {
		joins = nodeGet(own, "networks")
	} else {
		parsed, err := composeServices(file)
		if err != nil {
			return "", err
		}
		var names []string
		for i := 0; i+1 < len(services.Content); i += 2 {
			name, reader := services.Content[i].Value, services.Content[i+1]
			if !slices.ContainsFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return uses(parsed[name], s.GetName()) }) || nodeGet(reader, "network_mode") != nil {
				continue
			}
			nets := nodeGet(reader, "networks")
			switch {
			case nets == nil:
				names = append(names, "default")
			case nets.Kind == yaml.SequenceNode:
				for _, x := range nets.Content {
					names = append(names, x.Value)
				}
			case nets.Kind == yaml.MappingNode:
				for j := 0; j+1 < len(nets.Content); j += 2 {
					names = append(names, nets.Content[j].Value)
				}
			}
		}
		slices.Sort(names)
		names = slices.Compact(names)
		if len(names) > 0 && !slices.Equal(names, []string{"default"}) {
			joins = &yaml.Node{Kind: yaml.SequenceNode}
			for _, x := range names {
				joins.Content = append(joins.Content, scalar(x))
			}
		}
	}
	if joins == nil {
		return added, nil
	}
	svc.Content = append(deleteKeys(svc.Content, "networks"), scalar("networks"), joins)
	return encode(&more)
}

// deleteKeys takes keys out of a mapping's content.
func deleteKeys(content []*yaml.Node, keys ...string) []*yaml.Node {
	var out []*yaml.Node
	for i := 0; i+1 < len(content); i += 2 {
		if !slices.Contains(keys, content[i].Value) {
			out = append(out, content[i], content[i+1])
		}
	}
	return out
}

// renameVolumes renames the volumes a service mounts, in either form.
func renameVolumes(svc *yaml.Node, renamed map[string]string) {
	vols := nodeGet(svc, "volumes")
	if vols == nil || vols.Kind != yaml.SequenceNode {
		return
	}
	for _, v := range vols.Content {
		switch v.Kind {
		case yaml.ScalarNode:
			source, rest, ok := strings.Cut(v.Value, ":")
			if to, named := renamed[source]; ok && named {
				v.Value = to + ":" + rest
			}
		case yaml.MappingNode:
			if s := nodeGet(v, "source"); s != nil {
				if to, named := renamed[s.Value]; named {
					s.Value = to
				}
			}
		default:
		}
	}
}

// setEnvironment sets variables in a service's environment, in whichever form it has.
func setEnvironment(svc *yaml.Node, values map[string]string) {
	env := nodeGet(svc, "environment")
	if env == nil {
		env = &yaml.Node{Kind: yaml.MappingNode}
		svc.Content = append(svc.Content, scalar("environment"), env)
	}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		switch env.Kind {
		case yaml.SequenceNode:
			env.Content = slices.DeleteFunc(env.Content, func(n *yaml.Node) bool { return strings.HasPrefix(n.Value, k+"=") })
			env.Content = append(env.Content, scalar(k+"="+values[k]))
		case yaml.MappingNode:
			env.Content = deleteKeys(env.Content, k)
			env.Content = append(env.Content, scalar(k), scalar(values[k]))
		default:
		}
	}
}

// walkStrings calls f with every scalar of a tree that isn't a mapping's key.
func walkStrings(n *yaml.Node, f func(*yaml.Node)) {
	switch n.Kind {
	case yaml.ScalarNode:
		f(n)
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			walkStrings(n.Content[i], f)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			walkStrings(c, f)
		}
	default:
	}
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func encode(doc *yaml.Node) (string, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("writing its Compose file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("writing its Compose file: %w", err)
	}
	return b.String(), nil
}

// NeedNames are the names an entry's needs set, so a person can't give one a value of
// their own: Dockwell sets each from what meets the need.
func NeedNames(e *catalogv1.Entry) map[string]bool {
	out := map[string]bool{}
	for _, n := range e.GetNeeds() {
		for _, s := range n.GetSets() {
			out[s.GetName()] = true
		}
		out[passwordName(n)] = true
	}
	return out
}

// Standing is an entry as a reading of it needs it, to check it or say what it
// installs: with an input for each name its needs set, valued as the need's own
// service, or else its first engine, would meet it, so its Compose file reads with
// nothing missing. An entry with no needs is as it is. Nothing installs from it: an
// install meets its needs with Need.
func Standing(e *catalogv1.Entry) *catalogv1.Entry {
	if len(e.GetNeeds()) == 0 {
		return e
	}
	services, _ := composeServices(e.GetCompose())
	out := proto.CloneOf(e)
	for _, n := range e.GetNeeds() {
		w := Way{Need: n}
		if len(n.GetAccepts()) > 0 {
			w.Engine = n.GetAccepts()[0].GetEngine()
		}
		if own := n.GetService(); own != "" {
			image, _ := mapGet(services[own], "image").(string)
			w.Engine = EngineOfImage(image)
		}
		values := needValues(n, w)
		for _, s := range n.GetSets() {
			in := &catalogv1.Input{Name: s.GetName(), Label: s.GetName(), Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: values[s.GetPart()], Optional: true}
			if SecretPart(s) || s.GetPart() == catalogv1.Part_PART_URL {
				in = &catalogv1.Input{Name: s.GetName(), Label: s.GetName(), Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32}
			}
			out.Inputs = append(out.Inputs, in)
		}
	}
	return out
}

// PlainNames are the names an entry's needs set that hold no password, which an
// install keeps as its environment's variables. A URL is a secret Dockwell writes,
// whatever it holds.
func PlainNames(e *catalogv1.Entry) []string {
	var out []string
	for _, n := range e.GetNeeds() {
		for _, s := range n.GetSets() {
			if !SecretPart(s) && s.GetPart() != catalogv1.Part_PART_URL {
				out = append(out, s.GetName())
			}
		}
	}
	return out
}
