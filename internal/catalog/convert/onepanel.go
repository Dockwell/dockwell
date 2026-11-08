// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"fmt"
	"path"
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
		Type    string   `yaml:"type"`
		Website string   `yaml:"website"`
		Github  string   `yaml:"github"`
		// Architectures are the platforms 1Panel says the app runs on.
		Architectures []string `yaml:"architectures"`
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

// onePanelWeb are 1Panel's types of app that a person opens in a browser.
var onePanelWeb = []string{"website", "aiwebsite"}

// OnePanel reads the apps in a checkout of 1Panel's app store, each at its newest
// version: apps/<key>/data.yml says what the app is, and the version's folder has
// its form, data.yml, and its docker-compose.yml. It reads only regular files and
// folders inside the checkout, each up to MaxFile.
func OnePanel(root string) ([]App, error) {
	t, err := openTree(root)
	if err != nil {
		return nil, fmt.Errorf("reading 1Panel's app store: %w. Name the folder of a checkout of github.com/1Panel-dev/appstore", err)
	}
	defer t.Close()
	dirs, err := t.readDir("apps")
	if err != nil {
		return nil, fmt.Errorf("reading 1Panel's app store: %w. Name the folder of a checkout of github.com/1Panel-dev/appstore", err)
	}
	var out []App
	for _, d := range dirs {
		if d.IsDir() {
			out = append(out, onePanelRead(t, d.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("1Panel's app store at %s: %w", root, ErrNoApps)
	}
	return out, nil
}

func onePanelRead(t *tree, dir string) App {
	appDir := path.Join("apps", dir)
	// An entry's ID takes dashes where 1Panel's keys take underscores.
	app := App{ID: strings.ReplaceAll(strings.ToLower(dir), "_", "-"), Name: dir, Path: appDir}
	var meta onePanelApp
	if err := readYAML(t, path.Join(appDir, "data.yml"), &meta); err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	app.ID = strings.ReplaceAll(strings.ToLower(cmp.Or(meta.More.Key, dir)), "_", "-")
	app.Name = cmp.Or(meta.Name, dir)
	app.Homepage = cmp.Or(meta.More.Website, meta.More.Github)
	app.Web = slices.Contains(onePanelWeb, strings.TrimSpace(meta.More.Type))
	for _, tag := range meta.More.Tags {
		if c := onePanelCategories[tag]; c != "" {
			app.Category = c
			break
		}
	}
	versions, err := t.readDir(appDir)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	var names []string
	latest := false
	for _, v := range versions {
		if !v.IsDir() || !t.isFile(path.Join(appDir, v.Name(), "docker-compose.yml")) {
			continue
		}
		if unversioned[v.Name()] {
			latest = true
			continue
		}
		names = append(names, v.Name())
	}
	switch {
	case len(names) == 0 && latest:
		app.LeftOut, app.Class = "it has only a version named latest, which moves, so no entry could pin it", ClassNoVersion
		return app
	case len(names) == 0:
		app.LeftOut, app.Class = "it has no version with a Compose file of its own", ClassFile
		return app
	}
	if arch := meta.More.Architectures; len(arch) > 0 && (!slices.Contains(arch, "amd64") || !slices.Contains(arch, "arm64")) {
		app.LeftOut, app.Class = fmt.Sprintf("1Panel says it runs on %s alone, where an entry runs on amd64 and arm64 both", strings.Join(arch, " and ")), ClassArch
		return app
	}
	slices.SortFunc(names, compareVersions)
	// Each version, newest first, so one that's too young can fall back to the one
	// before, within its major version.
	var newest *App
	for i := len(names) - 1; i >= 0; i-- {
		v := onePanelVersion(t, app, appDir, names[i])
		if newest == nil {
			newest = &v
			continue
		}
		last := newest
		for last.Older != nil {
			last = last.Older
		}
		last.Older = &v
	}
	return *newest
}

// onePanelVersion reads one version of an app.
func onePanelVersion(t *tree, app App, appDir, version string) App {
	app.Version = version
	versionDir := path.Join(appDir, version)
	app.Path = versionDir
	if t.isDir(path.Join(versionDir, "scripts")) {
		app.LeftOut, app.Class = "it runs scripts on the server as it installs or upgrades, which an entry can't, as a plan couldn't show what they do", ClassScripts
		return app
	}
	var err error
	if app.Compose, err = t.readFile(path.Join(versionDir, "docker-compose.yml")); err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	shipped, err := t.shipped(versionDir)
	if err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	for _, own := range []string{"docker-compose.yml", "data.yml", "README.md", "README_en.md"} {
		delete(shipped, own)
	}
	app.Shipped = func(rel string) bool { return shipped[rel] }
	var form struct {
		More struct {
			Fields []onePanelField `yaml:"formFields"`
		} `yaml:"additionalProperties"`
	}
	if err := readYAML(t, path.Join(versionDir, "data.yml"), &form); err != nil {
		app.LeftOut, app.Class = err.Error(), ClassFile
		return app
	}
	for i := range form.More.Fields {
		f := &form.More.Fields[i]
		if f.Disabled || f.EnvKey == "" {
			continue
		}
		if f.Type == "service" || f.Type == "apps" {
			what := cmp.Or(f.Key, f.LabelEn, f.EnvKey)
			app.LeftOut, app.Class = fmt.Sprintf("it uses %s from another app, for its %s, which the converter can't bring into its own project yet", what, f.EnvKey), ClassAnotherApp
			return app
		}
		app.Fields = append(app.Fields, onePanelFieldOf(f, app.Web))
	}
	return app
}

// secretName says a field holds a secret, by its name: a password, a key, a salt or
// a token, which 1Panel sometimes types as text.
var secretName = regexp.MustCompile(`PASSWORD|PASSWD|(^|_)PWD($|_)|SECRET|TOKEN|(^|_)SALT($|_)|(^|_)KEY($|_)`)

func onePanelFieldOf(f *onePanelField, web bool) Field {
	out := Field{
		Name:     f.EnvKey,
		Label:    cmp.Or(f.Label["en"], f.LabelEn, f.EnvKey),
		Optional: !f.Required,
		Web:      web && f.EnvKey == "PANEL_APP_PORT_HTTP",
	}
	if f.Default != nil {
		out.Default = fmt.Sprint(f.Default)
	}
	secret := f.Type == "password" || secretName.MatchString(strings.ToUpper(f.EnvKey))
	switch {
	case f.Rule == "paramPort" || strings.HasPrefix(f.EnvKey, "PANEL_APP_PORT_"):
		out.Kind = Port
	case secret && f.Random:
		// 1Panel makes one up at each install, which an entry generates and seals,
		// in place of the default it would otherwise share with everyone.
		out.Kind = Generated
	case secret:
		out.Kind = Secret
	default:
		out.Kind = Text
		for _, v := range f.Values {
			out.Choices = append(out.Choices, fmt.Sprint(v.Value))
		}
	}
	return out
}

func readYAML(t *tree, file string, into any) error {
	raw, err := t.readFile(file)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
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

// majorOf is a version's major: its first number, as 13 of 13.23-alpine, or its
// first two where the first is 0, as semantic versions read 0.31 as one.
func majorOf(v string) string {
	var nums []string
	for _, p := range versionPart.FindAllString(trimV(v), -1) {
		if _, err := strconv.Atoi(p); err == nil {
			nums = append(nums, p)
		}
	}
	switch {
	case len(nums) == 0:
		return ""
	case nums[0] == "0" && len(nums) > 1:
		return "0." + nums[1]
	}
	return nums[0]
}
