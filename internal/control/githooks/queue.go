// SPDX-License-Identifier: AGPL-3.0-only

package githooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// job is a push to deploy: the delivery, and the hook it came through, which the
// deploy acts as.
type job struct {
	hook     store.GitHook
	p        auth.Principal
	client   auth.Client
	delivery store.GitHookDelivery
}

// lane is one service's deploys from pushes, one at a time: the push being
// deployed, the change it applied while that runs, and the newest push that
// waits for it. Its fields are the Handler's mu's.
type lane struct {
	current, waiting *job
	change           string
	// cancelled is the change whose build a newer push cancelled while it waited
	// for its server.
	cancelled string
}

// enqueue deploys a push, now if nothing deploys its service from a push, or after
// what does. A newer push takes the place of one that waits, and cancels the build
// under way while it still waits for its server.
func (h *Handler) enqueue(j *job) {
	svc := j.hook.ResourceID
	h.mu.Lock()
	if h.base.Err() != nil {
		h.mu.Unlock()
		return
	}
	l := h.lanes[svc]
	if l == nil {
		l = &lane{current: j}
		h.lanes[svc] = l
		h.wg.Add(1)
		h.mu.Unlock()
		go h.run(svc, l)
		return
	}
	older := l.waiting
	l.waiting = j
	change := l.change
	if change != "" && h.cfg.Builds != nil {
		h.wg.Add(1)
	}
	h.mu.Unlock()
	if older != nil {
		d := older.delivery
		d.Outcome, d.Detail = store.HookSuperseded, "A newer push took its place before it began"
		h.tell(h.base, d, svc)
	}
	d := j.delivery
	d.Outcome, d.Detail = store.HookQueued, "Waits for the deploy under way from an earlier push"
	h.tell(h.base, d, svc)
	if change != "" && h.cfg.Builds != nil {
		go func() {
			defer h.wg.Done()
			h.cancelWaiting(j, l, change)
		}()
	}
}

// run deploys a service's pushes in turn, until none waits.
func (h *Handler) run(svc string, l *lane) {
	defer h.wg.Done()
	for {
		h.mu.Lock()
		j := l.current
		h.mu.Unlock()
		h.deploy(l, j)
		h.mu.Lock()
		l.current, l.waiting, l.change = l.waiting, nil, ""
		if l.current == nil || h.base.Err() != nil {
			delete(h.lanes, svc)
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
	}
}

// deploy reads what the push's branch names now and, if the service doesn't run
// that already, plans its deploy as the Deploy button does and applies it as the
// hook, with an API token's rights. A plan a token may not apply waits for a
// person. It returns once the deploy ends, so the next push waits for it.
func (h *Handler) deploy(l *lane, j *job) {
	asHook := authapi.As(h.base, j.client, j.p)
	svc, d := j.hook.ResourceID, j.delivery
	for tries := 0; ; tries++ {
		if !h.stillOn(asHook, j.hook, &d, svc, "") {
			return
		}
		// The hook deploys only what its maker may deploy now (ADR 0049): their
		// role, read for each push, where the service is, as the API checks a call.
		ctx, err := authapi.Check(asHook, h.cfg.Auth, h.cfg.Logger, dockwellv1connect.ServiceServiceDeployServiceProcedure, &dockwellv1.DeployServiceRequest{Id: svc})
		if err != nil {
			h.end(asHook, &d, svc, store.HookFailed, "This hook may not deploy it now: "+message(err))
			return
		}
		b, name, _, err := h.cfg.Services.BuildOf(ctx, svc, "", "")
		if err != nil {
			h.end(ctx, &d, svc, store.HookFailed, "Dockwell can't read the service it deploys: "+message(err))
			return
		}
		g := b.GetGit()
		if g == nil {
			h.end(ctx, &d, svc, store.HookIgnored, name+" doesn't build from Git now, so a push deploys nothing")
			return
		}
		commit, _, err := h.cfg.Sources.Resolve(ctx, g.GetUrl(), g.GetRef())
		if err != nil {
			h.end(ctx, &d, svc, store.HookFailed, fmt.Sprintf("Dockwell can't read %s: %s", refName(d.Ref), message(err)))
			return
		}
		d.Commit = commit
		if commit == g.GetCommit() {
			h.end(ctx, &d, svc, store.HookNothingNew, fmt.Sprintf("%s names %s, which %s runs already", refName(d.Ref), short(commit), name))
			return
		}
		res, err := h.cfg.Services.DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{
			Id: svc, Summary: fmt.Sprintf("Deploy %s to %s, pushed to %s", short(commit), name, refName(d.Ref)),
		}))
		if err != nil {
			h.end(ctx, &d, svc, store.HookFailed, "Dockwell couldn't plan its deploy: "+message(err))
			return
		}
		c := res.Msg.GetChange()
		d.ChangeID = c.GetId()
		if c.GetPlan().GetIntent().GetCommit() != "" {
			d.Commit = c.GetPlan().GetIntent().GetCommit()
		}
		h.replacePlanned(ctx, svc, c.GetId())
		r, err := h.cfg.Engine.Change(ctx, c.GetId())
		if err != nil {
			h.end(ctx, &d, svc, store.HookFailed, "Dockwell can't read the plan it made: "+message(err))
			return
		}
		if why := needsPerson(r.Plan); why != "" {
			h.waitForPerson(ctx, &d, svc, c.GetId(), why)
			return
		}
		switch h.apply(ctx, l, j.hook, &d, svc, c.GetId(), r.Fingerprint) {
		case applied:
			h.follow(ctx, l, &d, svc, c.GetId())
			return
		case stale:
			if tries < 2 {
				continue
			}
			h.end(ctx, &d, svc, store.HookFailed, "Other changes to the service kept its plan out of date. Push again, or deploy from the panel")
		case stopped:
		}
		return
	}
}

