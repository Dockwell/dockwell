// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
)

// DokploySource is Dokploy's templates, under the MIT licence its repository declares.
func DokploySource(commit string) Source {
	return Source{
		Name:        "Dokploy's templates",
		Repository:  "https://github.com/Dokploy/templates",
		Commit:      commit,
		License:     "MIT",
		LicenseFile: "LICENSE",
		// "Copyright (c) 2024 Dokploy and Carlos Ortiz" at b40194a4.
		CopyrightInLicense: true,
	}
}

// dokployMeta is what a template's meta.json says of its app.
type dokployMeta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Links       struct {
		Github  string `json:"github"`
		Website string `json:"website"`
		Docs    string `json:"docs"`
	} `json:"links"`
	Tags []string `json:"tags"`
}

// dokployTemplate is a template.toml: the values Dokploy makes up, the
// environment it writes from them, the domains it routes, and the files it writes.
type dokployTemplate struct {
	Variables map[string]any `toml:"variables"`
	Config    struct {
		Env     any              `toml:"env"`
		Mounts  []map[string]any `toml:"mounts"`
		Domains []struct {
			ServiceName string `toml:"serviceName"`
			Port        any    `toml:"port"`
			Host        string `toml:"host"`
		} `toml:"domains"`
	} `toml:"config"`
}

// dokployCategories are the categories Dokploy's tags suggest, where one does.
var dokployCategories = map[string]string{
	"ai": "ai", "analytics": "analytics", "automation": "automation", "cms": "cms", "blog": "cms",
	"database": "databases", "databases": "databases", "developer-tools": "developer tools", "development": "developer tools",
	"media": "media", "monitoring": "monitoring", "networking": "networking", "notes": "notes", "security": "security",
	"storage": "storage", "file-storage": "storage",
}

// Dokploy reads the templates in a checkout of Dokploy's templates repository, one
// folder each under blueprints/: its docker-compose.yml, template.toml and meta.json.
func Dokploy(root string) ([]App, error) {
	t, err := openTree(root)
	if err != nil {
		return nil, fmt.Errorf("reading Dokploy's templates: %w. Name the folder of a checkout of github.com/Dokploy/templates", err)
	}
	defer t.Close()
	return dokployApps(context.Background(), t, root)
}

