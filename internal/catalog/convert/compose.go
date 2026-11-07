// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// rewrite turns a source's Compose file into what an entry may say, and its form
// into the entry's inputs:
//
//   - no container_name, as Dockwell names each container from its project;
//   - no network the source's platform makes, which an entry can't reach, so each
//     service joins its project's own;
//   - every port on loopback, as every entry publishes until it has a domain;
//   - a folder the app makes as it runs, bound relative to the file, as a named
//     volume, which Dockwell backs up before an upgrade;
//   - the source's variables under plainer names, each one a field fills.
//
// A file it can't make into one is an error that says why, which leaves the app out.
func rewrite(app App) ([]byte, []*catalogv1.Input, []string, error) {
	doc, err := parseFile(app.Compose)
	if err != nil {
		return nil, nil, nil, err
	}
	root := doc.Content[0]
	var notes []string
	externals := map[string]bool{}
	if nets := mapGet(root, "networks"); nets != nil {
		for i := 0; i+1 < len(nets.Content); i += 2 {
			if ext := mapGet(nets.Content[i+1], "external"); ext != nil && ext.Value == "true" {
				externals[nets.Content[i].Value] = true
			}
		}
		for name := range externals {
			mapDel(nets, name)
		}
		if len(nets.Content) == 0 {
			mapDel(root, "networks")
		}
	}
	mapDel(root, "version")
	volumes := mapGet(root, "volumes")
	// What a container was named from the platform's variables, by the service it
	// names, as another service or the app itself reaches it by that name, which
	// Docker answers to by the service's own on the project's network.
	named := map[string]string{}
	names := serviceNames(doc)
	if len(names) == 0 {
		return nil, nil, nil, errors.New("its Compose file runs no service")
	}
	for _, name := range names {
		svc := service(doc, name)
		for _, key := range []string{"build", "env_file", "extends"} {
			if mapGet(svc, key) != nil {
				return nil, nil, nil, fmt.Errorf("its service %s has %s, which an entry can't carry", name, key)
			}
		}
		if n := mapGet(svc, "container_name"); n != nil && strings.Contains(n.Value, "$") {
			named[n.Value] = name
		}
		mapDel(svc, "container_name")
		if labels := mapGet(svc, "labels"); labels != nil {
			for _, platform := range []string{"createdBy", "skipStatusCheck"} {
				mapDel(labels, platform)
				labels.Content = slices.DeleteFunc(labels.Content, func(n *yaml.Node) bool {
					return labels.Kind == yaml.SequenceNode && strings.HasPrefix(n.Value, platform+"=")
				})
			}
			if len(labels.Content) == 0 {
				mapDel(svc, "labels")
			}
		}
		if nets := mapGet(svc, "networks"); nets != nil {
			switch nets.Kind {
			case yaml.SequenceNode:
				nets.Content = slices.DeleteFunc(nets.Content, func(n *yaml.Node) bool { return externals[n.Value] })
			case yaml.MappingNode:
				for ext := range externals {
					mapDel(nets, ext)
				}
			default:
			}
			if len(nets.Content) == 0 {
				mapDel(svc, "networks")
			}
		}
		if ports := mapGet(svc, "ports"); ports != nil {
			for _, p := range ports.Content {
				if err := loopback(p); err != nil {
					return nil, nil, nil, fmt.Errorf("its service %s's port %s: %w", name, nodeText(p), err)
				}
			}
		}
		if vols := mapGet(svc, "volumes"); vols != nil {
			kept := vols.Content[:0]
			for _, v := range vols.Content {
				named, drop, err := volume(app, v)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("its service %s's volume %s: %w", name, nodeText(v), err)
				}
				if drop != "" {
					notes = append(notes, drop)
					continue
				}
				if named != "" {
					if volumes == nil {
						volumes = &yaml.Node{Kind: yaml.MappingNode}
						root.Content = append(root.Content, scalar("volumes"), volumes)
					}
					if mapGet(volumes, named) == nil {
						volumes.Content = append(volumes.Content, scalar(named), &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle})
					}
				}
				kept = append(kept, v)
			}
			vols.Content = kept
			if len(vols.Content) == 0 {
				mapDel(svc, "volumes")
			}
		}
	}
	byLength := slices.SortedFunc(maps.Keys(named), func(a, b string) int { return len(b) - len(a) })
	walkScalars(root, func(n *yaml.Node) {
		for _, text := range byLength {
			n.Value = strings.ReplaceAll(n.Value, text, named[text])
		}
	})
	if err := dashServices(doc); err != nil {
		return nil, nil, nil, err
	}
	renames := renamesOf(app.Fields)
	walkScalars(root, func(n *yaml.Node) { n.Value = renameVars(n.Value, renames) })
	inputs := inputsOf(app.Fields, renames, root)
	asked := map[string]bool{}
	for _, in := range inputs {
		asked[in.GetName()] = true
	}
	var missing []string
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			if !asked[u.name] && u.op != ":-" && u.op != "-" && !slices.Contains(missing, u.name) {
				missing = append(missing, u.name)
			}
		}
	})
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, nil, nil, fmt.Errorf("its Compose file uses %s, which its platform fills in and no field asks for", strings.Join(missing, ", "))
	}
	out, err := encodeFile(doc)
	return out, inputs, notes, err
}

