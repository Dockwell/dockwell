// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
)

// options are the flags every API command shares.
type options struct {
	settings Settings
	json     bool
	// yes applies without asking, and planOnly proposes without applying.
	yes, planOnly bool
}

// runner runs one API command.
type runner struct {
	env     *Env
	opts    *options
	client  *Client
	command string // such as "dockwell projects create"
	// inline are the keys the command line gave a value, for a method to refuse
	// what's a secret.
	inline []string
	// choiceFlag names the flag that takes a choice, for a command whose
	// request isn't built from its flags field by field.
	choiceFlag func(*dockwellv1.Choice) string
	// logs prints log lines, from one message to the next.
	logs *logPrinter
}

func (r *runner) connect(ctx context.Context) error {
	c, err := r.env.Connect(ctx, r.opts.settings)
	if err != nil {
		return err
	}
	r.client = c
	return nil
}

// do makes one call, and with local access makes it again once: with a
// new token when the control plane rejected the one it had, or at the
// control plane's new address when it moved, as after a restart on another
// port.
func (r *runner) do(ctx context.Context, call func() error) error {
	err := call()
	if err == nil || !r.client.Local {
		return err
	}
	again := r.opts.settings
	switch {
	case Reason(err) == authapi.ReasonTokenRejected:
		again.Renew = true
	case Reason(err) == authapi.ReasonTokenLogs:
		// One made before local access's tokens could read logs needs replacing
		// too, though nothing else is wrong with it.
		again.Renew, again.Regrant = true, true
	case unreachable(err):
		again.Moved = true
	default:
		return err
	}
	c, rerr := r.env.Connect(ctx, again)
	if rerr != nil {
		return rerr
	}
	if again.Moved && c.URL == r.client.URL {
		return err // it didn't move: it isn't running
	}
	r.client = c
	return call()
}

// withChoices makes a call, and when the API says it left out something
// there is a choice of, asks which on a terminal, sets it in req, and calls
// again. Off a terminal it fails with the options and the flag to pass.
func (r *runner) withChoices(ctx context.Context, req proto.Message, call func() error) error {
	for range 5 {
		err := r.do(ctx, call)
		choice := ChoiceOf(err)
		if choice == nil {
			return err
		}
		if !r.env.interactive() || choice.GetField() == "" {
			return r.choiceError(err, choice)
		}
		picked, err := r.env.choose(choiceQuestion(choice), choice.GetOptions())
		if err != nil {
			return err
		}
		if err := setPath(req.ProtoReflect(), choice.GetField(), picked); err != nil {
			return err
		}
	}
	return errors.New("the API kept asking which one, so dockwell stopped. Pass each choice as a flag")
}

func choiceQuestion(c *dockwellv1.Choice) string {
	switch c.GetLevel() {
	case "environment":
		return fmt.Sprintf("Project %s has %d environments. Which one?", c.GetProject(), len(c.GetOptions()))
	case "server":
		return fmt.Sprintf("There are %d servers. Which one?", len(c.GetOptions()))
	}
	return fmt.Sprintf("There are %d %ss. Which one?", len(c.GetOptions()), c.GetLevel())
}

func (r *runner) choiceError(err error, c *dockwellv1.Choice) error {
	var cerr *connect.Error
	msg := err.Error()
	if errors.As(err, &cerr) {
		msg = cerr.Message()
	}
	flag := ""
	if r.choiceFlag != nil {
		flag = r.choiceFlag(c)
	}
	switch {
	case flag != "":
		return usage(fmt.Errorf("%s. Pass %s with one of them", msg, flag))
	case c.GetField() != "":
		return usage(fmt.Errorf("%s. Set %s in the request to one of them", msg, c.GetField()))
	}
	return usage(errors.New(msg))
}

