// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// adopter is a Runtime that can take on what runs on a server already.
type adopter interface {
	ReadAdoption(ctx context.Context, t Target, id string) (resource.Adoption, error)
	Adoption(ctx context.Context, t Target, id string, spec *resourcev1.Container) (resource.Decision, error)
	Adopt(ctx context.Context, t Target, id string, spec *resourcev1.Container, dec resource.Decision) error
}

// ReadAdoptionAt reads a container as what t would run, whose Spec is a Container.
func (d *Driver) ReadAdoptionAt(ctx context.Context, t Target, id string) (resource.Adoption, error) {
	a, ok := d.Runtime.(adopter)
	if !ok {
		return resource.Adoption{}, errors.New("this runtime can't take on a container")
	}
	return a.ReadAdoption(ctx, t, id)
}

// AdoptsAt says what taking a container on as t takes.
func (d *Driver) AdoptsAt(ctx context.Context, t Target, id string, spec *resourcev1.Container) (resource.Decision, error) {
	a, ok := d.Runtime.(adopter)
	if !ok {
		return resource.Decision{}, errors.New("this runtime can't take on a container")
	}
	return a.Adoption(ctx, t, id, spec)
}

// inspectToAdopt reads a container that is to be taken on as t: as it will read
// once it has been, so what it comes to is what Observe reads from then on.
func (d *Docker) inspectToAdopt(ctx context.Context, t Target, id string) (*docker.Container, *resourcev1.Container, error) {
	c, err := d.Client.InspectContainer(ctx, id)
	if docker.IsNotFound(err) {
		return nil, nil, fmt.Errorf("the container %.12s isn't on this server any more, as what made it may have made it again. Plan taking it on again, from what's there now", id)
	}
	if err != nil {
		return nil, nil, err
	}
	view := docker.TakenView(c, docker.Taken{Project: t.Project, Service: t.Service})
	spec, err := d.projected(ctx, t, view)
	if err != nil {
		return nil, nil, err
	}
	Normalize(spec)
	return c, spec, nil
}

// ReadAdoption reads a container as what t would run, and says what would stop it
// being taken on. Nothing is touched.
func (d *Docker) ReadAdoption(ctx context.Context, t Target, id string) (resource.Adoption, error) {
	c, spec, err := d.inspectToAdopt(ctx, t, id)
	if err != nil {
		return resource.Adoption{}, err
	}
	options, err := d.Client.InspectOptions(ctx, c.ID)
	if err != nil {
		return resource.Adoption{}, err
	}
	info, err := d.Client.Info(ctx)
	if err != nil {
		return resource.Adoption{}, fmt.Errorf("can't ask Docker how it runs containers, so what this one needs can't be read: %w", err)
	}
	leaning, err := d.leaningOn(ctx, c)
	if err != nil {
		return resource.Adoption{}, err
	}
	refused := noSpecHolds(c, options, info)
	// What's fine to Docker and no spec can say is refused by what it is, here, rather
	// than by a plan that can't name the container. What's refused above would fail too.
	var invalid string
	if len(refused) == 0 {
		if err := Validate(proto.Clone(spec).(*resourcev1.Container)); err != nil {
			invalid = err.Error()
		}
	}
	return resource.Adoption{
		Spec: spec, Refused: refused, Invalid: invalid, Shows: valuesShown(spec), LeanedOn: leaning,
		Manager: docker.ManagedBy(c.Config.Labels), Folder: c.Config.Labels[docker.LabelWorkingDir],
	}, nil
}

// shownWords are the fields valuesShown has words of its own for.
var shownWords = map[protoreflect.Name]string{"healthcheck": "health check"}

// notSecrets are fields that hold a path, which a value of the environment often is
// by no accident: TZ=Asia/Shanghai beside a mount of its zoneinfo, or a data
// directory beside the volume at it. A plan shows them, and a password isn't one.
var notSecrets = map[protoreflect.Name]bool{"mounts": true, "working_dir": true}

// secretName says a variable's name says what it holds is a secret.
var secretName = regexp.MustCompile(`(?i)PASS|SECRET|TOKEN|KEY|CREDENTIAL`)

// shortest is the fewest characters a value of a variable has to have to count as shown
// when its name says nothing, as anything shorter could be anywhere by chance (ADR 0031).
const shortest = 6

