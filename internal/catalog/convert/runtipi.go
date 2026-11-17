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
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
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
// keeps in named volumes of its own: ${APP_DATA_DIR}/data/<name>, ${APP_DATA_DIR}/<name>,
// or the whole folder, ${APP_DATA_DIR}, as ./data.
var runtipiData = regexp.MustCompile(`\$\{APP_DATA_DIR\}((/data)?/|:|$)`)

// runtipiByHand are apps a review found can't start as converted, for a reason the
// converter can't read from their files, and why.
var runtipiByHand = map[string]string{
	// Its image makes /cache as root's, which Docker copies into the new volume, and
	// it runs as 1000, so it can't write there (#570's Runtipi review, H2).
	"tubearchivist": "its volume at /cache would be root's, where it runs as user 1000 and writes there; Runtipi makes the folder for that user before it starts, and an entry gives a volume an owner only from a store's own script",
}

// runtipiFillsItself are apps whose image writes its own starting files into an
// empty folder, so each ran on an empty volume before Runtipi's files were carried,
// and still does: the files Runtipi ships for them aren't copied until #778, and a
// folder of them the app writes to, or the whole folder they land in, doesn't leave
// them out (#570's Runtipi recheck, N1).
var runtipiFillsItself = map[string]bool{
	"calibre-web":     true, // starts with an empty config, and a library a person names at /books
	"dashy":           true, // its image's user-data, conf.yml with it, is copied into a new volume
	"homeassistant-1": true, // writes a default configuration.yaml into an empty /config
	"nodered":         true, // writes its default settings.js into an empty /data
	"searxng":         true, // its entrypoint writes settings.yml when there's none
}

