// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// TestAppStore measures what Dockwell reads of a checkout of 1Panel's public app
// store, and prints a table: how many of its Compose files read, what they need a
// person to approve, every distinct refusal with a count, and what keys lie behind
// the first refusal each file met, and what a plan warns of once every variable has
// a value. It reads them as a change does, through each spec's Validate, with what
// 1Panel itself would pass an install: each app's form defaults, its container's
// name, and no HOST_IP. It isn't run in CI, and nothing
// of the store goes in this repository: it is GPL-3.0, and ADR 0029 says what that
// means for us.
//
//	git clone --depth 1 https://github.com/1Panel-dev/appstore /tmp/appstore
//	DOCKWELL_APPSTORE=/tmp/appstore go test ./internal/compose -run TestAppStore -v
func TestAppStore(t *testing.T) {
	root := os.Getenv("DOCKWELL_APPSTORE")
	if root == "" {
		t.Skip("set DOCKWELL_APPSTORE to a checkout of https://github.com/1Panel-dev/appstore to measure it")
	}
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "apps"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "docker-compose.yml" {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("found no Compose files under %s: %v", root, err)
	}
	var r storeReport
	for _, file := range files {
		r.read(t, root, file)
	}
	t.Log("\n" + r.String())
}

type storeReport struct {
	files, reads int
	// without is how many read with no directory for what's relative to them, and
	// given how many read once every variable has a value, as a real install's
	// .env gives each.
	without, given int
	final          map[string]int
	apps           map[string]bool
	readApps       map[string]bool
	refusals       map[string]int
	examples       map[string]string
	access         map[string]int
	noticed        map[string]int
	missing        map[string]int
	keys           map[string]int
	alone          map[string]int
	aloneApps      map[string]map[string]bool
	// What a plan warns of, with a value for every variable. An empty form default
	// has none to warn of, where a real install has one, so these give it one.
	filled, warned, mostWarnings, keptDefaults int
	warnings, warnedFiles                      map[string]int
	warnedCredentials                          map[string]int
	warnedVariables, perFile                   map[string]int
	// What might say a field that doesn't warn should, from each file as it stands:
	// a credential, by what its name says or its app's form does, and a variable in
	// a URL.
	quietCredentials, quietURLs map[string]int
}

func (r *storeReport) read(t *testing.T, root, file string) {
	t.Helper()
	dir := filepath.Dir(file)
	rel, _ := filepath.Rel(root, dir)
	app := strings.Split(rel, string(filepath.Separator))[1]
	if r.apps == nil {
		r.apps, r.readApps, r.aloneApps = map[string]bool{}, map[string]bool{}, map[string]map[string]bool{}
		r.refusals, r.examples, r.access, r.noticed = map[string]int{}, map[string]string{}, map[string]int{}, map[string]int{}
		r.missing, r.keys, r.alone, r.final = map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
		r.warnings, r.warnedFiles, r.warnedCredentials = map[string]int{}, map[string]int{}, map[string]int{}
		r.warnedVariables, r.perFile = map[string]int{}, map[string]int{}
		r.quietCredentials, r.quietURLs = map[string]int{}, map[string]int{}
	}
	r.files++
	r.apps[app] = true
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	r.noteKeys(text, app)
	passwords := storePasswords(dir)
	r.noteQuiet(text, passwords)
	variables := storeVariables(t, dir, app)
	directory := "/opt/1panel/apps/" + filepath.ToSlash(rel)
	stack, err := readStore(text, variables, directory)
	if _, without := readStore(text, variables, ""); without == nil {
		r.without++
	}
	// What has no value in the store a real install's .env gives one, so what lies
	// behind it counts too.
	final := err
	for tries := 0; final != nil && tries < 2; tries++ {
		names := missingNames.FindAllString(firstGroup(noValue, Message(final)), -1)
		if len(names) == 0 {
			break
		}
		for _, name := range names {
			if tries == 0 {
				r.missing[name]++
			}
			variables[name] = placeholder(name)
		}
		_, final = readStore(text, variables, directory)
	}
	if final == nil {
		r.given++
	} else {
		r.final[classify(Message(final))]++
	}
	r.noteWarnings(text, variables, passwords, directory)
	if err != nil {
		class := classify(Message(err))
		r.refusals[class]++
		if r.examples[class] == "" {
			r.examples[class] = rel
		}
		return
	}
	r.count(stack)
	r.readApps[app] = true
}