// setPath sets a string field at a path of field names from msg, such as
// "edits[0].resource.scope".
func setPath(msg protoreflect.Message, path, value string) error {
	parts := strings.Split(path, ".")
	m := msg
	for i, part := range parts {
		name, index := part, -1
		if open := strings.IndexByte(part, '['); open >= 0 && strings.HasSuffix(part, "]") {
			n, err := strconv.Atoi(part[open+1 : len(part)-1])
			if err != nil {
				return fmt.Errorf("the API asked which one at %s, which this CLI can't follow, so it can't set your answer. Pass it as a flag instead", path)
			}
			name, index = part[:open], n
		}
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return fmt.Errorf("the API asked which one at %s, which the request doesn't have, so this CLI may be older than the control plane. "+
				"Update it to the control plane's version", path)
		}
		last := i == len(parts)-1
		switch {
		case index >= 0:
			l := m.Mutable(fd).List()
			if index >= l.Len() || fd.Message() == nil {
				return fmt.Errorf("the API asked which one at %s, which the request doesn't have, so this CLI may be older than the control plane. "+
					"Update it to the control plane's version", path)
			}
			m = l.Get(index).Message()
		case last && fd.Kind() == protoreflect.StringKind && fd.IsList():
			m.Mutable(fd).List().Append(protoreflect.ValueOfString(value))
		case last && fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(value))
		case fd.Message() != nil && !last:
			m = m.Mutable(fd).Message()
		default:
			return fmt.Errorf("the API asked which one at %s, which this CLI can't set, so it can't send your answer. Pass it as a flag instead", path)
		}
	}
	return nil
}

// print writes one response: for people, or as JSON. lines writes JSON one
// message to a line, for commands that print several.
func (r *runner) print(msg proto.Message, lines bool) error {
	if r.opts.json {
		return JSON(r.env.Out, msg, !lines)
	}
	switch m := msg.(type) {
	case *dockwellv1.WatchLogsResponse:
		r.logPrinter().watch(m)
		return nil
	case *dockwellv1.ReadLogsResponse:
		r.logPrinter().read(m)
		return nil
	}
	Render(r.env.Out, msg, r.env.now())
	return nil
}

// logPrinter prints log lines, remembering their sources from one message to
// the next.
func (r *runner) logPrinter() *logPrinter {
	if r.logs == nil {
		r.logs = newLogPrinter(r.env.Out, r.env.Err)
	}
	return r.logs
}

// method runs a generated command: it calls the method with req, and acts
// on the method's effect. Reads print what comes back. Plans that propose a
// change show it and ask before applying it. Applies ask first, and
// applying a planned change shows its plan. Destructive methods need a
// person, in the panel.
func (r *runner) method(ctx context.Context, m *Method, req proto.Message) error {
	if m.Call == nil {
		return fmt.Errorf("the CLI can't call %s yet, as it streams requests. Use the panel", m.Name)
	}
	if m.Effect == dockwellv1.Effect_EFFECT_DESTRUCTIVE {
		// No API token may make a destructive call (ADR 0016), and the CLI
		// signs in only with one, so it refuses before it has one: local
		// access would make one only to say so.
		return r.needsPanel(ctx)
	}
	if err := r.connect(ctx); err != nil {
		return err
	}
	if err := r.refuseInlineSecret(ctx, m); err != nil {
		return err
	}
	if apply, ok := req.(*dockwellv1.ApplyChangeRequest); ok {
		return r.applyPlanned(ctx, apply)
	}
	if m.Effect == dockwellv1.Effect_EFFECT_APPLY && !r.opts.yes {
		if err := r.ask(m.Short + "?"); err != nil {
			return err
		}
	}
	if m.Call.Streams() {
		return r.stream(ctx, m, req)
	}
	var res proto.Message
	err := r.withChoices(ctx, req, func() error {
		return m.Call.Invoke(ctx, r.client, req, func(msg proto.Message) error { res = msg; return nil })
	})
	if err != nil {
		return r.explain(err)
	}
	if c := changeIn(res); c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		if plans, _ := proposes(m); plans {
			return r.planned(ctx, res, c)
		}
	}
	return r.print(res, false)
}

// refuseInlineSecret refuses a catalog input that's a secret, given a value on the
// command line, where it would stay in a shell's history and the process list. Which
// inputs are secrets is each entry's to say, so it asks. A name that's a secret in any
// entry counts, as an upgrade names a project and not an entry, and the name alone
// always works. Nothing of the value leaves here.
func (r *runner) refuseInlineSecret(ctx context.Context, m *Method) error {
	if len(r.inline) == 0 || !strings.HasPrefix(m.Name, "dockwell.v1.CatalogService.") {
		return nil
	}
	res, err := r.client.Catalog().ListCatalogEntries(ctx, connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
	if err != nil {
		return r.explain(err)
	}
	for _, e := range res.Msg.GetEntries() {
		for _, in := range e.GetInputs() {
			if in.GetType() == catalogv1.InputType_INPUT_TYPE_SECRET && slices.Contains(r.inline, in.GetName()) {
				return fmt.Errorf("--values %[1]s gives a secret, so it would stay in your shell's history and the process list. Give --values %[1]s alone to take its value from %[1]s in your environment", in.GetName())
			}
		}
	}
	return nil
}

// needsPanel refuses a destructive method, and says where the panel is when
// that is known without a token.
func (r *runner) needsPanel(ctx context.Context) error {
	where := "Sign in to the panel and do it there"
	located := r.opts.settings
	located.Locate = true
	if c, err := r.env.Connect(ctx, located); err == nil && c.URL != "" {
		where = "Sign in to the panel at " + c.PanelURL("/") + " and do it there"
	}
	return ExitError{ExitNeedsPerson, fmt.Errorf("%s removes data or cuts off access, which no API token may do. %s", r.command, where)}
}

// ask asks before a call that changes something, or fails when there is
// nobody to ask.
func (r *runner) ask(question string) error {
	if !r.env.interactive() {
		return usage(fmt.Errorf("%s changes things, and there's no terminal to ask on. Pass --yes to go ahead without asking", r.command))
	}
	ok, err := r.env.confirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("you didn't say yes, so %s didn't run. Nothing changed", r.command)
	}
	return nil
}

