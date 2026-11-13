// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/resource"
)

// A store's app often runs its own database beside it and reaches it by a URL with
// the database's password in it, such as postgres://umami:${DB_PASSWORD}@db:5432/umami.
// An entry can't hold that: a password inside a longer value is out of an upgrade's
// and a rotation's reach (ADR 0029). Such a database becomes the app's need, met by
// that same service, with the URL as a template of the need's parts, so the URL is
// Dockwell's to write (ADR 0084, ADR 0085).

// bundledEngine is what the converter knows of an engine's official image: the
// first-start variables it makes the app's user, password and database from, in
// the order a file may name them, the user it makes where none is named, and the
// port it listens on.
type bundledEngine struct {
	user, password, database []string
	defaultUser, port        string
}

var bundledEngines = map[catalogv1.Engine]bundledEngine{
	catalogv1.Engine_ENGINE_POSTGRESQL: {
		user: []string{"POSTGRES_USER"}, password: []string{"POSTGRES_PASSWORD"}, database: []string{"POSTGRES_DB"},
		defaultUser: "postgres", port: "5432",
	},
	catalogv1.Engine_ENGINE_MYSQL: {
		user: []string{"MYSQL_USER"}, password: []string{"MYSQL_PASSWORD"}, database: []string{"MYSQL_DATABASE"},
		port: "3306",
	},
	catalogv1.Engine_ENGINE_MARIADB: {
		user: []string{"MARIADB_USER", "MYSQL_USER"}, password: []string{"MARIADB_PASSWORD", "MYSQL_PASSWORD"},
		database: []string{"MARIADB_DATABASE", "MYSQL_DATABASE"}, port: "3306",
	},
}

