// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// OnePanelSource is 1Panel's app store, under the licence its repository declares.
func OnePanelSource(commit string) Source {
	return Source{
		Name:       "1Panel's app store",
		Repository: "https://github.com/1Panel-dev/appstore",
		Commit:     commit,
		License:    "GPL-3.0-only",
	}
}

// onePanelApp is what an app's data.yml says of it.
type onePanelApp struct {
	Name string `yaml:"name"`
	More struct {
		Key     string   `yaml:"key"`
		Tags    []string `yaml:"tags"`
		Website string   `yaml:"website"`
		Github  string   `yaml:"github"`
	} `yaml:"additionalProperties"`
}

// onePanelField is one field of a version's form.
type onePanelField struct {
	Default  any               `yaml:"default"`
	EnvKey   string            `yaml:"envKey"`
	Key      string            `yaml:"key"`
	LabelEn  string            `yaml:"labelEn"`
	Label    map[string]string `yaml:"-"`
	Required bool              `yaml:"required"`
	Disabled bool              `yaml:"disabled"`
	Random   bool              `yaml:"random"`
	Rule     string            `yaml:"rule"`
	Type     string            `yaml:"type"`
	Values   []struct {
		Label string `yaml:"label"`
		Value any    `yaml:"value"`
	} `yaml:"values"`
}

func (f *onePanelField) UnmarshalYAML(n *yaml.Node) error {
	type plain onePanelField
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	// label is a map of languages in newer apps and a string in a few older ones.
	if l := mapGet(n, "label"); l != nil && l.Kind == yaml.MappingNode {
		_ = l.Decode(&p.Label)
	}
	*f = onePanelField(p)
	return nil
}

// onePanelCategories are the categories 1Panel's tags suggest, where one does.
var onePanelCategories = map[string]string{
	"AI": "ai", "BI": "analytics", "Database": "databases", "DevOps": "developer tools", "DevTool": "developer tools",
	"Media": "media", "Security": "security", "Storage": "storage", "Website": "cms",
}

// OnePanel reads the apps in a checkout of 1Panel's app store, each at its newest
// version: apps/<key>/data.yml says what the app is, and the version's folder has
// its form, data.yml, and its docker-compose.yml.
func OnePanel(root string) ([]App, error) {
	dirs, err := os.ReadDir(filepath.Join(root, "apps"))
	if err != nil {
		return nil, fmt.Errorf("reading 1Panel's app store: %w. Name the folder of a checkout of github.com/1Panel-dev/appstore", err)
	}
	var out []App
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		app, ok := onePanelRead(root, d.Name())
		if ok {
			out = append(out, app)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("1Panel's app store at %s: %w", root, ErrNoApps)
	}
	return out, nil
}

func onePanelRead(root, dir string) (App, bool) {
	appDir := filepath.Join(root, "apps", dir)
	app := App{ID: strings.ToLower(dir)}
	versions, err := os.ReadDir(appDir)
	if err != nil {
		return app, false
	}
	var names []string
	for _, v := range versions {
		if _, err := os.Stat(filepath.Join(appDir, v.Name(), "docker-compose.yml")); err == nil && v.IsDir() && v.Name() != "latest" {
			names = append(names, v.Name())
		}
	}
	if len(names) == 0 {
		return app, false
	}
	slices.SortFunc(names, compareVersions)
	var meta onePanelApp
	if err := readYAML(filepath.Join(appDir, "data.yml"), &meta); err != nil {
		app.LeftOut = err.Error()
		return app, true
	}
	// An entry's ID takes dashes where 1Panel's keys take underscores.
	app.ID = strings.ReplaceAll(strings.ToLower(cmp.Or(meta.More.Key, dir)), "_", "-")
	app.Name = cmp.Or(meta.Name, dir)
	app.Homepage = cmp.Or(meta.More.Website, meta.More.Github)
	for _, tag := range meta.More.Tags {
		if c := onePanelCategories[tag]; c != "" {
			app.Category = c
			break
		}
	}
	// Each version, newest first, so one that's too young falls back to the one before.
	var newest *App
	for i := len(names) - 1; i >= 0; i-- {
		v := onePanelVersion(app, appDir, dir, names[i])
		if newest == nil {
			newest = &v
		} else {
			last := newest
			for last.Older != nil {
				last = last.Older
			}
			last.Older = &v
		}
	}
	return *newest, true
}