// stream prints each message of a stream as it comes. A change is followed
// until it ends, as after applying it.
func (r *runner) stream(ctx context.Context, m *Method, req proto.Message) error {
	if watch, ok := req.(*dockwellv1.WatchChangeRequest); ok {
		return r.follow(ctx, watch.GetId(), time.Time{})
	}
	if watch, ok := req.(*dockwellv1.WatchLogsRequest); ok {
		defer func() {
			if r.logs != nil {
				r.logs.finish()
			}
		}()
		if !r.opts.json {
			r.logPrinter()
		}
		return r.followLogs(ctx, m, watch)
	}
	err := r.do(ctx, func() error {
		return m.Call.Invoke(ctx, r.client, req, func(msg proto.Message) error { return r.print(msg, true) })
	})
	if err != nil {
		return r.explain(err)
	}
	return nil
}

// followLogs prints a stream of logs, and carries on from where it was when the
// stream drops, as when the control plane restarts, or gave up on a reader that
// had stopped reading, such as a pager left open. Only what can't get better ends
// it, or a run of failures with nothing read in between.
func (r *runner) followLogs(ctx context.Context, m *Method, req *dockwellv1.WatchLogsRequest) error {
	const tries = 5
	failed := 0
	for {
		progress := false
		err := r.do(ctx, func() error {
			return m.Call.Invoke(ctx, r.client, req, func(msg proto.Message) error {
				if res, ok := msg.(*dockwellv1.WatchLogsResponse); ok && res.GetResumeToken() != "" {
					req.ResumeToken = res.GetResumeToken()
					progress = true
				}
				return r.print(msg, true)
			})
		})
		if err == nil {
			return nil
		}
		if progress {
			failed = 0
		}
		failed++
		if ctx.Err() != nil || failed >= tries || !droppedStream(err) {
			return r.explain(err)
		}
		fmt.Fprintln(r.env.Err, "The stream of logs dropped. Dockwell carries on from where it was, with no line twice.")
		select {
		case <-time.After(time.Duration(failed) * time.Second):
		case <-ctx.Done():
			return r.explain(err)
		}
	}
}

// droppedStream reports an error that says the connection went, as opposed to
// one about what was asked for.
func droppedStream(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeInternal, connect.CodeUnknown, connect.CodeAborted, connect.CodeDeadlineExceeded:
		return true
	default:
		return false
	}
}

// planned shows a planned change and, once the person says so, applies it
// and follows it to an end. res is the response that carried it.
func (r *runner) planned(ctx context.Context, res proto.Message, c *dockwellv1.Change) error {
	applies := r.applies(c)
	// Before it prints, so what it prints says what became of it: a script reading
	// --json would otherwise take a change that's gone for one left planned.
	nothing := empty(c.GetPlan())
	if nothing && r.discard(ctx, c.GetId()) {
		c.State = dockwellv1.ChangeState_CHANGE_STATE_DISCARDED
	}
	if err := r.print(res, applies); err != nil {
		return err
	}
	if nothing {
		fmt.Fprintln(r.env.Err, "Nothing to change: everything is already as you asked.")
		return nil
	}
	if r.opts.planOnly {
		fmt.Fprintf(r.env.Err, "\n%s is planned. Apply it with dockwell changes apply %s, or discard it with dockwell changes discard %s.\n",
			c.GetId(), c.GetId(), c.GetId())
		return nil
	}
	return r.confirmAndApply(ctx, c, "")
}

