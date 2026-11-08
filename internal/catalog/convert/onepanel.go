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
	"unicode"

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
	// A default written as an example in another language, as some of 1Panel's are,
	// is no value to start from.
	if strings.IndexFunc(out.Default, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		out.Default = ""
	}
	// 1Panel sometimes types a secret as text, so its name says too.
	secret := f.Type == "password" || secretNamed(f.EnvKey)
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

// version is a version as the converter reads one: 0.8.7-pg13 is the numbers 0 8 7,
// in the variant pg13, and 2.0.0-beta.1 is 2.0.0's pre-release beta 1.
type version struct {
	// nums are its numbers before any word, or all of them if a word comes first,
	// as nvidia-0.33.0 does.
	nums []int
	// variant are the words that name what it's built on or with, each with the
	// number written right after it: pg13, alpine3, postgres, standalone.
	variant []string
	// pre is the word that makes it a pre-release, such as beta, empty for none, and
	// preNums the numbers after it, as 10 of rc.10.
	pre     string
	preNums []int
	// calendar says it's dated, as 20250803 or 2026.10.4 are, which moves on as one
	// line: a new date is no new major.
	calendar bool
}

var (
	versionToken = regexp.MustCompile(`\d+|[A-Za-z]+`)
	// A build's commit, as in 2024.10.22-7ca5933, which says nothing of what it is.
	commitPart = regexp.MustCompile(`^[0-9a-f]*[a-f][0-9a-f]*$`)
	// A build's own number, which moves with each build of one version: linuxserver's
	// ls245, and a package's revision, r1.
	buildPart = regexp.MustCompile(`^(ls|r)\d+$`)
	// A time of day in a version, as MinIO's RELEASE.2025-04-22T22-12-26Z has.
	timePart    = regexp.MustCompile(`[Tt]\d{1,2}[-:]\d{2}([-:]\d{2})?(\.\d+)?Z?`)
	preReleases = []string{"alpha", "beta", "rc", "pre", "preview", "dev", "snapshot", "nightly", "canary", "next"}
	// Words that say nothing of what a version is built on.
	noWords = []string{"release", "version"}
)

func parseVersion(v string) version {
	v = trimV(v)
	// What follows + is a build's, which semantic versions say nothing of.
	v, _, _ = strings.Cut(v, "+")
	v = timePart.ReplaceAllString(v, "")
	var kept []string
	for i, part := range strings.FieldsFunc(v, func(r rune) bool { return r == '-' }) {
		lower := strings.ToLower(part)
		if i > 0 && (len(part) >= 7 && commitPart.MatchString(lower) || buildPart.MatchString(lower)) {
			continue
		}
		kept = append(kept, part)
	}
	v = strings.Join(kept, "-")
	var out version
	var after []int
	word, inPre := false, false
	idx := versionToken.FindAllStringIndex(v, -1)
	for i := 0; i < len(idx); i++ {
		tok := v[idx[i][0]:idx[i][1]]
		n, err := strconv.Atoi(tok)
		if err == nil {
			switch {
			case inPre:
				out.preNums = append(out.preNums, n)
			case word:
				after = append(after, n)
			default:
				out.nums = append(out.nums, n)
			}
			continue
		}
		word = true
		lower := strings.ToLower(tok)
		switch {
		case slices.Contains(preReleases, lower):
			out.pre, inPre = lower, true
			continue
		case slices.Contains(noWords, lower):
			continue
		}
		inPre = false
		// A word written right against a number names that version of something,
		// as pg13 does.
		if i+1 < len(idx) && idx[i+1][0] == idx[i][1] {
			if _, err := strconv.Atoi(v[idx[i+1][0]:idx[i+1][1]]); err == nil {
				lower += v[idx[i+1][0]:idx[i+1][1]]
				i++
			}
		}
		out.variant = append(out.variant, lower)
	}
	if len(out.nums) == 0 {
		out.nums = after
	}
	if len(out.nums) > 0 {
		first := out.nums[0]
		out.calendar = first >= 1990 && first <= 2100 || first >= 19900101 && first <= 21001231
	}
	return out
}

// major is what an entry may move within without the breaking marker: a calendar
// line, or the first number, or the first two where the first is 0, as semantic
// versions read 0.31 as one, with the variant it's built on.
func (v version) major() string {
	var m string
	switch {
	case v.calendar:
		m = "calendar"
	case len(v.nums) == 0:
		m = ""
	case v.nums[0] == 0 && len(v.nums) > 1:
		m = "0." + strconv.Itoa(v.nums[1])
	default:
		m = strconv.Itoa(v.nums[0])
	}
	return strings.Join(append([]string{m}, v.variant...), "-")
}

// trimV takes the v off a version such as v2.0, which counts as 2.0.
func trimV(v string) string {
	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		return v[1:]
	}
	return v
}

// compareVersions orders versions as people read them, by each number in turn, so
// 1.10 comes after 1.9, and rc.10 after rc.2, a pre-release before its release, and
// a plain version after one built on something, as the plain one is what's meant.
func compareVersions(a, b string) int {
	va, vb := parseVersion(a), parseVersion(b)
	if c := compareNums(va.nums, vb.nums); c != 0 {
		return c
	}
	switch {
	case va.pre == "" && vb.pre != "":
		return 1
	case va.pre != "" && vb.pre == "":
		return -1
	}
	if c := cmp.Or(strings.Compare(va.pre, vb.pre), compareNums(va.preNums, vb.preNums)); c != 0 {
		return c
	}
	if c := cmp.Compare(len(vb.variant), len(va.variant)); c != 0 {
		return c
	}
	for i := range va.variant {
		if c := compareWords(va.variant[i], vb.variant[i]); c != 0 {
			return c
		}
	}
	return strings.Compare(a, b)
}

func compareNums(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var na, nb int
		if i < len(a) {
			na = a[i]
		}
		if i < len(b) {
			nb = b[i]
		}
		if c := cmp.Compare(na, nb); c != 0 {
			return c
		}
	}
	return 0
}

// compareWords orders two variants by their letters, then their numbers as numbers,
// so pg9 comes before pg16.
func compareWords(a, b string) int {
	la, na := splitWord(a)
	lb, nb := splitWord(b)
	return cmp.Or(strings.Compare(la, lb), cmp.Compare(na, nb))
}

func splitWord(w string) (string, int) {
	i := strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' })
	if i < 0 {
		return w, -1
	}
	n, _ := strconv.Atoi(w[i:])
	return w[:i], n
}

// majorOf is a version's major, as version.major says.
func majorOf(v string) string {
	return parseVersion(v).major()
}