type applying int

const (
	applied applying = iota
	stale
	stopped
)

// apply applies a plan, by ApplyChange, as the hook. While another change to the
// service is under way it tries again, unless a newer push waits by then, which
// takes its place, or it has waited BusyFor, when it leaves the plan for a person.
// Each try is checked as ApplyChange is through the API, with the maker's role
// read again: a maker who may plan a deploy but not apply it there, as a
// developer in a protected environment, or who lost the role while it waited,
// leaves it for someone who may.
func (h *Handler) apply(ctx context.Context, l *lane, hook store.GitHook, d *store.GitHookDelivery, svc, changeID, fingerprint string) applying {
	req := &dockwellv1.ApplyChangeRequest{Id: changeID, Fingerprint: fingerprint}
	began := h.now()
	for {
		if !h.stillOn(ctx, hook, d, svc, changeID) {
			return stopped
		}
		cctx, err := authapi.Check(ctx, h.cfg.Auth, h.cfg.Logger, dockwellv1connect.ChangeServiceApplyChangeProcedure, req)
		switch {
		case reasonOf(err) == authapi.ReasonNeedsRole:
			h.waitForPerson(ctx, d, svc, changeID, "needs someone who may apply it there")
			return stopped
		case err != nil:
			h.end(ctx, d, svc, store.HookFailed, fmt.Sprintf("Dockwell couldn't apply %s: %s. Apply it from the panel", changeID, message(err)))
			return stopped
		}
		_, err = h.cfg.Changes.ApplyChange(cctx, connect.NewRequest(req))
		if err == nil {
			h.mu.Lock()
			l.change = changeID
			h.mu.Unlock()
			h.end(ctx, d, svc, store.HookDeploying, "Deploying "+short(d.Commit))
			return applied
		}
		switch reasonOf(err) {
		case "busy":
			h.mu.Lock()
			newer := l.waiting != nil
			h.mu.Unlock()
			if newer {
				h.discard(ctx, changeID)
				h.end(ctx, d, svc, store.HookSuperseded, "A newer push took its place before it began")
				return stopped
			}
			if h.now().Sub(began) >= BusyFor {
				h.waitForPerson(ctx, d, svc, changeID, "waited 30 minutes for another change to the app")
				return stopped
			}
			select {
			case <-ctx.Done():
				return stopped
			case <-time.After(h.wait()):
			}
		case "plan_stale", "plan_changed":
			h.discard(ctx, changeID)
			return stale
		case authapi.ReasonNeedsSession, authapi.ReasonNeedsStepUp:
			h.waitForPerson(ctx, d, svc, changeID, "needs a person")
			return stopped
		default:
			h.end(ctx, d, svc, store.HookFailed, fmt.Sprintf("Dockwell couldn't apply %s: %s. Apply it from the panel", changeID, message(err)))
			return stopped
		}
	}
}

