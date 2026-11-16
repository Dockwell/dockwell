// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
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
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// A secret inside a longer value of a service's environment, as a connection URL
// holds a password (DATABASE_URL: postgres://app:${DB_PASSWORD}@db/app), is written
// as a secret of its own, whose value Dockwell writes from a template of the
// environment's values (ADR 0084's ValueFrom), as a need's URL is: so an upgrade
// stands nothing in for it, and a rotation of the password reaches it (#570).

// placeholderIn is a Compose file's ${NAME}, ${NAME:-default} and its kin, or
// $NAME, with $$ for a $ of its own.
var placeholderIn = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)((:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// valueRef is one placeholder a value holds, where it stands.
type valueRef struct {
	start, end int
	name       string
	// op and fallback are its ${NAME:-fallback} and kin.
	op, fallback string
}

// placeholders are what a value names, in order, leaving out $$.
func placeholders(v string) []valueRef {
	var out []valueRef
	for _, m := range placeholderIn.FindAllStringSubmatchIndex(v, -1) {
		p := valueRef{start: m[0], end: m[1]}
		switch {
		case m[2] >= 0:
			p.name = v[m[2]:m[3]]
			if m[6] >= 0 {
				p.op, p.fallback = v[m[6]:m[7]], v[m[8]:m[9]]
			}
		case m[10] >= 0:
			p.name = v[m[10]:m[11]]
		default:
			continue // $$
		}
		out = append(out, p)
	}
	return out
}

// insideAt says how a value at a place in text is written where it stands: as a
// URL's user or password before its host's @, in its path, or in its query; as it
// is in text that isn't a URL.
func insideAt(text string, at int) resourcev1.Encoding {
	i := strings.Index(text, "://")
	// Only a value that's one URL is written as one (#570's review, L1).
	if i < 0 || !oneURL(text) {
		return resourcev1.Encoding_ENCODING_UNSPECIFIED
	}
	authority, userinfo, query := i+3, -1, strings.IndexByte(text, '?')
	end := len(text)
	if j := strings.IndexAny(text[authority:], "/?#"); j >= 0 {
		end = authority + j
	}
	if j := strings.LastIndexByte(text[authority:end], '@'); j >= 0 {
		userinfo = authority + j
	}
	switch {
	case at < userinfo:
		return resourcev1.Encoding_ENCODING_USERINFO
	case query >= 0 && at > query:
		return resourcev1.Encoding_ENCODING_QUERY
	case at >= end:
		return resourcev1.Encoding_ENCODING_PATH
	}
	return resourcev1.Encoding_ENCODING_UNSPECIFIED
}

// insideTemplate is the template a value with a secret inside it is written from: the
// value's own text, each secret named as ${NAME}, written as it stands. A plain value
// it names stays as the value had it, for Fix to write in once the install has it,
// as only secrets are the environment's values a template can name.
func insideTemplate(v string, secret map[string]bool) *resourcev1.ValueTemplate {
	t := &resourcev1.ValueTemplate{}
	var b strings.Builder
	last := 0
	for _, p := range placeholders(v) {
		if !secret[p.name] {
			continue
		}
		b.WriteString(v[last:p.start])
		b.WriteString("${" + p.name + "}")
		last = p.end
		if !slices.ContainsFunc(t.GetNames(), func(n *resourcev1.TemplateName) bool { return n.GetName() == p.name }) {
			t.Names = append(t.Names, &resourcev1.TemplateName{Name: p.name, Secret: true, Encoding: insideAt(v, p.start)})
		}
	}
	b.WriteString(v[last:])
	t.Text = b.String()
	return t
}

// insideRefusal says why a value of a service's environment that names the secret
// name can't be written from a template, or nil where it can. Each place it names
// the secret must be a placeholder of its own: inside another's default, as in
// ${GIVEN_URL:-postgres://u:${PASSWORD}@db/x}, Compose reads it, and a template
// can't hold it, so the install would write the secret's stand-in (#570's review,
// H1).
func insideRefusal(v, name string) error {
	// $$ is a $ of its own, so $${NAME} names nothing.
	plain := strings.ReplaceAll(v, "$$", "\x00\x00")
	refs := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `([^A-Za-z0-9_]|$)`)
	starts := map[int]bool{}
	for _, p := range placeholders(v) {
		if p.name == name {
			starts[p.start] = true
		}
	}
	for _, at := range refs.FindAllStringIndex(plain, -1) {
		if !starts[at[0]] {
			return fmt.Errorf("the secret %s is inside another value's default, %q, which a template can't hold. Give the app the secret as a value of its own", name, strings.ReplaceAll(v, "\n", " "))
		}
	}
	// One URL, or a line of text: anything else, such as JSON or two URLs, a template
	// can't write as the app reads it (#570's review, L1).
	if !oneURL(v) && !oneLine(v) {
		return fmt.Errorf("the secret %s is inside %q, which isn't one URL or a line of text, so a template couldn't write it as the app reads it. Give the app the secret as a value of its own", name, strings.ReplaceAll(v, "\n", " "))
	}
	return nil
}

// bare is a value with each placeholder as a word of no meaning, and each $$ as one
// character, for what's around them to be read.
func bare(v string) string {
	return placeholderIn.ReplaceAllString(v, "x")
}

// urlShape is a URL from its start: a scheme, then // and no space, quote, brace or
// bracket.
var urlShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^\s"'{}\[\]]*$`)

// unquoted is a value without the one pair of quotes it's wrapped in, if it is.
func unquoted(b string) string {
	if len(b) >= 2 && (b[0] == '"' || b[0] == '\'') && b[len(b)-1] == b[0] {
		return b[1 : len(b)-1]
	}
	return b
}

// oneURL says a value is one URL and nothing else, in a pair of quotes or none. Its
// shape is read up to its query: a query may hold what a format reads, such as
// capso's ssl={"rejectUnauthorized":false}, where its password isn't (#570's recheck).
func oneURL(v string) bool {
	b := unquoted(bare(v))
	head, query, _ := strings.Cut(b, "?")
	return urlShape.MatchString(head) && !strings.ContainsAny(query, " \t\r\n") && strings.Count(b, "://") == 1
}

// oneLine says a value is a line of text that isn't a URL and holds nothing a format
// reads, such as JSON's braces and quotes, as a key=value DSN or a prefix is, in a
// pair of quotes or none.
func oneLine(v string) bool {
	b := unquoted(bare(v))
	return !strings.Contains(b, "://") && !strings.ContainsAny(b, "{}[]\"'\n\r")
}

// holdsInside says a value names a secret inside a longer value.
func holdsInside(v string, secret map[string]bool) bool {
	ps := placeholders(v)
	for _, p := range ps {
		// The secret whole, ${NAME}, is a value of its own; anything else is inside.
		if secret[p.name] && (len(ps) != 1 || p.start != 0 || p.end != len(v) || p.op != "") {
			return true
		}
	}
	return false
}

// InsideName is the name a value's secret takes: the variable's own, or with its
// service's before it where that's taken, as an input or another value is.
func InsideName(service, key string, taken map[string]bool) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z':
				return r - 'a' + 'A'
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
				return r
			}
			return '_'
		}, s)
	}
	name := clean(key)
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "V_" + name
	}
	if !taken[name] {
		return name
	}
	base := clean(service) + "_" + name
	name = base
	for i := 2; taken[name]; i++ {
		name = base + "_" + strings.Repeat("X", i-1)
	}
	return name
}

