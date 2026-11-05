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
// The service driver does all of that as it runs a service's container
// (service.Driver.Apply). The container runtime gets the result as any
// container's spec, and environment() there turns it into Docker's.
package binding

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

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

// App is the container a service runs as: its app, with env as its
// environment, and the LabelKey label when it uses anything. The service's
// own spec stays as it is, so the values in env go no further than the
// call that holds them.
func App(svc *resourcev1.Service, env map[string]string) *resourcev1.Container {
	app := proto.CloneOf(svc.GetApp())
	app.Env = env
	if label := Label(svc); label != "" {
		if app.Labels == nil {
			app.Labels = map[string]string{}
		}
		app.Labels[LabelKey] = label
	}
	return app
}

// ParseLabel reads a LabelKey label back into what it records.
func ParseLabel(label string) ([]resource.Use, error) {
	if label == "" {
		return nil, nil
	}
	var uses []resource.Use
	for _, part := range strings.Split(label, ",") {
		as, rest, ok1 := strings.Cut(part, "=")
		kind, rest, ok2 := strings.Cut(rest, ":")
		name, rev, ok3 := strings.Cut(rest, "@")
		n, err := strconv.Atoi(rev)
		if !ok1 || !ok2 || !ok3 || err != nil || as == "" || name == "" {
			return nil, fmt.Errorf("the %s label, %q, isn't one Dockwell wrote", LabelKey, label)
		}
		if k := resource.Kind(kind); k != variable.VariableKind && k != variable.SecretKind {
			return nil, fmt.Errorf("the %s label uses a %q, which isn't a kind a service uses", LabelKey, kind)
		}
		uses = append(uses, resource.Use{As: as, Kind: resource.Kind(kind), Name: name, Revision: n})
	}
	return uses, nil
}

// Use records that svc uses what u names, under the name it sees it as.
func Use(svc *resourcev1.Service, u resource.Use) {
	uses := &svc.Variables
	if u.Kind == variable.SecretKind {
		uses = &svc.Secrets
	}
	if *uses == nil {
		*uses = map[string]string{}
	}
	(*uses)[u.As] = u.Name
}

// Restore reads a service back from its container, as Observe returns it:
// it takes the LabelKey label off, and with it the variables it names from
// the environment, and puts them back as what the service uses. A
// container without the label used nothing. Nor does one use what the
// label names and its environment doesn't have, as when docker compose up
// ran an export without the value in its shell: that reads as drift, as
// the container isn't running with it.
func Restore(c *resourcev1.Container) (*resourcev1.Service, error) {
	svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
	label, ok := c.GetLabels()[LabelKey]
	if !ok {
		return svc, nil
	}
	uses, err := ParseLabel(label)
	if err != nil {
		return nil, fmt.Errorf("the container can't be read back: %w", err)
	}
	delete(c.Labels, LabelKey)
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	for _, u := range uses {
		if _, ok := c.Env[u.As]; !ok {
			continue
		}
		Use(svc, u)
		if u.Revision > 0 {
			if svc.Revisions == nil {
				svc.Revisions = map[string]int64{}
			}
			svc.Revisions[u.As] = int64(u.Revision)
		}
		delete(c.Env, u.As)
	}
	if len(c.Env) == 0 {
		c.Env = nil
	}
	return svc, nil
}
