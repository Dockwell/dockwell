// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
)

// rewritten is a source's Compose file as an entry may say it, with its inputs.
type rewritten struct {
	file   []byte
	inputs []*catalogv1.Input
	// notes are what the converter changed beyond the rules every entry follows.
	notes []string
	// reaches is what the file reaches through a value a person types, such as a
	// folder of theirs, which no reading of the file with stand-ins can see.
	reaches []string
	// web is the input that names the port a person opens, empty for none.
	web string
	// typed are the volumes and networks whose driver or options a person types, as
	// "volume data", which a reading of the file with stand-ins can't speak for.
	typed map[string]bool
	// unread are the shipped files the app mounts that weren't fetched, by their
	// paths relative to its Compose file.
	unread []string
}

// rewrite turns a source's Compose file into what an entry may say, and its form
// into the entry's inputs:
//
//   - no container_name, as Dockwell names each container from its project, and a
//     name another service reached it by becomes the service's own;
//   - no network the source's platform makes, which an entry can't reach, so each
//     service joins its project's own;
//   - every port on loopback, as every entry publishes until it has a domain;
//   - a folder the app makes as it runs, bound relative to the file, as a named
//     volume of its own, which Dockwell backs up before an upgrade;
//   - the source's variables under plainer names, each one a field fills.
//
// A file it can't make into one is an error that says why, which leaves the app out.
func rewrite(app App) (rewritten, error) {
	var out rewritten
	doc, err := parseFile(app.Compose)
	if err != nil {
		return out, err
	}
	if err := expandAliases(doc); err != nil {
		return out, err
	}
	// A source's comments are its own, and what follows moves the lines they sat on.
	dropComments(doc)
	root := doc.Content[0]
	// What a file a volume writes out holds is the app's own: a bare $, as nginx's
	// $host, is its own, and only ${NAME} is a variable.
	for _, name := range serviceNames(doc) {
		vols := mapGet(service(doc, name), "volumes")
		for i := 0; vols != nil && i < len(vols.Content); i++ {
			if c := mapGet(vols.Content[i], "content"); c != nil {
				c.Value = ownDollars(c.Value)
			}
		}
	}
	// Compose reads $NAME as ${NAME}, and the rest of this reads the second.
	walkScalars(root, func(n *yaml.Node) { n.Value = braceVars(n.Value) })
	// An image whose tag, or name, a variable gives with a default is the default: a
	// new version is a new release of the entry, and an install pins the build either
	// way (ADR 0085).
	for _, name := range serviceNames(doc) {
		if img := mapGet(service(doc, name), "image"); img != nil {
			img.Value = imageDefaults.ReplaceAllString(img.Value, "$1")
		}
	}
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
	// What a container was named, by the service it names, as another service or the
	// app itself reaches it by that name, which Docker answers to by the service's own
	// on the project's network.
	named := map[string]string{}
	names := serviceNames(doc)
	if len(names) == 0 {
		return out, leave(ClassFile, "its Compose file runs no service")
	}
	// The folders bound as volumes, by the volume each became.
	folders := map[string]string{}
	for _, name := range names {
		svc := service(doc, name)
		for _, key := range []string{"build", "env_file", "extends"} {
			if mapGet(svc, key) != nil {
				return out, leave(ClassCantSay, "its service %s has %s, which an entry can't carry", name, key)
			}
		}
		// Memory locked with no limit can't be reclaimed from the server, so a store's
		// app doesn't get it (ADR 0085).
		if unlimitedMemlock(mapGet(mapGet(svc, "ulimits"), "memlock")) {
			return out, leave(ClassCantRun, "its service %s locks memory with no limit (ulimits memlock -1), which can't be reclaimed from the server, so a store's app doesn't get it", name)
		}
		if n := mapGet(svc, "container_name"); n != nil {
			named[n.Value] = name
		}
		mapDel(svc, "container_name")
		// Coolify's own, which says what its health page leaves out.
		mapDel(svc, "exclude_from_hc")
		if labels := mapGet(svc, "labels"); labels != nil {
			dropLabels(labels)
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
					return out, leave(ClassCantSay, "its service %s's port %s %s", name, nodeText(p), err)
				}
			}
		}
		if vols := mapGet(svc, "volumes"); vols != nil {
			kept := vols.Content[:0]
			for _, v := range vols.Content {
				vol, err := volume(app, v, folders)
				if err != nil {
					return out, fmt.Errorf("its service %s's volume %s: %w", name, nodeText(v), err)
				}
				switch {
				case len(vol.unread) > 0:
					out.unread = append(out.unread, vol.unread...)
					continue
				case len(vol.files) > 0:
					addConfigs(root, svc, name, vol.files)
					continue
				case vol.dropped != "":
					out.notes = append(out.notes, vol.dropped)
					continue
				case vol.reaches != "":
					out.reaches = append(out.reaches, name+": "+vol.reaches)
				case vol.named != "":
					if vol.owner != "" {
						addOwner(svc, vol.target, vol.owner)
					}
					if vol.mode != 0 {
						addExtension(svc, "make_mode", vol.target, fmt.Sprintf("0%o", vol.mode))
					}
					if len(vol.seeds) > 0 {
						addSeeds(root, svc, name, vol.target, vol.seeds)
					}
					if volumes == nil {
						volumes = &yaml.Node{Kind: yaml.MappingNode}
						root.Content = append(root.Content, scalar("volumes"), volumes)
					}
					if mapGet(volumes, vol.named) == nil {
						volumes.Content = append(volumes.Content, scalar(vol.named), &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle})
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
	if len(out.unread) > 0 {
		slices.Sort(out.unread)
		return out, unreadError(slices.Compact(out.unread))
	}
	if err := nested(folders); err != nil {
		return out, err
	}
	if err := ownedInside(app.scripts, folders); err != nil {
		return out, err
	}
	out.reaches, out.typed = append(out.reaches, typedAccess(root)...), typedResources(root)
	images := imageNodes(doc)
	// Longest first, so ${CONTAINER_NAME}-worker is renamed before ${CONTAINER_NAME}
	// is, and in one order every run.
	byLength := slices.SortedFunc(maps.Keys(named), func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	for _, text := range byLength {
		to := named[text]
		if text == to {
			continue
		}
		if strings.Contains(text, "$") {
			// A name made of the platform's variables is written only where the
			// platform's name for the container is meant.
			walkScalars(root, func(n *yaml.Node) {
				if !images[n] {
					n.Value = strings.ReplaceAll(n.Value, text, to)
				}
			})
			continue
		}
		hosts(root, images, text, to)
	}
	if err := dashServices(doc); err != nil {
		return out, err
	}
	renames := renamesOf(app.Fields)
	walkScalars(root, func(n *yaml.Node) { n.Value = renameVars(n.Value, renames) })
	for i := range out.reaches {
		out.reaches[i] = renameVars(out.reaches[i], renames)
	}
	fields := slices.Clone(app.Fields)
	for i := range fields {
		if to, ok := renames[fields[i].Name]; ok {
			fields[i].Name = to
		}
	}
	made, err := sharedSecrets(root, fields, images)
	if err != nil {
		return out, err
	}
	out.inputs = inputsOf(append(fields, made...), nil, root)
	asked := map[string]bool{}
	for _, in := range out.inputs {
		asked[in.GetName()] = true
		if in.GetType() == catalogv1.InputType_INPUT_TYPE_SECRET {
			// A secret is a whole value, written plainly: the form asks for it as :?
			// asked Compose to, and a default in the file is one everyone would share.
			walkScalars(root, func(n *yaml.Node) { n.Value = replaceVar(n.Value, in.GetName(), "${"+in.GetName()+"}") })
		}
	}
	for _, f := range app.Fields {
		if f.Web && f.Kind == Port {
			if to, ok := renames[f.Name]; ok {
				out.web = to
			} else {
				out.web = f.Name
			}
		}
	}
	if !asked[out.web] {
		out.web = ""
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
		return out, leave(ClassPlatform, "its Compose file uses %s, which its platform fills in and no field asks for", strings.Join(missing, ", "))
	}
	out.file, err = encodeFile(doc)
	return out, err
}

// dropComments takes every comment off a file.
func dropComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		dropComments(c)
	}
}

// sharedSecrets finds each secret the file writes out, where everyone who installs it
// would share it and every plan would show it: a value written under a secret's name,
// as ADMIN_PASSWORD: example, or the default of a variable no field fills, where the
// variable or what it's written under is named as a secret, as DB_PASS:
// ${DB_PASS:-example}. Each becomes a secret Dockwell generates and seals, one for
// each distinct value, so a database and its app that wrote the same one still agree.
// A switch, such as true, or a path is no secret.
//
// A value written inside a longer one too, as a connection URL holds a password,
// couldn't follow a generated secret, as the catalog keeps a secret a value of its
// own, so the app is left out, saying so. Where a name is written, as an image, a
// volume, a host, a path or a user is, the same word is a name, not the secret.
func sharedSecrets(root *yaml.Node, fields []Field, images map[*yaml.Node]bool) ([]Field, error) {
	filled := map[string]bool{}
	for _, f := range fields {
		filled[f.Name] = true
	}
	// What the file names already, which a secret named for a key takes no name of.
	taken := usedNames(root)
	for name := range filled {
		taken[name] = true
	}
	// The key each value of an environment is written under.
	keyOf := map[*yaml.Node]string{}
	services := mapGet(root, "services")
	for i := 1; services != nil && i < len(services.Content); i += 2 {
		env := mapGet(services.Content[i], "environment")
		if env == nil {
			continue
		}
		switch env.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(env.Content); j += 2 {
				keyOf[env.Content[j+1]] = env.Content[j].Value
			}
		case yaml.SequenceNode:
			for _, n := range env.Content {
				if k, _, ok := strings.Cut(n.Value, "="); ok {
					keyOf[n] = strings.TrimSpace(k)
				}
			}
		default:
		}
	}
	byValue := map[string]string{} // a value, by the secret it becomes
	byVar := map[string]string{}   // a variable, by the secret it becomes
	var out []Field
	var listed error
	secret := func(value, name string) string {
		value = strings.TrimSpace(value)
		if input, ok := byValue[value]; ok {
			return input
		}
		input := freeName(strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)), taken)
		byValue[value] = input
		out = append(out, Field{Name: input, Label: labelOf(input), Kind: Generated})
		return input
	}
	walkScalars(root, func(n *yaml.Node) {
		key, inEnv := keyOf[n]
		value := n.Value
		if inEnv && n.Kind == yaml.ScalarNode && strings.HasPrefix(value, key+"=") {
			value = strings.TrimPrefix(value, key+"=")
		}
		uses := usesOf(value)
		if inEnv && listed == nil && listsSecrets(key) && (len(uses) == 0 && literalSecret(value) ||
			len(uses) == 1 && (uses[0].op == ":-" || uses[0].op == "-") && literalSecret(defaultOf(value, uses[0]))) {
			listed = leave(ClassCantSay, "its %s writes out secrets every install would share, in a form of its app's own, "+
				"which one generated secret can't take yet", key)
		}
		if inEnv && len(uses) == 0 && secretNamed(key) && literalSecret(value) {
			ref := "${" + secret(value, key) + "}"
			if n.Value != value {
				n.Value = key + "=" + ref
			} else {
				n.Value, n.Tag, n.Style = ref, "!!str", 0
			}
			return
		}
		for _, u := range uses {
			if u.op != ":-" && u.op != "-" || filled[u.name] || byVar[u.name] != "" {
				continue
			}
			def := defaultOf(value, u)
			whole := inEnv && u.at == 2 && u.end+len(u.op)+len(def)+1 == len(value)
			if !literalSecret(def) || !secretNamed(u.name) && (!whole || !secretNamed(key)) {
				continue
			}
			if input, ok := byValue[strings.TrimSpace(def)]; ok {
				byVar[u.name] = input
			} else {
				// The variable keeps its own name, as it's the one the file uses.
				taken[u.name] = false
				byVar[u.name] = secret(def, u.name)
			}
		}
	})
	if listed != nil {
		return nil, listed
	}
	if len(out) == 0 {
		return nil, nil
	}
	walkScalars(root, func(n *yaml.Node) {
		for name, input := range byVar {
			n.Value = replaceVar(n.Value, name, "${"+input+"}")
		}
	})
	names := nameNodes(root, images)
	for _, value := range slices.Sorted(maps.Keys(byValue)) {
		input := byValue[value]
		var elsewhere string
		walkScalars(root, func(n *yaml.Node) {
			whole := n.Value
			if key, ok := keyOf[n]; ok && strings.HasPrefix(whole, key+"=") {
				whole = strings.TrimPrefix(whole, key+"=")
			}
			// The same value whole, as a database named as its password is, is another
			// value that happens to match.
			if elsewhere == "" && !names[n] && strings.TrimSpace(whole) != value && holdsSecret(n.Value, value) {
				elsewhere = n.Value
			}
		})
		if elsewhere != "" {
			return nil, leave(ClassInside, "its secret %s's value is written elsewhere in the file too, in %q, which a generated secret couldn't follow", input, nodeText(&yaml.Node{Kind: yaml.ScalarNode, Value: elsewhere}))
		}
	}
	return out, nil
}