// applies reports whether the CLI goes on to apply a planned change and
// follow it, printing more than the plan: then --json prints each message
// on a line of its own, and otherwise the plan alone, indented.
func (r *runner) applies(c *dockwellv1.Change) bool {
	return !r.opts.planOnly && !empty(c.GetPlan()) && !needsPerson(c.GetPlan()) && (r.opts.yes || r.env.interactive())
}

// empty reports a plan that changes nothing.
func empty(p *dockwellv1.Plan) bool {
	s := p.GetSummary()
	if s.GetCreate()+s.GetChange()+s.GetReplace()+s.GetDestroy() > 0 {
		return false
	}
	for _, sp := range p.GetServers() {
		if len(sp.GetSteps()) > 0 {
			return false
		}
	}
	return true
}

// needsPerson reports a plan that only a signed-in person may apply: one
// that is destructive, or gives a container more of its server.
func needsPerson(p *dockwellv1.Plan) bool { return p.GetDestructive() || p.GetHostAccess() }

// needsPerson stops at a plan no API token may apply, and says where a
// person can. The CLI signs in with an API token, even after dockwell
// login, so that is the panel.
func (r *runner) needsPerson(c *dockwellv1.Change) error {
	return ExitError{ExitNeedsPerson, fmt.Errorf("%s %s, so it needs a person to approve it, which no API token can, and the CLI uses one even after dockwell login. "+
		"It stays planned: sign in to the panel and apply it at %s", c.GetId(), whyPerson(c.GetPlan()), r.client.PanelURL("/changes/"+c.GetId()))}
}

// whyPerson says what about a plan needs a person, from what it does.
func whyPerson(p *dockwellv1.Plan) string {
	var why []string
	if p.GetDestructive() {
		why = append(why, "destroys data, may cut off access, or lets two containers write to one place")
	}
	add := func(w string) {
		if !slices.Contains(why, w) {
			why = append(why, w)
		}
	}
	for _, r := range p.GetResources() {
		for _, a := range r.GetHostAccess() {
			switch a.GetKind() {
			case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN:
				add("takes a domain another environment held")
			case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PANEL_DOMAIN:
				add("moves the panel's domain behind " + a.GetDetail())
			case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PORT:
				add("sends a route to " + a.GetPath() + " on the server itself, past its containers")
			case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TRUSTED_PROXY:
				add("has a route believe " + a.GetPath() + " on where a request came from")
			case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PLAIN_FOR_ANYONE:
				add("has a route answer anyone over plain HTTP on " + a.GetPath())
			case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HTTPS_OFF:
				add("takes " + a.GetPath() + " off HTTPS")
			default:
				add("gives a container more access to its server")
			}
		}
	}
	if len(why) == 0 {
		return "gives a container more access to its server"
	}
	return human.List(why)
}

// confirmAndApply asks before applying a planned change, then applies the
// plan shown, by its fingerprint, and follows it.
func (r *runner) confirmAndApply(ctx context.Context, c *dockwellv1.Change, fingerprint string) error {
	if needsPerson(c.GetPlan()) {
		return r.needsPerson(c)
	}
	if !r.opts.yes {
		if !r.env.interactive() {
			return usage(fmt.Errorf("%s is planned, but there's no terminal to ask whether to apply it, so it wasn't. "+
				"Pass --yes to apply without asking, or apply it with dockwell changes apply %s --yes", c.GetId(), c.GetId()))
		}
		fmt.Fprintln(r.env.Err)
		ok, err := r.env.confirm("Apply this plan?")
		if err != nil {
			return err
		}
		if !ok {
			r.discard(ctx, c.GetId())
			return fmt.Errorf("you didn't apply %s, so Dockwell discarded it. Nothing changed", c.GetId())
		}
	}
	if fingerprint == "" {
		fingerprint = c.GetPlan().GetFingerprint()
	}
	began := r.env.now()
	var applied *dockwellv1.ApplyChangeResponse
	err := r.do(ctx, func() error {
		res, err := r.client.Changes().ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: c.GetId(), Fingerprint: fingerprint}))
		if err == nil {
			applied = res.Msg
		}
		return err
	})
	if Reason(err) == authapi.ReasonNeedsSession {
		return r.needsPerson(c)
	}
	if err != nil {
		return r.explain(err)
	}
	if r.opts.json {
		if err := JSON(r.env.Out, applied, false); err != nil {
			return err
		}
	}
	return r.follow(ctx, c.GetId(), began)
}

// discard drops a planned change nobody will apply, as best it can, and says
// whether it did.
func (r *runner) discard(ctx context.Context, id string) bool {
	_, err := r.client.Changes().DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: id}))
	return err == nil
}