// BusyFor is how long a push's deploy waits for another change to its app
// before it leaves its plan for a person.
const BusyFor = 30 * time.Minute

// follow waits for a deploy to end, as the next push waits for it. One whose build
// a newer push cancelled, as it waited for its server, gave its place to that push.
func (h *Handler) follow(ctx context.Context, l *lane, d *store.GitHookDelivery, svc, changeID string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := h.cfg.Engine.Watch(ctx, changeID)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		r, err := h.cfg.Engine.Change(ctx, changeID)
		if err == nil && ended(r.State) {
			h.mu.Lock()
			gave := l.cancelled == changeID
			h.mu.Unlock()
			if gave && r.State == store.ChangeFailed {
				h.end(ctx, d, svc, store.HookSuperseded, "A newer push took its place before its build began")
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	next:
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				break next
			case u, ok := <-updates:
				if !ok {
					// It fell behind; the ticker reads it from now on.
					updates = nil
					break next
				}
				if u.Step == nil && ended(u.State) {
					break next
				}
			}
		}
	}
}

// cancelWaiting cancels the build of the deploy under way, change, while it waits
// for its server, as the newer push j takes its place.
func (h *Handler) cancelWaiting(j *job, l *lane, change string) {
	ctx := authapi.As(h.base, j.client, j.p)
	var waiting []store.Build
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListBuilds(ctx, store.BuildFilter{ResourceID: j.hook.ResourceID, ChangeIDs: []string{change}})
		for i := range list {
			if list[i].State == store.BuildQueued {
				waiting = append(waiting, list[i])
			}
		}
		return err
	})
	if err != nil {
		h.cfg.Logger.WarnContext(ctx, "can't read the build a newer push would cancel", slog.String("change", change), slog.String("error", err.Error()))
		return
	}
	for i := range waiting {
		// Cancelling is checked as CancelBuild is through the API: a maker who
		// may not cancel there leaves the build, and the newer push waits for it.
		req := &dockwellv1.CancelBuildRequest{Id: waiting[i].ID}
		cctx, err := authapi.Check(ctx, h.cfg.Auth, h.cfg.Logger, dockwellv1connect.BuildServiceCancelBuildProcedure, req)
		if err != nil {
			h.cfg.Logger.DebugContext(ctx, "didn't cancel a build for a newer push, as the hook's maker may not", slog.String("build", waiting[i].ID), slog.String("error", err.Error()))
			continue
		}
		if _, err := h.cfg.Builds.CancelBuild(cctx, connect.NewRequest(req)); err != nil {
			// It began, or ended, meanwhile: the newer push waits for it.
			h.cfg.Logger.DebugContext(ctx, "didn't cancel a build for a newer push", slog.String("build", waiting[i].ID), slog.String("error", err.Error()))
			continue
		}
		h.mu.Lock()
		l.cancelled = change
		h.mu.Unlock()
	}
}

