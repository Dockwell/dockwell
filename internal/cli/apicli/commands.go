// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// GroupID is the help group the API commands are listed under.
const GroupID = "api"

// The environment variables that say which control plane to call, and
// with which API token.
const (
	EnvURL   = "DOCKWELL_URL"
	EnvToken = "DOCKWELL_TOKEN"
)

// Commands returns a command for each noun, with a command under it for
// each of its methods, and a command for each kind of resource with
// create, update and delete under it.
func Commands(env *Env, methods []*Method, nouns []Noun, kinds []*Kind) ([]*cobra.Command, error) {
	env.commands = commandNames(methods)
	var out []*cobra.Command
	parents := map[string]*cobra.Command{}
	opts := map[string]*options{}
	for _, n := range nouns {
		o := &options{}
		cmd := nounCommand(n.Name, n.Short, n.Long, o)
		parents[n.Name], opts[n.Name] = cmd, o
		out = append(out, cmd)
	}
	for _, m := range methods {
		parent, ok := parents[m.Noun]
		if !ok {
			return nil, fmt.Errorf("%s has no noun %s; run make generate", m.Name, m.Noun)
		}
		cmd, err := methodCommand(env, m, opts[m.Noun])
		if err != nil {
			return nil, err
		}
		parent.AddCommand(cmd)
	}
	for _, k := range kinds {
		cmd, err := kindCommand(env, k)
		if err != nil {
			return nil, err
		}
		out = append(out, cmd)
	}
	return out, nil
}

// commandNames replaces the name of each method in a message, such as
// "ListServers", with its command, "dockwell servers list".
func commandNames(methods []*Method) *strings.Replacer {
	sorted := slices.Clone(methods)
	// Longer names first, so none is taken for the start of another.
	slices.SortFunc(sorted, func(a, b *Method) int { return len(b.Name) - len(a.Name) })
	var pairs []string
	for _, m := range sorted {
		name := m.Name[strings.LastIndex(m.Name, ".")+1:]
		pairs = append(pairs, name, "dockwell "+m.Noun+" "+m.Verb)
	}
	return strings.NewReplacer(pairs...)
}

// nounCommand holds a noun's verbs, and the flags they share.
func nounCommand(name, short, long string, o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     name,
		Short:   short,
		Long:    strings.TrimSpace(short + ".\n\n" + long),
		GroupID: GroupID,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	settingsFlags(cmd, o)
	return cmd
}

// settingsFlags are the flags every API command takes.
func settingsFlags(cmd *cobra.Command, o *options) {
	pf := cmd.PersistentFlags()
	pf.StringVar(&o.settings.URL, "url", "",
		"the control plane, such as https://panel.acme.dev; defaults to "+EnvURL+", then the one dockwell login signed in to")
	pf.StringVar(&o.settings.Token, "token", "",
		"the API token; defaults to "+EnvToken+", then the one dockwell login stored. Set it there rather than here, so it stays out of the process list")
	pf.BoolVar(&o.json, "json", false, "print what the API returns as JSON, in the shape REST returns it; one message to a line when there are several")
}

// argument names a command's argument from its field's help: "The
// change's ID." is "the change's ID".
func argument(f Field) string {
	help := strings.TrimSuffix(firstSentence(f.Help), ".")
	if help == "" {
		return "its " + f.Path
	}
	return strings.ToLower(help[:1]) + help[1:]
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

func methodCommand(env *Env, m *Method, o *options) (*cobra.Command, error) {
	use := m.Verb
	var arg *Field
	for i := range m.Fields {
		if m.Fields[i].Arg {
			arg = &m.Fields[i]
		}
	}
	if arg != nil {
		use += " <" + FlagName(arg.Path) + ">"
	}
	long := m.Long
	plans := false
	if m.Call != nil {
		plans = m.Effect == dockwellv1.Effect_EFFECT_PLAN && carriesChange(m.Call.Response())
	}
	switch {
	case plans:
		long += "\n\nIt shows the plan and asks before applying it. --yes applies it without asking, and --plan-only leaves it planned, to apply later or in the panel."
	case m.Effect == dockwellv1.Effect_EFFECT_APPLY:
		long += "\n\nIt asks first. --yes goes ahead without asking."
	case m.Effect == dockwellv1.Effect_EFFECT_DESTRUCTIVE:
		long += "\n\nIt removes data or cuts off access, which no API token may do, so do it in the panel."
	}
	if m.HTTP != "" {
		long += "\n\nAPI: " + m.HTTP
	} else {
		long += "\n\nAPI: " + strings.TrimPrefix(m.Procedure, "/") + ", a stream"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: m.Short,
		Long:  strings.TrimSpace(long),
	}
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		switch {
		case arg != nil && len(args) != 1:
			return usage(fmt.Errorf("%s takes one argument, %s", cmd.CommandPath(), argument(*arg)))
		case arg == nil && len(args) > 0:
			return usage(fmt.Errorf("%s takes no arguments, but got %q. Run %q to see how to use it", cmd.CommandPath(), args[0], cmd.CommandPath()+" --help"))
		}
		return nil
	}
	var b *binding
	if m.Call != nil {
		var err error
		if b, err = bind(cmd.Flags(), env, m.Call.NewRequest().ProtoReflect().Descriptor(), m.Fields); err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	if m.Effect == dockwellv1.Effect_EFFECT_PLAN || m.Effect == dockwellv1.Effect_EFFECT_APPLY {
		cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "apply without asking")
	}
	if plans {
		cmd.Flags().BoolVar(&o.planOnly, "plan-only", false, "show the plan and leave it planned, without applying it")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if m.Call == nil {
			return fmt.Errorf("the CLI can't call %s yet, as it streams requests. Use the panel", m.Name)
		}
		req := m.Call.NewRequest()
		if err := b.apply(req.ProtoReflect(), args); err != nil {
			return err
		}
		r := &runner{env: env, opts: o, command: cmd.CommandPath()}
		return r.method(cmd.Context(), m, req)
	}
	return cmd, nil
}