// applyPlanned is dockwell changes apply: it shows the change's plan, asks,
// and applies that plan.
func (r *runner) applyPlanned(ctx context.Context, req *dockwellv1.ApplyChangeRequest) error {
	var got *dockwellv1.GetChangeResponse
	err := r.do(ctx, func() error {
		res, err := r.client.Changes().GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: req.GetId()}))
		if err == nil {
			got = res.Msg
		}
		return err
	})
	if err != nil {
		return r.explain(err)
	}
	c := got.GetChange()
	if c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		return fmt.Errorf("%s is %s, not planned, so there's nothing to apply. Propose it again for a new plan",
			c.GetId(), strings.ToLower(word(c.GetState())))
	}
	if err := r.print(got, r.applies(c)); err != nil {
		return err
	}
	if fp := req.GetFingerprint(); fp != "" && fp != c.GetPlan().GetFingerprint() {
		return fmt.Errorf("%s's plan has fingerprint %s, not %s, so it isn't the plan you reviewed. Review it with dockwell changes get %s, then apply it",
			c.GetId(), c.GetPlan().GetFingerprint(), fp, c.GetId())
	}
	return r.confirmAndApply(ctx, c, req.GetFingerprint())
}

// reasonWatchBehind ends a watch of a change that fell behind it, and asks
// the client to watch again.
const reasonWatchBehind = "watch_behind"

// maxRewatches is how many times follow watches a change again after a
// watch fell behind it, before it gives up.
const maxRewatches = 10

// follow watches a change apply, printing each step as it moves on, until
// it is applied, failed or discarded. began is when this CLI applied it,
// on its own clock, or zero when it only watches.
func (r *runner) follow(ctx context.Context, id string, began time.Time) error {
	last, err := r.watchTo(ctx, id, false)
	if err != nil {
		return err
	}
	if last.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		last = r.followRollback(ctx, last)
	}
	return r.outcome(last, began)
}

// rollbackFollow is how long a change that failed waits for its rollback to
// end, so what it says last is whether what ran is back. A rollback waiting
// for a server can take far longer, and says so.
const rollbackFollow = 2 * time.Minute

// followRollback follows the rollback of a change that failed, while it
// applies, so the person who deployed reads how it ended where they are
// looking: that what ran never stopped, say, rather than only that the deploy
// failed. It returns the change read again, with how the rollback went, or as
// it was if that can't be told within rollbackFollow.
func (r *runner) followRollback(ctx context.Context, c *dockwellv1.Change) *dockwellv1.Change {
	id := c.GetRolledBackBy()
	if id == "" || c.GetRollbackStatus().GetState() != dockwellv1.RollbackState_ROLLBACK_STATE_APPLYING {
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, rollbackFollow)
	defer cancel()
	if !r.opts.json {
		fmt.Fprintf(r.env.Out, "\nRolling back in %s\n", id)
	}
	if _, err := r.watchTo(ctx, id, true); err != nil {
		return c
	}
	var got *dockwellv1.GetChangeResponse
	err := r.do(ctx, func() error {
		res, err := r.client.Changes().GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: c.GetId()}))
		if err == nil {
			got = res.Msg
		}
		return err
	})
	if err != nil {
		return c
	}
	return got.GetChange()
}