// nameNodes are where a file writes names alone: each service's image, volumes,
// ports, networks, host and the services it waits for.
func nameNodes(root *yaml.Node, images map[*yaml.Node]bool) map[*yaml.Node]bool {
	out := maps.Clone(images)
	services := mapGet(root, "services")
	for i := 1; services != nil && i < len(services.Content); i += 2 {
		for _, key := range []string{"volumes", "ports", "networks", "hostname", "container_name", "depends_on", "links", "extends", "volumes_from"} {
			if n := mapGet(services.Content[i], key); n != nil {
				walkScalars(n, func(s *yaml.Node) { out[s] = true })
			}
		}
	}
	return out
}

// holdsSecret says a longer value holds a secret's: the same word, where a name
// isn't written. A word runs on through - _ and ., so postgres isn't in postgres-data,
// and a word after ://, @ or /, or after a user's or database's flag or key, as in
// pg_isready -U postgres -d postgres, is a user, a host, a database or a path.
func holdsSecret(s, value string) bool {
	word := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '.'
	}
	for at := 0; ; {
		i := strings.Index(s[at:], value)
		if i < 0 {
			return false
		}
		start, end := at+i, at+i+len(value)
		at = start + 1
		if start > 0 && word(s[start-1]) || end < len(s) && word(s[end]) {
			continue
		}
		if !nameBefore.MatchString(s[:start]) {
			return true
		}
	}
}