// InsideKey is a value of a service's environment, as InsideFile and Needed's
// InsideKeys name it: "<service>/<variable>".
func InsideKey(service, key string) string { return service + "/" + key }

// InsideFile rewrites each value of a service's environment in a Compose file that
// holds one of secret inside a longer value to name the secret name gives it, whole,
// as an install does (inside). It returns the file, and each value's name by
// InsideKey. An upgrade reads the file the install ran through it too, so a person's
// edit of such a value reads as theirs, against what runs (#570's review, H2).
func InsideFile(file string, secret map[string]bool, name func(service, key string) string) (string, map[string]string, error) {
	return rewriteInside(file, secret, func(service, key, _ string) string { return name(service, key) })
}

// rewriteInside is InsideFile, with each value's own text for name to read.
func rewriteInside(file string, secret map[string]bool, name func(service, key, value string) string) (string, map[string]string, error) {
	named := map[string]string{}
	if len(secret) == 0 {
		return file, named, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", nil, errors.New("its Compose file isn't YAML")
	}
	write := func(service, key, value string) string {
		n := name(service, key, value)
		named[InsideKey(service, key)] = n
		return "${" + n + "}"
	}
	services := nodeGet(doc.Content[0], "services")
	for i := 0; services != nil && i+1 < len(services.Content); i += 2 {
		service := services.Content[i].Value
		env := nodeGet(services.Content[i+1], "environment")
		if env == nil {
			continue
		}
		switch env.Kind {
		case yaml.SequenceNode:
			for _, n := range env.Content {
				key, value, ok := strings.Cut(n.Value, "=")
				if ok && holdsInside(value, secret) {
					n.Value, n.Style = key+"="+write(service, key, value), 0
				}
			}
		case yaml.MappingNode:
			for j := 0; j+1 < len(env.Content); j += 2 {
				if v := env.Content[j+1]; v.Kind == yaml.ScalarNode && holdsInside(v.Value, secret) {
					v.Value, v.Style, v.Tag = write(service, env.Content[j].Value, v.Value), 0, "!!str"
				}
			}
		default:
		}
	}
	if len(named) == 0 {
		return file, named, nil
	}
	out, err := encode(&doc)
	return out, named, err
}