// dokployApps reads each template, stopping once ctx is done.
func dokployApps(ctx context.Context, t *tree, root string) ([]App, error) {
	ents, err := t.readDir("blueprints")
	if err != nil {
		return nil, fmt.Errorf("reading Dokploy's templates: %w. Name the folder of a checkout of github.com/Dokploy/templates", err)
	}
	var out []App
	for _, d := range ents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if d.IsDir() {
			out = append(out, dokployRead(t, d.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Dokploy's templates at %s: %w", root, ErrNoApps) //nolint:staticcheck // it starts with a name
	}
	return out, nil
}

func dokployRead(t *tree, name string) App {
	at := path.Join("blueprints", name)
	app := App{ID: strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(name)), Name: name, Path: at, Web: true}
	file, err := t.readFile(path.Join(at, "docker-compose.yml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	var meta dokployMeta
	if raw, err := t.readFile(path.Join(at, "meta.json")); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	app.Name = cmp.Or(meta.Name, name)
	app.Homepage = cmp.Or(meta.Links.Website, meta.Links.Github)
	app.Summary, app.Docs, app.Code = firstSentence(meta.Description), meta.Links.Docs, meta.Links.Github
	for _, tag := range meta.Tags {
		if c := dokployCategories[strings.ToLower(tag)]; c != "" {
			app.Category = c
			break
		}
	}
	raw, err := t.readFile(path.Join(at, "template.toml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	var tmpl dokployTemplate
	if _, err := toml.Decode(string(raw), &tmpl); err != nil {
		app.LeftOut, app.Class = "reading its template.toml: "+err.Error(), ClassFile
		return app
	}
	shipped, err := t.shipped(at)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	if terms := ownTerms(shipped); terms != "" {
		app.LeftOut, app.Class = fmt.Sprintf("its folder carries terms of its own, in %s, beside Dokploy's licence. "+
			"A person reads them before anything of it is converted", terms), ClassTerms
		return app
	}
	if n := len(tmpl.Config.Mounts); n > 0 {
		app.LeftOut, app.Class = fmt.Sprintf("it ships %d files beside its Compose file, which an entry can't declare yet", n), ClassShipped
		return app
	}
	app.Compose, app.Fields, err = dokployCompose(file, &tmpl)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	app.Version = meta.Version
	if app.Version == "" || unversioned[app.Version] {
		app.Version = webTag(app.Compose, app.Fields)
	}
	for _, own := range []string{"docker-compose.yml", "template.toml", "meta.json"} {
		delete(shipped, own)
	}
	app.Shipped = func(rel string) bool { return shipped[rel] }
	return app
}

// dokployCompose writes what Dokploy does at an install into the file: the port each
// domain routes, published, and the environment its template makes, with each value
// it makes up as a field.
func dokployCompose(file []byte, t *dokployTemplate) ([]byte, []Field, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, nil, err
	}
	walkScalars(doc.Content[0], func(n *yaml.Node) { n.Value = braceVars(n.Value) })
	ev := &dokployEval{vars: t.Variables, hosts: map[string]string{}}
	env, err := envPairs(t.Config.Env)
	if err != nil {
		return nil, nil, err
	}
	// A routed port's input takes a name nothing in the template or its file has, so
	// a template's own PORT stays the app's.
	taken := usedNames(doc.Content[0])
	for _, kv := range env {
		taken[kv[0]] = true
	}
	for v := range t.Variables {
		taken[strings.ToUpper(v)] = true
	}
	var fields []Field
	for i, d := range t.Config.Domains {
		port := fmt.Sprint(d.Port)
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, nil, leave(ClassCantSay, "its domain routes to %s's port %q, which isn't a port", d.ServiceName, port)
		}
		base := "PORT"
		if i > 0 {
			base = envName(d.ServiceName) + "_PORT"
		}
		name := freeName(base, taken, "HOST_"+base)
		published, err := publish(doc, d.ServiceName, name, port)
		if err != nil {
			return nil, nil, err
		}
		// A service on the server's own network answers on its own port there.
		address := "localhost:" + port
		if published {
			fields = append(fields, Field{Name: name, Label: "Port", Kind: Port, Default: port, Web: i == 0})
			address = "localhost:${" + name + "}"
		}
		if v, ok := soleVar(d.Host); ok {
			ev.hosts[v] = address
		}
		if ev.firstHost == "" {
			ev.firstHost = address
		}
	}
	literal := map[string]string{}
	claimed := map[string]string{}
	var keys []string
	for _, kv := range env {
		keys = append(keys, kv[0])
		key, value := kv[0], kv[1]
		text, made, err := ev.value(value)
		if err != nil {
			return nil, nil, fmt.Errorf("its template's %s %w", key, err)
		}
		switch {
		case made == nil && strings.TrimSpace(text) == "" && inputLike.MatchString(key):
			// Nothing written is a value a person may give, a secret where its name
			// says so, and no fixed empty one.
			f := Field{Name: key, Label: labelOf(key), Kind: Text, Optional: true}
			if secretNamed(key) {
				f.Kind = Secret
			}
			fields = append(fields, f)
		case made == nil:
			if literal[key], err = writtenOut(key, localHTTP(text)); err != nil {
				return nil, nil, err
			}
		case claimed[made.id] != "":
			literal[key] = "${" + claimed[made.id] + "}"
		case !inputLike.MatchString(key):
			return nil, nil, leave(ClassCantSay, "its template's %s makes up a value, under a name an entry's input can't take", key)
		default:
			claimed[made.id] = key
			f := made.field
			f.Name, f.Label = key, labelOf(key)
			fields = append(fields, f)
		}
	}
	if err := envFile(doc, keys); err != nil {
		return nil, nil, err
	}
	// A variable of the template's own that the file uses directly, which Dokploy
	// writes into the file too: what it comes to, or the field it makes.
	direct := map[string]bool{}
	walkScalars(doc.Content[0], func(n *yaml.Node) {
		for _, u := range usesOf(braceVars(n.Value)) {
			if _, ok := t.Variables[u.name]; ok && !slices.Contains(keys, u.name) {
				direct[u.name] = true
			}
		}
	})
	for _, name := range slices.Sorted(maps.Keys(direct)) {
		text, made, err := ev.expand("${"+name+"}", name, 0)
		switch {
		case err != nil:
			return nil, nil, fmt.Errorf("its template's %s %w", name, err)
		case made == nil:
			if literal[name], err = writtenOut(name, localHTTP(text)); err != nil {
				return nil, nil, err
			}
		case claimed[made.id] != "":
			literal[name] = "${" + claimed[made.id] + "}"
		default:
			input := strings.ToUpper(name)
			if !inputLike.MatchString(input) {
				return nil, nil, leave(ClassCantSay, "its template's %s makes up a value, under a name an entry's input can't take", name)
			}
			claimed[made.id] = input
			literal[name] = "${" + input + "}"
			f := made.field
			f.Name = input
			fields = append(fields, f)
		}
	}
	// A folder Dokploy keeps beside a template, in its files folder, which the app
	// makes as it runs where the template ships nothing there.
	for _, name := range serviceNames(doc) {
		vols := mapGet(service(doc, name), "volumes")
		for i := 0; vols != nil && i < len(vols.Content); i++ {
			if source, set := volumeSource(vols.Content[i]); strings.HasPrefix(source, "../files/") {
				set("./" + strings.TrimPrefix(source, "../files/"))
			}
		}
	}
	walkScalars(doc.Content[0], func(n *yaml.Node) {
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range literal {
				if !yield(k) {
					return
				}
			}
		}) {
			n.Value = replaceVar(n.Value, key, literal[key])
		}
		// An address the file writes around a domain's, over HTTPS, is plain HTTP on
		// loopback until a person adds one.
		n.Value = localHTTP(n.Value)
	})
	// A variable the template doesn't set, which Dokploy lets a person set.
	asked := askedFields(doc.Content[0], fields)
	walkScalars(doc.Content[0], func(n *yaml.Node) {
		for _, f := range asked {
			if !f.Optional {
				n.Value = replaceVar(n.Value, f.Name, "${"+f.Name+"}")
			}
		}
	})
	fields = append(fields, asked...)
	out, err := encodeFile(doc)
	return out, fields, err
}

// ownTerms names what in a template's folder puts it under terms of its own: a
// licence, a notice, or a vendor folder of someone else's files, as Harbor's
// template carries Harbor's licence and notice.
func ownTerms(shipped map[string]bool) string {
	for _, rel := range slices.Sorted(maps.Keys(shipped)) {
		for part := range strings.SplitSeq(rel, "/") {
			upper := strings.ToUpper(part)
			if part == "vendor" || strings.Contains(upper, "LICENSE") || strings.Contains(upper, "LICENCE") ||
				strings.Contains(upper, "NOTICE") || strings.Contains(upper, "COPYING") {
				return rel
			}
		}
	}
	return ""
}

// writtenOut is what a value a template writes out stands as in the file: itself, or,
// for a secret, the variable it's held in with the value as its default, so it keeps
// its name wherever the file uses it. rewrite then generates it in its place, as it
// does any secret's default, and the catalog refuses it inside a longer value, as in
// redis-server --requirepass ${REDIS_PASSWORD}.
func writtenOut(name, value string) (string, error) {
	if !secretNamed(name) || !literalSecret(value) {
		return value, nil
	}
	if !composeVar.MatchString(name) || strings.ContainsAny(value, "{}") {
		return "", leave(ClassCantSay, "its template's secret %s is written out, in a form the converter can't keep apart from the file's own", name)
	}
	return "${" + name + ":-" + strings.TrimSpace(value) + "}", nil
}

// composeVar is a name Compose reads as a variable's.
var composeVar = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// labelOf names an input from its variable until a person does: ADMIN_PASSWORD reads
// Admin password.
func labelOf(name string) string {
	return sentenceCase(titleOf(strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(name))))
}