// readStore reads a file as a change does, through each spec's Validate.
func readStore(text []byte, variables map[string]string, directory string) (*Stack, error) {
	stack, err := parse(context.Background(), Input{File: text, Variables: variables, Directory: directory})
	if err != nil {
		return nil, err
	}
	return stack, validateStack(stack)
}

// placeholder is a value of the kind a variable's name says it holds.
func placeholder(name string) string {
	switch {
	case strings.HasSuffix(name, "PORT") || strings.Contains(name, "_PORT_"):
		return "8080"
	case strings.HasSuffix(name, "_DIR") || strings.HasSuffix(name, "_FILE") || strings.HasSuffix(name, "_PATH"):
		return "/srv/app/" + strings.ToLower(name)
	case strings.Contains(name, "IMAGE"):
		return "example.com/app:1"
	}
	return "1"
}

func (r *storeReport) count(stack *Stack) {
	r.reads++
	seen := map[string]bool{}
	d := &service.Driver{}
	for _, s := range stack.Services {
		for _, a := range d.HostAccess(s.Spec) {
			seen[accessClass(a)] = true
		}
	}
	for k := range seen {
		r.access[k]++
	}
	noted := map[string]bool{}
	for _, n := range stack.Notices {
		noted[n.Reason] = true
	}
	for k := range noted {
		r.noticed[k]++
	}
}

// noteWarnings counts what a plan warns of with a value for every variable the file
// names, even one its own default would do for, so it is the most an .env could get.
// HOST_IP stays empty, as 1Panel passes it, and is in a field that doesn't warn. A
// value of the wrong kind for a default can be refused, and then it keeps the default.
func (r *storeReport) noteWarnings(text []byte, variables map[string]string, passwords map[string]bool, directory string) {
	filled := maps.Clone(variables)
	for k, v := range filled {
		if v == "" && k != "HOST_IP" {
			filled[k] = placeholder(k)
		}
	}
	every := maps.Clone(filled)
	for _, m := range storeVariable.FindAllSubmatch(text, -1) {
		if name := string(m[1]); every[name] == "" && name != "HOST_IP" {
			every[name] = placeholder(name)
		}
	}
	stack, err := readStore(text, every, directory)
	if err != nil {
		if stack, err = readStore(text, filled, directory); err != nil {
			return
		}
		r.keptDefaults++
	}
	r.filled++
	if len(stack.Exposed) > 0 {
		r.warned++
		r.perFile[strconv.Itoa(len(stack.Exposed))]++
	}
	r.mostWarnings = max(r.mostWarnings, len(stack.Exposed))
	fields, named := map[string]bool{}, map[string]bool{}
	for _, e := range stack.Exposed {
		field := e.Kind + " " + e.Field
		r.warnings[field]++
		fields[field] = true
		named[field+": "+e.Variable] = true
		if passwords[e.Variable] || credentialName.MatchString(e.Variable) {
			r.warnedCredentials[field]++
		}
	}
	for field := range fields {
		r.warnedFiles[field]++
	}
	for name := range named {
		r.warnedVariables[name]++
	}
}

// noteQuiet looks for a secret in what a plan doesn't warn of, as the file stands,
// so it sees the files that don't read too. A sensitive field is where one belongs.
func (r *storeReport) noteQuiet(text []byte, passwords map[string]bool) {
	var doc map[string]any
	if yaml.Unmarshal(text, &doc) != nil {
		return
	}
	for section, c := range map[string]struct {
		kind  string
		quiet map[string]bool
	}{"services": {"service", quietServiceFields}, "networks": {"network", quietNetworkFields}, "volumes": {"volume", quietVolumeFields}} {
		entries, _ := doc[section].(map[string]any)
		for _, entry := range entries {
			fields, _ := entry.(map[string]any)
			for k, v := range fields {
				eachString(k, v, func(path, s string) {
					field, quiet := quietField(c.quiet, path)
					if !quiet || field == "environment" || c.kind == "volume" && field == "driver_opts" {
						return
					}
					where := c.kind + " " + field
					for _, m := range storeVariable.FindAllStringSubmatch(s, -1) {
						if passwords[m[1]] || credentialName.MatchString(m[1]) {
							r.quietCredentials[where+": "+m[1]]++
						}
					}
					if storeURL.MatchString(s) {
						r.quietURLs[where]++
					}
				})
			}
		}
	}
}

