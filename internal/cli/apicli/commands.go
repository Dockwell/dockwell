// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
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
	servicekind "github.com/dockwell/dockwell/internal/resource/service"
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
		// A kind whose noun a service's methods have already, such as
		// secrets, adds its verbs to that noun.
		cmd, err := kindCommand(env, k, parents[k.Noun], opts[k.Noun])
		if err != nil {
			return nil, err
		}
		if _, ok := parents[k.Noun]; !ok {
			out = append(out, cmd)
		}
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
		Args:    noVerb,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		// A verb two letters off, as "lsit" for list, is named.
		SuggestionsMinimumDistance: 2,
	}
	settingsFlags(cmd, o)
	return cmd
}

// noVerb refuses a command that holds others, run with what isn't one of
// them, as a mistyped verb, as a usage error that points to its help.
func noVerb(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%s has no command %q", cmd.CommandPath(), args[0])
	if near := cmd.SuggestionsFor(args[0]); len(near) > 0 {
		msg += ", though it has " + near[0]
	}
	return usage(fmt.Errorf("%s. Run %q to see its commands", msg, cmd.CommandPath()+" --help"))
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
		name := "<" + FlagName(arg.Path) + ">"
		if arg.OptionalArg {
			name = "[" + name + "]"
		}
		use += " " + name
	}
	long := m.Long
	plans, err := proposes(m)
	if err != nil {
		return nil, err
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
		case arg != nil && arg.OptionalArg && len(args) > 1:
			return usage(fmt.Errorf("%s takes at most one argument, %s. Run %q to see how to use it", cmd.CommandPath(), argument(*arg), cmd.CommandPath()+" --help"))
		case arg != nil && !arg.OptionalArg && len(args) != 1:
			return usage(fmt.Errorf("%s takes one argument, %s. Run %q to see how to use it", cmd.CommandPath(), argument(*arg), cmd.CommandPath()+" --help"))
		case arg == nil && len(args) > 0:
			return usage(fmt.Errorf("%s takes no arguments, but got %q. Run %q to see how to use it", cmd.CommandPath(), args[0], cmd.CommandPath()+" --help"))
		}
		return nil
	}
	if plans || m.Effect == dockwellv1.Effect_EFFECT_APPLY {
		cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "apply without asking")
	}
	if plans {
		cmd.Flags().BoolVar(&o.planOnly, "plan-only", false, "show the plan and leave it planned, without applying it")
	}
	var b *binding
	if m.Call != nil {
		if b, err = bind(cmd.Flags(), env, m.Call.NewRequest().ProtoReflect().Descriptor(), m.Fields); err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if m.Call == nil {
			return fmt.Errorf("the CLI can't call %s yet, as it streams requests. Use the panel", m.Name)
		}
		req := m.Call.NewRequest()
		if err := b.apply(req.ProtoReflect(), args); err != nil {
			return err
		}
		r := &runner{env: env, opts: o, command: cmd.CommandPath(), inline: b.inline()}
		return r.method(cmd.Context(), m, req)
	}
	return cmd, nil
}

// proposers says, of each plan method that returns a change, whether it
// proposes a new one, with a plan to show and apply, as ProposeChange,
// UndoChange, RestartService, PlanInstallUpdates, PlanReboot, PlanSystemServiceAction, RevertDrift, AdoptDrift, RestoreBackup and ImportCompose do. DiscardChange returns the
// change it ended, which is only printed. A plan method that returns a
// change and isn't here fails the commands' building, and so their tests.
var proposers = map[string]bool{
	"dockwell.v1.ChangeService.ProposeChange":            true,
	"dockwell.v1.ChangeService.UndoChange":               true,
	"dockwell.v1.ChangeService.RevertDrift":              true,
	"dockwell.v1.ChangeService.AdoptDrift":               true,
	"dockwell.v1.ChangeService.RestartService":           true,
	"dockwell.v1.ChangeService.PlanInstallUpdates":       true,
	"dockwell.v1.ChangeService.PlanReboot":               true,
	"dockwell.v1.ChangeService.PlanSystemServiceAction":  true,
	"dockwell.v1.ChangeService.DiscardChange":            false,
	"dockwell.v1.ComposeService.ImportCompose":           true,
	"dockwell.v1.ComposeService.AdoptCompose":            true,
	"dockwell.v1.CatalogService.InstallCatalogEntry":     true,
	"dockwell.v1.CatalogService.UpgradeCatalogInstall":   true,
	"dockwell.v1.CatalogService.SetCatalogCompanion":     true,
	"dockwell.v1.CatalogService.SetCatalogNeed":          true,
	"dockwell.v1.CatalogService.OpenCatalogInstall":      true,
	"dockwell.v1.CatalogService.SetCatalogSource":        true,
	"dockwell.v1.CatalogService.RemoveCatalogSource":     true,
	"dockwell.v1.ContainerService.AdoptContainer":        true,
	"dockwell.v1.ServiceService.DeployService":           true,
	"dockwell.v1.ServiceService.RollBackService":         true,
	"dockwell.v1.ServiceService.AddServicePort":          true,
	"dockwell.v1.ServiceService.ChangeServiceRuntime":    true,
	"dockwell.v1.ServiceService.UpgradeService":          true,
	"dockwell.v1.BackupService.RestoreBackup":            true,
	"dockwell.v1.WebsiteService.CreateWebsite":           true,
	"dockwell.v1.WebsiteService.UpdateWebsite":           true,
	"dockwell.v1.WebsiteService.DeleteWebsite":           true,
	"dockwell.v1.ImportService.ImportOnePanel":           true,
	"dockwell.v1.DatabaseService.CreateDatabaseUser":     true,
	"dockwell.v1.DatabaseService.ChangeDatabasePassword": true,
}