// onePanelVersion reads one version of an app.
func onePanelVersion(app App, appDir, dir, version string) App {
	app.Version = version
	versionDir := filepath.Join(appDir, version)
	app.Path = filepath.ToSlash(filepath.Join("apps", dir, version))
	var err error
	if fi, err := os.Stat(filepath.Join(versionDir, "scripts")); err == nil && fi.IsDir() {
		app.LeftOut = "it runs scripts on the server as it installs or upgrades, which an entry can't, as a plan couldn't show what they do"
		return app
	}
	app.Compose, err = os.ReadFile(filepath.Join(versionDir, "docker-compose.yml")) //nolint:gosec // a file of the checkout the person running it names
	if err != nil {
		app.LeftOut = err.Error()
		return app
	}
	app.Shipped = func(rel string) bool {
		_, err := os.Stat(filepath.Join(versionDir, filepath.FromSlash(rel)))
		return err == nil
	}
	var form struct {
		More struct {
			Fields []onePanelField `yaml:"formFields"`
		} `yaml:"additionalProperties"`
	}
	if err := readYAML(filepath.Join(versionDir, "data.yml"), &form); err != nil {
		app.LeftOut = err.Error()
		return app
	}
	for i := range form.More.Fields {
		f := &form.More.Fields[i]
		if f.Disabled || f.EnvKey == "" {
			continue
		}
		if f.Type == "service" || f.Type == "apps" {
			app.LeftOut = fmt.Sprintf("it uses %s from another app, which the converter can't bring into its own project yet", cmp.Or(f.Key, "a service"))
			return app
		}
		app.Fields = append(app.Fields, onePanelFieldOf(f))
	}
	return app
}

func onePanelFieldOf(f *onePanelField) Field {
	out := Field{
		Name:     f.EnvKey,
		Label:    cmp.Or(f.Label["en"], f.LabelEn, f.EnvKey),
		Optional: !f.Required,
		Web:      f.EnvKey == "PANEL_APP_PORT_HTTP",
	}
	if f.Default != nil {
		out.Default = fmt.Sprint(f.Default)
	}
	switch {
	case f.Rule == "paramPort" || strings.HasPrefix(f.EnvKey, "PANEL_APP_PORT_"):
		out.Kind = Port
	case f.Type == "password" && f.Random:
		out.Kind = Generated
	case f.Type == "password":
		out.Kind = Secret
	default:
		out.Kind = Text
		for _, v := range f.Values {
			out.Choices = append(out.Choices, fmt.Sprint(v.Value))
		}
	}
	return out
}

func readYAML(file string, into any) error {
	raw, err := os.ReadFile(file) //nolint:gosec // a file of the checkout the person running it names
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(file), err)
	}
	return nil
}

var versionPart = regexp.MustCompile(`\d+|[^\d.\-_]+`)

// trimV takes the v off a version such as v2.0, which counts as 2.0.
func trimV(v string) string {
	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		return v[1:]
	}
	return v
}

// compareVersions orders versions as people read them, by each number in turn, so
// 1.10 comes after 1.9.
func compareVersions(a, b string) int {
	pa, pb := versionPart.FindAllString(trimV(a), -1), versionPart.FindAllString(trimV(b), -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil:
			if c := cmp.Compare(na, nb); c != 0 {
				return c
			}
		case ea == nil:
			return 1
		case eb == nil:
			return -1
		default:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(len(pa), len(pb))
}
