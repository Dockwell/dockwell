// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// CoolifySource is Coolify's templates, under the Apache-2.0 licence its repository
// declares.
func CoolifySource(commit string) Source {
	return Source{
		Name:        "Coolify's templates",
		Repository:  "https://github.com/coollabsio/coolify",
		Commit:      commit,
		License:     "Apache-2.0",
		LicenseFile: "LICENSE",
	}
}

// coolifyCategories are the categories Coolify's own suggest, where one does.
var coolifyCategories = map[string]string{
	"ai": "ai", "analytics": "analytics", "automation": "automation", "cms": "cms", "database": "databases",
	"databases": "databases", "development": "developer tools", "devtools": "developer tools", "media": "media",
	"monitoring": "monitoring", "networking": "networking", "productivity": "notes", "security": "security",
	"storage": "storage",
}

// Coolify reads the templates in a checkout of Coolify's repository, one Compose file
// each in templates/compose/, with what it says of itself in comments at its top.
func Coolify(root string) ([]App, error) {
	t, err := openTree(root)
	if err != nil {
		return nil, fmt.Errorf("reading Coolify's templates: %w. Name the folder of a checkout of github.com/coollabsio/coolify", err)
	}
	defer t.Close()
	return coolifyApps(context.Background(), t, root)
}

// coolifyApps reads each template, stopping once ctx is done.
func coolifyApps(ctx context.Context, t *tree, root string) ([]App, error) {
	dir := path.Join("templates", "compose")
	ents, err := t.readDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading Coolify's templates: %w. Name the folder of a checkout of github.com/coollabsio/coolify", err)
	}
	var out []App
	for _, d := range ents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, ok := strings.CutSuffix(d.Name(), ".yaml")
		if !ok || d.IsDir() {
			continue
		}
		out = append(out, coolifyRead(t, dir, name))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Coolify's templates at %s: %w", root, ErrNoApps) //nolint:staticcheck // it starts with a name
	}
	return out, nil
}

