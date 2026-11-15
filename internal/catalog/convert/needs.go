// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// NeedDraft is what a source's form says an app takes from another app, such as a
// database server it shares, by the variables the source fills from it. The converter
// makes it a need (ADR 0084): met by a new one beside the app by default, or by one a
// person runs already, on which the app gets a user of its own and never the server's
// administrator.
type NeedDraft struct {
	// Name is the need's name in the entry, such as "db", and Label what people call it.
	Name, Label string
	// Engines are what meets it, the first the default.
	Engines []catalogv1.Engine
	// Parts are the variables the source fills from it, by what each holds.
	Parts map[string]catalogv1.Part
	// Words are the app's word for each engine, for a variable the source fills with
	// it (an engine part), such as PANEL_DB_TYPE: "mysql" for MySQL, "postgres" for
	// PostgreSQL.
	Words map[catalogv1.Engine]string
}

// partWords are the words a need's template writes each part as.
var partWords = map[catalogv1.Part]string{
	catalogv1.Part_PART_HOST:     "host",
	catalogv1.Part_PART_PORT:     "port",
	catalogv1.Part_PART_DATABASE: "database",
	catalogv1.Part_PART_USER:     "user",
	catalogv1.Part_PART_PASSWORD: "password",
}

var setName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// applyNeeds makes the entry's needs from the app's drafts, rewriting the file so each
// value a need sets is a whole value of a service's environment, as an entry's need
// sets it (ADR 0084):
//   - a variable used whole keeps its use, under a plain name, as DB_HOST;
//   - a value made of one need's parts and fixed text, as ${HOST}:${PORT} or a
//     connection URL, becomes a variable of its own the need writes, named for the
//     environment key it fills: an address, or a URL from a template.
//
// A value that mixes a need's parts with other variables, a need's part used anywhere
// but a service's environment, and a database need with no way to reach it or sign in,
// leave the app out, saying why. taken are the names the file uses already.
func applyNeeds(root *yaml.Node, services []string, app App, taken map[string]bool) ([]*catalogv1.Need, []string, error) {
	if len(app.Needs) == 0 {
		return nil, nil, nil
	}
	// Each variable a draft fills, by the draft and what it holds.
	type part struct {
		draft int
		part  catalogv1.Part
	}
	drafts := slices.Clone(app.Needs)
	for i := range drafts {
		drafts[i] = engineWritten(root, services, drafts[i])
	}
	of := map[string]part{}
	for i, d := range drafts {
		for v, p := range d.Parts {
			of[v] = part{i, p}
		}
	}
	sets := make([][]*catalogv1.NeedSet, len(drafts))
	named := map[string]string{} // a variable used whole, by the name it's set as
	claim := func(want string) string {
		name := strings.ToUpper(want)
		for i := 2; !setName.MatchString(name) || taken[name]; i++ {
			name = fmt.Sprintf("%s_%d", strings.ToUpper(strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
					return r
				}
				return '_'
			}, want)), i)
			if !setName.MatchString(name) {
				name = fmt.Sprintf("NEED_%d", i)
			}
		}
		taken[name] = true
		return name
	}
	// A value of a service's environment, under its key, as the need sets it.
	value := func(svc, key, v string) (string, error) {
		uses := usesOf(v)
		var mine, others []varUse
		for _, u := range uses {
			if _, ok := of[u.name]; ok {
				mine = append(mine, u)
			} else {
				others = append(others, u)
			}
		}
		draft := -1
		for _, u := range mine {
			switch p := of[u.name]; {
			case u.op != "":
				return "", leave(ClassAnotherApp, "its service %s gives %s a default or a check where it takes another app's %s, which a need can't write", svc, key, u.name)
			case draft >= 0 && draft != p.draft:
				return "", leave(ClassAnotherApp, "its service %s builds %s from two other apps' values, which a need can't write together", svc, key)
			default:
				draft = p.draft
			}
		}
		if draft >= 0 && len(others) > 0 {
			return "", leave(ClassAnotherApp, "its service %s builds %s from another app's %s and %s, which a need can't write together", svc, key, needVars(mine, of), "${"+others[0].name+"}")
		}
		if draft < 0 {
			return v, nil
		}
		if len(uses) == 1 && v == "${"+uses[0].name+"}" {
			u := uses[0]
			name, ok := named[u.name]
			if !ok {
				name = claim(plainName(Field{Name: u.name}))
				named[u.name] = name
				sets[draft] = append(sets[draft], &catalogv1.NeedSet{Name: name, Part: of[u.name].part})
			}
			return "${" + name + "}", nil
		}
		// A value made of the need's parts and fixed text, written by the need.
		template := v
		for i := len(uses) - 1; i >= 0; i-- {
			u := uses[i]
			end := u.end + 1 // the closing brace
			template = template[:u.at-2] + "{" + partWords[of[u.name].part] + "}" + template[end:]
		}
		name := claim(key)
		if template == "{host}:{port}" {
			sets[draft] = append(sets[draft], &catalogv1.NeedSet{Name: name, Part: catalogv1.Part_PART_ADDRESS})
		} else {
			sets[draft] = append(sets[draft], &catalogv1.NeedSet{Name: name, Part: catalogv1.Part_PART_URL, Template: template})
		}
		return "${" + name + "}", nil
	}
	for _, svc := range services {
		env := mapGet(serviceNode(root, svc), "environment")
		if env == nil {
			continue
		}
		switch env.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(env.Content); i += 2 {
				got, err := value(svc, env.Content[i].Value, env.Content[i+1].Value)
				if err != nil {
					return nil, nil, err
				}
				env.Content[i+1].Value = got
			}
		case yaml.SequenceNode:
			for _, item := range env.Content {
				key, v, ok := strings.Cut(item.Value, "=")
				if !ok {
					continue
				}
				got, err := value(svc, key, v)
				if err != nil {
					return nil, nil, err
				}
				item.Value = key + "=" + got
			}
		case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode, yaml.StreamNode:
			// No environment of its own to set.
		}
	}
	// What's left of a need's parts is used outside a service's environment.
	var outside error
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			if _, ok := of[u.name]; ok && outside == nil {
				outside = leave(ClassAnotherApp, "it uses another app's %s outside a service's environment, where a need's value can't reach", u.name)
			}
		}
	})
	if outside != nil {
		return nil, nil, outside
	}
	var needs []*catalogv1.Need
	var setNames []string
	for i, d := range drafts {
		if len(sets[i]) == 0 {
			continue // the file doesn't use it
		}
		// A URL that names its engine, as postgres:// does, takes that engine alone.
		d.Engines = slices.DeleteFunc(slices.Clone(d.Engines), func(e catalogv1.Engine) bool { return !urlsTake(sets[i], e) })
		if len(d.Engines) == 0 {
			return nil, nil, leave(ClassAnotherApp, "its URL for %s names an engine its form doesn't offer", d.Label)
		}
		for _, s := range sets[i] {
			if s.GetPart() == catalogv1.Part_PART_ENGINE {
				s.Words = wordsOf(d)
			}
		}
		if err := reaches(d, sets[i]); err != nil {
			return nil, nil, err
		}
		n := &catalogv1.Need{Name: d.Name, Label: d.Label, Sets: sets[i]}
		for slices.Contains(services, n.GetName()) {
			n.Name += "-need"
		}
		for _, e := range d.Engines {
			n.Accepts = append(n.Accepts, &catalogv1.NeedAccept{Engine: e})
		}
		if sql(d.Engines) {
			// The app's own database and user, named for the app.
			n.Database = databaseName(app.ID)
			n.User = n.Database
		}
		for _, s := range sets[i] {
			setNames = append(setNames, s.GetName())
		}
		needs = append(needs, n)
	}
	return needs, setNames, nil
}