// runtipiPlatform are the variables Runtipi fills in that an entry can say in its own
// terms: the address it answers at, as Coolify's are, with the port a field asks for,
// and the server's time zone, which a container reads as UTC unless told. The hosts
// it fills in are runtipiHosts', and its resolver dropRuntipiDNS's. The rest of
// Runtipi's own, such as ${NETWORK_INTERFACE} or its own folder, leave the app out,
// as an entry can't know them.
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
var runtipiMedia = regexp.MustCompile(`\$\{ROOT_FOLDER_HOST\}/media([/:]|$)`)

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
			out = append(out, readSafely(d.Name(), path.Join("apps", d.Name()), func() App { return runtipiRead(t, d.Name()) }))
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
	app.Mail = mailFor("runtipi", app.ID)
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
	if why := runtipiByHand[app.ID]; why != "" {
		app.LeftOut, app.Class = why, ClassCantSay
		return app
	}
	switch {
	case meta.Available != nil && !*meta.Available, meta.Deprecated:
		app.LeftOut, app.Class = "Runtipi marks it as one to leave out", ClassSkipped
		return app
	case meta.UID != nil || meta.GID != nil:
		app.LeftOut, app.Class = "Runtipi gives its data folder to another user on the server as it installs, which an entry can't do yet", ClassScripts
		return app
	case meta.GenerateVapidKeys:
		app.LeftOut, app.Class = "Runtipi makes a pair of keys for it at install, which Dockwell doesn't make yet", ClassMadeUp
		return app
	}
	var archErr error
	if app.Architectures, archErr = architecturesOf("Runtipi", meta.Arch); archErr != nil {
		app.LeftOut, app.Class = archErr.Error(), ClassArch
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
	if app.Compose, err = runtipiCompose(file, dir); err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	if strings.Contains(string(app.Compose), "${MEDIA_DIR}") && !slices.ContainsFunc(fields, func(f Field) bool { return f.Name == mediaField.Name }) {
		fields = append(fields, mediaField)
	}
	app.Fields = fields
	app.Address = runtipiAddress(app.ID, file)
	if err := runtipiShipped(t, &app, at, file); err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassShipped)
		return app
	}
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
func runtipiCompose(file []byte, id string) ([]byte, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, err
	}
	if err := expandAliases(doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	var climbing, ownFolder, fromInside, noHost string
	walkScalars(root, func(n *yaml.Node) {
		v := braceVars(n.Value)
		if climbsAfter(runtipiData, v) || climbsAfter(runtipiMedia, v) {
			climbing = cmp.Or(climbing, n.Value)
		}
		v = inFolder(runtipiData, v, "./data")
		v, hosts := runtipiHosts(v, id)
		if !hosts {
			noHost = cmp.Or(noHost, n.Value)
		}
		v = inFolder(runtipiMedia, runtipiPlatform.Replace(v), "${MEDIA_DIR}")
		if strings.Contains(v, "${ROOT_FOLDER_HOST}") {
			ownFolder = cmp.Or(ownFolder, n.Value)
		}
		if strings.Contains(v, "${INTERNAL_IP}") || strings.Contains(v, "${APP_HOST}") {
			fromInside = cmp.Or(fromInside, n.Value)
		}
		n.Value = v
	})
	switch {
	case climbing != "":
		return nil, leave(ClassCantSay, "it names %s, which climbs out of the folder it starts in", climbing)
	case ownFolder != "":
		return nil, leave(ClassPlatform, "it binds Runtipi's own folder on the server, %s, which Dockwell has nothing like", ownFolder)
	case fromInside != "":
		return nil, leave(ClassPlatform, "it names the server's address, %s, which an entry has none of to give", fromInside)
	case noHost != "":
		return nil, leave(ClassPlatform, "it accepts only hosts Runtipi's own proxy answers, %s, which leaves it none", noHost)
	}
	for _, name := range serviceNames(doc) {
		svc := service(doc, name)
		dropRuntipiDNS(svc)
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

// runtipiOwnHost are Runtipi's names for a host only its own proxy answers to,
// <app>.<local domain>, which a list of hosts or origins an app accepts drops.
var runtipiOwnHost = regexp.MustCompile(`\$\{(APP_)?LOCAL_DOMAIN\}`)

// runtipiServerURL is the server's address in Runtipi's words in a URL a person's
// browser opens the app at, at the app's own port, which an entry says as localhost,
// as it says ${APP_DOMAIN}; runtipiServerItem is the same as an item of a list of
// hosts. At another port it's a container reaching the server, which isn't
// localhost there (#570's Runtipi review, M2).
var (
	runtipiServerURL  = regexp.MustCompile(`(https?://)\$\{(?:INTERNAL_IP|APP_HOST)\}(:\$\{APP_PORT\})`)
	runtipiServerItem = regexp.MustCompile(`^\$\{(?:INTERNAL_IP|APP_HOST)\}(:\$\{APP_PORT\})$`)
)

// envKey is the name before a value in a list of environment, KEY=value.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// runtipiHosts writes what Runtipi fills in about the app and the server as an entry
// says it: the app's ID as it is, the user it runs as as 1000, as the store's other
// apps write it, and the server's address where a browser opens the app as
// localhost. A list of hosts or origins the app accepts drops Runtipi's own and
// names each once, and says false where none is left. The server's address anywhere
// else is left as it is, which leaves the app out: inside a container, localhost is
// the container.
func runtipiHosts(v, id string) (string, bool) {
	v = strings.NewReplacer("${APP_ID}", id, "${TIPI_UID}", "1000", "${TIPI_GID}", "1000").Replace(v)
	asked := v
	v = runtipiServerURL.ReplaceAllString(v, "${1}localhost${2}")
	if !strings.Contains(v, ",") || v == asked && !runtipiOwnHost.MatchString(v) && !strings.Contains(v, "${INTERNAL_IP}") && !strings.Contains(v, "${APP_HOST}") {
		return v, true
	}
	key := envKey.FindString(v)
	// Each host once, as Dockwell says it: the server's address and ${APP_DOMAIN} are
	// both localhost.
	var items, said []string
	for _, item := range strings.Split(strings.TrimPrefix(v, key), ",") {
		if runtipiOwnHost.MatchString(item) {
			continue
		}
		item = runtipiServerItem.ReplaceAllString(item, "localhost${1}")
		if as := runtipiPlatform.Replace(item); !slices.Contains(said, as) {
			items, said = append(items, item), append(said, as)
		}
	}
	// A list of Runtipi's own hosts alone leaves none, which an app reads as no host
	// at all, or as every one (#570's Runtipi review, L1).
	return key + strings.Join(items, ","), len(items) > 0
}

// dropRuntipiDNS drops the resolver Runtipi sets for a service, its own setting,
// so the service asks the server's own, as every container Dockwell runs does.
func dropRuntipiDNS(svc *yaml.Node) {
	dns := mapGet(svc, "dns")
	switch {
	case dns == nil:
	case dns.Kind == yaml.ScalarNode && dns.Value == "${DNS_IP}":
		mapDel(svc, "dns")
	case dns.Kind == yaml.SequenceNode:
		dns.Content = slices.DeleteFunc(dns.Content, func(n *yaml.Node) bool { return n.Value == "${DNS_IP}" })
		if len(dns.Content) == 0 {
			mapDel(svc, "dns")
		}
	}
}

// runtipiUse is a path in the app's data folder its Compose file names, after
// ${APP_DATA_DIR}: "" for the folder itself, /data/<path> for a path in what its
// store ships, or another.
var runtipiUse = regexp.MustCompile(`\$\{?APP_DATA_DIR\}?([^\s:'"]*)`)

// runtipiShipped gives the app the files its store ships in its data/ folder,
// which Runtipi copies into ${APP_DATA_DIR}/data as it installs: a file or a
// read-only folder the app binds, at ${APP_DATA_DIR}/data/<path>, is one Dockwell
// writes, as the other stores' are (ADR 0085), and a folder the app writes to stays
// out, as theirs do. ./data/<path> is that path, so a file is fetched by its own
// path in the store. What Runtipi does more than copy leaves the app out: a binding
// of the whole folder its files land in, and a .template, which Runtipi fills in
// with the app's values.
func runtipiShipped(t *tree, app *App, at string, file []byte) error {
	if runtipiFillsItself[app.ID] {
		app.Shipped = func(string) bool { return false }
		return nil
	}
	dir := path.Join(at, "data")
	ships := map[string]bool{}
	if t.isDir(dir) {
		var err error
		if ships, err = t.shipped(dir); err != nil {
			return leave(ClassFile, "%s", err)
		}
	}
	// What the file names in data/, as ./data/<path>, and what it names elsewhere in
	// the app's folder, which Runtipi ships nothing into.
	elsewhere := map[string]bool{}
	whole := ""
	for _, m := range runtipiUse.FindAllStringSubmatch(string(file), -1) {
		rest := path.Clean("/" + m[1])
		switch {
		case rest == "/" || rest == "/data":
			whole = cmp.Or(whole, m[0])
			elsewhere[path.Join("data", strings.TrimPrefix(rest, "/"))] = true
		case !strings.HasPrefix(rest, "/data/"):
			elsewhere[path.Join("data", rest)] = true
		}
	}
	if whole != "" && len(ships) > 0 {
		return leave(ClassShipped, "it binds %s, the folder Runtipi copies the files its store ships into as it installs, which Dockwell would copy into its volume. It doesn't yet", whole)
	}
	var templates []string
	for rel := range ships {
		if strings.HasSuffix(rel, ".template") {
			templates = append(templates, rel)
		}
	}
	slices.Sort(templates)
	for _, m := range runtipiUse.FindAllStringSubmatch(string(file), -1) {
		named := strings.TrimPrefix(path.Clean("/"+m[1]), "/data/")
		for _, tmpl := range templates {
			if made := strings.TrimSuffix(tmpl, ".template"); made == named || strings.HasPrefix(made, named+"/") {
				return leave(ClassShipped, "it ships data/%s, which Runtipi fills in with the app's values as it installs, and Dockwell doesn't yet", tmpl)
			}
		}
	}
	inData := func(rel string) (string, bool) {
		in, ok := strings.CutPrefix(rel, "data/")
		return in, ok && !elsewhere[rel] && ships[in]
	}
	app.Shipped = func(rel string) bool {
		_, ok := inData(rel)
		return ok
	}
	// Each file Runtipi ships is a file as it is.
	app.LiteralFiles = true
	// A checkout closes once its store is read, so what it ships is read with it; a
	// commit in memory reads, or asks for, a file as a container mounts it.
	var read map[string][]byte
	if t.mem == nil {
		read = map[string][]byte{}
		for rel := range ships {
			if b, err := t.readFile(path.Join(dir, rel)); err == nil && len(b) <= maxShipped {
				read[rel] = b
			}
		}
	}
	app.ReadShipped = func(rel string) ([]byte, error) {
		in, ok := inData(rel)
		switch {
		case !ok:
			return nil, fmt.Errorf("%s isn't a file it ships", rel)
		case read != nil && read[in] == nil:
			return nil, fmt.Errorf("it ships %s, which is a folder, or over %d KiB", rel, maxShipped>>10)
		case read != nil:
			return read[in], nil
		}
		return t.readFile(path.Join(dir, in))
	}
	app.ShippedUnder = func(rel string) []string {
		var out []string
		for p := range ships {
			if full := path.Join("data", p); strings.HasPrefix(full, rel+"/") && t.isFile(path.Join(dir, p)) {
				out = append(out, full)
			}
		}
		slices.Sort(out)
		return out
	}
	return nil
}

// runtipiProtocol is a value that is Runtipi's protocol alone, which the converter
// writes as http.
var runtipiProtocol = regexp.MustCompile(`^\$\{APP_PROTOCOL(:-https?)?\}$`)

// runtipiAddress are what follows a Runtipi app's address beyond what names it at
// its port: a value that is Runtipi's protocol alone takes the address's scheme, as
// Nextcloud's OVERWRITEPROTOCOL does, and what runtipiAddressByHand adds (ADR 0103;
// #779's review, M4).
func runtipiAddress(id string, file []byte) []*catalogv1.AddressVariable {
	doc, err := parseFile(file)
	if err != nil || len(doc.Content) == 0 {
		return nil
	}
	var out []*catalogv1.AddressVariable
	for _, name := range serviceNames(doc) {
		for _, kv := range serviceEnv(mapGet(service(doc, name), "environment")) {
			if runtipiProtocol.MatchString(strings.TrimSpace(kv[1])) {
				out = append(out, &catalogv1.AddressVariable{Service: name, Variable: kv[0], Value: "{scheme}"})
			}
		}
	}
	for _, a := range runtipiAddressByHand[id] {
		if service(doc, a.GetService()) != nil {
			out = append(out, proto.CloneOf(a))
		}
	}
	return out
}

// runtipiAddressByHand are variables a review found an app needs to follow its
// address that its store's file doesn't name, by app.
var runtipiAddressByHand = map[string][]*catalogv1.AddressVariable{
	// Nextcloud's image reads OVERWRITEHOST at every start, and Nextcloud trusts the
	// host it names, where NEXTCLOUD_TRUSTED_DOMAINS is read on its first install
	// alone, so a domain added later needs it.
	"nextcloud": {{Service: "nextcloud", Variable: "OVERWRITEHOST", Value: "{host}"}},
}