// valuesShown names the variables whose values a spec's other fields hold. A plan
// shows those, where the environment is sealed, and a Compose file that wrote
// ${PASSWORD} into a command or a label leaves its value there on the container. It
// looks through every field but those that can't hold one, so a field added to the
// spec is looked through too. It says which by name, and a value of any length counts
// for a variable whose name says it's a secret: a short password is still one.
func valuesShown(spec *resourcev1.Container) []string {
	out := CredentialLabels(spec)
	eachWord(spec.ProtoReflect(), "", func(where, word string) {
		for name, value := range spec.GetEnv() {
			if value == "" || !strings.Contains(word, value) || len(value) < shortest && !secretName.MatchString(name) {
				continue
			}
			// A label's name can be what holds it, so it can't say where.
			if strings.Contains(where, value) {
				where = "labels"
			}
			out = append(out, "the value of "+name+" in its "+where)
		}
	})
	slices.Sort(out)
	return slices.Compact(out)
}

// CredentialIn starts what Shows says of a label that holds a credential, as a
// caller that has a fix of its own for it, such as turning off a panel's basic auth,
// looks for.
const CredentialIn = "a credential in its label " //nolint:gosec // words that name a label, not a credential

// credentialKey is a label whose value is a credential by its key: a proxy's users
// with their password hashes, as Traefik's basicauth.users and digestauth.users and
// Caddy's basic_auth are, an Authorization header, or a key named for a secret. It
// compiles on first use, as the agent's start stays small.
var credentialKey = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|\.)(basic|digest)auth\.users$|^caddy(_\d+)?(\.[^.]+)*\.basic_?auth(\.|$)|authorization|token|secret|passw(or)?d|api[-_]?key|private[-_]?key|credential`)
})

// credentialValue is a value that's a credential whatever its label is called: a
// password hash as htpasswd and crypt write one, an Authorization header's value, or
// a token in a form its issuer gives every one of.
var credentialValue = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\$(2[abxy]|apr1|1|5|6|y)\$[./A-Za-z0-9$]{20,}|\$argon2(id|i|d)\$|(^|:)\{S?SHA\}|^(?i:bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{16,}$|\b(gh[pousr]_[A-Za-z0-9]{36}|github_pat_\w{22,}|glpat-[\w-]{20,}|xox[abpr]-[\w-]{10,})`)
})

// CredentialLabels names the labels of spec that hold a credential. A plan shows a
// container's labels as they are, so a password hash there, such as a panel's basic
// auth on its proxy, would show to anyone who can read plans (ADR 0089). It says
// which by the label's key and never its value.
func CredentialLabels(spec *resourcev1.Container) []string {
	var out []string
	for k, v := range spec.GetLabels() {
		if v != "" && (credentialKey().MatchString(k) || credentialValue().MatchString(v)) {
			out = append(out, CredentialIn+k)
		}
	}
	slices.Sort(out)
	return out
}

// eachWord calls visit with each string in m, and the field it's in, in words.
func eachWord(m protoreflect.Message, in string, visit func(where, word string)) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if resource.IsSensitive(fd) || notSecrets[fd.Name()] {
			return true
		}
		where := in
		if where == "" {
			where = cmp.Or(shownWords[fd.Name()], strings.ReplaceAll(string(fd.Name()), "_", " "))
		}
		switch {
		case fd.IsMap():
			v.Map().Range(func(k protoreflect.MapKey, e protoreflect.Value) bool {
				visit(where, k.String())
				if fd.MapValue().Kind() == protoreflect.StringKind {
					visit(strings.TrimSuffix(where, "s")+" "+k.String(), e.String())
				}
				return true
			})
		case fd.IsList():
			for i := range v.List().Len() {
				switch e := v.List().Get(i); fd.Kind() {
				case protoreflect.MessageKind:
					eachWord(e.Message(), where, visit)
				case protoreflect.StringKind:
					visit(where, e.String())
				default:
				}
			}
		case fd.Kind() == protoreflect.MessageKind:
			eachWord(v.Message(), where, visit)
		case fd.Kind() == protoreflect.StringKind:
			visit(where, v.String())
		}
		return true
	})
}