// dbURL is a URL with a password that's a variable alone: its scheme, user, password's
// variable, host, port, database and query.
var dbURL = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)([^:@/?#]*):\$\{([A-Za-z_][A-Za-z0-9_]*)\}@([A-Za-z0-9_.-]+)(?::([0-9]+))?(?:/([^/?#]*))?(\?[^#]*)?$`)

// soleVarDefault is a value that's one variable, with the default it may have.
var soleVarDefault = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^${}]*))?\}$`)

// word is a part of a value as a store writes it: a variable, with the default it
// may have, or text.
type word struct {
	name, text string
}

func wordOf(s string) (word, bool) {
	if m := soleVarDefault.FindStringSubmatch(s); m != nil {
		return word{name: m[1], text: m[2]}, true
	}
	if strings.ContainsAny(s, "${}") {
		return word{}, false
	}
	return word{text: s}, true
}

// envEntry is one variable of a service's environment, as its file writes it, and
// how to write its value anew.
type envEntry struct {
	key, value string
	set        func(string)
}

func envOf(svc *yaml.Node) []envEntry {
	env := mapGet(svc, "environment")
	var out []envEntry
	switch {
	case env == nil:
	case env.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(env.Content); i += 2 {
			v := env.Content[i+1]
			if v.Kind != yaml.ScalarNode {
				continue
			}
			out = append(out, envEntry{key: env.Content[i].Value, value: v.Value, set: func(s string) {
				v.Value, v.Tag, v.Style = s, "!!str", 0
			}})
		}
	case env.Kind == yaml.SequenceNode:
		for _, item := range env.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			k, v, ok := strings.Cut(item.Value, "=")
			if !ok {
				continue
			}
			out = append(out, envEntry{key: k, value: v, set: func(s string) { item.Value, item.Style = k+"="+s, 0 }})
		}
	}
	return out
}

// setEnv writes a variable of a service's environment, in the form its file writes
// the rest.
func setEnv(svc *yaml.Node, key, value string) {
	for _, e := range envOf(svc) {
		if e.key == key {
			e.set(value)
			return
		}
	}
	env := mapGet(svc, "environment")
	if env == nil {
		env = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		svc.Content = append(svc.Content, scalar("environment"), env)
	}
	switch env.Kind {
	case yaml.MappingNode:
		env.Content = append(env.Content, scalar(key), scalar(value))
	case yaml.SequenceNode:
		env.Content = append(env.Content, scalar(key+"="+value))
	default:
	}
}

// reservedUsers and reservedDatabases are what an engine keeps for itself, which a
// need's user or database is never (ADR 0084).
var (
	reservedUsers     = map[string]bool{"root": true, "postgres": true, "mysql": true, "mariadb": true, "default": true, "admin": true}
	reservedDatabases = map[string]bool{
		"mysql": true, "information_schema": true, "performance_schema": true, "sys": true, "postgres": true,
		"template0": true, "template1": true, "admin": true, "local": true, "config": true,
	}
	databaseWord = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// sqlName is a name for a user or a database: the store's, where an engine takes it
// as it is and keeps nothing of that name for itself, or else one of the app's ID.
func sqlName(was, id string, reserved map[string]bool) string {
	if databaseWord.MatchString(was) && !reserved[was] && !strings.HasPrefix(was, "pg_") {
		return was
	}
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return '_'
		}
		return -1
	}, strings.ToLower(id))
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "app_" + s
	}
	if len(s) > 28 {
		s = strings.TrimRight(s[:28], "_")
	}
	if reserved[s] || strings.HasPrefix(s, "pg_") {
		s += "_app"
	}
	return s
}

// bundledNeeds makes a need of each database e's file runs that another of its
// services reaches by a URL with the database's password in it, where the URL names
// the database's own user, host, port and database. The need is met by that
// database, as the file runs it, and sets its user, password and database, which
// the database is made with, and the URL, as a template of them, which the app
// reads whole. A database that doesn't read as one with its URLs stays as it is,
// for the catalog's checks to say why.
func bundledNeeds(e *catalogv1.Entry) error {
	if len(e.GetNeeds()) > 0 {
		return nil
	}
	doc, err := parseFile([]byte(e.GetCompose()))
	if err != nil {
		return err
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range e.GetInputs() {
		inputs[in.GetName()] = in
	}
	var needs []*catalogv1.Need
	drop := map[string]bool{}
	for _, name := range serviceNames(doc) {
		try := cloneNode(doc)
		n, dropped, why := bundledNeed(e.GetId(), try, name, inputs, needs)
		if why != "" {
			continue
		}
		doc = try
		needs = append(needs, n)
		for _, d := range dropped {
			drop[d] = true
		}
	}
	if len(needs) == 0 {
		return nil
	}
	// A top-level extension is only a source of anchors, which the file's services
	// hold by now, so a URL in one would stand there as it was.
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); {
		if strings.HasPrefix(root.Content[i].Value, "x-") {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			continue
		}
		i += 2
	}
	file, err := encodeFile(doc)
	if err != nil {
		return err
	}
	e.Compose = string(file)
	e.Inputs = slices.DeleteFunc(e.Inputs, func(in *catalogv1.Input) bool { return drop[in.GetName()] })
	e.Needs = needs
	for _, n := range needs {
		if !slices.Contains(e.Databases, n.GetService()) {
			e.Databases = append(e.Databases, n.GetService())
		}
	}
	return nil
}

// bundledNeed makes the need the database db meets, writing doc as the need runs it,
// with the inputs it takes over. why says why db isn't such a database, empty where
// it is.
func bundledNeed(id string, doc *yaml.Node, db string, inputs map[string]*catalogv1.Input, earlier []*catalogv1.Need) (*catalogv1.Need, []string, string) {
	svc := service(doc, db)
	image := ""
	if n := mapGet(svc, "image"); n != nil {
		image = n.Value
	}
	eng := catalog.EngineOfImage(image)
	if !resource.IsLabel(db) {
		return nil, nil, "its name isn't a need's"
	}
	b := &bundled{id: id, doc: doc, db: db, svc: svc, eng: eng, inputs: inputs, taken: usedNames(doc.Content[0])}
	for name := range inputs {
		b.taken[name] = true
	}
	for _, n := range earlier {
		for _, s := range n.GetSets() {
			b.taken[s.GetName()] = true
		}
	}
	var need *catalogv1.Need
	var why string
	switch eng {
	case catalogv1.Engine_ENGINE_REDIS, catalogv1.Engine_ENGINE_VALKEY:
		need, why = b.redis()
	default:
		keys, ok := bundledEngines[eng]
		if !ok {
			return nil, nil, "not a database the converter knows"
		}
		need, why = b.sql(keys)
	}
	if why != "" {
		return nil, nil, why
	}
	// The shell's own $${NAME}, which the catalog would read as a use of the need's
	// value, is $$NAME, which the shell reads the same.
	for _, s := range need.GetSets() {
		walkScalars(doc.Content[0], func(n *yaml.Node) { n.Value = shellOwn(n.Value, s.GetName()) })
	}
	var dropped []string
	for _, s := range need.GetSets() {
		if inputs[s.GetName()] != nil {
			dropped = append(dropped, s.GetName())
		}
	}
	return need, dropped, ""
}

// bundled is a database a store's file runs, as bundledNeed reads it.
type bundled struct {
	id, db string
	doc    *yaml.Node
	svc    *yaml.Node
	eng    catalogv1.Engine
	inputs map[string]*catalogv1.Input
	// taken are the names the file, its inputs and earlier needs have.
	taken map[string]bool
}

// resolved is what a word comes to: its input's default, or its own text.
func (b *bundled) resolved(w word) string {
	if in := b.inputs[w.name]; in != nil && in.GetDefaultValue() != "" {
		return in.GetDefaultValue()
	}
	return w.text
}

// secret says a variable is one of the app's secrets.
func (b *bundled) secret(name string) bool {
	in := b.inputs[name]
	return in != nil && catalog.Secret(in)
}

// urlUse is a URL another service reaches the database by, as the value of a
// variable, and the template it comes to.
type urlUse struct {
	entry    envEntry
	template string
}

// urlUses are the values of other services' environments that hold password inside
// a longer value, each read by read, which says the template it comes to or why it
// can't be one.
func (b *bundled) urlUses(password string, read func(string) (string, string)) ([]urlUse, string) {
	inside := regexp.MustCompile(`\$\{` + regexp.QuoteMeta(password) + `[}:?+-]`)
	var out []urlUse
	for _, other := range serviceNames(b.doc) {
		if other == b.db {
			continue
		}
		for _, v := range envOf(service(b.doc, other)) {
			if v.value == "${"+password+"}" || !inside.MatchString(v.value) {
				continue
			}
			t, why := read(v.value)
			if why != "" {
				return nil, fmt.Sprintf("%s's %s %s", other, v.key, why)
			}
			out = append(out, urlUse{entry: v, template: t})
		}
	}
	if len(out) == 0 {
		return nil, "no URL holds its password"
	}
	return out, ""
}

// setURLs gives each URL's template a name the need sets, the variable's own where
// it's free, and writes each use as that name.
func (b *bundled) setURLs(need *catalogv1.Need, uses []urlUse) {
	names := map[string]string{}
	for _, u := range uses {
		name, ok := names[u.template]
		if !ok {
			name = u.entry.key
			if !inputLike.MatchString(name) || b.taken[name] {
				name = freeName(envName(b.db)+"_URL", b.taken)
			}
			b.taken[name] = true
			names[u.template] = name
			need.Sets = append(need.Sets, &catalogv1.NeedSet{Name: name, Part: catalogv1.Part_PART_URL, Template: u.template})
		}
		u.entry.set("${" + name + "}")
	}
}

// sql reads a PostgreSQL, MySQL or MariaDB the file runs, whose image makes its user,
// password and database from keys.
func (b *bundled) sql(keys bundledEngine) (*catalogv1.Need, string) {
	env := map[string]envEntry{}
	for _, v := range envOf(b.svc) {
		env[v.key] = v
	}
	pick := func(names []string) (string, envEntry, bool) {
		for _, k := range names {
			if v, ok := env[k]; ok {
				return k, v, true
			}
		}
		return names[0], envEntry{}, false
	}
	pwKey, pwEntry, ok := pick(keys.password)
	if !ok {
		return nil, "its image makes no user of the app's own with a password"
	}
	pw, ok := wordOf(pwEntry.value)
	if !ok || pw.name == "" || !b.secret(pw.name) {
		return nil, "its password isn't one of the app's secrets"
	}
	// The other first-start variables, of the same family as the password's, as
	// MariaDB's image reads MYSQL_ names too.
	family := func(names []string) []string {
		if strings.HasPrefix(pwKey, "MYSQL_") && len(names) > 1 {
			return names[1:]
		}
		return names
	}
	userKey, userEntry, hasUser := pick(family(keys.user))
	dbKey, dbEntry, hasDB := pick(family(keys.database))
	user := word{text: keys.defaultUser}
	if hasUser {
		if user, ok = wordOf(userEntry.value); !ok {
			return nil, "its user is more than one value"
		}
	}
	if b.resolved(user) == "" {
		return nil, "its image makes no user of the app's own"
	}
	var database word
	if hasDB {
		if database, ok = wordOf(dbEntry.value); !ok {
			return nil, "its database is more than one value"
		}
	}
	same := func(a, c word) bool {
		return a.name != "" && a.name == c.name || b.resolved(a) != "" && b.resolved(a) == b.resolved(c)
	}
	uses, why := b.urlUses(pw.name, func(v string) (string, string) {
		m := dbURL.FindStringSubmatch(v)
		switch {
		case len(m) < 8 || m[3] != pw.name:
			return "", "isn't a URL with the password alone"
		case m[4] != b.db:
			return "", "names another host"
		case m[5] != "" && m[5] != keys.port:
			return "", "names another port"
		case m[6] == "":
			return "", "names no database"
		case strings.ContainsAny(m[7], "${}"):
			return "", "has a variable in its query"
		}
		uw, ok1 := wordOf(m[2])
		dw, ok2 := wordOf(m[6])
		switch {
		case !ok1 || !same(uw, user):
			return "", "signs in as another user"
		case !ok2:
			return "", "names a database of more than one value"
		case !hasDB && database.text == "" && database.name == "":
			// Its image makes no database by name, so it makes the one the URL names.
			database = dw
		case !same(dw, database):
			return "", "names another database"
		}
		t := m[1] + "{user}:{password}@{host}"
		if m[5] != "" {
			t += ":{port}"
		}
		return t + "/{database}" + m[7], ""
	})
	if why != "" {
		return nil, why
	}
	prefix := envName(b.db) + "_"
	setName := func(w word, key string) string {
		if w.name != "" && inputLike.MatchString(w.name) {
			return w.name
		}
		return freeName(key, b.taken, prefix+key)
	}
	userSet := setName(word{name: user.name}, userKey)
	dbSet := setName(word{name: database.name}, dbKey)
	if userSet == dbSet || userSet == pw.name || dbSet == pw.name {
		return nil, "two of its values share a name"
	}
	need := &catalogv1.Need{
		Name: b.db, Label: "Database", Service: b.db,
		Accepts:  []*catalogv1.NeedAccept{{Engine: b.eng}},
		User:     sqlName(b.resolved(user), b.id, reservedUsers),
		Database: sqlName(b.resolved(database), b.id, reservedDatabases),
		Sets: []*catalogv1.NeedSet{
			{Name: userSet, Part: catalogv1.Part_PART_USER},
			{Name: pw.name, Part: catalogv1.Part_PART_PASSWORD},
			{Name: dbSet, Part: catalogv1.Part_PART_DATABASE},
		},
	}
	setEnv(b.svc, userKey, "${"+userSet+"}")
	setEnv(b.svc, pwKey, "${"+pw.name+"}")
	setEnv(b.svc, dbKey, "${"+dbSet+"}")
	b.setURLs(need, uses)
	if !shellReads(b.svc, []string{userSet, dbSet}) {
		return nil, "its health check names its user or database in a word a shell can't read as it is"
	}
	// A plain part stands as a whole value of an environment, where the need sets it,
	// so a default the file gives it there goes: the need always has one.
	for _, name := range []string{userSet, dbSet} {
		for _, other := range serviceNames(b.doc) {
			for _, v := range envOf(service(b.doc, other)) {
				if w, ok := wordOf(v.value); ok && w.name == name {
					v.set("${" + name + "}")
				}
			}
		}
		for _, s := range need.GetSets() {
			walkScalars(b.doc.Content[0], func(n *yaml.Node) { n.Value = shellOwn(n.Value, s.GetName()) })
		}
		if where := notWhole(b.doc, name); where != "" {
			return nil, fmt.Sprintf("its %s stands inside a longer value, %q", name, where)
		}
	}
	return need, ""
}

// redisURL is a URL of Redis or Valkey with a password that's a variable alone: its
// scheme, user, password's variable, host, port and database's number.
var redisURL = regexp.MustCompile(`^(rediss?://)([A-Za-z0-9_-]*):\$\{([A-Za-z_][A-Za-z0-9_]*)\}@([A-Za-z0-9_.-]+)(?::([0-9]+))?(/[0-9]{1,2})?$`)

// redis reads a Redis or Valkey the file runs with a password: given to its server
// as --requirepass, or as the REDIS_PASSWORD some images read.
func (b *bundled) redis() (*catalogv1.Need, string) {
	password := ""
	for _, v := range envOf(b.svc) {
		if w, ok := wordOf(v.value); ok && v.key == "REDIS_PASSWORD" && w.name != "" {
			password = w.name
		}
	}
	if command := mapGet(b.svc, "command"); password == "" && command != nil {
		var words []string
		switch command.Kind {
		case yaml.ScalarNode:
			words = strings.Fields(command.Value)
		case yaml.SequenceNode:
			for _, w := range command.Content {
				words = append(words, strings.Fields(w.Value)...)
			}
		default:
		}
		for i, w := range words {
			if w == "--requirepass" && i+1 < len(words) {
				if pw, ok := wordOf(strings.Trim(words[i+1], `"'`)); ok && pw.name != "" {
					password = pw.name
				}
			}
		}
	}
	if password == "" || !b.secret(password) {
		return nil, "it has no password of the app's secrets"
	}
	uses, why := b.urlUses(password, func(v string) (string, string) {
		m := redisURL.FindStringSubmatch(v)
		switch {
		case len(m) < 7 || m[3] != password:
			return "", "isn't a URL with the password alone"
		case m[4] != b.db:
			return "", "names another host"
		case m[5] != "" && m[5] != "6379":
			return "", "names another port"
		case m[2] != "" && m[2] != "default":
			return "", "signs in as a user of its own"
		}
		t := m[1] + m[2] + ":{password}@{host}"
		if m[5] != "" {
			t += ":{port}"
		}
		return t + m[6], ""
	})
	if why != "" {
		return nil, why
	}
	need := &catalogv1.Need{
		Name: b.db, Label: "Cache", Service: b.db,
		Accepts: []*catalogv1.NeedAccept{{Engine: b.eng}},
		Sets:    []*catalogv1.NeedSet{{Name: password, Part: catalogv1.Part_PART_PASSWORD}},
	}
	b.setURLs(need, uses)
	return need, ""
}

// shellReads writes a use of each of names in the database's own health check as
// the shell's, which reads it from the container's environment, where the database
// has it: a plain value a need sets stands only as a whole value of an environment
// (ADR 0084). A check in exec form becomes a shell's. It says false where a word
// it would have to write isn't one a shell reads as it is.
func shellReads(svc *yaml.Node, names []string) bool {
	test := mapGet(mapGet(svc, "healthcheck"), "test")
	if test == nil {
		return true
	}
	uses := func(s string) bool {
		for _, u := range usesOf(s) {
			if slices.Contains(names, u.name) {
				return true
			}
		}
		return false
	}
	asShell := func(s string) string {
		for _, name := range names {
			s = replaceVar(s, name, "$$"+name)
		}
		return s
	}
	switch {
	case test.Kind == yaml.ScalarNode:
		test.Value = asShell(test.Value)
	case test.Kind == yaml.SequenceNode && len(test.Content) == 2 && test.Content[0].Value == "CMD-SHELL":
		test.Content[1].Value = asShell(test.Content[1].Value)
	case test.Kind == yaml.SequenceNode && len(test.Content) > 1 && test.Content[0].Value == "CMD":
		words := test.Content[1:]
		if !slices.ContainsFunc(words, func(w *yaml.Node) bool { return uses(w.Value) }) {
			return true
		}
		var script []string
		for _, w := range words {
			if !plainWord.MatchString(w.Value) {
				return false
			}
			script = append(script, asShell(w.Value))
		}
		test.Content = []*yaml.Node{scalar("CMD-SHELL"), scalar(strings.Join(script, " "))}
	}
	return true
}

// plainWord is a word a shell reads as it is, or with a variable in it.
var plainWord = regexp.MustCompile(`^([A-Za-z0-9_./:=@%+,-]|\$\{[A-Za-z_][A-Za-z0-9_]*(:?-[A-Za-z0-9_./-]*)?\})+$`)

// shellOwn writes $${name}, the shell's own variable, as $$name, where what follows
// can't be read as more of its name.
func shellOwn(s, name string) string {
	own := "$${" + name + "}"
	var b strings.Builder
	for {
		i := strings.Index(s, own)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		rest := s[i+len(own):]
		b.WriteString(s[:i])
		if rest != "" && (rest[0] == '_' || rest[0] >= 'A' && rest[0] <= 'Z' || rest[0] >= 'a' && rest[0] <= 'z' || rest[0] >= '0' && rest[0] <= '9') {
			b.WriteString(own)
		} else {
			b.WriteString("$$" + name)
		}
		s = rest
	}
}

// cloneNode is a deep copy of a YAML node.
func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		out.Content[i] = cloneNode(c)
	}
	out.Alias = cloneNode(n.Alias)
	return &out
}