func coolifyRead(t *tree, dir, name string) App {
	id := strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(name))
	app := App{ID: id, Name: titleOf(id), Path: path.Join(dir, name+".yaml"), Web: true}
	file, err := t.readFile(path.Join(dir, name+".yaml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	head := coolifyHeader(file)
	app.Homepage = head["documentation"]
	app.Summary = firstSentence(head["slogan"])
	// # logo: svgs/<name> names a file in the repository's public/svgs.
	if logo, ok := strings.CutPrefix(head["logo"], "svgs/"); ok {
		app.Icon = storeIcon(t, "public/svgs", logo)
	}
	for _, c := range strings.Split(head["category"], ",") {
		if c := coolifyCategories[strings.TrimSpace(strings.ToLower(c))]; c != "" {
			app.Category = c
			break
		}
	}
	switch {
	case head["ignore"] == "true":
		app.LeftOut, app.Class = "Coolify marks it as one to leave out", ClassSkipped
		return app
	case head["amd_only"] == "true":
		app.LeftOut, app.Class = "Coolify says it runs on amd64 alone, where an entry runs on amd64 and arm64 both", ClassArch
		return app
	}
	app.Compose, app.Fields, err = coolifyCompose(file, head["port"])
	if err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	// Coolify writes no file beside a template, so a folder it binds is one the app
	// makes as it runs.
	app.Shipped = func(string) bool { return false }
	app.Version = webTag(app.Compose, app.Fields)
	return app
}

// coolifyHeader reads the comments at a template's top, such as # port: 2368.
func coolifyHeader(file []byte) map[string]string {
	out := map[string]string{}
	s := bufio.NewScanner(bytes.NewReader(file))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, "#")
		if !ok {
			break
		}
		if k, v, ok := strings.Cut(rest, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// titleOf names an app from its ID until a person does: n8n-with-postgresql reads
// N8n with postgresql.
func titleOf(id string) string {
	return upperFirst(strings.ReplaceAll(id, "-", " "))
}

// What Coolify makes up for each kind of its SERVICE_ variables, longest first, as
// its generateEnvValue does.
var coolifyMade = []struct {
	kind  string
	field Field
}{
	{"PASSWORDWITHSYMBOLS_64", Field{Kind: Generated, Length: 64}},
	{"PASSWORDWITHSYMBOLS", Field{Kind: Generated, Length: 32}},
	{"PASSWORD_64", Field{Kind: Generated, Length: 64}},
	{"PASSWORD", Field{Kind: Generated, Length: 32}},
	{"REALBASE64_128", Field{Kind: Generated, Length: 128, As: catalogv1.GenerateAs_GENERATE_AS_BASE64}},
	{"REALBASE64_64", Field{Kind: Generated, Length: 64, As: catalogv1.GenerateAs_GENERATE_AS_BASE64}},
	{"REALBASE64_32", Field{Kind: Generated, Length: 32, As: catalogv1.GenerateAs_GENERATE_AS_BASE64}},
	{"REALBASE64", Field{Kind: Generated, Length: 32, As: catalogv1.GenerateAs_GENERATE_AS_BASE64}},
	// Coolify's own comment: this is not base64, it's just a random string.
	{"BASE64_128", Field{Kind: Generated, Length: 128}},
	{"BASE64_64", Field{Kind: Generated, Length: 64}},
	{"BASE64_32", Field{Kind: Generated, Length: 32}},
	{"BASE64", Field{Kind: Generated, Length: 32}},
	{"HEX_128", Field{Kind: Generated, Length: 128, As: catalogv1.GenerateAs_GENERATE_AS_HEX}},
	{"HEX_64", Field{Kind: Generated, Length: 64, As: catalogv1.GenerateAs_GENERATE_AS_HEX}},
	{"HEX_32", Field{Kind: Generated, Length: 32, As: catalogv1.GenerateAs_GENERATE_AS_HEX}},
	{"LOWERCASEUSER", Field{Kind: Text}},
	{"USER", Field{Kind: Text}},
	{"URL", Field{}},
	{"FQDN", Field{}},
}

// coolifyVar reads one of Coolify's SERVICE_ variables: its kind, and the
// identifier and port after it, as in SERVICE_URL_GHOST_2368.
func coolifyVar(name string) (kind, id, port string, ok bool) {
	rest, ok := strings.CutPrefix(name, "SERVICE_")
	if !ok {
		return "", "", "", false
	}
	for i := range coolifyMade {
		m := &coolifyMade[i]
		if after, found := strings.CutPrefix(rest, m.kind+"_"); found && after != "" {
			id = after
			if m.kind == "URL" || m.kind == "FQDN" {
				if i := strings.LastIndex(after, "_"); i > 0 {
					if _, err := strconv.Atoi(after[i+1:]); err == nil {
						id, port = after[:i], after[i+1:]
					}
				}
			}
			return m.kind, id, port, true
		}
	}
	return "", "", "", false
}

// route is a port Coolify's proxy reaches a service on, and the path it routes there,
// if any, as SERVICE_URL_API_8080=/api routes /api.
type route struct{ service, id, port, path, variable string }

// coolifyCompose writes what Coolify does at an install into the file: the port its
// proxy reaches, published, its SERVICE_ variables as fields or as the address the
// app answers at, and each variable a person sets as one.
func coolifyCompose(file []byte, headPort string) ([]byte, []Field, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, nil, err
	}
	root := doc.Content[0]
	walkScalars(root, func(n *yaml.Node) { n.Value = braceVars(n.Value) })
	// What the template or its file names already, which a port Dockwell publishes by
	// or a value it makes up takes no name of.
	taken := usedNames(root)
	var routes []route
	for _, name := range serviceNames(doc) {
		env := mapGet(service(doc, name), "environment")
		if env == nil {
			continue
		}
		// keep says whether a variable stays in the environment: a route Coolify's proxy
		// takes, with no value or a path, goes.
		keep := func(key, value string) bool {
			kind, id, port, ok := coolifyVar(key)
			value = strings.TrimSpace(value)
			if !ok || kind != "URL" && kind != "FQDN" || value != "" && !strings.HasPrefix(value, "/") {
				return true
			}
			routes = append(routes, route{service: name, id: id, port: port, path: strings.TrimRight(value, "/")})
			return false
		}
		switch env.Kind {
		case yaml.SequenceNode:
			env.Content = slices.DeleteFunc(env.Content, func(n *yaml.Node) bool {
				key, value, set := strings.Cut(n.Value, "=")
				key = strings.TrimSpace(key)
				if !set {
					// A variable the container takes from those Compose reads.
					taken[key] = true
				}
				if !keep(key, value) {
					return true
				}
				if _, _, _, ok := coolifyVar(key); ok && !set {
					// A value Coolify makes, declared bare, which the container gets.
					n.Value = key + "=${" + key + "}"
				}
				return false
			})
		case yaml.MappingNode:
			for i := 0; i+1 < len(env.Content); {
				key, v := env.Content[i].Value, env.Content[i+1]
				value := v.Value
				if v.Tag == "!!null" {
					value = ""
					taken[key] = true
				}
				if !keep(key, value) {
					env.Content = append(env.Content[:i], env.Content[i+2:]...)
					continue
				}
				if _, _, _, ok := coolifyVar(key); ok && value == "" {
					v.Value, v.Tag, v.Style = "${"+key+"}", "!!str", 0
				}
				i += 2
			}
		default:
		}
		if len(env.Content) == 0 {
			mapDel(service(doc, name), "environment")
		}
	}
	// The web port first: the header's, or the first route's.
	slices.SortStableFunc(routes, func(a, b route) int {
		return boolInt(b.port == headPort) - boolInt(a.port == headPort)
	})
	var fields []Field
	for i := range routes {
		r := &routes[i]
		if r.port == "" {
			r.port = headPort
		}
		if r.port == "" {
			return nil, nil, leave(ClassCantSay, "its service %s is reached through Coolify's proxy on no port it names", r.service)
		}
		if j := slices.IndexFunc(routes[:i], func(o route) bool { return o.service == r.service && o.port == r.port }); j >= 0 {
			r.variable = routes[j].variable
			continue
		}
		base := "PORT"
		if i > 0 {
			base = envName(r.id) + "_PORT"
		}
		variable := freeName(base, taken, "HOST_"+base)
		published, err := publish(doc, r.service, variable, r.port)
		if err != nil {
			return nil, nil, err
		}
		if !published {
			// A service on the server's own network answers on its own port there.
			continue
		}
		r.variable = variable
		fields = append(fields, Field{Name: r.variable, Label: "Port", Kind: Port, Default: r.port, Web: i == 0})
	}
	address := func(id string, scheme bool) string {
		var at *route
		for i := range routes {
			if routes[i].id == id || at == nil {
				at = &routes[i]
			}
		}
		switch {
		case !scheme:
			return "localhost"
		case at == nil:
			return ""
		case at.variable == "":
			return "http://localhost:" + at.port + at.path
		}
		return "http://localhost:${" + at.variable + "}" + at.path
	}
	// Each SERVICE_ variable becomes what Coolify makes of it.
	made := map[string]string{}
	var fail error
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			kind, id, _, ok := coolifyVar(u.name)
			if !ok {
				switch {
				case fail != nil:
				case strings.HasPrefix(u.name, "SERVICE_"):
					fail = leave(ClassMadeUp, "it uses %s, which Coolify makes and Dockwell doesn't yet", u.name)
				case strings.HasPrefix(u.name, "COOLIFY_"):
					fail = leave(ClassPlatform, "it uses %s, which Coolify fills in about the app it runs, and no person could", u.name)
				}
				continue
			}
			switch kind {
			case "URL", "FQDN":
				with := address(id, kind == "URL")
				if with == "" && fail == nil {
					fail = leave(ClassCantSay, "it uses %s, the address of a service Coolify's proxy doesn't reach", u.name)
				}
				n.Value = replaceVar(n.Value, u.name, with)
			default:
				input, ok := made[u.name]
				if !ok {
					input = coolifyInput(kind, id, taken)
					made[u.name] = input
					f := coolifyField(kind)
					f.Name, f.Label = input, labelOf(input)
					if f.Kind == Text {
						f.Default = strings.ToLower(id)
					}
					fields = append(fields, f)
				}
				n.Value = replaceVar(n.Value, u.name, "${"+input+"}")
			}
		}
	})
	if fail != nil {
		return nil, nil, fail
	}
	asked := askedFields(root, fields)
	// One a person must set is asked for by the form, as :? asked Compose to.
	walkScalars(root, func(n *yaml.Node) {
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

func coolifyField(kind string) Field {
	for i := range coolifyMade {
		m := &coolifyMade[i]
		if m.kind == kind {
			return m.field
		}
	}
	return Field{}
}

// coolifyInput names the field a SERVICE_ variable becomes, by a name nothing else
// has: SERVICE_PASSWORD_MYSQL becomes MYSQL_PASSWORD, SERVICE_USER_MYSQL MYSQL_USER,
// and a key MYSQL_SECRET, where the identifier doesn't say so already, as
// SERVICE_BASE64_JWT_SECRET's does.
func coolifyInput(kind, id string, taken map[string]bool) string {
	suffix := "SECRET"
	switch {
	case strings.HasPrefix(kind, "PASSWORD"):
		suffix = "PASSWORD"
	case strings.HasSuffix(kind, "USER"):
		suffix = "USER"
	}
	base := envName(id)
	if !strings.HasSuffix(base, suffix) {
		base += "_" + suffix
	}
	return freeName(base, taken)
}

// askedFields makes a field of each variable a person sets: one the file uses with
// no default somewhere, with the default it has elsewhere, if any. One it always
// gives a default stays as the file says.
func askedFields(root *yaml.Node, have []Field) []Field {
	type seen struct {
		bare, required bool
		def            string
	}
	vars := map[string]*seen{}
	var order []string
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			if slices.ContainsFunc(have, func(f Field) bool { return f.Name == u.name }) {
				continue
			}
			v := vars[u.name]
			if v == nil {
				v = &seen{}
				vars[u.name] = v
				order = append(order, u.name)
			}
			switch u.op {
			case "":
				v.bare = true
			case ":?", "?":
				v.required = true
			case ":-", "-":
				if v.def == "" {
					v.def = defaultOf(n.Value, u)
				}
			}
		}
	})
	var out []Field
	for _, name := range order {
		v := vars[name]
		if !v.bare && !v.required {
			// One the file always gives a default stays as it says, but for a secret's,
			// which rewrite makes a secret of its own.
			continue
		}
		f := Field{Name: name, Label: labelOf(name), Kind: Text, Default: v.def, Optional: !v.required}
		switch {
		case !secretNamed(name):
		case literalSecret(v.def):
			// A default everyone would share is a secret Dockwell makes up instead.
			f = Field{Name: name, Label: f.Label, Kind: Generated}
		default:
			f.Kind, f.Default = Secret, ""
		}
		out = append(out, f)
	}
	return out
}

// defaultOf reads the default a use of a variable gives, as in ${PORT:-8080}.
func defaultOf(s string, u varUse) string {
	start := u.end + len(u.op)
	end := start
	for depth := 1; end < len(s); end++ {
		switch {
		case strings.HasPrefix(s[end:], "${"):
			depth++
			end++
		case s[end] == '}':
			depth--
			if depth == 0 {
				return s[start:end]
			}
		}
	}
	return ""
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