// eachString calls f with each string in v, and where it is, as place writes it.
func eachString(path string, v any, f func(path, s string)) {
	switch v := v.(type) {
	case string:
		f(path, v)
	case map[string]any:
		for k, e := range v {
			eachString(path+"."+k, e, f)
		}
	case []any:
		for i, e := range v {
			eachString(path+"["+strconv.Itoa(i)+"]", e, f)
		}
	}
}

// accessClass names what a person approves, without what it's about.
func accessClass(a resource.Access) string {
	switch a.Kind {
	case resource.AccessPath:
		return "a server path"
	case resource.AccessDocker:
		return "Docker's socket"
	case resource.AccessNetwork:
		return "the server's own network"
	case resource.AccessMount:
		return "a mount from a volume's options"
	case resource.AccessNetworkDriver:
		return "a network past the server's bridge"
	case resource.AccessExternalNetwork:
		return "a network the server has"
	case resource.AccessCapability:
		return "the capability " + a.Path
	case resource.AccessGroup:
		return "a group"
	case resource.AccessPrivileged:
		return "privileged mode"
	case resource.AccessDomain, resource.AccessPanelDomain:
		return "a domain"
	case resource.AccessServerPort, resource.AccessTrustedProxy, resource.AccessPlainForAnyone, resource.AccessHTTPSOff:
		return "a route's reach"
	}
	return fmt.Sprintf("access(%d)", int(a.Kind))
}

// noteKeys counts the keys a service has beyond what Dockwell takes, as a file
// stops at the first, which hides the rest.
func (r *storeReport) noteKeys(text []byte, app string) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if yaml.Unmarshal(text, &doc) != nil {
		return
	}
	beyond := map[string]bool{}
	for _, svc := range doc.Services {
		for k := range svc {
			if !serviceKeys[k] && !strings.HasPrefix(k, "x-") {
				beyond[k] = true
			}
		}
	}
	for k := range beyond {
		r.keys[k]++
		if len(beyond) == 1 {
			r.alone[k]++
			if r.aloneApps[k] == nil {
				r.aloneApps[k] = map[string]bool{}
			}
			r.aloneApps[k][app] = true
		}
	}
}

func validateStack(stack *Stack) error {
	for _, n := range stack.Networks {
		if err := (&network.Driver{}).Validate(proto.Clone(n.Spec)); err != nil {
			return invalid("%v", err)
		}
	}
	for _, v := range stack.Volumes {
		if err := (&volume.Driver{}).Validate(proto.Clone(v.Spec)); err != nil {
			return invalid("%v", err)
		}
	}
	for _, s := range stack.Services {
		if err := (&service.Driver{}).Validate(s.Spec); err != nil {
			return invalid("%v", err)
		}
	}
	return nil
}

// storePasswords are the variables an app's form has as passwords.
func storePasswords(dir string) map[string]bool {
	out := map[string]bool{}
	text, err := os.ReadFile(filepath.Join(dir, "data.yml"))
	if err != nil {
		return out
	}
	var data struct {
		Additional struct {
			Fields []struct {
				Key  string `yaml:"envKey"`
				Type string `yaml:"type"`
			} `yaml:"formFields"`
		} `yaml:"additionalProperties"`
	}
	if yaml.Unmarshal(text, &data) != nil {
		return out
	}
	for _, f := range data.Additional.Fields {
		if f.Type == "password" {
			out[f.Key] = true
		}
	}
	return out
}

// storeVariables are what 1Panel would pass an install of the app: the defaults
// of its form, its container's name, and no HOST_IP.
func storeVariables(t *testing.T, dir, app string) map[string]string {
	t.Helper()
	out := map[string]string{"CONTAINER_NAME": "1Panel-" + app + "-abcd", "HOST_IP": ""}
	text, err := os.ReadFile(filepath.Join(dir, "data.yml"))
	if err != nil {
		return out
	}
	var data struct {
		Additional struct {
			Fields []struct {
				Key     string `yaml:"envKey"`
				Default any    `yaml:"default"`
			} `yaml:"formFields"`
		} `yaml:"additionalProperties"`
	}
	if yaml.Unmarshal(text, &data) != nil {
		return out
	}
	for _, f := range data.Additional.Fields {
		if f.Key != "" {
			out[f.Key] = strings.TrimSpace(fmt.Sprint(cmp.Or[any](f.Default, "")))
		}
	}
	return out
}