// secretInside says where a secret stands inside a longer value of a service's
// environment that no need took over, out of an upgrade's and a rotation's reach
// (ADR 0029), such as a URL of a database another app runs.
func secretInside(e *catalogv1.Entry) error {
	secrets := map[string]bool{}
	for _, in := range e.GetInputs() {
		if catalog.Secret(in) {
			secrets[in.GetName()] = true
		}
	}
	if len(secrets) == 0 {
		return nil
	}
	doc, err := parseFile([]byte(e.GetCompose()))
	if err != nil {
		return err
	}
	for _, name := range serviceNames(doc) {
		for _, v := range envOf(service(doc, name)) {
			for _, u := range usesOf(v.value) {
				if secrets[u.name] && v.value != "${"+u.name+"}" {
					return leave(ClassInside, "its %s's %s puts the secret %s inside a longer value, which an upgrade and a rotation couldn't reach", name, v.key, u.name)
				}
			}
		}
	}
	return nil
}

// needsWait says why an entry with needs can't be installed yet, where an install
// couldn't meet one as it says: a database URL with its password, which Dockwell
// writes once a value can come from a template (ADR 0084). Nil where every need
// can be met. Each is asked of the catalog, so an entry installs as soon as the
// catalog meets what it needs, with no change here.
func needsWait(e *catalogv1.Entry) error {
	for _, n := range e.GetNeeds() {
		if n.GetService() == "" && !slices.ContainsFunc(n.GetAccepts(), func(a *catalogv1.NeedAccept) bool { return catalogMakes()[a.GetEngine()] }) {
			var names []string
			for _, a := range n.GetAccepts() {
				names = append(names, catalog.EngineName(a.GetEngine()))
			}
			return leave(ClassNewEngine, "it needs %s, which Dockwell doesn't make beside an app yet (ADR 0084)", strings.Join(names, " or "))
		}
		for _, s := range n.GetSets() {
			if s.GetPart() == catalogv1.Part_PART_URL && catalog.SecretPart(s) && !writesURLs() {
				return leave(ClassURL, "it reaches its database %s by a URL with its password in it, %s, which Dockwell writes once a value can come from a template (ADR 0084)", n.GetService(), s.GetName())
			}
		}
	}
	return nil
}