// usedNames are the variables a file uses.
func usedNames(root *yaml.Node) map[string]bool {
	out := map[string]bool{}
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			out[u.name] = true
		}
	})
	return out
}

// freeName is the first name taken hasn't, which it then has: base, another name
// for it, or base with a number after it.
func freeName(base string, taken map[string]bool, others ...string) string {
	for _, name := range append([]string{base}, others...) {
		if !taken[name] {
			taken[name] = true
			return name
		}
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = base + "_" + strconv.Itoa(i)
	}
	taken[name] = true
	return name
}

// inputLike is a name an entry's input can take.
var inputLike = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// made is a value a source makes up at an install, which becomes a field.
type made struct {
	// id is what makes two uses of it one value: the variable it's held in.
	id    string
	field Field
}

// dokployEval works out what a template's values come to.
type dokployEval struct {
	vars map[string]any
	// hosts are the domains the template routes, by the variable that holds each, as
	// the address an app answers at before a person adds one.
	hosts     map[string]string
	firstHost string
	inline    int
	// calls and written are the work its values have taken so far: how many times a
	// variable was worked out, and how many bytes that wrote, each bounded, as
	// variables that each name another ten times stand for gigabytes in a template of
	// a few hundred bytes.
	calls, written int
}

// What working out one template's values may take: a value is a variable's text, a
// few dozen bytes, and a template has a few dozen of them.
const (
	maxValue      = 4 << 10
	maxValueCalls = 10_000
	maxValueBytes = compose.MaxFile
)

