// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"regexp"
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
	dir := path.Join("templates", "compose")
	ents, err := t.readDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading Coolify's templates: %w. Name the folder of a checkout of github.com/coollabsio/coolify", err)
	}
	var out []App
	for _, d := range ents {
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

// route is a port Coolify's proxy reaches a service on.
type route struct{ service, id, port, variable string }

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
	var routes []route
	for _, name := range serviceNames(doc) {
		env := mapGet(service(doc, name), "environment")
		if env == nil {
			continue
		}
		keep := func(key string) bool {
			kind, id, port, ok := coolifyVar(key)
			if !ok || kind != "URL" && kind != "FQDN" {
				return true
			}
			routes = append(routes, route{service: name, id: id, port: port})
			return false
		}
		switch env.Kind {
		case yaml.SequenceNode:
			env.Content = slices.DeleteFunc(env.Content, func(n *yaml.Node) bool { return !strings.Contains(n.Value, "=") && !keep(strings.TrimSpace(n.Value)) })
		case yaml.MappingNode:
			for i := 0; i+1 < len(env.Content); {
				if v := env.Content[i+1]; (v.Value == "" || v.Tag == "!!null") && !keep(env.Content[i].Value) {
					env.Content = append(env.Content[:i], env.Content[i+2:]...)
				} else {
					i += 2
				}
			}
		default:
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
		r.variable = "PORT"
		if i > 0 {
			r.variable = envName(r.id) + "_PORT"
		}
		if err := publish(doc, r.service, r.variable, r.port); err != nil {
			return nil, nil, err
		}
		fields = append(fields, Field{Name: r.variable, Label: "Port", Kind: Port, Default: r.port, Web: i == 0})
	}
	address := func(id string, scheme bool) string {
		at := ""
		for _, r := range routes {
			if r.id == id || at == "" {
				at = r.variable
			}
		}
		switch {
		case !scheme:
			return "localhost"
		case at == "":
			return ""
		}
		return "http://localhost:${" + at + "}"
	}
	// Each SERVICE_ variable becomes what Coolify makes of it.
	made := map[string]string{}
	var fail error
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			kind, id, _, ok := coolifyVar(u.name)
			if !ok {
				if strings.HasPrefix(u.name, "SERVICE_") && fail == nil {
					fail = leave(ClassMadeUp, "it uses %s, which Coolify makes and Dockwell doesn't yet", u.name)
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
					input = coolifyInput(kind, id, fields)
					made[u.name] = input
					f := coolifyField(kind)
					f.Name, f.Label = input, sentenceCase(titleOf(strings.ToLower(strings.ReplaceAll(input, "_", "-"))))
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

// coolifyInput names the field a SERVICE_ variable becomes: SERVICE_PASSWORD_MYSQL
// becomes MYSQL_PASSWORD, SERVICE_USER_MYSQL MYSQL_USER, and a key MYSQL_SECRET.
func coolifyInput(kind, id string, fields []Field) string {
	suffix := "_SECRET"
	switch {
	case strings.HasPrefix(kind, "PASSWORD"):
		suffix = "_PASSWORD"
	case strings.HasSuffix(kind, "USER"):
		suffix = "_USER"
	}
	name := envName(id) + suffix
	for i := 2; slices.ContainsFunc(fields, func(f Field) bool { return f.Name == name }); i++ {
		name = envName(id) + suffix + strconv.Itoa(i)
	}
	return name
}

// secretLike says a variable a person sets holds a secret, by its name.
var secretLike = regexp.MustCompile(`PASS|SECRET|TOKEN|(^|_)KEY($|_)|PRIVATE`)

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
		// A secret the file gives a default everyone would share is one a person
		// must type, as an entry's secret takes no default.
		shared := sharedSecret(name, v.def)
		if !v.bare && !v.required && !shared {
			continue
		}
		f := Field{Name: name, Label: sentenceCase(titleOf(strings.ToLower(strings.ReplaceAll(name, "_", "-")))), Kind: Text, Default: v.def, Optional: !v.required && !shared}
		if secretLike.MatchString(name) {
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
