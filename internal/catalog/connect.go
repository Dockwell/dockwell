// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"fmt"
	"slices"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Connected is what linking an app from no entry to a database server it shares sets
// in the app's environment, as an install's need would (ADR 0084, Connect a database).
type Connected struct {
	// Link is the link, with what meets it left for the caller to say.
	Link *resourcev1.Link
	// Plain are the values of the names the need sets that hold no password.
	Plain map[string]string
	// Written are the URLs it sets, each a template of the environment's names.
	Written map[string]*resourcev1.ValueTemplate
	// Added are the names a URL keeps a part in that the need sets none of, with the
	// value each holds, which the caller adds to the environment.
	Added map[string]string
}

// Connect works out what a need that a person made for an app from no entry sets when
// w meets it: w's server, database and user, as w.Alias reaches it. app names the app
// in what goes wrong, and taken are the names its environment has already, which a
// URL's parts aren't kept in.
func Connect(app string, n *catalogv1.Need, w Way, taken []string) (*Connected, error) {
	values := needValues(n, w)
	values[catalogv1.Part_PART_DATABASE], values[catalogv1.Part_PART_USER] = w.Database, w.User
	// The names the environment has already count as an entry's inputs, so a URL's
	// part goes in a name of its own.
	entry := &catalogv1.Entry{Name: app, Needs: []*catalogv1.Need{n}}
	for _, name := range taken {
		entry.Inputs = append(entry.Inputs, &catalogv1.Input{Name: name})
	}
	out := &Connected{Plain: map[string]string{}, Written: map[string]*resourcev1.ValueTemplate{}, Added: map[string]string{}}
	for _, s := range n.GetSets() {
		switch {
		case s.GetPart() == catalogv1.Part_PART_URL:
			t, parts, err := urlTemplate(entry, n, w, s, values)
			if err != nil {
				return nil, err
			}
			out.Written[s.GetName()] = t
			for _, p := range parts {
				// A part some other name of the need sets is the need's own.
				if !slices.ContainsFunc(n.GetSets(), func(o *catalogv1.NeedSet) bool { return o.GetName() == p.name }) {
					out.Added[p.name] = p.value
				}
			}
		case SecretPart(s):
		default:
			v, ok := values[s.GetPart()]
			if !ok {
				return nil, fmt.Errorf("%s can't be set to the %s, which a link to a database doesn't hold", s.GetName(), partWord(s.GetPart()))
			}
			out.Plain[s.GetName()] = v
		}
	}
	out.Link = linkOf(n, w, values)
	return out, nil
}

// partWord is a part as a person says it, such as "host".
func partWord(p catalogv1.Part) string {
	if w, ok := catalogv1.Part_name[int32(p)]; ok && len(w) > len("PART_") {
		return toLowerWord(w[len("PART_"):])
	}
	return "part"
}

// toLowerWord writes an enum's word, such as KEY_PREFIX, as "key prefix".
func toLowerWord(w string) string {
	b := []byte(w)
	for i, c := range b {
		switch {
		case c == '_':
			b[i] = ' '
		case c >= 'A' && c <= 'Z':
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
