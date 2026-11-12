// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A companion is a service an app runs without, such as Adminer beside PostgreSQL,
// which a person installs only if they want it. An install's choice of them, by
// service, says which run. See docs/adr/0076-optional-companions-in-a-catalog-entry.md.

// maxPurpose is how long what a companion is for may be: a few words, on one line.
const maxPurpose = 60

// Chosen is which companions an install runs: what's given, and its entry's default
// for each one that isn't. It refuses a companion the entry hasn't.
func Chosen(e *catalogv1.Entry, given map[string]bool) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range e.GetCompanions() {
		out[c.GetService()] = c.GetOn()
	}
	for _, s := range slices.Sorted(maps.Keys(given)) {
		if _, ok := out[s]; !ok {
			return nil, fmt.Errorf("%s has no companion %s. %s", e.GetName(), s, CompanionList(e))
		}
		out[s] = given[s]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Resolve is which of an entry's companions an install runs, from its pin: what the
// pin says, and for one it says nothing of, whether the file it ran had it, as an
// install from before companions ran every service, or else the entry's default, as
// for one a newer release adds.
func Resolve(e *catalogv1.Entry, pin *resourcev1.CatalogPin) map[string]bool {
	if len(e.GetCompanions()) == 0 {
		return nil
	}
	ran, _ := composeServices(string(pin.GetFile()))
	out := map[string]bool{}
	for _, c := range e.GetCompanions() {
		s := c.GetService()
		if on, said := pin.GetCompanions()[s]; said {
			out[s] = on
			continue
		}
		_, had := ran[s]
		out[s] = had || c.GetOn()
	}
	return out
}

// Off lists the companions a choice leaves out, sorted.
func Off(choice map[string]bool) []string {
	var out []string
	for s, on := range choice {
		if !on {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// Choose is the entry as an install with a choice of companions runs it: its Compose
// file without those left out, and without the inputs only they use. It changes
// nothing of the entry it's given, which is the catalog's.
func Choose(e *catalogv1.Entry, choice map[string]bool) (*catalogv1.Entry, error) {
	off := Off(choice)
	if len(off) == 0 {
		return e, nil
	}
	file, err := Without(e.GetCompose(), off)
	if err != nil {
		return nil, fmt.Errorf("the catalog entry %s: %w", e.GetId(), err)
	}
	dropped := map[string]bool{}
	for _, c := range e.GetCompanions() {
		if slices.Contains(off, c.GetService()) {
			for _, in := range c.GetInputs() {
				dropped[in] = true
			}
		}
	}
	out := proto.CloneOf(e)
	out.Compose = file
	out.Inputs = slices.DeleteFunc(out.Inputs, func(in *catalogv1.Input) bool { return dropped[in.GetName()] })
	return out, nil
}

// LeftOut refuses a value given for an input that only a companion the choice leaves
// out uses, as nothing would use it. It never quotes the value.
func LeftOut(e *catalogv1.Entry, choice map[string]bool, given map[string]string) error {
	for _, c := range e.GetCompanions() {
		if choice[c.GetService()] {
			continue
		}
		for _, in := range c.GetInputs() {
			if _, ok := given[in]; ok {
				return fmt.Errorf("%s is %s's, which this install leaves out. Turn %s on, or leave %s out", in, c.GetLabel(), c.GetService(), in)
			}
		}
	}
	return nil
}

// Companion finds one of an entry's companions by its service.
func Companion(e *catalogv1.Entry, service string) (*catalogv1.Companion, bool) {
	i := slices.IndexFunc(e.GetCompanions(), func(c *catalogv1.Companion) bool { return c.GetService() == service })
	if i < 0 {
		return nil, false
	}
	return e.GetCompanions()[i], true
}

// Runs reports whether a Compose file has a service.
func Runs(file, service string) bool {
	services, err := composeServices(file)
	if err != nil {
		return false
	}
	_, ok := services[service]
	return ok
}

// CompanionValues fills a companion's inputs for turning it on: what's given, or
// what's suggested, each checked as an install checks it. It refuses a value for an
// input that isn't the companion's.
func CompanionValues(e *catalogv1.Entry, c *catalogv1.Companion, given, suggested map[string]string) (map[string]string, error) {
	for _, name := range slices.Sorted(maps.Keys(given)) {
		if !slices.Contains(c.GetInputs(), name) {
			return nil, fmt.Errorf("%s takes no input %s. It takes %s", c.GetLabel(), name, strings.Join(c.GetInputs(), ", "))
		}
	}
	out := map[string]string{}
	for _, in := range e.GetInputs() {
		if !slices.Contains(c.GetInputs(), in.GetName()) {
			continue
		}
		value := strings.TrimSpace(given[in.GetName()])
		if value == "" {
			value = suggested[in.GetName()]
		}
		if err := checkValue(in, value, false); err != nil {
			return nil, err
		}
		out[in.GetName()] = value
	}
	return out, nil
}

// CompanionList says which companions an entry has, as a refusal names them.
func CompanionList(e *catalogv1.Entry) string {
	var names []string
	for _, c := range e.GetCompanions() {
		names = append(names, c.GetService())
	}
	if len(names) == 0 {
		return "It has none"
	}
	return "It has " + strings.Join(names, ", ")
}

// Without is a Compose file without the services named, and without the volumes only
// they used. It edits the file's YAML as written, so the rest means what it did.
func Without(file string, services []string) (string, error) {
	if len(services) == 0 {
		return file, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		return "", fmt.Errorf("its Compose file isn't YAML: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("its Compose file isn't a mapping")
	}
	root := doc.Content[0]
	all := nodeGet(root, "services")
	if all == nil || all.Kind != yaml.MappingNode {
		return "", errors.New("its Compose file has no services")
	}
	used, dropped := map[string]bool{}, map[string]bool{}
	var kept []*yaml.Node
	for i := 0; i+1 < len(all.Content); i += 2 {
		name, svc := all.Content[i], all.Content[i+1]
		into := used
		if slices.Contains(services, name.Value) {
			into = dropped
		} else {
			kept = append(kept, name, svc)
		}
		for _, v := range namedVolumes(svc) {
			into[v] = true
		}
	}
	all.Content = kept
	if volumes := nodeGet(root, "volumes"); volumes != nil && volumes.Kind == yaml.MappingNode {
		var keep []*yaml.Node
		for i := 0; i+1 < len(volumes.Content); i += 2 {
			if name := volumes.Content[i].Value; !dropped[name] || used[name] {
				keep = append(keep, volumes.Content[i], volumes.Content[i+1])
			}
		}
		volumes.Content = keep
		for i := 0; len(keep) == 0 && i+1 < len(root.Content); i += 2 {
			if root.Content[i+1] == volumes {
				root.Content = slices.Delete(root.Content, i, i+2)
				break
			}
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", fmt.Errorf("writing its Compose file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("writing its Compose file: %w", err)
	}
	return b.String(), nil
}

// nodeGet is a mapping's value for a key.
func nodeGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// namedVolumes are the named volumes a service mounts, as "data:/var/lib/data" or a
// mount of type volume names one. A path is a bind, not a volume.
func namedVolumes(svc *yaml.Node) []string {
	list := nodeGet(svc, "volumes")
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, item := range list.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			source, _, _ := strings.Cut(item.Value, ":")
			if source != "" && !strings.ContainsAny(source[:1], "/.~$") {
				out = append(out, source)
			}
		case yaml.MappingNode:
			kind := nodeGet(item, "type")
			if source := nodeGet(item, "source"); source != nil && (kind == nil || kind.Value == "volume") {
				out = append(out, source.Value)
			}
		case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode, yaml.StreamNode:
		}
	}
	return out
}

// usesInput reports whether a file uses an input, as ${NAME} or with a default.
func usesInput(file, name string) bool {
	return regexp.MustCompile(`\$\{` + regexp.QuoteMeta(name) + `(\}|:?[-?+])`).MatchString(file)
}

// checkCompanions refuses a companion an app couldn't run without, or that names what
// the entry hasn't: a service it doesn't run, one of its databases, one another
// service depends on, a secret, which turning it on later couldn't generate, and an
// input it doesn't use alone, or one it uses alone that it doesn't name.
func checkCompanions(e *catalogv1.Entry) error {
	if len(e.GetCompanions()) == 0 {
		return nil
	}
	services, err := composeServices(e.GetCompose())
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(slices.Collect(maps.Keys(services)), func(s string) bool {
		_, companion := Companion(e, s)
		return !companion
	}) {
		return errors.New("every service it runs is a companion, so it could install nothing. Take one off its companions")
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range e.GetInputs() {
		inputs[in.GetName()] = in
	}
	seen := map[string]bool{}
	for _, c := range e.GetCompanions() {
		s := c.GetService()
		_, runs := services[s]
		switch {
		case !runs:
			return fmt.Errorf("it marks %s as a companion, which its Compose file doesn't run. Name one of its services", s)
		case seen[s]:
			return fmt.Errorf("it marks %s as a companion twice. Keep one", s)
		case slices.Contains(e.GetDatabases(), s):
			return fmt.Errorf("%s is one of its databases, which the app keeps its data in, so it can't be left out. Take it off its companions", s)
		case strings.TrimSpace(c.GetLabel()) == "" || strings.TrimSpace(c.GetPurpose()) == "":
			return fmt.Errorf("its companion %s needs a label, and what it's for", s)
		case strings.ContainsAny(c.GetPurpose(), "\r\n") || len(c.GetPurpose()) > maxPurpose:
			return fmt.Errorf("what its companion %s is for isn't one line of up to %d characters. Say it in fewer words", s, maxPurpose)
		}
		seen[s] = true
		for _, other := range slices.Sorted(maps.Keys(services)) {
			if other != s && dependsOn(services[other], s) {
				return fmt.Errorf("%s depends on %s, so %s can't be left out. Take that depends_on away, or %s off its companions", other, s, s, s)
			}
		}
		rest, err := Without(e.GetCompose(), []string{s})
		if err != nil {
			return err
		}
		for _, name := range c.GetInputs() {
			switch in := inputs[name]; {
			case in == nil:
				return fmt.Errorf("its companion %s names the input %s, which it hasn't", s, name)
			case Secret(in):
				return fmt.Errorf("its companion %s takes the secret %s, which turning it on later couldn't generate. Take %s off its inputs", s, name, name)
			case usesInput(rest, name):
				return fmt.Errorf("the input %s is used beyond its companion %s. Take it off the companion's inputs", name, s)
			}
		}
		for _, in := range e.GetInputs() {
			if name := in.GetName(); !slices.Contains(c.GetInputs(), name) && !usesInput(rest, name) {
				return fmt.Errorf("only its companion %s uses the input %s. Name %s among the companion's inputs", s, name, name)
			}
		}
		for _, n := range c.GetNotes() {
			if strings.TrimSpace(n) == "" || strings.ContainsAny(n, "\r\n") || len(n) > maxNote {
				return fmt.Errorf("its companion %s has a note %q that isn't one line of up to %d characters. Split it, or say it in fewer words", s, n, maxNote)
			}
		}
	}
	return nil
}

// dependsOn reports whether a service's depends_on names another, in either of its
// forms.
func dependsOn(svc any, name string) bool {
	m, _ := svc.(map[string]any)
	switch d := m["depends_on"].(type) {
	case []any:
		return slices.Contains(d, any(name))
	case map[string]any:
		_, ok := d[name]
		return ok
	}
	return false
}
