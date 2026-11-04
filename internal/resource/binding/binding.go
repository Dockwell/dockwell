// SPDX-License-Identifier: AGPL-3.0-only

// Package binding is how a service gets the values of the variables and
// secrets it uses, on the server it runs on. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
//
// The control plane seals the values for one step to the agent stream's
// delivery key (secrets.SealValues). The agent opens them in memory
// (delivery.Key.Open) and hands them to the driver for that call alone
// (resource.WithValues). The driver builds the container's environment
// from them (Environment) and records on the container which variables and
// secrets it used, never their values (Label), so it can read the service
// back as its spec (Restore).
//
// The Docker driver (#26) calls Environment when it creates a service's
// container, and gets everything else from the service driver here.
package binding

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// Environment returns the environment a service's container runs with:
// its app's own variables, then each variable and secret it uses, under
// the name it sees it as, with the values resource.WithValues gave the
// call. It fails, naming what is missing but never a value, when a value
// wasn't sent.
func Environment(ctx context.Context, svc *resourcev1.Service) (map[string]string, error) {
	env := maps.Clone(svc.GetApp().GetEnv())
	uses := Uses(svc)
	if len(uses) == 0 {
		return env, nil
	}
	if env == nil {
		env = map[string]string{}
	}
	values := resource.ValuesFrom(ctx)
	for _, u := range uses {
		v, ok := values[u.As]
		if !ok {
			return nil, fmt.Errorf("the control plane sent no value for %s, which is %s %s; plan the change again", u.As, u.Kind, u.Name)
		}
		env[u.As] = v
	}
	return env, nil
}

// Uses lists what a service uses, by the name it sees each as, sorted.
func Uses(svc *resourcev1.Service) []resource.Use {
	var out []resource.Use
	add := func(kind resource.Kind, m map[string]string) {
		for as, name := range m {
			if name == "" {
				name = as
			}
			out = append(out, resource.Use{As: as, Kind: kind, Name: name, Revision: int(svc.GetRevisions()[as])})
		}
	}
	add(variable.VariableKind, svc.GetVariables())
	add(variable.SecretKind, svc.GetSecrets())
	slices.SortFunc(out, func(a, b resource.Use) int { return strings.Compare(a.As, b.As) })
	return out
}

// LabelKey is the container label that records what a service uses: each
// name it sees, the kind and name of what it uses, and the revision, such
// as "DATABASE_URL=secret:DATABASE_URL@4,LOG_LEVEL=variable:LOG_LEVEL@2".
// Never a value.
const LabelKey = "dockwell.uses"

// Label is the LabelKey label for a service, or empty when it uses
// nothing.
func Label(svc *resourcev1.Service) string {
	uses := Uses(svc)
	parts := make([]string, len(uses))
	for i, u := range uses {
		parts[i] = fmt.Sprintf("%s=%s:%s@%d", u.As, u.Kind, u.Name, u.Revision)
	}
	return strings.Join(parts, ",")
}

// Restore reads a service back from its container, as Observe returns it:
// it takes the LabelKey label off, and with it the variables it names from
// the environment, and puts them back as what the service uses. A
// container without the label used nothing.
func Restore(c *resourcev1.Container) (*resourcev1.Service, error) {
	svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
	label, ok := c.GetLabels()[LabelKey]
	if !ok {
		return svc, nil
	}
	delete(c.Labels, LabelKey)
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	if label == "" {
		return svc, nil
	}
	for _, part := range strings.Split(label, ",") {
		as, rest, ok1 := strings.Cut(part, "=")
		kind, rest, ok2 := strings.Cut(rest, ":")
		name, rev, ok3 := strings.Cut(rest, "@")
		n, err := strconv.Atoi(rev)
		if !ok1 || !ok2 || !ok3 || err != nil || as == "" || name == "" {
			return nil, fmt.Errorf("the container's %s label, %q, isn't one Dockwell wrote", LabelKey, label)
		}
		switch resource.Kind(kind) {
		case variable.VariableKind:
			if svc.Variables == nil {
				svc.Variables = map[string]string{}
			}
			svc.Variables[as] = name
		case variable.SecretKind:
			if svc.Secrets == nil {
				svc.Secrets = map[string]string{}
			}
			svc.Secrets[as] = name
		default:
			return nil, fmt.Errorf("the container's %s label uses a %q, which isn't a kind a service uses", LabelKey, kind)
		}
		if n > 0 {
			if svc.Revisions == nil {
				svc.Revisions = map[string]int64{}
			}
			svc.Revisions[as] = int64(n)
		}
		delete(c.Env, as)
	}
	if len(c.Env) == 0 {
		c.Env = nil
	}
	return svc, nil
}