// nameBefore is what comes before a name: a URL's scheme, a host's @, a path's /, a
// user's flag or key, or a database's flag, as pg_isready -d writes it.
var nameBefore = regexp.MustCompile(`(?i)(://|@|/|-u\s*|--user(name)?[\s=]+|\buser(name)?\s*[=:]\s*|\buser id\s*=\s*|\buid\s*=\s*|\s-d\s+|--dbname[\s=]+)$`)

// literalSecret says a value written under a secret's name is one: written out, and no
// switch, such as true or 0, or path, such as /run/secrets/db.
func literalSecret(value string) bool {
	value = strings.TrimSpace(value)
	switch {
	case value == "", strings.Contains(value, "$"),
		strings.HasPrefix(value, "/"), strings.HasPrefix(value, "./"), strings.HasPrefix(value, "../"), strings.HasPrefix(value, "~/"):
		return false
	}
	// A switch, or a word for none, as YANDEX_MAP_TOKEN: 0 and MAIL_PASSWORD: null say
	// there's no token or password.
	return !slices.Contains([]string{"true", "false", "yes", "no", "on", "off", "0", "1", "null", "none"}, strings.ToLower(value))
}

// maxNodes bounds what expanding a file's aliases makes, as a few aliases of aliases
// can stand for more than any Compose file says.
const maxNodes = 100000

// nodeBytes is what each node written out counts against maxExpanded beyond its
// value, for the structure that holds it.
const nodeBytes = 8

// maxExpanded bounds what expanding a file's aliases writes out, in bytes: the most a
// Compose file may be once it's read (compose.MaxFile). An anchor of 100 KiB named
// through two levels of ten aliases stands for 10 MB in a file of 100 KiB, and every
// later step writes it out again, so it's counted as it's copied, not once it's built.
const maxExpanded = compose.MaxFile