var token = regexp.MustCompile(`\$\{([^{}]*)\}`)

// value works out an expression: a value made up whole, which it returns as made, or
// text, with any variable it uses in its place.
func (ev *dokployEval) value(expr string) (string, *made, error) {
	return ev.expand(expr, "", 0)
}

func (ev *dokployEval) expand(expr, holder string, depth int) (string, *made, error) {
	if depth > 8 {
		return "", nil, leave(ClassCantSay, "uses variables that use each other")
	}
	ev.calls++
	if ev.calls > maxValueCalls {
		return "", nil, leave(ClassFile, "its template's variables take over %d steps to work out, more than any template does", maxValueCalls)
	}
	text, m, err := ev.expandOnce(expr, holder, depth)
	switch {
	case err != nil:
		return "", nil, err
	case len(text) > maxValue:
		return "", nil, leave(ClassFile, "a value of its template comes to over %d KiB, more than any value a template sets", maxValue>>10)
	}
	if ev.written += len(text); ev.written > maxValueBytes {
		return "", nil, leave(ClassFile, "its template's values come to over %d KiB, more than any template's do", maxValueBytes>>10)
	}
	return text, m, nil
}

func (ev *dokployEval) expandOnce(expr, holder string, depth int) (string, *made, error) {
	if m := token.FindStringSubmatch(strings.TrimSpace(expr)); len(m) == 2 && m[0] == strings.TrimSpace(expr) {
		// A variable named as a helper is, as username = "${username}" is, uses the helper.
		if v, ok := ev.vars[m[1]]; ok && m[1] != holder {
			if address, ok := ev.hosts[m[1]]; ok {
				return address, nil, nil
			}
			return ev.expand(fmt.Sprint(v), m[1], depth+1)
		}
		if f, ok, err := helper(m[1]); ok || err != nil {
			if err != nil {
				return "", nil, err
			}
			id := holder
			if id == "" {
				ev.inline++
				id = "inline " + strconv.Itoa(ev.inline)
			}
			return "", &made{id: id, field: f}, nil
		}
		if m[1] == "domain" {
			return ev.firstHost, nil, nil
		}
	}
	var failed error
	text := token.ReplaceAllStringFunc(expr, func(t string) string {
		name := t[2 : len(t)-1]
		if address, ok := ev.hosts[name]; ok {
			return address
		}
		if name == "domain" {
			return ev.firstHost
		}
		if v, ok := ev.vars[name]; ok && name != holder {
			s, m, err := ev.expand(fmt.Sprint(v), name, depth+1)
			switch {
			case err != nil:
				failed = err
			case m != nil:
				failed = leave(ClassInside, "puts a value it makes up, %s, inside a longer one, which an upgrade and a rotation couldn't reach", name)
			}
			return s
		}
		if f, ok, err := helper(name); ok || err != nil {
			if err == nil && f.Kind == Text {
				// A user's name inside a longer value, which is no secret.
				return f.Default
			}
			failed = cmp.Or(err, leave(ClassInside, "puts a value it makes up inside a longer one, which an upgrade and a rotation couldn't reach"))
		}
		return t
	})
	return text, nil, failed
}