// dashServices renames a service whose name has an underscore, which a container's
// name can't take, and what reaches it by that name: its depends_on, and a value
// that names it whole, such as http://act_runner:8080, but never an image.
func dashServices(doc *yaml.Node) error {
	services := mapGet(doc.Content[0], "services")
	renamed := map[string]string{}
	for i := 0; i+1 < len(services.Content); i += 2 {
		key := services.Content[i]
		if to := strings.ReplaceAll(key.Value, "_", "-"); to != key.Value {
			if mapGet(services, to) != nil {
				return fmt.Errorf("its services %s and %s would have one name", key.Value, to)
			}
			renamed[key.Value], key.Value = to, to
		}
	}
	if len(renamed) == 0 {
		return nil
	}
	for from, to := range renamed {
		whole := regexp.MustCompile(`\b` + regexp.QuoteMeta(from) + `\b`)
		for _, name := range serviceNames(doc) {
			if deps := mapGet(service(doc, name), "depends_on"); deps != nil {
				for _, d := range deps.Content {
					if d.Value == from {
						d.Value = to
					}
				}
			}
		}
		images := map[*yaml.Node]bool{}
		for _, name := range serviceNames(doc) {
			if img := mapGet(service(doc, name), "image"); img != nil {
				images[img] = true
			}
		}
		walkScalars(doc.Content[0], func(n *yaml.Node) {
			// An image is named by its publisher, as gitea/act_runner is.
			if !images[n] {
				n.Value = whole.ReplaceAllString(n.Value, to)
			}
		})
	}
	return nil
}

// loopback publishes a port on 127.0.0.1, whatever address the source gave it.
func loopback(p *yaml.Node) error {
	switch p.Kind {
	case yaml.ScalarNode:
		parts := splitPort(p.Value)
		switch len(parts) {
		case 1:
			// A container port alone, which Docker publishes on a port it picks.
			p.Value = "127.0.0.1::" + parts[0]
		case 2:
			p.Value = "127.0.0.1:" + parts[0] + ":" + parts[1]
		case 3:
			p.Value = "127.0.0.1:" + parts[1] + ":" + parts[2]
		default:
			return errNoPort
		}
		p.Style = yaml.DoubleQuotedStyle
	case yaml.MappingNode:
		if ip := mapGet(p, "host_ip"); ip != nil {
			ip.Value = "127.0.0.1"
		} else {
			p.Content = append(p.Content, scalar("host_ip"), scalar("127.0.0.1"))
		}
	default:
		return errNoPort
	}
	return nil
}

var errNoPort = errors.New("reads as no port Docker would publish")

// splitPort splits a short port at its colons, outside any ${...}.
func splitPort(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "${"):
			depth++
			i++
		case s[i] == '}' && depth > 0:
			depth--
		case s[i] == ':' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// volume reads one of a service's volumes. A folder the app makes, bound relative to
