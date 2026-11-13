// SPDX-License-Identifier: AGPL-3.0-only

package variable

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A variable or a secret may say where its value comes from, rather than hold it: a
// template of its environment's other values, or for a link, another environment's
// value. Dockwell writes it out at the revisions its spec pins, as it's sent or
// revealed, so a change to what it names reaches every service that uses it, in the
// same plan. See
// docs/adr/0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md.

// FromUse is the name a value from another environment is used as, which no
// template name can be, as it isn't a name an environment takes.
const FromUse = "from:value"

// FromOf is where a variable's or a secret's value comes from, or nil for one that
// holds its value.
func FromOf(spec proto.Message) *resourcev1.ValueFrom {
	switch v := spec.(type) {
	case *resourcev1.Variable:
		return v.GetFrom()
	case *resourcev1.Secret:
		return v.GetFrom()
	}
	return nil
}

// checkFrom refuses a from that can't be written out. secret says the value is a
// secret's, which alone may name a secret.
func checkFrom(what string, from *resourcev1.ValueFrom, value string, secret bool) error {
	if from == nil {
		return nil
	}
	if value != "" {
		return fmt.Errorf("a %s either holds its value or says where it comes from, not both. Leave value out", what)
	}
	switch src := from.GetSource().(type) {
	case *resourcev1.ValueFrom_Template:
		return checkTemplate(what, src.Template, secret)
	case *resourcev1.ValueFrom_Value:
		ref := src.Value
		switch {
		case !strings.HasPrefix(ref.GetEnvironment(), "env_"):
			return fmt.Errorf("a %s's value from another environment names that environment by its ID, such as env_…", what)
		case ref.GetLink() == "":
			return fmt.Errorf("only a link writes a %s's value from another environment, and this one names none", what)
		case ref.GetSecret() && !secret:
			return errors.New("a variable can't take its value from a secret, as plans show a variable. Make it a secret")
		}
		return CheckName(what, ref.GetName())
	default:
		return fmt.Errorf("a %s's from says neither a template nor a value", what)
	}
}

// checkTemplate refuses a template that names what it doesn't list, lists what it
// doesn't name, or names a secret from a variable.
func checkTemplate(what string, t *resourcev1.ValueTemplate, secret bool) error {
	if t.GetText() == "" {
		return fmt.Errorf("a %s's template is empty", what)
	}
	used, err := templateNames(t.GetText())
	if err != nil {
		return fmt.Errorf("a %s's template: %w", what, err)
	}
	listed := map[string]bool{}
	for _, n := range t.GetNames() {
		if err := CheckName(what, n.GetName()); err != nil {
			return err
		}
		switch {
		case listed[n.GetName()]:
			return fmt.Errorf("a %s's template lists %s twice", what, n.GetName())
		case !slices.Contains(used, n.GetName()):
			return fmt.Errorf("a %s's template lists %s and doesn't use it. Write ${%s} where it goes, or take it out", what, n.GetName(), n.GetName())
		case n.GetSecret() && !secret:
			return fmt.Errorf("a variable's template names the secret %s, and plans show a variable. Make it a secret", n.GetName())
		}
		listed[n.GetName()] = true
	}
	for _, name := range used {
		if !listed[name] {
			return fmt.Errorf("a %s's template uses ${%s} without listing it. Add it to names, as a variable or a secret", what, name)
		}
	}
	return checkValue(what, t.GetText())
}

// templateNames lists the ${NAME}s a template uses, in order, once each. $$ is a $ of
// its own, and any other $ is refused, as it couldn't be told from a name.
func templateNames(text string) ([]string, error) {
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '$' {
			continue
		}
		switch {
		case strings.HasPrefix(text[i:], "$$"):
			i++
		case strings.HasPrefix(text[i:], "${"):
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return nil, errors.New("a ${ with no }")
			}
			name := text[i+2 : i+end]
			if CheckName("value", name) != nil {
				return nil, fmt.Errorf("${%s} doesn't name a value", name)
			}
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
			i += end
		default:
			return nil, errors.New("a $ that isn't ${NAME}. Write a $ of its own as $$")
		}
	}
	return out, nil
}

// fromUses lists what a from uses, by the name the value sees each as.
func fromUses(from *resourcev1.ValueFrom, revisions map[string]int64) []resource.Use {
	switch src := from.GetSource().(type) {
	case *resourcev1.ValueFrom_Template:
		out := make([]resource.Use, 0, len(src.Template.GetNames()))
		for _, n := range src.Template.GetNames() {
			kind := VariableKind
			if n.GetSecret() {
				kind = SecretKind
			}
			out = append(out, resource.Use{As: n.GetName(), Kind: kind, Name: n.GetName(), Revision: int(revisions[n.GetName()])})
		}
		return out
	case *resourcev1.ValueFrom_Value:
		kind := VariableKind
		if src.Value.GetSecret() {
			kind = SecretKind
		}
		return []resource.Use{{As: FromUse, Kind: kind, Name: src.Value.GetName(), Scope: src.Value.GetEnvironment(), Revision: int(revisions[FromUse])}}
	}
	return nil
}

func pinned(revisions map[string]int) map[string]int64 {
	if len(revisions) == 0 {
		return nil
	}
	out := make(map[string]int64, len(revisions))
	for as, r := range revisions {
		out[as] = int64(r)
	}
	return out
}

// Write writes a template out with the values it names, by name, each encoded for
// where it stands. It refuses one it has no value for, naming it, never a value.
func Write(t *resourcev1.ValueTemplate, values map[string]string) (string, error) {
	encoding := map[string]resourcev1.Encoding{}
	for _, n := range t.GetNames() {
		encoding[n.GetName()] = n.GetEncoding()
	}
	text := t.GetText()
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch {
		case strings.HasPrefix(text[i:], "$$"):
			b.WriteByte('$')
			i++
		case strings.HasPrefix(text[i:], "${"):
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return "", errors.New("a template with a ${ and no }")
			}
			name := text[i+2 : i+end]
			v, ok := values[name]
			if !ok {
				return "", fmt.Errorf("there's no value of %s to write into the template", name)
			}
			b.WriteString(encode(v, encoding[name]))
			i += end
		default:
			b.WriteByte(text[i])
		}
	}
	return b.String(), nil
}

// encode writes a value as a URL takes it where it stands.
func encode(v string, e resourcev1.Encoding) string {
	switch e {
	case resourcev1.Encoding_ENCODING_USERINFO:
		// url.UserPassword encodes for a user and a password alike, as : stays
		// encoded in a password and a user takes no :.
		return strings.TrimSuffix(url.UserPassword(v, "").String(), ":")
	case resourcev1.Encoding_ENCODING_PATH:
		return url.PathEscape(v)
	case resourcev1.Encoding_ENCODING_QUERY:
		return url.QueryEscape(v)
	default:
		return v
	}
}
