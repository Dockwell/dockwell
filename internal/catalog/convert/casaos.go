// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// casaosApp is what an app's docker-compose.yml says of it in its x-casaos, in
// CasaOS's app store, each word by its language's tag, such as en_US and zh_CN.
type casaosApp struct {
	Architectures []string          `yaml:"architectures"`
	Main          string            `yaml:"main"`
	Category      string            `yaml:"category"`
	Title         map[string]string `yaml:"title"`
	Tagline       map[string]string `yaml:"tagline"`
	Description   map[string]string `yaml:"description"`
	Icon          string            `yaml:"icon"`
	Screenshots   []string          `yaml:"screenshot_link"`
	PortMap       any               `yaml:"port_map"`
	Version       string            `yaml:"version"`
	Website       string            `yaml:"website"`
	Repo          string            `yaml:"repo"`
	Docs          string            `yaml:"docs"`
}

// casaosCategories are the categories CasaOS's suggest, where one does.
var casaosCategories = map[string]string{
	"media": "media", "downloader": "media", "gallery": "media", "developer": "developer tools",
	"network": "networking", "backup": "storage", "cloud": "storage", "file": "storage",
	"analytics": "analytics", "home automation": "automation", "security": "security", "ai": "ai",
	"chat": "cms", "blog": "cms", "database": "databases",
}

// casaosLink is an address CasaOS names a file of its own repository by, through
// jsDelivr or GitHub's raw files: the path after the repository and ref is the file's,
// which Dockwell reads from the commit itself and never fetches.
var casaosLink = regexp.MustCompile(`^https://(?:cdn\.jsdelivr\.net/gh/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[A-Za-z0-9_./-]+?|raw\.githubusercontent\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(Apps/[A-Za-z0-9_.+-]+/[A-Za-z0-9_.+-]+)$`)

var (
	// casaosAppData is where CasaOS keeps an app's data, by the app's ID, which an
	// entry keeps in named volumes of its own when the ID is the app's.
	casaosAppData = regexp.MustCompile(`^/DATA/AppData/([A-Za-z0-9_.-]+)(/|$)`)
	// casaosMedia and casaosDownloads are a person's folders, which an entry asks for
	// as folders a person names.
	casaosMedia     = regexp.MustCompile(`^/DATA/Media\b`)
	casaosDownloads = regexp.MustCompile(`^/DATA/Downloads\b`)
)

// downloadsField is the folder of a person's downloads, where an app binds CasaOS's.
var downloadsField = Field{Name: "DOWNLOADS_DIR", Label: "Downloads folder on the server", Kind: Text, Default: "/srv/downloads"}

// casaosApps reads each app in CasaOS's app store, Apps/<name>/docker-compose.yml,
// stopping once ctx is done.
func casaosApps(ctx context.Context, t *tree, root string) ([]App, error) {
	dirs, err := t.readDir("Apps")
	if err != nil {
		return nil, fmt.Errorf("reading CasaOS's app store: %w. Name the folder of a checkout of github.com/IceWhaleTech/CasaOS-AppStore", err)
	}
	var out []App
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if d.IsDir() && t.isFile(path.Join("Apps", d.Name(), "docker-compose.yml")) {
			out = append(out, casaosRead(t, d.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("CasaOS's app store at %s: %w", root, ErrNoApps)
	}
	return out, nil
}

func casaosRead(t *tree, dir string) App {
	at := path.Join("Apps", dir)
	app := App{ID: strings.NewReplacer("_", "-", ".", "-", "+", "-").Replace(strings.ToLower(dir)), Name: dir, Path: at, Web: true}
	file, err := t.readFile(path.Join(at, "docker-compose.yml"))
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	doc, err := parseFile(file)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	// Bounded, so an alias of aliases in x-casaos stands for no more than a Compose file may.
	if err := expandAliases(doc); err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassFile)
		return app
	}
	var meta casaosApp
	if x := mapGet(doc.Content[0], "x-casaos"); x != nil {
		_ = x.Decode(&meta)
	}
	app.Name = cmp.Or(strings.TrimSpace(meta.Title["en_US"]), dir)
	app.Summary, app.Description = meta.Tagline["en_US"], meta.Description["en_US"]
	if zh := (Words{Summary: meta.Tagline["zh_CN"], Description: meta.Description["zh_CN"]}); zh != (Words{}) {
		app.Words = map[string]Words{"zh-CN": zh}
	}
	app.Homepage, app.Code, app.Docs = cmp.Or(meta.Website, meta.Repo), meta.Repo, meta.Docs
	app.Category = casaosCategories[strings.ToLower(strings.TrimSpace(meta.Category))]
	app.Icon = casaosFile(t, at, meta.Icon)
	for _, link := range meta.Screenshots {
		if p := casaosFile(t, at, link); p != "" && len(app.Screenshots) < 8 {
			app.Screenshots = append(app.Screenshots, Shot{Path: p, Source: link})
		}
	}
	if len(meta.Architectures) > 0 && (!slices.Contains(meta.Architectures, "amd64") || !slices.Contains(meta.Architectures, "arm64")) {
		app.LeftOut, app.Class = fmt.Sprintf("CasaOS says it runs on %s alone, where an entry runs on amd64 and arm64 both", strings.Join(meta.Architectures, " and ")), ClassArch
		return app
	}
	port := ""
	switch v := meta.PortMap.(type) {
	case string:
		port = strings.TrimSpace(v)
	case int:
		port = strconv.Itoa(v)
	}
	app.Compose, app.Fields, err = casaosCompose(doc, app.ID, meta.Main, port)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), classOf(err, ClassCantSay)
		return app
	}
	app.Web = slices.ContainsFunc(app.Fields, func(f Field) bool { return f.Web })
	// CasaOS writes no file beside a Compose file that the app binds.
	app.Shipped = func(string) bool { return false }
	app.Version = strings.TrimSpace(meta.Version)
	if app.Version == "" || unversioned[app.Version] {
		app.Version = webTag(app.Compose, app.Fields)
	}
	return app
}