// the file, becomes a named volume, which it returns. A file the source ships can't
// be said by an entry yet. A read-only mount of the server's time zone goes, as the
// container's own is UTC and the mount is host access a person would have to approve
// for nothing more.
func volume(app App, v *yaml.Node) (named, dropped string, err error) {
	source, set := volumeSource(v)
	if source == "" {
		return "", "", nil
	}
	switch {
	case source == "/etc/localtime" || source == "/etc/timezone":
		if readOnly(v) {
			return "", "the server's " + source + ", which only set the container's time zone", nil
		}
		return "", "", nil
	case strings.HasPrefix(source, "~"):
		return "", "", errors.New("it binds a folder in a home directory, which an entry can't know")
	case !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") && source != ".":
		return "", "", nil
	}
	rel := path.Clean(source)
	if strings.HasPrefix(rel, "..") {
		return "", "", fmt.Errorf("it binds %s, outside the app's own folder", source)
	}
	if app.Shipped != nil && app.Shipped(rel) {
		return "", "", fmt.Errorf("it ships %s beside its Compose file, which an entry can't declare yet", rel)
	}
	named = volumeName(rel)
	set(named)
	return named, "", nil
}

// volumeSource finds what a volume mounts, and how to change it.
func volumeSource(v *yaml.Node) (string, func(string)) {
	switch v.Kind {
	case yaml.ScalarNode:
		parts := strings.SplitN(v.Value, ":", 2)
		if len(parts) < 2 {
			return "", nil
		}
		return parts[0], func(named string) { v.Value = named + ":" + parts[1] }
	case yaml.MappingNode:
		src := mapGet(v, "source")
		if src == nil {
			return "", nil
		}
		return src.Value, func(named string) {
			src.Value = named
			if t := mapGet(v, "type"); t != nil {
				t.Value = "volume"
			}
		}
	default:
	}
	return "", nil
}

func readOnly(v *yaml.Node) bool {
	if v.Kind == yaml.MappingNode {
		ro := mapGet(v, "read_only")
		return ro != nil && ro.Value == "true"
	}
	parts := strings.Split(v.Value, ":")
	return len(parts) == 3 && slices.Contains(strings.Split(parts[2], ","), "ro")
}