// inside writes each value of a service's environment that holds a secret inside a
// longer value as a secret of its own: the value names it whole, ${NAME}, the entry
// gains an input of that name, which Fill generates and the install drops, and
// Written holds the template Dockwell writes it from, which Inside marks for Fix and
// InsideKeys names by the value. A value running names, by InsideKey, keeps that
// name, unless the entry has an input or a need's URL of it; the others take a name
// none of running has (NeedAs).
func inside(out *Needed, running map[string]string) error {
	e := out.Entry
	secret := map[string]bool{}
	taken := map[string]bool{}
	for _, in := range e.GetInputs() {
		taken[in.GetName()] = true
		if Secret(in) {
			secret[in.GetName()] = true
		}
	}
	for name := range out.Written {
		taken[name] = true
	}
	kept := map[string]bool{}
	for _, name := range running {
		kept[name] = true
	}
	var adds []*catalogv1.Input
	file, named, err := rewriteInside(e.GetCompose(), secret, func(service, key, value string) string {
		name := running[InsideKey(service, key)]
		if name != "" && taken[name] {
			if out.Taken == nil {
				out.Taken = map[string]string{}
			}
			out.Taken[InsideKey(service, key)] = name
		}
		if name == "" || taken[name] {
			name = InsideName(service, key, func() map[string]bool {
				all := maps.Clone(taken)
				maps.Copy(all, kept)
				return all
			}())
		}
		taken[name] = true
		out.Written[name] = insideTemplate(value, secret)
		adds = append(adds, &catalogv1.Input{Name: name, Label: key, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32})
		return name
	})
	if err != nil || len(named) == 0 {
		return err
	}
	out.Inside, out.InsideKeys = map[string]bool{}, named
	for _, name := range named {
		out.Inside[name] = true
	}
	needed := proto.CloneOf(e)
	needed.Compose, needed.Inputs = file, append(slices.Clone(e.GetInputs()), adds...)
	out.Entry = needed
	return nil
}

// Fix writes into each template inside made the plain values it names, as the
// install has them in variables (fixTemplate).
func (n *Needed) Fix(variables map[string]string) {
	for name := range n.Inside {
		if t := n.Written[name]; t != nil {
			n.Written[name] = fixTemplate(t, variables)
		}
	}
}

// fixTemplate is a template of a value with a secret inside, with the plain values it
// names written in, as the install has them in variables, encoded for where each
// stands in a URL, with a $ of their own as $$; so the template names the secrets
// alone, which the environment holds. A name the install has no value for takes its
// fallback, as Compose would.
func fixTemplate(t *resourcev1.ValueTemplate, variables map[string]string) *resourcev1.ValueTemplate {
	secret := map[string]bool{}
	for _, tn := range t.GetNames() {
		secret[tn.GetName()] = tn.GetSecret()
	}
	text := t.GetText()
	var b strings.Builder
	last := 0
	for _, p := range placeholders(text) {
		if secret[p.name] {
			continue
		}
		value, set := variables[p.name]
		switch p.op {
		case ":-":
			if value == "" {
				value = p.fallback
			}
		case "-":
			if !set {
				value = p.fallback
			}
		case ":+":
			if value != "" {
				value = p.fallback
			}
		case "+":
			if set {
				value = p.fallback
			}
		}
		b.WriteString(text[last:p.start])
		// Encoded for where it stands, as a secret is: a user of me@corp in a URL's
		// userinfo is me%40corp (#570's review, L2).
		value = variable.Encode(value, insideAt(text, p.start))
		b.WriteString(strings.ReplaceAll(value, "$", "$$"))
		last = p.end
	}
	b.WriteString(text[last:])
	fixed := proto.CloneOf(t)
	fixed.Text = b.String()
	return fixed
}

// UsedBy are the services of a Compose file whose environment names name, as
// ${NAME} or $NAME, sorted.
func UsedBy(file, name string) []string {
	cf, err := readCompose(file)
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `([^A-Za-z0-9_]|$)`)
	names := func(v any) bool {
		s, ok := v.(string)
		return ok && re.MatchString(strings.ReplaceAll(s, "$$", "\x00\x00"))
	}
	var out []string
	for service, def := range cf.services {
		m, _ := def.(map[string]any)
		var hit bool
		switch env := m["environment"].(type) {
		case map[string]any:
			for _, v := range env {
				hit = hit || names(v)
			}
		case []any:
			for _, v := range env {
				hit = hit || names(v)
			}
		}
		if hit {
			out = append(out, service)
		}
	}
	slices.Sort(out)
	return out
}