// writesURLs says an install writes a URL with a need's password in it, as the
// catalog says of a need met by an entry's own database.
var writesURLs = sync.OnceValue(func() bool {
	e := &catalogv1.Entry{
		Id: "probe", Name: "Probe",
		Compose: "services:\n  app:\n    image: example.com/app:1\n    environment:\n      URL: ${URL}\n" +
			"  db:\n    image: postgres:17\n    environment:\n      POSTGRES_PASSWORD: ${PW}\n",
		Databases: []string{"db"},
		Needs: []*catalogv1.Need{{
			Name: "db", Label: "Database", Service: "db",
			Accepts: []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_POSTGRESQL}},
			Sets: []*catalogv1.NeedSet{
				{Name: "PW", Part: catalogv1.Part_PART_PASSWORD},
				{Name: "URL", Part: catalogv1.Part_PART_URL, Template: "postgres://{user}:{password}@{host}:{port}/{database}"},
			},
		}},
	}
	met, err := catalog.Meet(e, catalog.Ways(e, nil), nil, nil)
	if err != nil {
		return false
	}
	_, err = catalog.Need(e, met)
	return err == nil
})

// onePanelEngines are the engines 1Panel's database apps run, by the word its forms
// name each with.
var onePanelEngines = map[string]catalogv1.Engine{
	"mysql": catalogv1.Engine_ENGINE_MYSQL, "mariadb": catalogv1.Engine_ENGINE_MARIADB,
	"postgresql": catalogv1.Engine_ENGINE_POSTGRESQL, "postgres": catalogv1.Engine_ENGINE_POSTGRESQL,
	"redis": catalogv1.Engine_ENGINE_REDIS,
}