// volumeName names a volume for the folder it stands for: ./data/db becomes data-db.
func volumeName(rel string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(rel) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.' && b.Len() > 0:
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// usedVar finds where a Compose value uses a variable: its name, and the operator
// after it, such as :- for a default. It reads no further, so a default that uses
// another variable is found too.
var usedVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?+])?`)

// varUse is one use of a variable in a value.
type varUse struct {
	// at and end bound its name in the value.
	at, end  int
	name, op string
}

// usesOf finds each variable a value uses. $$ reaches the container as $, so what
// follows it is no variable.
func usesOf(s string) []varUse {
	var out []varUse
	for _, m := range usedVar.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > 0 && s[m[0]-1] == '$' {
			continue
		}
		u := varUse{at: m[2], end: m[3], name: s[m[2]:m[3]]}
		if m[4] >= 0 {
			u.op = s[m[4]:m[5]]
		}
		out = append(out, u)
	}
	return out
}

// renameVars renames the variables a value uses.
func renameVars(s string, renames map[string]string) string {
	if len(renames) == 0 || !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	last := 0
	for _, u := range usesOf(s) {
		if to, ok := renames[u.name]; ok {
			b.WriteString(s[last:u.at])
			b.WriteString(to)
			last = u.end
		}
	}
	b.WriteString(s[last:])
	return b.String()
}

// renamesOf gives each of a platform's variables a plainer name: its web port PORT,
// another port by what it's for, such as SSH_PORT, and anything else without the
// platform's prefix. A name another field has already stays.
func renamesOf(fields []Field) map[string]string {
	have := map[string]bool{}
	for _, f := range fields {
		have[f.Name] = true
	}
	out := map[string]string{}
	for _, f := range fields {
		to := plainName(f)
		if to != f.Name && !have[to] && !slices.Contains(valuesOf(out), to) {
			out[f.Name] = to
		}
	}
	return out
}

func valuesOf(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func plainName(f Field) string {
	name := strings.ToUpper(f.Name)
	for _, prefix := range []string{"PANEL_APP_PORT_", "PANEL_"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" {
			if prefix == "PANEL_APP_PORT_" {
				if f.Web || rest == "HTTP" {
					return "PORT"
				}
				return rest + "_PORT"
			}
			return rest
		}
	}
	return name
}

// inputsOf makes the entry's inputs from the fields its file still uses.
func inputsOf(fields []Field, renames map[string]string, root *yaml.Node) []*catalogv1.Input {
	used := map[string]bool{}
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			used[u.name] = true
		}
	})
	var out []*catalogv1.Input
	for _, f := range fields {
		name := f.Name
		if to, ok := renames[name]; ok {
			name = to
		}
		if !used[name] {
			continue
		}
		in := &catalogv1.Input{Name: name, Label: sentenceCase(f.Label), DefaultValue: f.Default, Optional: f.Optional}
		switch f.Kind {
		case Port:
			in.Type = catalogv1.InputType_INPUT_TYPE_PORT
			in.Label = portLabel(in.GetLabel(), name == "PORT")
			in.Hint = "Only this server reaches it."
			if name == "PORT" {
				in.Hint += " To reach it from elsewhere, add a domain once it runs."
			}
		case Secret:
			in.Type, in.DefaultValue = catalogv1.InputType_INPUT_TYPE_SECRET, ""
		case Generated:
			in.Type, in.DefaultValue, in.Generate, in.Optional = catalogv1.InputType_INPUT_TYPE_SECRET, "", 32, false
		default:
			in.Type = catalogv1.InputType_INPUT_TYPE_TEXT
			if len(f.Choices) > 0 {
				in.Hint = "One of " + strings.Join(f.Choices, ", ") + "."
			}
		}
		if in.GetLabel() == "" {
			in.Label = name
		}
		out = append(out, in)
	}
	return out
}

// portLabel says a port is on this server, as every entry's form does.
func portLabel(label string, web bool) string {
	if web || label == "" || strings.EqualFold(label, "port") {
		return "Port on this server"
	}
	label = strings.TrimSuffix(strings.TrimSuffix(label, " port"), " Port")
	return label + " port on this server"
}

// sentenceCase writes a title-cased label as a sentence is, leaving an acronym or
// a name in mixed case as it is: "Web Port" becomes "Web port", "MinIO Root User"
// "MinIO root user".
func sentenceCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if i == 0 {
			continue
		}
		runes := []rune(w)
		if len(runes) > 1 && unicode.IsUpper(runes[0]) && strings.ToLower(string(runes[1:])) == string(runes[1:]) {
			words[i] = strings.ToLower(w)
		}
	}
	return strings.Join(words, " ")
}

// parseFile reads a Compose file as YAML nodes, which keep its order.
func parseFile(file []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(file, &doc); err != nil {
		return nil, fmt.Errorf("its Compose file isn't YAML: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("its Compose file isn't a mapping")
	}
	return &doc, nil
}

func encodeFile(doc *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func serviceNames(doc *yaml.Node) []string {
	services := mapGet(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(services.Content); i += 2 {
		out = append(out, services.Content[i].Value)
	}
	return out
}

func service(doc *yaml.Node, name string) *yaml.Node {
	return mapGet(mapGet(doc.Content[0], "services"), name)
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
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

func mapDel(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

// walkScalars calls f on every scalar value, and not on a mapping's keys.
func walkScalars(n *yaml.Node, f func(*yaml.Node)) {
	switch n.Kind {
	case yaml.ScalarNode:
		f(n)
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			walkScalars(n.Content[i], f)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			walkScalars(c, f)
		}
	default:
	}
}

// nodeText is a node as YAML on one line, for a message.
func nodeText(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return "?"
	}
	return strings.Join(strings.Fields(string(out)), " ")
}
