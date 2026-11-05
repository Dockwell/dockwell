// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
// the first refusal each file met. It reads them as a change does, through each
// spec's Validate, with what 1Panel itself would pass an install: each app's form
// defaults, its container's name, and no HOST_IP. It isn't run in CI, and nothing
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
	}
	r.files++
	r.apps[app] = true
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	r.noteKeys(text, app)
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
	stack, err := parse(Input{File: text, Variables: variables, Directory: directory})
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