// enginePorts are what each engine a 1Panel app may need listens on.
var enginePorts = map[catalogv1.Engine]string{
	catalogv1.Engine_ENGINE_MYSQL: "3306", catalogv1.Engine_ENGINE_MARIADB: "3306",
	catalogv1.Engine_ENGINE_POSTGRESQL: "5432", catalogv1.Engine_ENGINE_REDIS: "6379",
}

// onePanelNeed makes the database another 1Panel app runs the app's need, met beside
// it at an install, or otherwise (ADR 0084). The app's form picks that app with an
// apps field, whose choice is the engine, and a service field its host fills in;
// the rest of what 1Panel fills in is named for the host, PANEL_DB_PORT, _NAME,
// _USER and _USER_PASSWORD, which become what the need sets. Where the file names
// the engine, the need takes one, the first the catalog makes beside an app.
func onePanelNeed(app *App, fields []*onePanelField) error {
	var kind, host *onePanelField
	for _, f := range fields {
		switch {
		case f.Type == "apps" && kind == nil:
			kind = f
		case f.Type == "service" && host == nil:
			host = f
		default:
			return leave(ClassAnotherApp, "it uses more than one other app, for its %s, which the converter can't bring into its own project yet", f.EnvKey)
		}
	}
	if host == nil {
		return leave(ClassAnotherApp, "it uses another app, for its %s, which the converter can't bring into its own project yet", kind.EnvKey)
	}
	var words []string
	if kind != nil {
		for _, v := range kind.Values {
			words = append(words, strings.ToLower(fmt.Sprint(v.Value)))
		}
	} else {
		words = []string{strings.ToLower(host.Key)}
	}
	var engines []catalogv1.Engine
	var said []string
	for _, w := range words {
		if e, ok := onePanelEngines[w]; ok && !slices.Contains(engines, e) {
			engines, said = append(engines, e), append(said, w)
		}
	}
	prefix, ok := strings.CutSuffix(host.EnvKey, "_HOST")
	if len(engines) == 0 || !ok {
		return leave(ClassAnotherApp, "it uses %s from another app, for its %s, which Dockwell doesn't make beside an app", cmp.Or(strings.Join(words, " or "), host.EnvKey), host.EnvKey)
	}
	doc, err := parseFile(app.Compose)
	if err != nil {
		return err
	}
	root := doc.Content[0]
	used := usedNames(root)
	password := prefix + "_USER_PASSWORD"
	for _, other := range []string{prefix + "_PASSWORD", prefix + "_ROOT_PASSWORD"} {
		if !used[password] {
			password = other
		}
	}
	names := map[catalogv1.Part]string{
		catalogv1.Part_PART_HOST: host.EnvKey, catalogv1.Part_PART_PORT: prefix + "_PORT",
		catalogv1.Part_PART_DATABASE: prefix + "_NAME", catalogv1.Part_PART_USER: prefix + "_USER",
		catalogv1.Part_PART_PASSWORD: password,
	}
	// Where the file names the engine, as KC_DB=${PANEL_DB_TYPE} does, the need takes
	// one, and the file names it as 1Panel does.
	if kind != nil && used[kind.EnvKey] {
		chosen := 0
		for i, e := range engines {
			if catalogMakes()[e] {
				chosen = i
				break
			}
		}
		engines, said = engines[chosen:chosen+1], said[chosen:chosen+1]
		walkScalars(root, func(n *yaml.Node) { n.Value = replaceVar(n.Value, kind.EnvKey, said[0]) })
	}
	need := &catalogv1.Need{
		Name: freeLabel("db", serviceNames(doc)), Label: "Database",
		Database: sqlName("", app.ID, reservedDatabases), User: sqlName("", app.ID, reservedUsers),
	}
	for _, e := range engines {
		need.Accepts = append(need.Accepts, &catalogv1.NeedAccept{Engine: e})
	}
	// A URL of them all with the password is a URL the need writes.
	n := func(p catalogv1.Part) string { return regexp.QuoteMeta("${" + names[p] + "}") }
	url := regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.:-]*://)` + n(catalogv1.Part_PART_USER) + `:` + n(catalogv1.Part_PART_PASSWORD) + `@` +
		n(catalogv1.Part_PART_HOST) + `:` + n(catalogv1.Part_PART_PORT) + `/` + n(catalogv1.Part_PART_DATABASE) + `(\?[^${}]*)?$`)
	taken := maps.Clone(used)
	var urls []*catalogv1.NeedSet
	for _, name := range serviceNames(doc) {
		for _, v := range envOf(service(doc, name)) {
			m := url.FindStringSubmatch(v.value)
			if m == nil {
				continue
			}
			set := v.key
			if !inputLike.MatchString(set) || taken[set] {
				set = freeName(prefix+"_URL", taken)
			}
			taken[set] = true
			urls = append(urls, &catalogv1.NeedSet{Name: set, Part: catalogv1.Part_PART_URL, Template: m[1] + "{user}:{password}@{host}:{port}/{database}" + m[2]})
			v.set("${" + set + "}")
		}
	}
	// Its host and port together, as WORDPRESS_DB_HOST=${PANEL_DB_HOST}:${PANEL_DB_PORT}
	// takes them, are its address.
	// So is its host with the port each engine it takes listens on, as
	// ${PANEL_DB_HOST}:3306 is MySQL's and MariaDB's.
	addresses := []string{"${" + names[catalogv1.Part_PART_HOST] + "}:${" + names[catalogv1.Part_PART_PORT] + "}"}
	if port := enginePorts[engines[0]]; port != "" && !slices.ContainsFunc(engines, func(e catalogv1.Engine) bool { return enginePorts[e] != port }) {
		addresses = append(addresses, "${"+names[catalogv1.Part_PART_HOST]+"}:"+port)
	}
	walkScalars(root, func(n *yaml.Node) {
		for _, address := range addresses {
			if !strings.Contains(n.Value, address) {
				continue
			}
			if names[catalogv1.Part_PART_ADDRESS] == "" {
				names[catalogv1.Part_PART_ADDRESS] = freeName(prefix+"_ADDRESS", taken)
			}
			n.Value = replaceWhole(n.Value, address, "${"+names[catalogv1.Part_PART_ADDRESS]+"}")
		}
	})
	used = usedNames(root)
	for _, p := range []catalogv1.Part{catalogv1.Part_PART_HOST, catalogv1.Part_PART_PORT, catalogv1.Part_PART_ADDRESS, catalogv1.Part_PART_DATABASE, catalogv1.Part_PART_USER, catalogv1.Part_PART_PASSWORD} {
		if name := names[p]; name != "" && used[name] {
			need.Sets = append(need.Sets, &catalogv1.NeedSet{Name: name, Part: p})
		}
	}
	need.Sets = append(need.Sets, urls...)
	// Each stands as a whole value of an environment, as an install sets it there.
	for _, s := range need.GetSets() {
		if where := notWhole(doc, s.GetName()); where != "" {
			return leave(ClassAnotherApp, "it writes its database's %s inside a longer value, %q, which the database it needs couldn't follow", s.GetName(), where)
		}
	}
	file, err := encodeFile(doc)
	if err != nil {
		return err
	}
	app.Compose = file
	app.Needs = []*catalogv1.Need{need}
	set := map[string]bool{}
	for _, s := range need.GetSets() {
		set[s.GetName()] = true
	}
	for _, f := range fields {
		set[f.EnvKey] = true
	}
	app.Fields = slices.DeleteFunc(app.Fields, func(f Field) bool { return set[f.Name] })
	return nil
}