// Adoption checks a container can be taken on as t with what was read of it, which a
// person is about to approve, and says it costs nothing.
func (d *Docker) Adoption(ctx context.Context, t Target, id string, spec *resourcev1.Container) (resource.Decision, error) {
	if d.Client.Adopted == nil {
		return resource.Decision{}, errors.New("this agent keeps no record of containers it takes on, so it can't take one on")
	}
	c, got, err := d.inspectToAdopt(ctx, t, id)
	if err != nil {
		return resource.Decision{}, err
	}
	name := strings.TrimPrefix(c.Name, "/")
	if c.Config.Labels[docker.LabelAdopted] != "" {
		// A step that resumes plans again once the agent has done it.
		if c.Config.Labels[docker.LabelProject] == t.Project && c.Config.Labels[docker.LabelService] == t.Service {
			return resource.Decision{Action: resource.ActionCreate, Adopts: c.ID}, nil
		}
		return resource.Decision{}, fmt.Errorf("%s is already what the service %s runs", name, c.Config.Labels[docker.LabelService])
	}
	all, err := d.instances(ctx, t)
	if err != nil {
		return resource.Decision{}, err
	}
	if len(all) > 0 {
		return resource.Decision{}, fmt.Errorf("the service %s already runs on this server, so it can't take on %s as well. Choose another name for it", t.Service, name)
	}
	if changes := resource.Diff(spec, got); len(changes) > 0 {
		return resource.Decision{}, fmt.Errorf("%s has changed since it was read: its %s differs. Take it on again, to plan it from what it is now", name, changes[0].Path)
	}
	shared, err := d.sharedNames(ctx, t, spec, c.ID)
	if err != nil {
		return resource.Decision{}, err
	}
	return resource.Decision{Action: resource.ActionCreate, Adopts: c.ID, SharedNames: shared}, nil
}

// Adopt records a container as what t runs, and where its paths lead, as a label
// would if it could have one, and does nothing else to it. Done already, as a step
// that resumes finds it, it has nothing to do.
func (d *Docker) Adopt(ctx context.Context, t Target, id string, spec *resourcev1.Container, dec resource.Decision) error {
	if _, err := d.Adoption(ctx, t, id, spec); err != nil {
		return err
	}
	c, err := d.Client.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if c.Config.Labels[docker.LabelAdopted] != "" {
		return nil
	}
	leads, err := d.leadsAsPlanned(ctx, t, spec, dec, "it wasn't taken on")
	if err != nil {
		return err
	}
	return d.Client.Adopted.Take(id, docker.Taken{
		Project: t.Project, Service: t.Service, Name: strings.TrimPrefix(c.Name, "/"), Compose: c.Config.Labels[docker.LabelProject],
		Leads: leads,
	})
}

// takenOn is the full ID of what t runs, while that is a container that was taken on
// and is all t runs: a rollout partway has replaced it already. What can't be read
// is an error, and never none: that would read as Dockwell's to remove.
func (d *Docker) takenOn(ctx context.Context, t Target) (string, error) {
	if d.Client.Adopted == nil {
		return "", nil
	}
	all, err := d.instances(ctx, t)
	if err != nil {
		return "", fmt.Errorf("can't read what %s runs, so it can't say whether that was taken on, which goes on running, or is Dockwell's to remove: %w", t.Service, err)
	}
	if len(all) != 1 || all[0].Config.Labels[docker.LabelAdopted] == "" {
		return "", nil
	}
	return all[0].ID, nil
}

// lettingGo says deleting t hands back a container that was taken on, which goes on
// running. Dockwell never made it, so deleting a service is no reason to remove it,
// and an undo of taking it on leaves it as it was.
func (d *Docker) lettingGo(ctx context.Context, t Target) (bool, error) {
	id, err := d.takenOn(ctx, t)
	return id != "", err
}

// elsewhere says what else runs a container that was taken on from a Compose project
// and hasn't been replaced since, for the plan that's about to replace it.
func elsewhere(c *docker.Container) *resource.Elsewhere {
	project := c.Config.Labels[docker.LabelAdoptedFrom]
	if project == "" {
		return nil
	}
	return &resource.Elsewhere{
		Manager: docker.ManagedBy(c.Config.Labels), Container: c.Config.Labels[docker.LabelAdopted],
		Project: project, Folder: c.Config.Labels[docker.LabelWorkingDir],
	}
}