// expandAliases writes out each YAML alias as what it stands for, and each merge key
// into the mapping it's in, as Compose reads them, so nothing the rest of this reads
// hides behind one. It stops past maxNodes nodes or maxExpanded bytes of what it
// writes out.
func expandAliases(doc *yaml.Node) error {
	count, size := 0, 0
	var expand func(n *yaml.Node) (*yaml.Node, error)
	expand = func(n *yaml.Node) (*yaml.Node, error) {
		count++
		if count > maxNodes {
			return nil, leave(ClassFile, "its Compose file's aliases stand for over %d nodes, more than any Compose file says", maxNodes)
		}
		if size += nodeBytes + len(n.Value) + len(n.Tag); size > maxExpanded {
			return nil, leave(ClassFile, "its Compose file's aliases write out over %d KiB, more than a Compose file may be", maxExpanded>>10)
		}
		if n.Kind == yaml.AliasNode {
			return expand(n.Alias)
		}
		c := *n
		c.Anchor = ""
		c.Content = make([]*yaml.Node, 0, len(n.Content))
		for _, child := range n.Content {
			e, err := expand(child)
			if err != nil {
				return nil, err
			}
			c.Content = append(c.Content, e)
		}
		if c.Kind == yaml.MappingNode {
			merge(&c)
		}
		return &c, nil
	}
	for i, n := range doc.Content {
		e, err := expand(n)
		if err != nil {
			return err
		}
		doc.Content[i] = e
	}
	return nil
}

// merge writes a mapping's merge keys into it: each key of what it merges that the
// mapping doesn't set itself, in order.
func merge(m *yaml.Node) {
	var from []*yaml.Node
	own := m.Content[:0:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "<<" && m.Content[i].Tag != "!!str" {
			v := m.Content[i+1]
			if v.Kind == yaml.SequenceNode {
				from = append(from, v.Content...)
			} else {
				from = append(from, v)
			}
			continue
		}
		own = append(own, m.Content[i], m.Content[i+1])
	}
	m.Content = own
	for _, f := range from {
		for i := 0; f.Kind == yaml.MappingNode && i+1 < len(f.Content); i += 2 {
			if mapGet(m, f.Content[i].Value) == nil {
				m.Content = append(m.Content, f.Content[i], f.Content[i+1])
			}
		}
	}
}

// imageNodes are the values that name images, which are their publishers' names and
// never a service's.
func imageNodes(doc *yaml.Node) map[*yaml.Node]bool {
	out := map[*yaml.Node]bool{}
	for _, name := range serviceNames(doc) {
		if img := mapGet(service(doc, name), "image"); img != nil {
			out[img] = true
		}
	}
	return out
}

// hosts renames a host where a value names one: the whole value, or the name after
// //, @, = or a space, followed by its end, a port or a path, as in
// http://act_runner:8080. A path that ends in the name, as /usr/local/bin/act_runner
// does, stays as it is.
func hosts(root *yaml.Node, images map[*yaml.Node]bool, from, to string) {
	host := regexp.MustCompile(`(^|//|[@=\s,])` + regexp.QuoteMeta(from) + `($|[:/\s,])`)
	walkScalars(root, func(n *yaml.Node) {
		if images[n] {
			return
		}
		// Twice, as a match takes the character either side of it.
		for range 2 {
			n.Value = host.ReplaceAllString(n.Value, "${1}"+to+"${2}")
		}
	})
}

// dashServices renames a service whose name has an underscore, which a container's
// name can't take, and what reaches it by that name: its depends_on, and a value
// that names it as a host, but never an image or a path.
func dashServices(doc *yaml.Node) error {
	services := mapGet(doc.Content[0], "services")
	renamed := map[string]string{}
	for i := 0; i+1 < len(services.Content); i += 2 {
		key := services.Content[i]
		if to := strings.ReplaceAll(key.Value, "_", "-"); to != key.Value {
			if mapGet(services, to) != nil {
				return leave(ClassCantSay, "its services %s and %s would have one name", key.Value, to)
			}
			renamed[key.Value], key.Value = to, to
		}
	}
	images := imageNodes(doc)
	for from, to := range renamed {
		for _, name := range serviceNames(doc) {
			deps := mapGet(service(doc, name), "depends_on")
			if deps == nil {
				continue
			}
			for i, d := range deps.Content {
				// A list's names, or a mapping's keys.
				if d.Value == from && (deps.Kind == yaml.SequenceNode || i%2 == 0) {
					d.Value = to
				}
			}
		}
		hosts(doc.Content[0], images, from, to)
	}
	return nil
}

// hostFields are a service's fields that decide what it reaches on its server, so one
// a person types reaches it, whatever its default.
var hostFields = []string{
	"privileged", "network_mode", "pid", "ipc", "userns_mode", "cgroup_parent", "runtime",
	"cap_add", "devices", "security_opt", "sysctls", "group_add", "volumes_from",
}

// typedAccess says what a file reaches through values a person types: a field of a
// service's that decides what it reaches, and a volume's or a network's driver, its
// options, or the name of one the server has.
func typedAccess(root *yaml.Node) []string {
	var out []string
	services := mapGet(root, "services")
	for i := 0; services != nil && i+1 < len(services.Content); i += 2 {
		name, svc := services.Content[i].Value, services.Content[i+1]
		for _, field := range hostFields {
			if v := mapGet(svc, field); v != nil && typedNode(v) {
				out = append(out, fmt.Sprintf("%s: its %s, as a person sets it, %s", name, field, nodeText(v)))
			}
		}
	}
	for _, kind := range []string{"volumes", "networks"} {
		top := mapGet(root, kind)
		for i := 0; top != nil && i+1 < len(top.Content); i += 2 {
			name, spec := top.Content[i].Value, top.Content[i+1]
			what := strings.TrimSuffix(kind, "s") + " " + name
			for _, field := range []string{"driver", "name", "external"} {
				if v := mapGet(spec, field); v != nil && typedNode(v) {
					out = append(out, fmt.Sprintf("%s: its %s, as a person sets it, %s", what, field, nodeText(v)))
				}
			}
			opts := mapGet(spec, "driver_opts")
			for j := 0; opts != nil && j+1 < len(opts.Content); j += 2 {
				key, v := opts.Content[j].Value, opts.Content[j+1]
				switch {
				case !typedNode(v):
				case key == "device" || key == "mountpoint":
					out = append(out, fmt.Sprintf("%s: a server path a person names, %s", what, nodeText(v)))
				default:
					out = append(out, fmt.Sprintf("%s: its driver's %s, as a person sets it, %s", what, key, nodeText(v)))
				}
			}
		}
	}
	return out
}