// freeLabel is a name no service has: base, or base with a number after it.
func freeLabel(base string, taken []string) string {
	name := base
	for i := 2; slices.Contains(taken, name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// notWhole is a value that uses name other than as a whole value of a service's
// environment, or "" where none does.
func notWhole(doc *yaml.Node, name string) string {
	whole := map[*yaml.Node]bool{}
	for _, svc := range serviceNames(doc) {
		env := mapGet(service(doc, svc), "environment")
		if env == nil {
			continue
		}
		for _, item := range env.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			if _, v, ok := strings.Cut(item.Value, "="); env.Kind == yaml.SequenceNode && ok && v == "${"+name+"}" ||
				env.Kind == yaml.MappingNode && item.Value == "${"+name+"}" {
				whole[item] = true
			}
		}
	}
	found := ""
	walkScalars(doc.Content[0], func(n *yaml.Node) {
		if found != "" || whole[n] {
			return
		}
		for _, u := range usesOf(n.Value) {
			if u.name == name {
				found = n.Value
				return
			}
		}
	})
	return found
}

// catalogMakes says which engines the catalog makes beside an app, where a need
// names no service of its entry's own, as it says of an entry that runs each.
var catalogMakes = sync.OnceValue(func() map[catalogv1.Engine]bool {
	stub := &catalogv1.Entry{Id: "probe", Name: "Probe", Compose: "services:\n" +
		"  postgres:\n    image: postgres:17\n  mysql:\n    image: mysql:8.4\n  mariadb:\n    image: mariadb:11.4\n" +
		"  redis:\n    image: redis:7\n  valkey:\n    image: valkey/valkey:8\n  mongo:\n    image: mongo:8\n"}
	lookup := func(string) (*catalogv1.Entry, string, bool) { return stub, "probe", true }
	out := map[catalogv1.Engine]bool{}
	for e := range onePanelEngineSet() {
		probe := &catalogv1.Entry{Id: "app", Compose: "services:\n  app:\n    image: example.com/app:1\n", Needs: []*catalogv1.Need{{
			Name: "db", Accepts: []*catalogv1.NeedAccept{{Engine: e}},
		}}}
		out[e] = len(catalog.Ways(probe, lookup)["db"]) > 0
	}
	return out
})

// onePanelEngineSet are the engines a probe asks of.
func onePanelEngineSet() map[catalogv1.Engine]bool {
	return map[catalogv1.Engine]bool{
		catalogv1.Engine_ENGINE_POSTGRESQL: true, catalogv1.Engine_ENGINE_MYSQL: true, catalogv1.Engine_ENGINE_MARIADB: true,
		catalogv1.Engine_ENGINE_REDIS: true, catalogv1.Engine_ENGINE_VALKEY: true, catalogv1.Engine_ENGINE_MONGODB: true,
	}
}

// replaceWhole replaces each of old in s that no digit follows, as a port's number
// would go on.
func replaceWhole(s, old, with string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		rest := s[i+len(old):]
		b.WriteString(s[:i])
		if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			b.WriteString(old)
		} else {
			b.WriteString(with)
		}
		s = rest
	}
}

// urlUser is a URL's scheme and user, up to the colon its password follows.
var urlUser = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[A-Za-z0-9._~%-]*:$`)

// urlPassword writes a URL whose password is value, written out, with input's name in
// its place, or s as it is where value stands anywhere else in it.
func urlPassword(s, value, input string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, ":"+value+"@")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		if urlUser.MatchString(b.String() + ":") {
			b.WriteString(":${" + input + "}@")
		} else {
			b.WriteString(":" + value + "@")
		}
		rest = rest[i+len(value)+2:]
	}
	if out := b.String(); !holdsSecret(out, value) {
		return out
	}
	return s
}