// helper reads one of Dokploy's helpers for a value it makes up.
func helper(call string) (Field, bool, error) {
	name, arg, _ := strings.Cut(call, ":")
	n := 0
	if arg != "" {
		var err error
		if n, err = strconv.Atoi(arg); err != nil && (name == "password" || name == "hash" || name == "base64") {
			return Field{}, true, leave(ClassCantSay, "asks for %s of %q, which isn't a length", name, arg)
		}
	}
	if n > catalog.MaxGenerate && (name == "password" || name == "hash" || name == "base64") {
		return Field{}, true, leave(ClassCantSay, "asks for a %s of %d characters, more than the %d Dockwell generates", name, n, catalog.MaxGenerate)
	}
	switch name {
	case "password":
		return Field{Label: "Password", Kind: Generated, Length: n}, true, nil
	case "hash":
		return Field{Label: "Key", Kind: Generated, Length: n, As: catalogv1.GenerateAs_GENERATE_AS_HEX}, true, nil
	case "base64":
		return Field{Label: "Key", Kind: Generated, Length: n, As: catalogv1.GenerateAs_GENERATE_AS_BASE64}, true, nil
	case "username":
		return Field{Label: "User", Kind: Text, Default: "admin"}, true, nil
	case "email":
		return Field{Label: "Email", Kind: Email}, true, nil
	case "uuid", "jwt", "timestamp", "timestamps", "timestampms", "randomPort":
		return Field{}, true, leave(ClassMadeUp, "asks for a %s made up at install, which Dockwell doesn't make yet", name)
	}
	return Field{}, false, nil
}

// soleVar says the variable a value is, when it's one alone.
func soleVar(s string) (string, bool) {
	m := token.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) != 2 || m[0] != strings.TrimSpace(s) {
		return "", false
	}
	return m[1], true
}

// localHTTP writes an address that would be on a domain as the one an app answers at
// until a person adds one: over plain HTTP, on loopback.
func localHTTP(s string) string {
	return strings.ReplaceAll(s, "https://localhost:${", "http://localhost:${")
}

// envPairs reads a template's env, a list of NAME=value or a table, in its order.
func envPairs(env any) ([][2]string, error) {
	var out [][2]string
	switch env := env.(type) {
	case nil:
	case []any:
		for _, e := range env {
			line := strings.TrimSpace(fmt.Sprint(e))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, leave(ClassFile, "its template's env has %q, which names no value", fmt.Sprint(e))
			}
			out = append(out, [2]string{strings.TrimSpace(k), v})
		}
	case map[string]any:
		for _, k := range slices.Sorted(func(yield func(string) bool) {
			for k := range env {
				if !yield(k) {
					return
				}
			}
		}) {
			out = append(out, [2]string{k, fmt.Sprint(env[k])})
		}
	default:
		return nil, leave(ClassFile, "its template's env is %T, where Dokploy reads a list or a table", env)
	}
	return out, nil
}

