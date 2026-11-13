// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// umbrelApp is what an app's umbrel-app.yml says of it, in Umbrel's app store.
type umbrelApp struct {
	Name         string   `yaml:"name"`
	Tagline      string   `yaml:"tagline"`
	Description  string   `yaml:"description"`
	Version      string   `yaml:"version"`
	Category     string   `yaml:"category"`
	Website      string   `yaml:"website"`
	Repo         string   `yaml:"repo"`
	Dependencies []string `yaml:"dependencies"`
}

// umbrelCategories are the categories Umbrel's suggest, where one does.
var umbrelCategories = map[string]string{
	"media": "media", "developer": "developer tools", "networking": "networking", "files": "storage",
	"automation": "automation", "ai": "ai", "social": "cms", "finance": "analytics",
}

var (
	// umbrelData is where Umbrel keeps an app's own data, which an entry keeps in named
	// volumes of its own.
	umbrelData = regexp.MustCompile(`\$\{APP_DATA_DIR\}(/data)?(/|$)`)
	// umbrelDownloads is Umbrel's folder of a person's downloads, which an entry asks
	// for as a folder a person names.
	umbrelDownloads = regexp.MustCompile(`\$\{UMBREL_ROOT\}/data/storage/downloads([/:]|$)`)
	// umbrelHost is the container Umbrel's proxy reaches the app at: the app's ID, the
	// service, and Compose's number for it, as actual_web_1.
	umbrelHost = regexp.MustCompile(`^[a-z0-9-]+_([a-z0-9_-]+?)_\d+$`)
)

// umbrelMade are the values Umbrel makes for each app, which an entry generates.
var umbrelMade = []Field{
	{Name: "APP_SEED", Label: "Seed", Kind: Generated, Length: 64, As: catalogv1.GenerateAs_GENERATE_AS_HEX},
	{Name: "APP_PASSWORD", Label: "Password", Kind: Generated, Length: 32},
}