// engineWritten is a draft whose engine's word the file uses only as whole values of
// its services' environment, which an engine part sets, as it is; or else, where the
// word is part of a longer value, such as a URL's scheme (${PANEL_DB_TYPE}://...), or
// is used outside the environment, the draft of its first engine alone, that engine's
// word written into the file, as a need writes no part inside another value.
func engineWritten(root *yaml.Node, services []string, d NeedDraft) NeedDraft {
	for v, p := range d.Parts {
		if p != catalogv1.Part_PART_ENGINE || wholeInEnvironment(root, services, v) {
			continue
		}
		word := d.Words[d.Engines[0]]
		walkScalars(root, func(n *yaml.Node) { n.Value = replaceVar(n.Value, v, word) })
		d.Parts = maps.Clone(d.Parts)
		delete(d.Parts, v)
		d.Engines, d.Words = d.Engines[:1], nil
	}
	return d
}

// wholeInEnvironment says the file uses variable v only as whole values of its
// services' environment.
func wholeInEnvironment(root *yaml.Node, services []string, v string) bool {
	all, whole := 0, 0
	walkScalars(root, func(n *yaml.Node) {
		for _, u := range usesOf(n.Value) {
			if u.name == v {
				all++
			}
		}
	})
	for _, svc := range services {
		env := mapGet(serviceNode(root, svc), "environment")
		if env == nil {
			continue
		}
		var values []string
		switch env.Kind {
		case yaml.MappingNode:
			for i := 1; i < len(env.Content); i += 2 {
				values = append(values, env.Content[i].Value)
			}
		case yaml.SequenceNode:
			for _, item := range env.Content {
				if _, value, ok := strings.Cut(item.Value, "="); ok {
					values = append(values, value)
				}
			}
		case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode, yaml.StreamNode:
		}
		for _, value := range values {
			if value == "${"+v+"}" {
				whole++
			}
		}
	}
	return all == whole
}