// envFile writes what a service read from Dokploy's .env as its own environment,
// as an entry ships no file beside its Compose file.
func envFile(doc *yaml.Node, keys []string) error {
	for _, name := range serviceNames(doc) {
		svc := service(doc, name)
		files := mapGet(svc, "env_file")
		if files == nil {
			continue
		}
		var paths []string
		switch files.Kind {
		case yaml.ScalarNode:
			paths = []string{files.Value}
		case yaml.SequenceNode:
			for _, f := range files.Content {
				if p := mapGet(f, "path"); p != nil {
					paths = append(paths, p.Value)
				} else {
					paths = append(paths, f.Value)
				}
			}
		default:
		}
		for _, p := range paths {
			if p != ".env" && p != "./.env" {
				return leave(ClassShipped, "its service %s reads %s, which an entry can't ship", name, p)
			}
		}
		mapDel(svc, "env_file")
		env := mapGet(svc, "environment")
		if env == nil {
			env = &yaml.Node{Kind: yaml.MappingNode}
			svc.Content = append(svc.Content, scalar("environment"), env)
		}
		for _, k := range keys {
			switch env.Kind {
			case yaml.MappingNode:
				if mapGet(env, k) == nil {
					env.Content = append(env.Content, scalar(k), scalar("${"+k+"}"))
				}
			case yaml.SequenceNode:
				if !slices.ContainsFunc(env.Content, func(n *yaml.Node) bool { return strings.HasPrefix(n.Value, k+"=") || n.Value == k }) {
					env.Content = append(env.Content, scalar(k+"=${"+k+"}"))
				}
			default:
			}
		}
	}
	return nil
}

// publish publishes a port of a service that a platform reaches through its own
// proxy, which Dockwell's reaches on loopback, by variable: in place of a port the
// file publishes already, or beside the rest. A service on the server's own network
// publishes none, as it answers on the server's addresses already, and says so.
func publish(doc *yaml.Node, serviceName, variable, port string) (bool, error) {
	svc := service(doc, serviceName)
	if svc == nil {
		return false, leave(ClassCantSay, "its domain routes to %s, a service its Compose file doesn't run", serviceName)
	}
	if mode := mapGet(svc, "network_mode"); mode != nil && strings.TrimSpace(mode.Value) == "host" {
		return false, nil
	}
	ports := mapGet(svc, "ports")
	if ports == nil {
		ports = &yaml.Node{Kind: yaml.SequenceNode}
		svc.Content = append(svc.Content, scalar("ports"), ports)
	}
	for _, p := range ports.Content {
		switch p.Kind {
		case yaml.ScalarNode:
			parts := splitPort(p.Value)
			inApp, proto, _ := strings.Cut(parts[len(parts)-1], "/")
			if inApp == port && (proto == "" || proto == "tcp") {
				p.Value, p.Tag, p.Style = "${"+variable+"}:"+port, "!!str", 0
				return true, nil
			}
		case yaml.MappingNode:
			if target := mapGet(p, "target"); target != nil && target.Value == port {
				if published := mapGet(p, "published"); published != nil {
					published.Value, published.Tag = "${"+variable+"}", "!!str"
				} else {
					p.Content = append(p.Content, scalar("published"), scalar("${"+variable+"}"))
				}
				return true, nil
			}
		default:
		}
	}
	ports.Content = append(ports.Content, scalar("${"+variable+"}:"+port))
	return true, nil
}

// envName is a service's name as part of a variable's: web-ui becomes WEB_UI.
func envName(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(s))
}

// webTag is the tag of the image a person opens, as a version where a source names none.
func webTag(file []byte, fields []Field) string {
	doc, err := parseFile(file)
	if err != nil {
		return ""
	}
	web := ""
	for _, f := range fields {
		if f.Web {
			web = f.Name
		}
	}
	tag := func(svc *yaml.Node) string {
		if img := mapGet(svc, "image"); img != nil {
			if r, err := parseRef(img.Value); err == nil {
				return r.tag
			}
		}
		return ""
	}
	names := serviceNames(doc)
	for _, name := range names {
		svc := service(doc, name)
		if ports := mapGet(svc, "ports"); web != "" && ports != nil && strings.Contains(nodeText(ports), "${"+web+"}") {
			return tag(svc)
		}
	}
	// With no port a person opens, the first service's.
	if len(names) > 0 {
		return tag(service(doc, names[0]))
	}
	return ""
}