// proposes reports a method that proposes a new change, by what it does,
// as proposers says: its command shows the plan and asks before applying
// it, and takes --plan-only.
func proposes(m *Method) (bool, error) {
	if m.Call == nil || m.Effect != dockwellv1.Effect_EFFECT_PLAN || !carriesChange(m.Call.Response()) {
		return false, nil
	}
	p, ok := proposers[m.Name]
	if !ok {
		return false, fmt.Errorf("%s plans and returns a change, but the CLI doesn't know whether it proposes one to apply. "+
			"Say so in proposers, in internal/cli/apicli/commands.go", m.Name)
	}
	return p, nil
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

// holdsOthers says whether a kind is a scope that other resources live in, which
// can be deleted with them (Edit.with_contents): a project or an environment.
func holdsOthers(kind resource.Kind) bool {
	return kind == "project" || kind == "environment"
}

// kindVerbs are the commands for a kind of resource.
var kindVerbs = []struct{ verb, short, long string }{
	{"create", "Create %s", "Proposes a change that creates %s %s, shows its plan and asks before applying it."},
	{"update", "Change %s's spec", "Proposes a change that sets the whole spec of %s %s: a field you leave out is cleared. It shows the plan, which says what changes, and asks before applying it."},
	{"delete", "Delete %s", "Proposes a change that deletes %s %s, shows its plan and asks before applying it."},
}

// kindCommand makes a kind's verbs, under parent with its options, or
// under a noun of its own when parent is nil.
func kindCommand(env *Env, k *Kind, parent *cobra.Command, o *options) (*cobra.Command, error) {
	specField := (&resourcev1.Spec{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(k.Field))
	if specField == nil || specField.Message() == nil {
		return nil, fmt.Errorf("the spec has no kind %s; run make generate", k.Field)
	}
	blank := (&resourcev1.Spec{}).ProtoReflect().NewField(specField).Message().Interface()
	kind, ok := env.Kinds.KindOf(blank)
	if !ok {
		return nil, fmt.Errorf("dockwell doesn't know the kind whose spec is %s, so it can't make its commands. Run make generate", specField.Message().FullName())
	}
	t, err := env.Kinds.Type(kind)
	if err != nil {
		return nil, err
	}
	what := strings.ReplaceAll(string(kind), ".", " ")
	if parent == nil {
		o = &options{}
		parent = &cobra.Command{
			Use:     k.Noun,
			Short:   "Create, update or delete " + what + "s",
			Long:    strings.TrimSpace(k.Short + ".\n\n" + k.Long + "\n\nEach command proposes a change, shows its plan, and asks before applying it."),
			GroupID: GroupID,
			Args:    noVerb,
			RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
			// A verb two letters off, as "lsit" for list, is named.
			SuggestionsMinimumDistance: 2,
		}
		settingsFlags(parent, o)
	}
	unplaced := resource.RunsOnNoServer(t)
	for _, v := range kindVerbs {
		cmd := &cobra.Command{
			Use:   v.verb + " <name>",
			Short: fmt.Sprintf(v.short, withArticle(what)),
			Long:  fmt.Sprintf(v.long, what, "<name>"),
		}
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usage(fmt.Errorf("%s takes one argument, the %s's name. Run %q to see how to use it", cmd.CommandPath(), what, cmd.CommandPath()+" --help"))
			}
			return nil
		}
		where := scopeFlags(cmd, t.Level(), what, unplaced)
		var summary string
		cmd.Flags().StringVar(&summary, "summary", "", "what the change is for, in your words, for the timeline; defaults to what the command does, such as \""+capitalize(v.verb)+" "+what+" web\"")
		cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "apply without asking")
		cmd.Flags().BoolVar(&o.planOnly, "plan-only", false, "show the plan and leave it planned, without applying it")
		var b *binding
		if v.verb != "delete" {
			if b, err = bind(cmd.Flags(), env, specField.Message(), k.Fields); err != nil {
				return nil, fmt.Errorf("%s: %w", k.Noun, err)
			}
		}
		// A project or an environment can go with what lives in it, in one change (ADR 0074).
		var contents bool
		if v.verb == "delete" && holdsOthers(kind) {
			cmd.Flags().BoolVar(&contents, "with-contents", false,
				"delete what lives in it too, in the same change: its apps, routes, volumes, variables and secrets. The plan lists each")
		}
		// An update of a service keeps the values of its own it doesn't set, as some read
		// back hidden and would otherwise be emptied (ADR 0081).
		var removeEnv []string
		keepsEnv := v.verb == "update" && kind == servicekind.Kind
		if keepsEnv {
			cmd.Flags().StringArrayVar(&removeEnv, "remove-env", nil,
				"one of its own environment variables to take out; repeat it for each. Every other one it has and this doesn't set stays as it is")
		}
		verb := v.verb
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			name := args[0]
			edit := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Name: name}}
			if verb == "delete" {
				edit.Resource.Kind, edit.Delete, edit.WithContents = string(kind), true, contents
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
			if keepsEnv {
				if err := r.keepOwnEnv(cmd.Context(), edit, removeEnv); err != nil {
					return err
				}
			}
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
	level                resource.Level
	project, environment string
	// on are the servers a service runs on, and server the one a
	// container or firewall rule is on.
	on                   []string
	server               oneServer
	flagProject, flagEnv bool
	flagOn               bool
}