// schemes are the engines a URL's scheme names, by the scheme.
var schemes = map[string][]catalogv1.Engine{
	"postgres":    {catalogv1.Engine_ENGINE_POSTGRESQL},
	"postgresql":  {catalogv1.Engine_ENGINE_POSTGRESQL},
	"mysql":       {catalogv1.Engine_ENGINE_MYSQL, catalogv1.Engine_ENGINE_MARIADB},
	"mariadb":     {catalogv1.Engine_ENGINE_MYSQL, catalogv1.Engine_ENGINE_MARIADB},
	"redis":       {catalogv1.Engine_ENGINE_REDIS, catalogv1.Engine_ENGINE_VALKEY},
	"rediss":      {catalogv1.Engine_ENGINE_REDIS, catalogv1.Engine_ENGINE_VALKEY},
	"mongodb":     {catalogv1.Engine_ENGINE_MONGODB},
	"mongodb+srv": {catalogv1.Engine_ENGINE_MONGODB},
}

// urlsTake says each URL a need writes can be of engine e: one whose scheme names an
// engine, as postgres:// or jdbc:mysql:// does, is of that engine alone.
func urlsTake(sets []*catalogv1.NeedSet, e catalogv1.Engine) bool {
	for _, s := range sets {
		scheme, _, ok := strings.Cut(s.GetTemplate(), "://")
		if s.GetPart() != catalogv1.Part_PART_URL || !ok {
			continue
		}
		named, takes := false, false
		for word := range strings.SplitSeq(strings.ToLower(scheme), ":") {
			if of, ok := schemes[word]; ok {
				named, takes = true, takes || slices.Contains(of, e)
			}
		}
		if named && !takes {
			return false
		}
	}
	return true
}

// wordsOf are a draft's words for its engines, in the order it accepts them.
func wordsOf(d NeedDraft) []*catalogv1.NeedWord {
	out := make([]*catalogv1.NeedWord, 0, len(d.Engines))
	for _, e := range d.Engines {
		out = append(out, &catalogv1.NeedWord{Engine: e, Word: d.Words[e]})
	}
	return out
}

// reaches refuses a need its app couldn't use: one that gives no host, or, for a
// database, no password, as an app signs in as a user of its own (ADR 0084).
func reaches(d NeedDraft, sets []*catalogv1.NeedSet) error {
	has := func(part catalogv1.Part, word string) bool {
		return slices.ContainsFunc(sets, func(s *catalogv1.NeedSet) bool {
			return s.GetPart() == part || s.GetPart() == catalogv1.Part_PART_URL && strings.Contains(s.GetTemplate(), "{"+word+"}") ||
				part == catalogv1.Part_PART_HOST && s.GetPart() == catalogv1.Part_PART_ADDRESS
		})
	}
	switch {
	case !has(catalogv1.Part_PART_HOST, "host"):
		return leave(ClassAnotherApp, "it takes %s from another app with no host to reach it at", d.Label)
	case sql(d.Engines) && !has(catalogv1.Part_PART_PASSWORD, "password"):
		return leave(ClassAnotherApp, "it signs in to %s with no password of its own", d.Label)
	}
	return nil
}

// sql reports engines with databases and users, as opposed to Redis's.
func sql(engines []catalogv1.Engine) bool {
	return slices.ContainsFunc(engines, func(e catalogv1.Engine) bool {
		return e == catalogv1.Engine_ENGINE_MYSQL || e == catalogv1.Engine_ENGINE_MARIADB || e == catalogv1.Engine_ENGINE_POSTGRESQL
	})
}

// databaseName is the database, and the user, made for an app on a server: its ID
// with underscores for dashes, as a database's name takes.
func databaseName(id string) string {
	name := strings.ReplaceAll(id, "-", "_")
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// needVars says which of a value's variables a need fills, for a reason.
func needVars[T any](uses []varUse, of map[string]T) string {
	var out []string
	for _, u := range uses {
		if _, ok := of[u.name]; ok {
			out = append(out, "${"+u.name+"}")
		}
	}
	return strings.Join(out, ", ")
}

// serviceNode is a service's node by its name, under the file's root.
func serviceNode(root *yaml.Node, name string) *yaml.Node {
	return mapGet(mapGet(root, "services"), name)
}