// casaosFile is the file of the app's folder an address of CasaOS's names, if it
// names one there that the commit holds, with an image's extension.
func casaosFile(t *tree, appDir, link string) string {
	m := casaosLink.FindStringSubmatch(strings.TrimSpace(link))
	if m == nil {
		return ""
	}
	p, err := url.PathUnescape(m[1])
	if err != nil || path.Dir(p) != appDir {
		return ""
	}
	return storeIcon(t, appDir, path.Base(p))
}

// casaosCompose writes what CasaOS does at an install into the file: the app's data
// folder as its own, a person's media and downloads as folders they name, the time
// zone and user as defaults, the port a person opens as a field, and nothing of
// CasaOS's own: no x-casaos, no name for the project, and no network_mode: bridge,
// which is Docker's default.
func casaosCompose(doc *yaml.Node, id, main, webPort string) ([]byte, []Field, error) {
	root := doc.Content[0]
	mapDel(root, "x-casaos")
	// CasaOS's ID for the app, as $AppID names it, is its project's name.
	own := id
	if n := mapGet(root, "name"); n != nil && n.Kind == yaml.ScalarNode && strings.TrimSpace(n.Value) != "" {
		own = strings.TrimSpace(n.Value)
	}
	mapDel(root, "name")
	walkScalars(root, func(n *yaml.Node) {
		v := strings.NewReplacer("${AppID}", own).Replace(braceVars(n.Value))
		v = strings.NewReplacer("${TZ}", "${TZ:-UTC}", "${PUID}", "${PUID:-1000}", "${PGID}", "${PGID:-1000}").Replace(v)
		n.Value = v
	})
	fields := []Field{}
	asked := map[string]bool{}
	ask := func(f Field) {
		if !asked[f.Name] {
			asked[f.Name] = true
			fields = append(fields, f)
		}
	}
	names := serviceNames(doc)
	if main == "" && len(names) > 0 {
		main = names[0]
	}
	for _, name := range names {
		svc := service(doc, name)
		mapDel(svc, "x-casaos")
		if nm := mapGet(svc, "network_mode"); nm != nil && nm.Value == "bridge" {
			mapDel(svc, "network_mode")
		}
		if vols := mapGet(svc, "volumes"); vols != nil {
			for _, v := range vols.Content {
				src := v
				if v.Kind == yaml.MappingNode {
					src = mapGet(v, "source")
				}
				if src == nil {
					continue
				}
				host, rest, _ := strings.Cut(src.Value, ":")
				if v.Kind == yaml.MappingNode {
					host, rest = src.Value, ""
				}
				data := casaosAppData.FindStringSubmatch(host)
				switch {
				case len(data) > 1 && !strings.EqualFold(data[1], own) && !strings.EqualFold(data[1], id):
					return nil, nil, leave(ClassAnotherApp, "its service %s binds %s, another app's data in CasaOS", name, host)
				case len(data) > 1:
					// Its folder's own folders keep their names; the folder itself is data.
					host = "./" + strings.TrimPrefix(casaosAppData.ReplaceAllLiteralString(host, ""), "/")
					if host == "./" {
						host = "./data"
					}
				case casaosMedia.MatchString(host):
					host = casaosMedia.ReplaceAllLiteralString(host, "${MEDIA_DIR}")
					ask(mediaField)
				case casaosDownloads.MatchString(host):
					host = casaosDownloads.ReplaceAllLiteralString(host, "${DOWNLOADS_DIR}")
					ask(downloadsField)
				case host == "/DATA" || strings.HasPrefix(host, "/DATA/"):
					return nil, nil, leave(ClassCantSay, "its service %s binds %s, a folder of CasaOS's own, which an entry can't know", name, host)
				}
				if rest != "" {
					host += ":" + rest
				}
				src.Value = host
			}
		}
		if name != main || webPort == "" {
			continue
		}
		if ports := mapGet(svc, "ports"); ports != nil {
			for _, p := range ports.Content {
				if casaosPublish(p, webPort) {
					ask(Field{Name: "PORT", Label: "Port", Kind: Port, Default: webPort, Web: true})
					break
				}
			}
		}
	}
	out, err := encodeFile(doc)
	return out, fields, err
}

// casaosPublish makes the port a service publishes as webPort the PORT field's, in
// either of Compose's ways to write one, and says whether it found it.
func casaosPublish(p *yaml.Node, webPort string) bool {
	if _, err := strconv.Atoi(webPort); err != nil {
		return false
	}
	switch p.Kind {
	case yaml.MappingNode:
		if pub := mapGet(p, "published"); pub != nil && strings.TrimSpace(pub.Value) == webPort {
			pub.Value, pub.Tag, pub.Style = "${PORT}", "!!str", yaml.DoubleQuotedStyle
			return true
		}
	case yaml.ScalarNode:
		parts := splitPort(p.Value)
		if len(parts) >= 2 && parts[len(parts)-2] == webPort {
			parts[len(parts)-2] = "${PORT}"
			p.Value, p.Tag, p.Style = strings.Join(parts, ":"), "!!str", yaml.DoubleQuotedStyle
			return true
		}
	default:
	}
	return false
}
