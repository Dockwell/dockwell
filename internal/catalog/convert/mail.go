// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// MailDraft is what a store's form says an app sends mail through: the fields it
// declares for a mail server, by the variable each fills, and the app's word for
// each way the server may be secured, by the variable that reads it. It becomes an
// optional need of a mail server (ADR 0099) only where the app's Compose file uses
// each of them as a whole value of a service's environment, which a need sets; a
// set that's short of any stays inputs, as before.
type MailDraft struct {
	// Parts are the variables the form fills, by what each holds: the host, the
	// port, and the user and password together.
	Parts map[string]catalogv1.Part
	// Security are the app's words for starttls, tls and none, by each variable
	// that reads how the server is secured. An app that can't be told one apart
	// from another, as one that upgrades to STARTTLS only where the server offers it,
	// has the same word for both, and isn't offered a connection secured so.
	Security map[string]map[string]string
	// Required says the store marks the fields required: the app can't start
	// without a mail server, so the need isn't optional.
	Required bool
}

// mailDrafts are each store's apps whose form declares a mail server, by the store
// and the app's ID, each read from the app's own documentation of what its fields
// do: a reviewed table, never inferred from a name (ADR 0099). An app whose fields
// leave out how the server is secured stays on inputs, as an app told nothing of TLS
// may send its password before STARTTLS.
var mailDrafts = map[string]map[string]MailDraft{
	"runtipi": {
		// Plausible's Bamboo SMTP adapter takes ssl for TLS from the start, and
		// otherwise STARTTLS where the server offers it: it can't require STARTTLS, so
		// it takes TLS alone.
		"plausible-ce": {
			Parts: map[string]catalogv1.Part{
				"PLAUSIBLE_SMTP_HOST_ADDR": catalogv1.Part_PART_HOST, "PLAUSIBLE_SMTP_HOST_PORT": catalogv1.Part_PART_PORT,
				"PLAUSIBLE_SMTP_USER_NAME": catalogv1.Part_PART_USER, "PLAUSIBLE_SMTP_USER_PWD": catalogv1.Part_PART_PASSWORD,
			},
			Security: map[string]map[string]string{
				"PLAUSIBLE_SMTP_HOST_SSL_ENABLED": {"tls": "true", "starttls": "false", "none": "false"},
			},
		},
	},
	// 1Panel's one app with a mail server's fields, Outline, leaves out how it's
	// secured, and no Dokploy template declares a host, a port and how it's secured
	// together, so neither has a row (stores as of ADR 0099's survey).
}

// mailFor is the mail server the store's form declares for the app, or nil.
func mailFor(store, id string) *MailDraft {
	d, ok := mailDrafts[store][id]
	if !ok {
		return nil
	}
	return &d
}

// applyMail makes an app's mail draft its need of a mail server, where the file uses
// each of its variables, renamed as fields are, as whole values of its services'
// environment, with no default but the form's, which the need's being met replaces.
// Its names are then the need's to set, not inputs. With any short, it's nil, and
// the fields stay inputs.
func applyMail(root *yaml.Node, services []string, app App, renames map[string]string, taken map[string]bool) (*catalogv1.Need, []string) {
	d := app.Mail
	if d == nil {
		return nil, nil
	}
	declared := map[string]bool{}
	for _, f := range app.Fields {
		declared[renamedVar(renames, f.Name)] = true
	}
	parts := map[string]catalogv1.Part{}
	for v, p := range d.Parts {
		parts[renamedVar(renames, v)] = p
	}
	for v := range d.Security {
		parts[renamedVar(renames, v)] = catalogv1.Part_PART_SECURITY
	}
	have := map[catalogv1.Part]bool{}
	for v, p := range parts {
		if !declared[v] {
			return nil, nil
		}
		have[p] = true
	}
	if !have[catalogv1.Part_PART_HOST] || !have[catalogv1.Part_PART_PORT] || !have[catalogv1.Part_PART_SECURITY] ||
		have[catalogv1.Part_PART_USER] != have[catalogv1.Part_PART_PASSWORD] {
		return nil, nil
	}
	// A default the store's form gives in the file, as ${X:-false}, is the app's
	// own while the need is unset, which leaves the name out.
	withoutDefaults(root, services, parts)
	for v := range parts {
		if !wholeInEnvironment(root, services, v) {
			return nil, nil
		}
	}
	n := &catalogv1.Need{Name: "mail", Label: "Mail", Kind: catalogv1.Kind_KIND_SMTP, Optional: !d.Required}
	for slices.Contains(services, n.GetName()) || taken[n.GetName()] {
		n.Name += "-need"
	}
	names := slices.Sorted(maps.Keys(parts))
	for _, v := range names {
		s := &catalogv1.NeedSet{Name: v, Part: parts[v]}
		for from, words := range d.Security {
			if renamedVar(renames, from) != v {
				continue
			}
			for _, value := range []string{"none", "starttls", "tls"} {
				s.Words = append(s.Words, &catalogv1.NeedWord{Value: value, Word: words[value]})
			}
		}
		n.Sets = append(n.Sets, s)
	}
	return n, names
}

// withoutDefaults writes each of a need's variables used as a whole value with a
// default, ${X:-d}, as ${X} alone.
func withoutDefaults(root *yaml.Node, services []string, parts map[string]catalogv1.Part) {
	plain := func(value string) string {
		for v := range parts {
			if strings.HasPrefix(value, "${"+v+":-") && strings.HasSuffix(value, "}") && !strings.Contains(value[len(v)+4:len(value)-1], "$") {
				return "${" + v + "}"
			}
		}
		return value
	}
	for _, svc := range services {
		env := mapGet(serviceNode(root, svc), "environment")
		if env == nil {
			continue
		}
		switch env.Kind {
		case yaml.MappingNode:
			for i := 1; i < len(env.Content); i += 2 {
				env.Content[i].Value = plain(env.Content[i].Value)
			}
		case yaml.SequenceNode:
			for _, item := range env.Content {
				if key, value, ok := strings.Cut(item.Value, "="); ok {
					item.Value = key + "=" + plain(value)
				}
			}
		case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode, yaml.StreamNode:
		}
	}
}

// renamedVar is a variable as renames left it.
func renamedVar(renames map[string]string, v string) string {
	if to, ok := renames[v]; ok {
		return to
	}
	return v
}
