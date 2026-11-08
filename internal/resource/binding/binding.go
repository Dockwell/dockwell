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
	"cmp"
	"context"
	"encoding/json"
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
		if IsBuildUse(u.As) {
			// A build's secret goes to its build alone, never into what runs.
			continue
		}
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
	for id, name := range svc.GetBuild().GetSecrets() {
		if name == "" {
			name = id
		}
		as := BuildPrefix + id
		out = append(out, resource.Use{As: as, Kind: variable.SecretKind, Name: name, Revision: int(svc.GetRevisions()[as])})
	}
	slices.SortFunc(out, func(a, b resource.Use) int { return strings.Compare(a.As, b.As) })
	return out
}

// BuildPrefix starts the name a build's secret is used as, such as
// "build:NPM_TOKEN", which no environment variable's name can be, so a value
// meant for a build never reaches what runs (ADR 0046).
const BuildPrefix = "build:"

// IsBuildUse reports the name of a use that is a build's secret.
func IsBuildUse(as string) bool { return strings.HasPrefix(as, BuildPrefix) }

// BuildSecrets are the values of the secrets a service's build uses, by the ID a
// Dockerfile names each by, from what the call brought. It fails, naming what is
// missing but never a value, when one wasn't sent.
func BuildSecrets(ctx context.Context, svc *resourcev1.Service) (map[string]string, error) {
	ids := svc.GetBuild().GetSecrets()
	if len(ids) == 0 {
		return nil, nil
	}
	values := resource.ValuesFrom(ctx)
	out := make(map[string]string, len(ids))
	for id, name := range ids {
		v, ok := values[BuildPrefix+id]
		if !ok {
			return nil, fmt.Errorf("the control plane sent no value for the build's secret %s, which is the secret %s; plan the change again", id, cmp.Or(name, id))
		}
		out[id] = v
	}
	return out, nil
}

// BuildLabelKey is the container label that records how a service's image was
// built, without its secrets, which LabelKey records by name, so the service reads
// back as its spec.
const BuildLabelKey = "dockwell.build"

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
	if b := svc.GetBuild(); b != nil {
		if app.Labels == nil {
			app.Labels = map[string]string{}
		}
		app.Labels[BuildLabelKey] = buildLabel(b)
	}
	return app
}

// builtFrom is what the BuildLabelKey label holds: a build, with its secrets left
// out. encoding/json sorts a map's keys, so one build always reads the same.
type builtFrom struct {
	URL        string            `json:"url,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Commit     string            `json:"commit,omitempty"`
	Context    string            `json:"context,omitempty"`
	Dockerfile string            `json:"dockerfile,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	Target     string            `json:"target,omitempty"`
	Builder    string            `json:"builder,omitempty"`
	Build      string            `json:"build_command,omitempty"`
	Start      string            `json:"start_command,omitempty"`
}

// builders are what builds an image, as a label names them.
var builders = map[resourcev1.ImageBuild_Builder]string{
	resourcev1.ImageBuild_BUILDER_DOCKERFILE: "dockerfile",
	resourcev1.ImageBuild_BUILDER_RAILPACK:   "railpack",
}

// buildLabel is the BuildLabelKey label for a build.
func buildLabel(b *resourcev1.ImageBuild) string {
	g := b.GetGit()
	out, _ := json.Marshal(builtFrom{
		URL: g.GetUrl(), Ref: g.GetRef(), Commit: g.GetCommit(), Context: b.GetContext(), Dockerfile: b.GetDockerfile(), Args: b.GetArgs(), Target: b.GetTarget(),
		Builder: builders[b.GetBuilder()], Build: b.GetBuildCommand(), Start: b.GetStartCommand(),
	})
	return string(out)
}

// readBuildLabel reads a BuildLabelKey label back into the build it records.
func readBuildLabel(label string) (*resourcev1.ImageBuild, error) {
	var f builtFrom
	if err := json.Unmarshal([]byte(label), &f); err != nil || f.URL == "" {
		return nil, fmt.Errorf("the container can't be read back: its %s label isn't one Dockwell wrote", BuildLabelKey)
	}
	b := &resourcev1.ImageBuild{
		Source:  &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: f.URL, Ref: f.Ref, Commit: f.Commit}},
		Context: f.Context, Dockerfile: f.Dockerfile, Args: f.Args, Target: f.Target,
		BuildCommand: f.Build, StartCommand: f.Start,
	}
	for k, v := range builders {
		if v == f.Builder {
			b.Builder = k
		}
	}
	return b, nil
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
	if label, ok := c.GetLabels()[BuildLabelKey]; ok {
		b, err := readBuildLabel(label)
		if err != nil {
			return nil, err
		}
		svc.Build = b
		delete(c.Labels, BuildLabelKey)
		if len(c.Labels) == 0 {
			c.Labels = nil
		}
	}
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
		if id, ok := strings.CutPrefix(u.As, BuildPrefix); ok {
			// What the build used never reached what runs, so the label alone says it.
			if svc.Build == nil {
				continue
			}
			if svc.Build.Secrets == nil {
				svc.Build.Secrets = map[string]string{}
			}
			svc.Build.Secrets[id] = u.Name
			if u.Revision > 0 {
				if svc.Revisions == nil {
					svc.Revisions = map[string]int64{}
				}
				svc.Revisions[u.As] = int64(u.Revision)
			}
			continue
		}
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