var (
	storeVariable  = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
	credentialName = regexp.MustCompile(`(?i)password|passwd|pwd|secret|token|credential|api_?key|access_?key|private_?key|(^|_)pass(_|$)|(^|_)auth(_|$)`)
	storeURL       = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S*\$`)

	noValue      = regexp.MustCompile(`uses (.*), with no value`)
	missingNames = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	classes      = []struct {
		re   *regexp.Regexp
		with string
	}{
		{noValue, "uses <variables>, with no value"},
		{regexp.MustCompile(`such as \$\{[^}]*\}`), "such as ${NAME:-value}"},
		{regexp.MustCompile(`the (service|volume|network) [^' ]+`), "the $1 <name>"},
		{regexp.MustCompile(`"[^"]*"`), `"<value>"`},
		{regexp.MustCompile(`/srv/[^ ,.]*(/data)?`), "/srv/<name>/data"},
		{regexp.MustCompile(`mounts? [^ ,]+( at [^ ,]+)?`), "mounts <path>"},
		{regexp.MustCompile(`/[^ ,]*/[^ ,]*`), "<path>"},
		{regexp.MustCompile(`\b\d+(\.\d+)?\b`), "<n>"},
	}
)

func firstGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// classify takes what is about one file out of a refusal, so that a table of them
// counts the reasons, not the names.
func classify(message string) string {
	for _, c := range classes {
		message = c.re.ReplaceAllString(message, c.with)
	}
	if len(message) > 200 {
		message = message[:200] + "…"
	}
	return message
}

func (r *storeReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d Compose files read as the store has them, from %d of %d apps. %d read with no directory for what is relative to them. %d read once every variable has a value, as a real install's .env gives each.\n\n",
		r.reads, r.files, len(r.readApps), len(r.apps), r.without, r.given)
	table := func(title string, m map[string]int) {
		fmt.Fprintf(&b, "| %s | Files |\n| --- | ---: |\n", title)
		for _, k := range byCount(m) {
			fmt.Fprintf(&b, "| %s | %d |\n", strings.ReplaceAll(k, "|", `\|`), m[k])
		}
		b.WriteString("\n")
	}
	table(fmt.Sprintf("Refused as the store has them, %d files", r.files-r.reads), r.refusals)
	table(fmt.Sprintf("Refused once every variable has a value, %d files", r.files-r.given), r.final)
	table("What the files that read need a person to approve", r.access)
	table("What the plan says of the files that read", r.noticed)
	table("Variables with no value in the store", r.missing)
	fmt.Fprintf(&b, "With a value for every variable a file names, %d files read, %d of them with their own defaults, as a value was refused. A plan warns on %d of them, with %d warnings at most in one file.\n\n", r.filled, r.keptDefaults, r.warned, r.mostWarnings)
	fmt.Fprintf(&b, "| What a plan warns of | Warnings | Files | Of a credential, by its name or its app's form |\n| --- | ---: | ---: | ---: |\n")
	for _, k := range byCount(r.warnings) {
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", k, r.warnings[k], r.warnedFiles[k], r.warnedCredentials[k])
	}
	b.WriteString("\n")
	table("Warnings a file gets", r.perFile)
	table("What each warning names", r.warnedVariables)
	table("A credential, by its name or its app's form, in a field that doesn't warn", r.quietCredentials)
	table("A variable in a URL, in a field that doesn't warn", r.quietURLs)
	fmt.Fprintf(&b, "| Keys of a service Dockwell doesn't take | Files | Alone: the only one in the file | Apps, alone |\n| --- | ---: | ---: | ---: |\n")
	for _, k := range byCount(r.keys) {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d |\n", k, r.keys[k], r.alone[k], len(r.aloneApps[k]))
	}
	return b.String()
}

func byCount(m map[string]int) []string {
	return slices.SortedFunc(maps.Keys(m), func(a, b string) int {
		return cmp.Or(cmp.Compare(m[b], m[a]), strings.Compare(a, b))
	})
}