// typedResources are the volumes and networks typedAccess speaks for.
func typedResources(root *yaml.Node) map[string]bool {
	out := map[string]bool{}
	for _, kind := range []string{"volumes", "networks"} {
		top := mapGet(root, kind)
		for i := 0; top != nil && i+1 < len(top.Content); i += 2 {
			spec := top.Content[i+1]
			for _, field := range []string{"driver", "driver_opts", "name", "external"} {
				if v := mapGet(spec, field); v != nil && typedNode(v) {
					out[strings.TrimSuffix(kind, "s")+" "+top.Content[i].Value] = true
				}
			}
		}
	}
	return out
}

// typedNode says a person types some of a value: it uses a variable.
func typedNode(n *yaml.Node) bool {
	typed := false
	walkScalars(n, func(s *yaml.Node) {
		if len(usesOf(braceVars(s.Value))) > 0 {
			typed = true
		}
	})
	if n.Kind == yaml.ScalarNode {
		typed = len(usesOf(braceVars(n.Value))) > 0
	}
	return typed
}

// nested refuses a folder bound inside another, as Docker shares what's in it and two
// volumes wouldn't.
func nested(folders map[string]string) error {
	rels := slices.Sorted(maps.Values(folders))
	for i, a := range rels {
		for _, b := range rels[i+1:] {
			if strings.HasPrefix(b, a+"/") {
				return leave(ClassCantSay, "it binds ./%s inside ./%s, which two volumes can't share as one folder does", b, a)
			}
		}
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
		// A port written as a number, such as 3000, is text once it says where.
		p.Tag, p.Style = "!!str", yaml.DoubleQuotedStyle
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

// mounted is what came of one of a service's volumes.
type mounted struct {
	// files are the files a source ships or writes out that it mounts, each a file
	// Dockwell writes (ADR 0085), and unread the shipped ones that weren't fetched.
	files  []shippedFile
	unread []string
	// named is the volume a folder became, which the file declares, owner whom its
	// scripts give it to and mode the mode they give it, mounted at target.
	named, owner, target string
	mode                 uint32
	// seeds are the files a folder the app writes to starts with, as its source
	// ships them, by their paths in the folder (ADR 0085).
	seeds []shippedFile
	// seedFolder says files are those of a folder the app writes to, which become
	// what its volume starts with.
	seedFolder bool
	// dropped says what the converter took away, and why.
	dropped string
	// reaches says what it reaches on the server through a value a person types.
	reaches string
}

// volume reads one of a service's volumes. A folder the app makes, bound relative to
// the file, becomes a named volume of its own. A file the source ships can't be said
// by an entry yet. A read-only mount of the server's time zone goes, as the container's
// own is UTC and the mount is host access a person would have to approve for nothing
// more. A folder a person names reaches the server, whatever its default.
func volume(app App, v *yaml.Node, folders map[string]string) (mounted, error) {
	if v.Kind == yaml.MappingNode {
		if c := mapGet(v, "content"); c != nil {
			target := mapGet(v, "target")
			if target == nil || !strings.HasPrefix(target.Value, "/") {
				return mounted{}, leave(ClassShipped, "it writes a file of its own into the container with no place for it")
			}
			return mounted{files: []shippedFile{{target: path.Clean(target.Value), content: c.Value}}}, nil
		}
		// Coolify's own, which makes the folder first, as Docker does for a volume.
		mapDel(v, "is_directory")
	}
	source, set := volumeSource(v)
	if source == "" {
		return mounted{}, nil
	}
	if volumeLike.MatchString(source) {
		// A named volume, which a platform may declare for its file where the file
		// doesn't.
		return mounted{named: source}, nil
	}
	if !strings.HasPrefix(source, "/") && !strings.HasPrefix(source, "~") && !strings.HasPrefix(source, "$") && !strings.HasPrefix(source, ".") {
		// A folder written without its ./, as Coolify's files do.
		source = "./" + source
	}
	switch {
	case source == "/etc/localtime" || source == "/etc/timezone":
		if readOnly(v) {
			return mounted{dropped: "the server's " + source + ", which only set the container's time zone"}, nil
		}
		return mounted{}, nil
	case strings.HasPrefix(source, "$") && climbs(source):
		// A folder a person names, then out of it, as ${MEDIA_DIR}/../../var/run.
		return mounted{}, leave(ClassCantSay, "it binds %s, which climbs out of the folder a person names", source)
	case strings.HasPrefix(source, "$"):
		return mounted{reaches: "a server path a person names, " + source}, nil
	case strings.HasPrefix(source, "~"):
		return mounted{}, leave(ClassCantSay, "it binds a folder in a home directory, which an entry can't know")
	case !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") && source != ".":
		return mounted{}, nil
	}
	rel := path.Clean(source)
	switch {
	case rel == ".":
		return mounted{}, leave(ClassCantSay, "it binds its own folder, with its Compose file in it, which an entry has none of")
	case strings.HasPrefix(rel, ".."):
		return mounted{}, leave(ClassCantSay, "it binds %s, outside the app's own folder", source)
	}
	var seeds []shippedFile
	if app.Shipped != nil && app.Shipped(rel) {
		shipped, err := shippedMount(app, v, rel)
		if err != nil || !shipped.seedFolder || len(shipped.unread) > 0 {
			return shipped, err
		}
		// A folder the app writes to, which it ships files into: a volume of its own,
		// which starts with them (ADR 0085).
		seeds = shipped.files
	}
	named := volumeName(rel)
	if named == "" {
		named = "data"
	}
	// One volume to a folder, so ./data-db and ./data/db stay two.
	for base, i := named, 2; folders[named] != "" && folders[named] != rel; i++ {
		named = base + "-" + strconv.Itoa(i)
	}
	folders[named] = rel
	set(named)
	out := mounted{named: named, seeds: seeds}
	owner, mode := app.scripts.ownerOf(rel), app.scripts.modeOf(rel)
	if owner != "" || mode != 0 || len(seeds) > 0 {
		target := volumeTarget(v)
		if !strings.HasPrefix(target, "/") || strings.Contains(target, "$") {
			return mounted{}, leave(ClassScripts, "its scripts or its files give %s an owner, a mode or files to start with, and it mounts that at a place a variable names, so an entry can't say how it's made", rel)
		}
		out.owner, out.mode, out.target = owner, mode, path.Clean(target)
	}
	return out, nil
}

// volumeTarget is where a volume is mounted in the container, as the file says it.
func volumeTarget(v *yaml.Node) string {
	if v.Kind == yaml.MappingNode {
		if t := mapGet(v, "target"); t != nil {
			return t.Value
		}
		return ""
	}
	if parts := splitPort(v.Value); len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// addOwner says in a service's x-dockwell whom the volume it mounts at target is
// made for, as its store's scripts gave the folder to that user (ADR 0085).
func addOwner(svc *yaml.Node, target, owner string) {
	addExtension(svc, "make_owner", target, owner)
}

// addExtension sets key's value for target in a service's x-dockwell, such as
// make_mode: {/data: "0770"}, quoted (ADR 0085).
func addExtension(svc *yaml.Node, key, target, value string) {
	x := mapGet(svc, "x-dockwell")
	if x == nil {
		x = &yaml.Node{Kind: yaml.MappingNode}
		svc.Content = append(svc.Content, scalar("x-dockwell"), x)
	}
	byTarget := mapGet(x, key)
	if byTarget == nil {
		byTarget = &yaml.Node{Kind: yaml.MappingNode}
		x.Content = append(x.Content, scalar(key), byTarget)
	}
	mapDel(byTarget, target)
	byTarget.Content = append(byTarget.Content, scalar(target), &yaml.Node{Kind: yaml.ScalarNode, Value: value, Style: yaml.DoubleQuotedStyle})
}

// imageDefaults finds a variable with a default in an image, ${TAG:-1.2} or
// ${TAG-1.2}, whose default holds no variable of its own.
var imageDefaults = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:?-([^${}]*)\}`)

// volumeLike is a named volume's name, as Compose tells one from a path.
var volumeLike = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// climbs says a path, or what follows a folder in a value, has a .. segment, which
// leaves the folder it starts in. A volume's target, after a colon, counts too.
func climbs(rest string) bool {
	return slices.Contains(strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == ':' }), "..")
}

// climbsAfter says what follows a match of a folder in a value climbs out of it, as
// ${ROOT_FOLDER_HOST}/media/../../etc does.
func climbsAfter(folder *regexp.Regexp, value string) bool {
	return slices.ContainsFunc(folder.FindAllStringIndex(value, -1), func(at []int) bool { return climbs(value[at[1]:]) })
}

// inFolder is a value with each match of a folder written as to. A match ends where
// the folder's name does: at a slash, at the colon before a volume's target, or at
// the value's end, and keeps that slash or colon.
func inFolder(folder *regexp.Regexp, value, to string) string {
	return folder.ReplaceAllStringFunc(value, func(m string) string {
		if end := m[len(m)-1]; end == '/' || end == ':' {
			return to + string(end)
		}
		return to
	})
}

// dropLabels drops the labels a platform reads, which nothing in Dockwell does:
// 1Panel's, Coolify's, and Traefik's, as Dockwell's proxy is Caddy.
func dropLabels(labels *yaml.Node) {
	platform := func(key string) bool {
		return key == "createdBy" || key == "skipStatusCheck" || strings.HasPrefix(key, "traefik.") || strings.HasPrefix(key, "coolify.")
	}
	switch labels.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(labels.Content); {
			if platform(labels.Content[i].Value) {
				labels.Content = append(labels.Content[:i], labels.Content[i+2:]...)
			} else {
				i += 2
			}
		}
	case yaml.SequenceNode:
		labels.Content = slices.DeleteFunc(labels.Content, func(n *yaml.Node) bool {
			key, _, _ := strings.Cut(n.Value, "=")
			return platform(key)
		})
	default:
	}
}

// volumeSource finds what a volume mounts, and how to change it.
func volumeSource(v *yaml.Node) (string, func(string)) {
	switch v.Kind {
	case yaml.ScalarNode:
		// At the first colon outside ${...}, so ${DATA:-./data}:/data is ${DATA:-./data}.
		parts := splitPort(v.Value)
		if len(parts) < 2 {
			return "", nil
		}
		rest := strings.Join(parts[1:], ":")
		return parts[0], func(named string) { v.Value = named + ":" + rest }
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
	parts := splitPort(v.Value)
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

// replaceVar replaces each use of a variable in a value, with whatever default it
// has, with what stands for it.
func replaceVar(s, name, with string) string {
	var b strings.Builder
	last := 0
	for _, u := range usesOf(s) {
		if u.name != name || u.at < last {
			continue
		}
		start := u.at - 2 // ${
		end := u.end
		for depth := 1; end < len(s) && depth > 0; end++ {
			switch {
			case strings.HasPrefix(s[end:], "${"):
				depth++
				end++
			case s[end] == '}':
				depth--
			}
		}
		b.WriteString(s[last:start])
		b.WriteString(with)
		last = end
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
	published := publishedVars(root)
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
		switch {
		case f.Kind == Port && !published[name]:
			// A port of something else, such as the server an agent reports to, which
			// is no port on this server and no port Dockwell may move.
			in.Type = catalogv1.InputType_INPUT_TYPE_TEXT
		case f.Kind == Port:
			in.Type = catalogv1.InputType_INPUT_TYPE_PORT
			in.Label = portLabel(in.GetLabel(), f.Web)
			in.Hint = "Only this server reaches it."
			if f.Web {
				in.Hint += " To reach it from elsewhere, add a domain once it runs."
			}
		case f.Kind == Secret:
			in.Type, in.DefaultValue = catalogv1.InputType_INPUT_TYPE_SECRET, ""
		case f.Kind == Email:
			in.Type = catalogv1.InputType_INPUT_TYPE_EMAIL
		case f.Kind == Generated:
			in.Type, in.DefaultValue, in.Optional = catalogv1.InputType_INPUT_TYPE_SECRET, "", false
			// The source's own length, as an app may want a key of exactly that, and at
			// least what the catalog takes.
			n := f.Length
			if n == 0 {
				n = 32
			}
			in.Generate, in.GenerateAs = uint32(max(n, 16)), f.As
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

// publishedVars are the variables a file publishes a port of this server by.
func publishedVars(root *yaml.Node) map[string]bool {
	out := map[string]bool{}
	services := mapGet(root, "services")
	for i := 1; services != nil && i < len(services.Content); i += 2 {
		ports := mapGet(services.Content[i], "ports")
		if ports == nil {
			continue
		}
		for _, p := range ports.Content {
			host := ""
			switch p.Kind {
			case yaml.ScalarNode:
				if parts := splitPort(p.Value); len(parts) == 3 {
					host = parts[1]
				}
			case yaml.MappingNode:
				if pub := mapGet(p, "published"); pub != nil {
					host = pub.Value
				}
			default:
			}
			for _, u := range usesOf(host) {
				out[u.name] = true
			}
		}
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
		return nil, leave(ClassFile, "its Compose file isn't YAML: %v", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, leave(ClassFile, "its Compose file isn't a mapping")
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
	text := []rune(strings.Join(strings.Fields(string(out)), " "))
	if len(text) > 80 {
		text = append(text[:77], []rune("...")...)
	}
	return string(text)
}

var bareVar = regexp.MustCompile(`(^|[^$])\$([A-Za-z_][A-Za-z0-9_]*)`)

// braceVars writes $NAME as ${NAME}, which Compose reads the same.
func braceVars(s string) string {
	return bareVar.ReplaceAllString(s, "${1}$${${2}}")
}

// unlimitedMemlock reports a memlock limit of -1, as one number or either of soft and
// hard.
func unlimitedMemlock(n *yaml.Node) bool {
	switch {
	case n == nil:
		return false
	case n.Kind == yaml.ScalarNode:
		return strings.TrimSpace(n.Value) == "-1"
	case n.Kind == yaml.MappingNode:
		return unlimitedMemlock(mapGet(n, "soft")) || unlimitedMemlock(mapGet(n, "hard"))
	}
	return false
}

// A shippedFile is one file a container mounts that its source ships beside its
// Compose file, or writes out in it, which an entry carries as a Compose config,
// read-only (ADR 0085).
type shippedFile struct {
	target, content string
}

// unreadError says the shipped files an app mounts weren't fetched, for the control
// plane to fetch and convert it again.
type unreadError []string

func (e unreadError) Error() string {
	return "it ships " + strings.Join(e, ", ") + " beside its Compose file, which wasn't read"
}

// errUnread is what an adapter's ReadShipped says of a file it didn't fetch.
var errUnread = errors.New("not fetched")

// maxShipped bounds what one app's shipped files may come to, in all, as they're
// written into its Compose file, which a server reads up to 256 KiB of.
const maxShipped = 128 << 10

// shippedMount mounts a file the source ships, or each file of a folder it ships
// that's mounted read-only, as files Dockwell writes. A folder the app writes to
// would need its files copied into a volume as it's installed, which stays out.
func shippedMount(app App, v *yaml.Node, rel string) (mounted, error) {
	if app.ReadShipped == nil {
		return mounted{}, leave(ClassShipped, "it ships %s beside its Compose file, which an entry can't declare yet", rel)
	}
	target, ok := mountTarget(v)
	if !ok {
		return mounted{}, leave(ClassShipped, "it ships %s beside its Compose file, with no place for it in the container", rel)
	}
	var paths []string
	if app.ShippedUnder != nil {
		paths = app.ShippedUnder(rel)
	}
	folder := len(paths) > 0
	var out mounted
	switch {
	case !folder:
		paths = []string{rel}
	case !readOnly(v):
		// A folder the app writes to: its volume starts with these files.
		out.seedFolder = true
	}
	size := 0
	for _, p := range paths {
		b, err := app.ReadShipped(p)
		switch {
		case errors.Is(err, errUnread):
			out.unread = append(out.unread, p)
			continue
		case err != nil:
			return mounted{}, leave(ClassFile, "%s", err)
		case !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0:
			return mounted{}, leave(ClassShipped, "it ships %s beside its Compose file, which isn't text, and Dockwell writes a text file alone", p)
		}
		if size += len(b); size > maxShipped {
			return mounted{}, leave(ClassShipped, "the files it ships beside its Compose file come to over %d KiB, more than Dockwell writes into an entry", maxShipped>>10)
		}
		at := target
		switch {
		case out.seedFolder:
			at = strings.TrimPrefix(p, rel+"/")
		case folder:
			at = path.Join(target, strings.TrimPrefix(p, rel+"/"))
		}
		content := string(b)
		if app.LiteralFiles {
			content = strings.ReplaceAll(content, "$", "$$")
		}
		out.files = append(out.files, shippedFile{target: at, content: content})
	}
	return out, nil
}

// mountTarget is where a volume mounts in its container.
func mountTarget(v *yaml.Node) (string, bool) {
	switch v.Kind {
	case yaml.ScalarNode:
		parts := splitPort(v.Value)
		if len(parts) < 2 || !strings.HasPrefix(parts[1], "/") {
			return "", false
		}
		return path.Clean(parts[1]), true
	case yaml.MappingNode:
		if t := mapGet(v, "target"); t != nil && strings.HasPrefix(t.Value, "/") {
			return path.Clean(t.Value), true
		}
	default:
	}
	return "", false
}

// ownDollars writes each bare $ in what a file holds as $$, a $ of its own to
// Compose, leaving ${NAME}, a variable, and $$ as they are.
func ownDollars(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] != '$':
			b.WriteByte(s[i])
		case i+1 < len(s) && s[i+1] == '$':
			b.WriteString("$$")
			i++
		case i+1 < len(s) && s[i+1] == '{':
			b.WriteByte('$')
		default:
			b.WriteString("$$")
		}
	}
	return b.String()
}

// addConfigs writes each file a service mounts as a Compose config: one of the
// file's own, by a name of its service and place, and a reference to it on the
// service, read-only, and executable where it's a script.
func addConfigs(root, svc *yaml.Node, service string, files []shippedFile) {
	configs := mapGet(root, "configs")
	if configs == nil {
		configs = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, scalar("configs"), configs)
	}
	refs := mapGet(svc, "configs")
	if refs == nil {
		refs = &yaml.Node{Kind: yaml.SequenceNode}
		svc.Content = append(svc.Content, scalar("configs"), refs)
	}
	for _, f := range files {
		name := volumeName(service + "-" + f.target)
		for base, i := name, 2; mapGet(configs, name) != nil; i++ {
			name = base + "-" + strconv.Itoa(i)
		}
		content := &yaml.Node{Kind: yaml.ScalarNode, Value: f.content}
		if strings.Contains(f.content, "\n") {
			content.Style = yaml.LiteralStyle
		}
		configs.Content = append(configs.Content, scalar(name), &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalar("content"), content}})
		mode := "0444"
		if strings.HasPrefix(f.content, "#!") {
			mode = "0555"
		}
		refs.Content = append(refs.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			scalar("source"), scalar(name), scalar("target"), scalar(f.target),
			scalar("mode"),
			{Kind: yaml.ScalarNode, Value: mode, Style: yaml.DoubleQuotedStyle},
		}})
	}
}

// addSeeds writes the files a volume starts with as the file's configs, and says in
// the service's x-dockwell which file of the volume at target each one is, by its
// path in the volume (ADR 0085).
func addSeeds(root, svc *yaml.Node, service, target string, files []shippedFile) {
	configs := mapGet(root, "configs")
	if configs == nil {
		configs = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, scalar("configs"), configs)
	}
	x := mapGet(svc, "x-dockwell")
	if x == nil {
		x = &yaml.Node{Kind: yaml.MappingNode}
		svc.Content = append(svc.Content, scalar("x-dockwell"), x)
	}
	seeds := mapGet(x, "seed")
	if seeds == nil {
		seeds = &yaml.Node{Kind: yaml.MappingNode}
		x.Content = append(x.Content, scalar("seed"), seeds)
	}
	byPath := &yaml.Node{Kind: yaml.MappingNode}
	seeds.Content = append(seeds.Content, scalar(target), byPath)
	for _, f := range files {
		name := volumeName(service + "-" + target + "-" + f.target)
		for base, i := name, 2; mapGet(configs, name) != nil; i++ {
			name = base + "-" + strconv.Itoa(i)
		}
		content := &yaml.Node{Kind: yaml.ScalarNode, Value: f.content}
		if strings.Contains(f.content, "\n") {
			content.Style = yaml.LiteralStyle
		}
		configs.Content = append(configs.Content, scalar(name), &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalar("content"), content}})
		byPath.Content = append(byPath.Content, scalar(f.target), scalar(name))
	}
}

// ownedInside refuses scripts that give a folder inside a volume to a user: Dockwell
// gives a volume's own folder an owner, as it makes it, and nothing inside it.
func ownedInside(steps scriptSteps, folders map[string]string) error {
	for _, dir := range slices.Sorted(maps.Keys(steps.owners)) {
		bound, inside := false, ""
		for _, rel := range folders {
			switch {
			case rel == dir || strings.HasPrefix(rel, dir+"/"):
				bound = true
			case strings.HasPrefix(dir, rel+"/"):
				inside = rel
			}
		}
		if !bound && inside != "" {
			return leave(ClassScripts, "its scripts give %s, a folder inside the volume for %s, to %s, and Dockwell gives a volume's own folder an owner, not one inside it", dir, inside, steps.owners[dir])
		}
	}
	// A mode given to a folder the app binds, or with -R to one it's inside, is the
	// volume's; one given to a file or a folder inside a volume isn't one Dockwell gives.
	for _, dir := range slices.Sorted(maps.Keys(steps.modes)) {
		m := steps.modes[dir]
		bound, inside := false, ""
		for _, rel := range folders {
			switch {
			case rel == dir || m.recursive && strings.HasPrefix(rel, dir+"/"):
				bound = true
			case strings.HasPrefix(dir, rel+"/"):
				inside = rel
			}
		}
		if !bound && inside != "" {
			return leave(ClassScripts, "its scripts give %s, inside the volume for %s, the mode %04o, and Dockwell gives a volume's own folder a mode, not something inside it", dir, inside, m.mode)
		}
	}
	return nil
}