// umbrelApps reads each app in Umbrel's app store, <id>/umbrel-app.yml at its top,
// stopping once ctx is done.
func umbrelApps(ctx context.Context, t *tree, root string) ([]App, error) {
	dirs, err := t.readDir(".")
	if err != nil {
		return nil, fmt.Errorf("reading Umbrel's app store: %w. Name the folder of a checkout of github.com/getumbrel/umbrel-apps", err)
	}
	var out []App
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if d.IsDir() && t.isFile(path.Join(d.Name(), "umbrel-app.yml")) {
			out = append(out, umbrelRead(t, d.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Umbrel's app store at %s: %w", root, ErrNoApps) //nolint:staticcheck // it starts with a name
	}
	return out, nil
}

func umbrelRead(t *tree, dir string) App {
	app := App{ID: strings.ToLower(dir), Name: dir, Path: dir, Web: true}
	raw, err := t.readFile(path.Join(dir, "umbrel-app.yml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	doc, err := parseFile(raw)
	if err == nil {
		err = expandAliases(doc)
	}
	if err != nil {
		app.LeftOut, app.Class = "reading its umbrel-app.yml: "+err.Error(), ClassFile
		return app
	}
	var meta umbrelApp
	_ = doc.Content[0].Decode(&meta)
	app.Name, app.Version = cmp.Or(strings.TrimSpace(meta.Name), dir), strings.TrimSpace(meta.Version)
	app.Summary, app.Description = meta.Tagline, meta.Description
	app.Homepage, app.Code = cmp.Or(meta.Website, meta.Repo), meta.Repo
	app.Category = umbrelCategories[strings.ToLower(strings.TrimSpace(meta.Category))]
	switch {
	case t.isDir(path.Join(dir, "hooks")):
		app.LeftOut, app.Class = "Umbrel runs its hooks, scripts, on the server as it installs or upgrades, which an entry can't, as a plan couldn't show what they do", ClassScripts
		return app
	case t.isFile(path.Join(dir, "exports.sh")):
		app.LeftOut, app.Class = "Umbrel runs its exports.sh, a script, to make its values, which an entry can't", ClassScripts
		return app
	case len(meta.Dependencies) > 0:
		app.LeftOut, app.Class = fmt.Sprintf("it needs %s installed beside it in Umbrel, which an entry can't bring into its own project yet", strings.Join(meta.Dependencies, ", ")), ClassAnotherApp
		return app
	}
	file, err := t.readFile(path.Join(dir, "docker-compose.yml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	if app.Compose, app.Fields, err = umbrelCompose(file, dir); err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	app.Web = len(app.Fields) > 0 && app.Fields[0].Web
	// Umbrel writes no file beside a Compose file that the app binds.
	app.Shipped = func(string) bool { return false }
	if app.Version == "" || unversioned[app.Version] {
		app.Version = webTag(app.Compose, app.Fields)
	}
	return app
}

// umbrelCompose writes what Umbrel does at an install into the file: its proxy goes,
// and the service and port it reached are published on a port a field asks for; the
// app's data folder is its own, the downloads folder one a person names, the address
// it answers at its local one, and the seed and password Umbrel makes, values an
// entry generates. A container its services reach by Umbrel's name for it, as
// actual_web_1, they reach by its service's name. Any other value of Umbrel's leaves
// the app out.
func umbrelCompose(file []byte, id string) ([]byte, []Field, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, nil, err
	}
	if err := expandAliases(doc); err != nil {
		return nil, nil, err
	}
	root := doc.Content[0]
	var fields []Field
	services := mapGet(root, "services")
	if proxy := mapGet(services, "app_proxy"); proxy != nil {
		host, port := envValue(mapGet(proxy, "environment"), "APP_HOST"), envValue(mapGet(proxy, "environment"), "APP_PORT")
		mapDel(services, "app_proxy")
		if host != nil && port != nil {
			m := umbrelHost.FindStringSubmatch(strings.TrimSpace(*host))
			n, perr := strconv.Atoi(strings.TrimSpace(*port))
			if m == nil || perr != nil || n <= 0 || n > 65535 {
				return nil, nil, leave(ClassCantSay, "its proxy reaches %s on %s, which names no service and port of its own", *host, *port)
			}
			svc := service(doc, m[1])
			if svc == nil {
				return nil, nil, leave(ClassCantSay, "its proxy reaches the service %s, which its Compose file doesn't run", m[1])
			}
			ports := mapGet(svc, "ports")
			if ports == nil {
				ports = &yaml.Node{Kind: yaml.SequenceNode}
				svc.Content = append(svc.Content, scalar("ports"), ports)
			}
			ports.Content = append(ports.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "${PORT}:" + strconv.Itoa(n), Tag: "!!str", Style: yaml.DoubleQuotedStyle})
			fields = append(fields, Field{Name: "PORT", Label: "Port", Kind: Port, Default: strconv.Itoa(8000 + n%1000), Web: true})
		}
	}
	images := imageNodes(doc)
	containers := map[string]string{}
	for _, name := range serviceNames(doc) {
		containers[id+"_"+name+"_1"] = name
	}
	var climbing string
	walkScalars(root, func(n *yaml.Node) {
		if images[n] {
			return
		}
		v := braceVars(n.Value)
		if climbsAfter(umbrelData, v) || climbsAfter(umbrelDownloads, v) {
			climbing = cmp.Or(climbing, n.Value)
		}
		v = renamed(umbrelData.ReplaceAllString(v, "./data$2"), containers)
		v = inFolder(umbrelDownloads, v, "${DOWNLOADS_DIR}")
		n.Value = strings.ReplaceAll(v, "${DEVICE_DOMAIN_NAME}", "localhost")
	})
	if climbing != "" {
		return nil, nil, leave(ClassCantSay, "it names %s, which climbs out of the folder it starts in", climbing)
	}
	out, err := encodeFile(doc)
	if err != nil {
		return nil, nil, err
	}
	text := string(out)
	for _, f := range umbrelMade {
		if strings.Contains(text, "${"+f.Name+"}") {
			fields = append(fields, f)
		}
	}
	if strings.Contains(text, "${DOWNLOADS_DIR}") {
		fields = append(fields, downloadsField)
	}
	return out, fields, nil
}

// renamed is a value with each container name Umbrel gives a service, as
// planka_db_1, written as the service's name, in one pass over the value. A name is
// one only where it stands whole, so planka_db_10 and myplanka_db_1 keep theirs.
func renamed(v string, containers map[string]string) string {
	if len(containers) == 0 || !strings.Contains(v, "_1") {
		return v
	}
	nameChar := func(b byte) bool {
		return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	var out strings.Builder
	for i := 0; i < len(v); {
		j := i
		for j < len(v) && nameChar(v[j]) {
			j++
		}
		if j == i {
			out.WriteByte(v[i])
			i++
			continue
		}
		if to, ok := containers[v[i:j]]; ok {
			out.WriteString(to)
		} else {
			out.WriteString(v[i:j])
		}
		i = j
	}
	return out.String()
}

// envValue is a variable's value in a service's environment, written as a mapping or
// as a list of NAME=value, or nil where it isn't set.
func envValue(env *yaml.Node, name string) *string {
	if env == nil {
		return nil
	}
	switch env.Kind {
	case yaml.MappingNode:
		if v := mapGet(env, name); v != nil && v.Kind == yaml.ScalarNode {
			return &v.Value
		}
	case yaml.SequenceNode:
		for _, item := range env.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok && k == name {
				return &v
			}
		}
	default:
	}
	return nil
}