// carriesChange reports a response with a change in it, as each method
// that proposes one returns.
func carriesChange(md protoreflect.MessageDescriptor) bool {
	fields := md.Fields()
	for i := range fields.Len() {
		if m := fields.Get(i).Message(); m != nil && m.FullName() == "dockwell.v1.Change" {
			return true
		}
	}
	return false
}

// kindVerbs are the commands for a kind of resource.
var kindVerbs = []struct{ verb, short, long string }{
	{"create", "Create a %s", "Proposes a change that creates %s %s, shows its plan and asks before applying it."},
	{"update", "Change a %s's spec", "Proposes a change that sets the whole spec of %s %s: a field you leave out is cleared. It shows the plan, which says what changes, and asks before applying it."},
	{"delete", "Delete a %s", "Proposes a change that deletes %s %s, shows its plan and asks before applying it."},
}

func kindCommand(env *Env, k *Kind) (*cobra.Command, error) {
	specField := (&resourcev1.Spec{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(k.Field))
	if specField == nil || specField.Message() == nil {
		return nil, fmt.Errorf("the spec has no kind %s; run make generate", k.Field)
	}
	blank := (&resourcev1.Spec{}).ProtoReflect().NewField(specField).Message().Interface()
	kind, ok := env.Kinds.KindOf(blank)
	if !ok {
		return nil, fmt.Errorf("dockwell doesn't know the kind whose spec is %s", specField.Message().FullName())
	}
	t, err := env.Kinds.Type(kind)
	if err != nil {
		return nil, err
	}
	what := strings.ReplaceAll(string(kind), ".", " ")
	o := &options{}
	parent := &cobra.Command{
		Use:     k.Noun,
		Short:   "Create, update or delete " + what + "s",
		Long:    strings.TrimSpace(k.Short + ".\n\n" + k.Long + "\n\nEach command proposes a change, shows its plan, and asks before applying it."),
		GroupID: GroupID,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	settingsFlags(parent, o)
	for _, v := range kindVerbs {
		cmd := &cobra.Command{
			Use:   v.verb + " <name>",
			Short: fmt.Sprintf(v.short, what),
			Long:  fmt.Sprintf(v.long, what, "<name>"),
		}
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usage(fmt.Errorf("%s takes one argument, the %s's name", cmd.CommandPath(), what))
			}
			return nil
		}
		var b *binding
		if v.verb != "delete" {
			if b, err = bind(cmd.Flags(), env, specField.Message(), k.Fields); err != nil {
				return nil, fmt.Errorf("%s: %w", k.Noun, err)
			}
		}
		where := scopeFlags(cmd, t.Level())
		var summary string
		cmd.Flags().StringVar(&summary, "summary", "", "what the change is for, in your words, for the timeline; defaults to what the command does, such as \""+capitalize(v.verb)+" "+what+" web\"")
		cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "apply without asking")
		cmd.Flags().BoolVar(&o.planOnly, "plan-only", false, "show the plan and leave it planned, without applying it")
		verb := v.verb
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			name := args[0]
			edit := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Name: name}}
			if verb == "delete" {
				edit.Resource.Kind, edit.Delete = string(kind), true
			} else {
				spec := &resourcev1.Spec{}
				m := spec.ProtoReflect().Mutable(specField).Message()
				if err := b.apply(m, nil); err != nil {
					return err
				}
				edit.Spec = spec
			}
			where.apply(edit)
			if summary == "" {
				summary = capitalize(verb) + " " + what + " " + name
			}
			req := &dockwellv1.ProposeChangeRequest{Summary: summary, Edits: []*dockwellv1.Edit{edit}}
			r := &runner{env: env, opts: o, command: cmd.CommandPath(), choiceFlag: where.flagFor}
			return r.kind(cmd, req, verb, kind, name, k.Noun)
		}
		parent.AddCommand(cmd)
	}
	return parent, nil
}