// watchTo watches a change until it is applied, failed or discarded, printing
// each step as it moves on, and returns it as it ended. A rollback, quiet, is
// one that follow follows after the change it rolls back: its steps show, and
// nothing else, as the JSON is the change's own.
func (r *runner) watchTo(ctx context.Context, id string, rollback bool) (*dockwellv1.Change, error) {
	shown := map[int32]dockwellv1.StepState{}
	var last *dockwellv1.Change
	announced := rollback
	if !r.opts.json && !rollback {
		fmt.Fprintln(r.env.Out)
	}
	step := func(i int32, s *dockwellv1.StepProgress) {
		if r.opts.json || shown[i] == s.GetState() || s.GetState() == dockwellv1.StepState_STEP_STATE_QUEUED {
			return
		}
		shown[i] = s.GetState()
		line := fmt.Sprintf("  %-8s %s", word(s.GetState()), stepText(s))
		if finalStep(s.GetState()) && s.GetStartTime() != nil && s.GetFinishTime() != nil {
			line += " (" + Elapsed(s.GetFinishTime().AsTime().Sub(s.GetStartTime().AsTime())) + ")"
		}
		fmt.Fprintln(r.env.Out, line)
	}
	watch := func() error {
		stream, err := r.client.Changes().WatchChange(ctx, connect.NewRequest(&dockwellv1.WatchChangeRequest{Id: id}))
		if err != nil {
			return err
		}
		defer stream.Close()
		for stream.Receive() {
			msg := stream.Msg()
			if r.opts.json && !rollback {
				if err := JSON(r.env.Out, msg, false); err != nil {
					return err
				}
			}
			switch e := msg.GetEvent().(type) {
			case *dockwellv1.WatchChangeResponse_Change:
				last = e.Change
				if !announced && !r.opts.json && last.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLYING {
					fmt.Fprintf(r.env.Out, "Applying %s\n", id)
					announced = true
				}
				for i, s := range last.GetSteps() {
					step(int32(i), s)
				}
			case *dockwellv1.WatchChangeResponse_Step:
				step(e.Step.GetIndex(), e.Step.GetStep())
			}
		}
		return stream.Err()
	}
	var err error
	// A watch that fell behind the change is ended by the control plane,
	// which asks for another: the change goes on, and so does following it.
	// Each new watch starts with the change as it is, so nothing is missed,
	// and steps already shown aren't shown again.
	for watches := 1; ; watches++ {
		err = r.do(ctx, watch)
		if Reason(err) != reasonWatchBehind || (last != nil && final(last.GetState())) {
			break
		}
		if watches > maxRewatches {
			return nil, fmt.Errorf("watching %s fell behind it %d times, so dockwell stopped following it. It goes on applying: "+
				"follow it again with dockwell changes watch %s, or see where it is with dockwell changes get %s", id, watches, id, id)
		}
	}
	if err != nil && (last == nil || !final(last.GetState())) {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("stopped following %s, which goes on applying. Follow it again with dockwell changes watch %s", id, id)
		}
		return nil, r.explain(err)
	}
	if last == nil {
		return nil, fmt.Errorf("the watch of %s ended before it said how the change went. See it with dockwell changes get %s", id, id)
	}
	return last, nil
}

// took is how long a change took to apply. By the change's own times, it
// is from when it started applying to when its last step finished; a
// change with no steps that finished, as one with none on any server, took
// as long as this CLI waited for it since began, when the CLI applied it.
// Otherwise it isn't known.
func took(c *dockwellv1.Change, began, now time.Time) (time.Duration, bool) {
	var end time.Time
	for _, s := range c.GetSteps() {
		if f := s.GetFinishTime(); f != nil && f.AsTime().After(end) {
			end = f.AsTime()
		}
	}
	if start := c.GetApplyTime(); start != nil && !end.IsZero() && !end.Before(start.AsTime()) {
		return end.Sub(start.AsTime()), true
	}
	if !began.IsZero() {
		return now.Sub(began), true
	}
	return 0, false
}

func final(s dockwellv1.ChangeState) bool {
	return s == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || s == dockwellv1.ChangeState_CHANGE_STATE_FAILED ||
		s == dockwellv1.ChangeState_CHANGE_STATE_DISCARDED
}

// outcome says how a change ended, and fails for one that didn't apply.
// began is when this CLI applied it, or zero.
func (r *runner) outcome(c *dockwellv1.Change, began time.Time) error {
	id := c.GetId()
	switch c.GetState() {
	case dockwellv1.ChangeState_CHANGE_STATE_APPLIED:
		if !r.opts.json {
			msg := "Applied " + id + "."
			if d, ok := took(c, began, r.env.now()); ok {
				msg = fmt.Sprintf("Applied %s in %s.", id, Elapsed(d))
			}
			if d := c.GetUndoDeadline(); d != nil {
				msg += fmt.Sprintf(" Undo it %s with dockwell changes undo %s.", untilWord(d.AsTime(), r.env.now()), id)
			}
			fmt.Fprintln(r.env.Out, msg)
		}
		return nil
	case dockwellv1.ChangeState_CHANGE_STATE_FAILED:
		msg := fmt.Sprintf("%s failed: %s", id, sentence(c.GetError()))
		if words := rollbackWords(c); words != "" {
			msg += " " + words
		}
		return errors.New(strings.TrimSuffix(msg, "."))
	case dockwellv1.ChangeState_CHANGE_STATE_DISCARDED:
		return fmt.Errorf("someone discarded %s before it applied, so nothing changed", id)
	default:
		return fmt.Errorf("%s is still %s. Follow it with dockwell changes watch %s", id, strings.ToLower(word(c.GetState())), id)
	}
}
