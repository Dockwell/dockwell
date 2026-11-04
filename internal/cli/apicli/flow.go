// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
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
	// choiceFlag names the flag that takes a choice, for a command whose
	// request isn't built from its flags field by field.
	choiceFlag func(*dockwellv1.Choice) string
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
				return fmt.Errorf("can't follow %s in the request", path)
			}
			name, index = part[:open], n
		}
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return fmt.Errorf("the request has no field %s; this CLI may be older than the control plane", path)
		}
		last := i == len(parts)-1
		switch {
		case index >= 0:
			l := m.Mutable(fd).List()
			if index >= l.Len() || fd.Message() == nil {
				return fmt.Errorf("the request has no %s", path)
			}
			m = l.Get(index).Message()
		case last && fd.Kind() == protoreflect.StringKind && fd.IsList():
			m.Mutable(fd).List().Append(protoreflect.ValueOfString(value))
		case last && fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(value))
		case fd.Message() != nil && !last:
			m = m.Mutable(fd).Message()
		default:
			return fmt.Errorf("the CLI can't set %s", path)
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
	Render(r.env.Out, msg, r.env.now())
	return nil
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
	if err := r.connect(ctx); err != nil {
		return err
	}
	if m.Effect == dockwellv1.Effect_EFFECT_DESTRUCTIVE {
		// No API token may make a destructive call (ADR 0016), and the CLI
		// signs in only with one.
		return ExitError{ExitNeedsPerson, fmt.Errorf("%s removes data or cuts off access, which no API token may do. Sign in to the panel at %s and do it there",
			r.command, r.client.PanelURL("/"))}
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
	if c := changeIn(res); m.Effect == dockwellv1.Effect_EFFECT_PLAN && c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		return r.planned(ctx, res, c)
	}
	return r.print(res, false)
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
		return errors.New("stopped. Nothing changed")
	}
	return nil
}

// stream prints each message of a stream as it comes.
func (r *runner) stream(ctx context.Context, m *Method, req proto.Message) error {
	if _, ok := req.(*dockwellv1.WatchChangeRequest); ok && !r.opts.json {
		return r.follow(ctx, req.(*dockwellv1.WatchChangeRequest).GetId(), r.env.now())
	}
	err := r.do(ctx, func() error {
		return m.Call.Invoke(ctx, r.client, req, func(msg proto.Message) error { return r.print(msg, true) })
	})
	if err != nil {
		return r.explain(err)
	}
	return nil
}

// planned shows a planned change and, once the person says so, applies it
// and follows it to an end. res is the response that carried it.
func (r *runner) planned(ctx context.Context, res proto.Message, c *dockwellv1.Change) error {
	if err := r.print(res, true); err != nil {
		return err
	}
	if empty(c.GetPlan()) {
		r.discard(ctx, c.GetId())
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

// needsPerson stops at a plan no API token may apply, and says where a
// person can.
func (r *runner) needsPerson(c *dockwellv1.Change) error {
	return ExitError{ExitNeedsPerson, fmt.Errorf("%s destroys data or may cut off access, so it needs a person to approve it, which no API token can. "+
		"It stays planned: sign in to the panel and apply it at %s", c.GetId(), r.client.PanelURL("/changes/"+c.GetId()))}
}

// confirmAndApply asks before applying a planned change, then applies the
// plan shown, by its fingerprint, and follows it.
func (r *runner) confirmAndApply(ctx context.Context, c *dockwellv1.Change, fingerprint string) error {
	if c.GetPlan().GetDestructive() {
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

// discard drops a planned change nobody will apply, as best it can.
func (r *runner) discard(ctx context.Context, id string) {
	_, _ = r.client.Changes().DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: id}))
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
	if err := r.print(got, true); err != nil {
		return err
	}
	if fp := req.GetFingerprint(); fp != "" && fp != c.GetPlan().GetFingerprint() {
		return fmt.Errorf("%s's plan has fingerprint %s, not %s, so it isn't the plan you reviewed. Review it with dockwell changes get %s, then apply it",
			c.GetId(), c.GetPlan().GetFingerprint(), fp, c.GetId())
	}
	return r.confirmAndApply(ctx, c, req.GetFingerprint())
}

// follow watches a change apply, printing each step as it moves on, until
// it is applied, failed or discarded. began is when this CLI applied it,
// on its own clock, or when it started watching.
func (r *runner) follow(ctx context.Context, id string, began time.Time) error {
	shown := map[int32]dockwellv1.StepState{}
	var last *dockwellv1.Change
	announced := false
	if !r.opts.json {
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
	err := r.do(ctx, func() error {
		stream, err := r.client.Changes().WatchChange(ctx, connect.NewRequest(&dockwellv1.WatchChangeRequest{Id: id}))
		if err != nil {
			return err
		}
		defer stream.Close()
		for stream.Receive() {
			msg := stream.Msg()
			if r.opts.json {
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
	})
	if err != nil && (last == nil || !final(last.GetState())) {
		if errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("stopped following %s, which goes on applying. Follow it again with dockwell changes watch %s", id, id)
		}
		return r.explain(err)
	}
	if last == nil {
		return fmt.Errorf("the watch of %s ended before it said how the change went. See it with dockwell changes get %s", id, id)
	}
	return r.outcome(last, r.env.now().Sub(began))
}

func final(s dockwellv1.ChangeState) bool {
	return s == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || s == dockwellv1.ChangeState_CHANGE_STATE_FAILED ||
		s == dockwellv1.ChangeState_CHANGE_STATE_DISCARDED
}

// outcome says how a change ended, and fails for one that didn't apply.
func (r *runner) outcome(c *dockwellv1.Change, took time.Duration) error {
	id := c.GetId()
	switch c.GetState() {
	case dockwellv1.ChangeState_CHANGE_STATE_APPLIED:
		if !r.opts.json {
			msg := fmt.Sprintf("Applied %s in %s.", id, Elapsed(took))
			if d := c.GetUndoDeadline(); d != nil {
				msg += fmt.Sprintf(" Undo it %s with dockwell changes undo %s.", untilWord(d.AsTime(), r.env.now()), id)
			}
			fmt.Fprintln(r.env.Out, msg)
		}
		return nil
	case dockwellv1.ChangeState_CHANGE_STATE_FAILED:
		msg := fmt.Sprintf("%s failed: %s", id, sentence(c.GetError()))
		if c.GetRolledBackBy() != "" {
			msg += fmt.Sprintf(" Dockwell is rolling back what it had done, in %s; follow it with dockwell changes watch %s", c.GetRolledBackBy(), c.GetRolledBackBy())
		}
		return errors.New(strings.TrimSuffix(msg, "."))
	case dockwellv1.ChangeState_CHANGE_STATE_DISCARDED:
		return fmt.Errorf("someone discarded %s before it applied, so nothing changed", id)
	default:
		return fmt.Errorf("%s is still %s. Follow it with dockwell changes watch %s", id, strings.ToLower(word(c.GetState())), id)
	}
}