// placement holds the flags that say where a resource lives, and where it
// runs, by its kind's level. Each is left out while there is one to
// choose from (PRD principle 4): the API says when there is a choice.
type placement struct {
	level                 resource.Level
	project, environment  string
	on                    []string
	flagProject, flagEnv  bool
	flagOn, onScopesToRef bool
}

func scopeFlags(cmd *cobra.Command, level resource.Level) *placement {
	p := &placement{level: level}
	f := cmd.Flags()
	switch level {
	case resource.LevelProject:
		f.StringVar(&p.project, "project", "", "the project it's in, by name or ID; leave it out while there's only one")
		p.flagProject = true
	case resource.LevelEnvironment:
		f.StringVar(&p.project, "project", "", "the project of its environment, by name or ID; leave it out while there's only one")
		f.StringVar(&p.environment, "environment", "", "the environment it's in, by name or ID; leave it out while its project has only one")
		f.StringArrayVar(&p.on, "on", nil, "a server it runs on, by ID; repeat it for each. Leave it out to keep it where it runs, or while there's only one server")
		p.flagProject, p.flagEnv, p.flagOn = true, true, true
	case resource.LevelServer:
		f.StringArrayVar(&p.on, "on", nil, "the server it's on, by name or ID; leave it out while there's only one")
		p.flagOn, p.onScopesToRef = true, true
	case resource.LevelOrganization:
	}
	return p
}

func (p *placement) apply(e *dockwellv1.Edit) {
	switch p.level {
	case resource.LevelProject:
		e.Resource.Scope = p.project
	case resource.LevelEnvironment:
		e.Project, e.Resource.Scope, e.Servers = p.project, p.environment, p.on
	case resource.LevelServer:
		if len(p.on) > 0 {
			e.Resource.Scope = p.on[len(p.on)-1]
		}
	case resource.LevelOrganization:
	}
}

// flagFor names the flag that takes a choice.
func (p *placement) flagFor(c *dockwellv1.Choice) string {
	switch {
	case c.GetLevel() == "project" && p.flagProject:
		return "--project"
	case c.GetLevel() == "environment" && p.flagEnv:
		return "--environment"
	case c.GetLevel() == "server" && p.flagOn:
		return "--on"
	}
	return ""
}

// kind proposes a change to one resource, checks that its plan does what
// the verb says, and goes on as for any plan.
func (r *runner) kind(cmd *cobra.Command, req *dockwellv1.ProposeChangeRequest, verb string, kind resource.Kind, name, noun string) error {
	ctx := cmd.Context()
	if err := r.connect(ctx); err != nil {
		return err
	}
	var res *dockwellv1.ProposeChangeResponse
	err := r.withChoices(ctx, req, func() error {
		out, err := r.client.Changes().ProposeChange(ctx, connect.NewRequest(proto.Clone(req).(*dockwellv1.ProposeChangeRequest)))
		if err == nil {
			res = out.Msg
		}
		return err
	})
	if err != nil {
		return r.explain(err)
	}
	c := res.GetChange()
	what := strings.ReplaceAll(string(kind), ".", " ")
	for _, pr := range c.GetPlan().GetResources() {
		if pr.GetResource().GetName() != name || pr.GetResource().GetKind() != string(kind) {
			continue
		}
		action := pr.GetAction()
		switch {
		case verb == "create" && action != dockwellv1.PlanAction_PLAN_ACTION_CREATE:
			r.discard(ctx, c.GetId())
			return usage(fmt.Errorf("%s %s already exists. Change it with dockwell %s update %s", what, name, noun, name))
		case verb == "update" && action == dockwellv1.PlanAction_PLAN_ACTION_CREATE:
			r.discard(ctx, c.GetId())
			return usage(fmt.Errorf("there's no %s %s to update. Create it with dockwell %s create %s", what, name, noun, name))
		}
	}
	if c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		return errors.New("the control plane returned a change that isn't planned")
	}
	return r.planned(ctx, res, c)
}