// waitForPerson leaves a plan for a person to apply, in place of the one an
// earlier push left.
func (h *Handler) waitForPerson(ctx context.Context, d *store.GitHookDelivery, svc, changeID, why string) {
	h.mu.Lock()
	h.planned[svc] = changeID
	h.mu.Unlock()
	h.end(ctx, d, svc, store.HookNeedsPerson, fmt.Sprintf("Its plan %s, so it waits for a person to apply it", why))
}

// replacePlanned discards the plan an earlier push left for a person, now a newer
// one has planned.
func (h *Handler) replacePlanned(ctx context.Context, svc, changeID string) {
	h.mu.Lock()
	old := h.planned[svc]
	delete(h.planned, svc)
	h.mu.Unlock()
	if old == "" || old == changeID {
		return
	}
	if r, err := h.cfg.Engine.Change(ctx, old); err != nil || r.State != store.ChangePlanned {
		return
	}
	h.discard(ctx, old)
}

// discard discards a plan a push made, checked as DiscardChange is through the
// API, with its maker's role read now.
func (h *Handler) discard(ctx context.Context, changeID string) {
	req := &dockwellv1.DiscardChangeRequest{Id: changeID}
	cctx, err := authapi.Check(ctx, h.cfg.Auth, h.cfg.Logger, dockwellv1connect.ChangeServiceDiscardChangeProcedure, req)
	if err == nil {
		_, err = h.cfg.Changes.DiscardChange(cctx, connect.NewRequest(req))
	}
	if err != nil {
		h.cfg.Logger.WarnContext(ctx, "can't discard a plan a push made", slog.String("change", changeID), slog.String("error", err.Error()))
	}
}

// stillOn reports a push's hook as it was when the push came: on, with the same
// secret and maker. Turning it off, or giving it a new secret, as after one
// leaked, ends what its pushes would still deploy, and the plan one left, if any.
// A hook turned off took its deliveries with it, so there's nothing to record.
func (h *Handler) stillOn(ctx context.Context, hook store.GitHook, d *store.GitHookDelivery, svc, planned string) bool {
	var now store.GitHook
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		now, err = tx.GetGitHook(ctx, hook.ID)
		return err
	})
	switch {
	case err == nil && now.SecretAt.Equal(hook.SecretAt) && now.UserID == hook.UserID:
		return true
	case err != nil && !errors.Is(err, store.ErrNotFound):
		// It can't be read: the push goes on, as the next step reads its maker again.
		h.cfg.Logger.WarnContext(ctx, "can't read a push's hook", slog.String("hook", hook.ID), slog.String("error", err.Error()))
		return true
	}
	if planned != "" {
		h.discard(ctx, planned)
	}
	if err == nil {
		h.end(ctx, d, svc, store.HookIgnored, "Deploy on push got a new secret after this push came, so it deploys nothing")
	}
	return false
}

// end keeps what a push came to.
func (h *Handler) end(ctx context.Context, d *store.GitHookDelivery, svc, outcome, detail string) {
	d.Outcome, d.Detail = outcome, detail
	h.tell(ctx, *d, svc)
}

func (h *Handler) wait() time.Duration {
	if h.Wait > 0 {
		return h.Wait
	}
	return 5 * time.Second
}

// needsPerson says why a token may not apply a plan, as ApplyChange holds one to,
// or "" when it may.
func needsPerson(p *revision.Plan) string {
	switch {
	case p.Destructive():
		return "destroys data, may cut off access to a server, or lets two containers write to one place"
	case p.HostAccess():
		return "gives the service access to its server, which a person approves"
	case p.AsGoodAsRoot():
		return "is as good as root on its server, which takes a person's passkey"
	}
	return ""
}

func ended(state string) bool {
	return state == store.ChangeApplied || state == store.ChangeFailed || state == store.ChangeDiscarded
}

// message is what an error from another handler says, for people.
func message(err error) string {
	msg := err.Error()
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		msg = cerr.Message()
	}
	return strings.TrimSuffix(msg, ".")
}

func reasonOf(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Meta().Get(authapi.ReasonKey)
	}
	return ""
}
