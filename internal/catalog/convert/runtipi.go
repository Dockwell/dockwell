// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// runtipiApp is what an app's config.json says of it, in Runtipi's app store.
type runtipiApp struct {
	Name        string   `json:"name"`
	ID          string   `json:"id"`
	Available   *bool    `json:"available"`
	Deprecated  bool     `json:"deprecated"`
	Port        int      `json:"port"`
	Version     string   `json:"version"`
	Categories  []string `json:"categories"`
	ShortDesc   string   `json:"short_desc"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Website     string   `json:"website"`
	NoGUI       bool     `json:"no_gui"`
	// UID and GID are the user and group Runtipi gives the app's data folder to, by
	// running chown on the server as it installs.
	UID  *int     `json:"uid"`
	GID  *int     `json:"gid"`
	Arch []string `json:"supported_architectures"`
	// GenerateVapidKeys has Runtipi make a pair of keys at install, which Dockwell
	// doesn't make.
	GenerateVapidKeys bool           `json:"generate_vapid_keys"`
	FormFields        []runtipiField `json:"form_fields"`
}

// runtipiField is one field of an app's form.
type runtipiField struct {
	Type     string `json:"type"`
	Label    string `json:"label"`
	Env      string `json:"env_variable"`
	Required bool   `json:"required"`
	Min      int    `json:"min"`
	Default  any    `json:"default"`
	Options  []struct {
		Label string `json:"label"`
		Value any    `json:"value"`
	} `json:"options"`
}

// runtipiCategories are the categories Runtipi's tags suggest, where one does.
var runtipiCategories = map[string]string{
	"ai": "ai", "automation": "automation", "data": "databases", "development": "developer tools",
	"media": "media", "music": "media", "photography": "media", "books": "media", "network": "networking",
	"security": "security", "finance": "analytics", "social": "cms",
}

// runtipiData is where Runtipi keeps an app's own data on the server, which an entry
// keeps in named volumes of its own: ${APP_DATA_DIR}/data/<name>, or ${APP_DATA_DIR}/<name>.
var runtipiData = regexp.MustCompile(`\$\{APP_DATA_DIR\}(/data)?(/|$)`)

// runtipiPlatform are the variables Runtipi fills in that an entry can say in its own
// terms: the address it answers at, as Coolify's are, with the port a field asks for,
// and the server's time zone, which a container reads as UTC unless told. The rest of
// Runtipi's own, such as ${NETWORK_INTERFACE}, leave the app out, as an entry can't
// know them.
var runtipiPlatform = strings.NewReplacer(
	"${APP_PROTOCOL:-http}", "http",
	"${APP_PROTOCOL:-https}", "http",
	"${APP_PROTOCOL}", "http",
	"${APP_DOMAIN}", "localhost:${APP_PORT}",
	"${TZ}", "${TZ:-UTC}",
)

// runtipiMedia is Runtipi's folder of a person's media on the server, which an entry
// asks for as a folder a person names, MEDIA_DIR, as the plan shows it as a path on
// the server for them to approve. Runtipi's own folder otherwise leaves the app out.
var runtipiMedia = regexp.MustCompile(`\$\{ROOT_FOLDER_HOST\}/media\b`)

// mediaField is the folder of a person's media, where an app binds Runtipi's.
var mediaField = Field{Name: "MEDIA_DIR", Label: "Media folder on the server", Kind: Text, Default: "/srv/media"}

// runtipiApps reads each app in Runtipi's app store, apps/<id>/, stopping once ctx is
// done.
func runtipiApps(ctx context.Context, t *tree, root string) ([]App, error) {
	dirs, err := t.readDir("apps")
	if err != nil {
		return nil, fmt.Errorf("reading Runtipi's app store: %w. Name the folder of a checkout of github.com/runtipi/runtipi-appstore", err)
	}
	var out []App
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if d.IsDir() && t.isFile(path.Join("apps", d.Name(), "config.json")) {
			out = append(out, runtipiRead(t, d.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Runtipi's app store at %s: %w", root, ErrNoApps) //nolint:staticcheck // it starts with a name
	}
	return out, nil
}

func runtipiRead(t *tree, dir string) App {
	at := path.Join("apps", dir)
	app := App{ID: strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(dir)), Name: dir, Path: at}
	raw, err := t.readFile(path.Join(at, "config.json"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	var meta runtipiApp
	if err := json.Unmarshal(raw, &meta); err != nil {
		app.LeftOut, app.Class = "reading its config.json: "+err.Error(), ClassFile
		return app
	}
	app.Name, app.Version = cmp.Or(strings.TrimSpace(meta.Name), dir), meta.Version
	app.Homepage, app.Code = cmp.Or(meta.Website, meta.Source), meta.Source
	app.Summary, app.Description = meta.ShortDesc, meta.Description
	app.Web = !meta.NoGUI
	for _, c := range meta.Categories {
		if cat := runtipiCategories[strings.ToLower(c)]; cat != "" {
			app.Category = cat
			break
		}
	}
	app.Icon = storeIcon(t, path.Join(at, "metadata"), "logo.jpg")
	if b, err := t.readFile(path.Join(at, "metadata", "description.md")); err == nil {
		app.Readme = string(b)
	}
	switch {
	case meta.Available != nil && !*meta.Available, meta.Deprecated:
		app.LeftOut, app.Class = "Runtipi marks it as one to leave out", ClassSkipped
		return app
	case len(meta.Arch) > 0 && (!slices.Contains(meta.Arch, "amd64") || !slices.Contains(meta.Arch, "arm64")):
		app.LeftOut, app.Class = fmt.Sprintf("Runtipi says it runs on %s alone, where an entry runs on amd64 and arm64 both", strings.Join(meta.Arch, " and ")), ClassArch
		return app
	case meta.UID != nil || meta.GID != nil:
		app.LeftOut, app.Class = "Runtipi gives its data folder to another user on the server as it installs, which an entry can't do yet", ClassScripts
		return app
	case meta.GenerateVapidKeys:
		app.LeftOut, app.Class = "Runtipi makes a pair of keys for it at install, which Dockwell doesn't make yet", ClassMadeUp
		return app
	}
	file, err := t.readFile(path.Join(at, "docker-compose.yml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	fields, err := runtipiFields(meta)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	if app.Compose, err = runtipiCompose(file); err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	if strings.Contains(string(app.Compose), "${MEDIA_DIR}") && !slices.ContainsFunc(fields, func(f Field) bool { return f.Name == mediaField.Name }) {
		fields = append(fields, mediaField)
	}
	app.Fields = fields
	// Runtipi writes no file beside its Compose file that the app binds: a folder it
	// binds is one the app makes as it runs.
	app.Shipped = func(string) bool { return false }
	if app.Version == "" || unversioned[app.Version] {
		app.Version = webTag(app.Compose, app.Fields)
	}
	return app
}

// runtipiFields reads an app's form as fields: the port Runtipi publishes it on,
// and each field by the variable it fills.
func runtipiFields(meta runtipiApp) ([]Field, error) {
	port := meta.Port
	if port <= 0 || port > 65535 {
		port = 8080
	}
	fields := []Field{{Name: "APP_PORT", Label: "Port", Kind: Port, Default: strconv.Itoa(port), Web: !meta.NoGUI}}
	seen := map[string]bool{"APP_PORT": true}
	for _, f := range meta.FormFields {
		name := strings.TrimSpace(f.Env)
		if !inputName.MatchString(name) {
			return nil, leave(ClassCantSay, "its form fills %q, which isn't a variable's name", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		field := Field{Name: name, Label: cmp.Or(strings.TrimSpace(f.Label), name), Optional: !f.Required, Default: plainDefault(f.Default)}
		switch f.Type {
		case "random":
			field.Kind, field.Optional, field.Default = Generated, false, ""
			field.Length = max(32, min(f.Min, 64))
		case "password":
			field.Kind, field.Default = Secret, ""
		case "email":
			field.Kind = Email
		case "number":
			field.Kind = Text
		case "boolean":
			field.Kind, field.Choices = Text, []string{"true", "false"}
			if field.Default == "" {
				field.Default = "false"
			}
		default:
			field.Kind = Text
			for _, o := range f.Options {
				if v := plainDefault(o.Value); v != "" {
					field.Choices = append(field.Choices, v)
				}
			}
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// inputName is a variable a form may fill.
var inputName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// plainDefault is a form's default as a value a Compose file takes, or "" for one
// that isn't a plain string, number or boolean.
func plainDefault(v any) string {
	switch d := v.(type) {
	case string:
		return d
	case float64:
		return strconv.FormatFloat(d, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(d)
	}
	return ""
}

// runtipiCompose writes what Runtipi does at an install into the file: its own data
// folder as the app's folder, which becomes named volumes, the address the app
// answers at, and no network or label of Runtipi's own.
func runtipiCompose(file []byte) ([]byte, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, err
	}
	if err := expandAliases(doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	walkScalars(root, func(n *yaml.Node) {
		n.Value = runtipiData.ReplaceAllString(braceVars(n.Value), "./data$2")
		n.Value = runtipiMedia.ReplaceAllLiteralString(runtipiPlatform.Replace(n.Value), "${MEDIA_DIR}")
	})
	for _, name := range serviceNames(doc) {
		svc := service(doc, name)
		if nets := mapGet(svc, "networks"); nets != nil {
			switch nets.Kind {
			case yaml.SequenceNode:
				nets.Content = slices.DeleteFunc(nets.Content, func(n *yaml.Node) bool { return n.Value == "tipi_main_network" })
			case yaml.MappingNode:
				mapDel(nets, "tipi_main_network")
			default:
			}
			if len(nets.Content) == 0 {
				mapDel(svc, "networks")
			}
		}
		if labels := mapGet(svc, "labels"); labels != nil {
			dropRuntipiLabels(labels)
			if len(labels.Content) == 0 {
				mapDel(svc, "labels")
			}
		}
	}
	if nets := mapGet(root, "networks"); nets != nil {
		mapDel(nets, "tipi_main_network")
		if len(nets.Content) == 0 {
			mapDel(root, "networks")
		}
	}
	return encodeFile(doc)
}

// dropRuntipiLabels drops Runtipi's own labels, which nothing in Dockwell reads; the
// rewrite drops Traefik's.
func dropRuntipiLabels(labels *yaml.Node) {
	switch labels.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(labels.Content); {
			if strings.HasPrefix(labels.Content[i].Value, "runtipi.") {
				labels.Content = append(labels.Content[:i], labels.Content[i+2:]...)
			} else {
				i += 2
			}
		}
	case yaml.SequenceNode:
		labels.Content = slices.DeleteFunc(labels.Content, func(n *yaml.Node) bool { return strings.HasPrefix(n.Value, "runtipi.") })
	default:
	}
}