// oneServer is --on for a kind that lives on one server: given twice, it
// says so, rather than keeping the last.
type oneServer struct{ name, kind string }

func (o *oneServer) String() string { return o.name }

func (o *oneServer) Type() string { return "string" }

func (o *oneServer) Set(s string) error {
	if o.name != "" && o.name != s {
		return fmt.Errorf("a %s is on one server, so --on takes one, but got %s and %s. Make one on each server, with a command for each", o.kind, o.name, s)
	}
	o.name = s
	return nil
}

// scopeFlags adds the flags that say where a resource lives, and where it
// runs. A kind that runs on no server itself, such as a secret, has no
// --on.
func scopeFlags(cmd *cobra.Command, level resource.Level, what string, unplaced bool) *placement {
	p := &placement{level: level, server: oneServer{kind: what}}
	f := cmd.Flags()
	switch level {
	case resource.LevelProject:
		f.StringVar(&p.project, "project", "", "the project it's in, by name or ID; leave it out while there's only one")
		p.flagProject = true
	case resource.LevelEnvironment:
		f.StringVar(&p.project, "project", "", "the project of its environment, by name or ID; leave it out while there's only one")
		f.StringVar(&p.environment, "environment", "", "the environment it's in, by name or ID; leave it out while its project has only one")
		p.flagProject, p.flagEnv = true, true
		if !unplaced {
			f.StringArrayVar(&p.on, "on", nil, "a server it runs on, by ID; repeat it for each. Leave it out to keep it where it runs, or while there's only one server")
			p.flagOn = true
		}
	case resource.LevelServer:
		f.Var(&p.server, "on", "the server it's on, by name or ID; leave it out while there's only one")
		p.flagOn = true
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
		e.Resource.Scope = p.server.name
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
		return fmt.Errorf("the control plane answered with %s %s, not planned, so there's no plan to show or apply. See it with dockwell changes get %s",
			c.GetId(), strings.ToLower(word(c.GetState())), c.GetId())
	}
	return r.planned(ctx, res, c)
}

// withArticle puts a or an before a kind's name, as English says it: "an alert
// rule", "a service".
func withArticle(what string) string {
	if what != "" && strings.ContainsRune("aeiou", rune(what[0])) {
		return "an " + what
	}
	return "a " + what
}
