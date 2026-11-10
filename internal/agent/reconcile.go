// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// How the agent reconciles on its own while the control plane is away (ADR 0054).
const (
	// DefaultAwayAfter is how long the agent goes without a stream before it acts
	// on its own, from its start or from the last stream ending.
	DefaultAwayAfter = 60 * time.Second
	// DefaultReconcileEvery is how often it looks again while away.
	DefaultReconcileEvery = 60 * time.Second
	// startTries is how many times it starts a container Docker couldn't, each
	// boot, before it stops and says so.
	startTries = 10
	// firstRetry and lastRetry bound the wait between two starts.
	firstRetry = 10 * time.Second
	lastRetry  = 5 * time.Minute
)

// How long the agent's own work on a resource takes at most, which a test shortens.
var (
	// lookTimeout bounds each of the agent's own calls to Docker for a resource,
	// and makeTimeout making one again, so a Docker that hangs can't hold the
	// resource's mark, and the control plane's plans of it, for longer.
	lookTimeout = time.Minute
	makeTimeout = 5 * time.Minute
	// pullTimeout bounds a pull by digest, which a stream opening also ends, as
	// stopping it destroys nothing.
	pullTimeout = 10 * time.Minute
)

// markTimeout is the longest the agent's own work on a resource holds it.
func markTimeout() time.Duration { return lookTimeout + pullTimeout + makeTimeout }

// The kinds the agent puts back on its own, in the order a plan goes in.
var reconciledKinds = map[resource.Kind]int{"network": 0, "volume": 1, "service": 2, "container": 2}

// leftAlone is every other kind an agent runs, with why the agent leaves it as it
// is while the control plane is away (ADR 0054, "What each kind gets"). Its record
// is kept all the same, so the control plane's fill and forget cover it. A kind
// new to the agent goes here or in reconciledKinds: a test in internal/cli fails
// until it's in one.
var leftAlone = map[resource.Kind]string{
	"route":         "the ingress starts with its last config and keeps it reconciled itself",
	"task":          "the agent keeps its tasks in tasks.json and runs them on its own clock (ADR 0053)",
	"database.user": "a user lives in its database's data, which comes back with the volume; making one again runs SQL with a password, which is a change, not a repair",
}

// ReconcileDecided says whether a kind an agent runs is either put back by the
// agent on its own or left alone on purpose, and why when it's left.
func ReconcileDecided(kind resource.Kind) (reconciled bool, why string, decided bool) {
	if _, ok := reconciledKinds[kind]; ok {
		return true, "", true
	}
	why, ok := leftAlone[kind]
	return false, why, ok
}

// presence is whether the agent has a stream to the control plane, since when it
// hasn't, and whether the control plane said this server was removed.
type presence struct {
	mu      sync.Mutex
	up      bool
	since   time.Time
	removed bool
	// wake has the reconciler look now, as a stream just ended.
	wake chan struct{}
	// opened closes as a stream opens.
	opened chan struct{}
}

func newPresence(now time.Time) *presence {
	return &presence{since: now, wake: make(chan struct{}, 1), opened: make(chan struct{})}
}

// opening is closed once a stream is up.
func (p *presence) opening() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opened
}

// set notes a stream opening, or ending.
func (p *presence) set(up bool, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up == up {
		return
	}
	p.up = up
	if up {
		close(p.opened)
	} else {
		p.opened = make(chan struct{})
		p.since = now
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// away says whether the agent has had no stream for at least after, and wasn't
// removed: when it may act on its own.
func (p *presence) away(now time.Time, after time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.up && !p.removed && now.Sub(p.since) >= after
}

// down says whether the agent has no stream now, and wasn't removed: when it notes
// what it sees, before it may act.
func (p *presence) down() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.up && !p.removed
}

// remove notes the control plane said this server was removed, and says whether
// that's news.
func (p *presence) remove() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := p.removed
	p.removed = true
	return !was
}

// tries counts the starts of a container Docker couldn't start.
type tries struct {
	n    int
	next time.Time
}

// reconciler puts back what its server should run while the control plane is
// away: it makes again what's gone and starts what Docker couldn't, and destroys,
// replaces or changes nothing (ADR 0054).
type reconciler struct {
	cfg      Config
	local    resource.Local
	presence *presence
	held     *unfinished
	now      func() time.Time
	after    time.Duration
	every    time.Duration

	tries map[resource.Ref]*tries
}

func newReconciler(cfg Config, p *presence, held *unfinished) *reconciler {
	return &reconciler{
		cfg: cfg, local: resource.Local{Drivers: cfg.Drivers}, presence: p, held: held, now: time.Now,
		after: cmp.Or(cfg.AwayAfter, DefaultAwayAfter), every: cmp.Or(cfg.ReconcileEvery, DefaultReconcileEvery),
		tries: map[resource.Ref]*tries{},
	}
}

// run looks every while, and as a stream ends, until ctx ends. While the agent has
// no stream it notes what it sees; once it has had none for a while, it acts.
func (r *reconciler) run(ctx context.Context) {
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		r.recoverActions(ctx)
		if r.presence.down() {
			r.pass(ctx, r.presence.away(r.now(), r.after))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.presence.wake:
		}
	}
}

// pass goes through what the agent keeps, in the order a plan does, noting what it
// sees, and, if act, putting back what it may. It stops as a stream opens: what it
// is doing then finishes, and nothing new starts. A Docker that doesn't answer is
// one wait for the whole server, not one for each resource.
func (r *reconciler) pass(ctx context.Context, act bool) {
	if c := r.cfg.Docker; c != nil {
		ping, cancel := context.WithTimeout(ctx, lookTimeout)
		err := c.Ping(ping)
		cancel()
		if err != nil {
			r.wait(resource.Ref{}, act, "Docker doesn't answer, so the agent can't see or put back what this server should run. "+
				"Start Docker, such as with systemctl start docker; the agent looks again every minute")
			return
		}
	}
	refs := slices.DeleteFunc(r.cfg.Desired.Refs(), func(ref resource.Ref) bool {
		_, ok := reconciledKinds[ref.Kind]
		return !ok
	})
	slices.SortStableFunc(refs, func(a, b resource.Ref) int { return cmp.Compare(reconciledKinds[a.Kind], reconciledKinds[b.Kind]) })
	for _, ref := range refs {
		if ctx.Err() != nil || !r.presence.down() {
			return
		}
		mark, ok := r.held.claim(ref)
		if !ok {
			continue // a step on it is under way: it has the say
		}
		r.one(ctx, ref, act && r.presence.away(r.now(), r.after))
		r.held.end(mark)
	}
}

// one looks at a resource, and puts it back if act and it may.
func (r *reconciler) one(ctx context.Context, ref resource.Ref, act bool) {
	rec, ok, err := r.cfg.Desired.Read(ref)
	if err != nil {
		r.wait(ref, act, fmt.Sprintf("Its record can't be read (%v), so the agent can't put it back. Check the agent's data folder", err))
		return
	}
	if !ok {
		return
	}
	spec, err := r.cfg.Drivers.Decode(ref.Kind, rec.Spec)
	if err != nil {
		r.wait(ref, act, fmt.Sprintf("This agent can't read its spec (%v), so it can't put it back. Update the agent", err))
		return
	}
	if rec.ScopeName != "" {
		ctx = resource.WithScopeNames(ctx, map[string]string{ref.Scope: rec.ScopeName})
	}
	if rv := r.local.Reviver(ref); rv != nil {
		r.runs(ctx, rv, rec, spec, act)
		return
	}
	look, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	observed, err := r.local.Observe(look, []resource.Ref{ref})
	switch {
	case err != nil:
		r.wait(ref, act, fmt.Sprintf("Docker can't say how it stands (%v), so the agent leaves it. It looks again every minute", err))
		return
	case len(observed) == 1 && observed[0] != nil:
		return // it's there; whether it differs is drift, for the control plane
	}
	if !act {
		return
	}
	switch ref.Kind {
	case "volume":
		r.journal(ref, desired.Waiting, "It's gone, and the agent doesn't make a volume again on its own, as an empty one would hide that its data is gone. "+
			"Put its data back, or apply it again from the control plane to make it empty")
	case "network":
		a := r.local.Attacher(ref)
		if a == nil {
			return
		}
		names, err := a.Attached(look, ref)
		switch {
		case err != nil:
			r.journal(ref, desired.Waiting, fmt.Sprintf("Docker can't say what's attached to it (%v), so the agent leaves it. It looks again every minute", err))
		case len(names) > 0:
			r.journal(ref, desired.Waiting, fmt.Sprintf("It's gone, and %s still name it, which a network made again wouldn't reach. "+
				"Apply them again from the control plane once it's back", strings.Join(names, ", ")))
		default:
			making, cancel := context.WithTimeout(ctx, makeTimeout)
			defer cancel()
			r.make(making, rec, spec, "")
		}
	}
}

// runs looks at a resource that runs something: one that's there is noted as
// running or stopped, and started if Docker couldn't start it; one that's gone is
// made again, unless it was last seen stopped or what it mounts is gone.
func (r *reconciler) runs(ctx context.Context, rv resource.Reviver, rec desired.Record, spec proto.Message, act bool) {
	ref := rec.Ref
	at := r.now()
	look, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	st, err := rv.RunState(look, ref)
	if err != nil {
		r.wait(ref, act, fmt.Sprintf("Docker can't say how it stands (%v), so the agent leaves it. It looks again every minute", err))
		return
	}
	if st.Exists {
		if r.finishAction(look, rv, ref, st, act) {
			return
		}
		r.seen(ref, st, at)
		if act && !st.Running && st.Failed != "" && st.Wants {
			r.start(look, rv, ref, st.Failed, st.ID)
		}
		return
	}
	if !act {
		return
	}
	if rec.Stopped {
		r.journal(ref, desired.Waiting, "It was stopped, and is gone. The agent doesn't make it again on its own, as that would start it. "+
			"Apply it again from the control plane to run it")
		return
	}
	why, err := rv.Hold(look, ref, spec, rec.Volumes)
	switch {
	case err != nil:
		r.journal(ref, desired.Waiting, fmt.Sprintf("Docker can't say whether what it mounts is there (%v), so the agent leaves it. It looks again every minute", err))
		return
	case why != "":
		r.journal(ref, desired.Waiting, why)
		return
	}
	note, why := r.image(ctx, rec, spec)
	if why != "" {
		r.journal(ref, desired.Waiting, why)
		return
	}
	making, cancelMaking := context.WithTimeout(resource.WithImageVolumes(resource.WithValues(ctx, rec.Values), rec.Volumes), makeTimeout)
	defer cancelMaking()
	r.make(making, rec, spec, note)
}

// seen notes how a resource stands, as of a look at at: stopped when it isn't
// running and Docker had no error starting it, as someone stopped it or it
// exited where its policy leaves it.
func (r *reconciler) seen(ref resource.Ref, st resource.RunState, at time.Time) {
	standing := desired.Standing{Stopped: !st.Running && st.Failed == "", Volumes: st.Volumes}
	if err := r.cfg.Desired.Seen(ref, standing, at); err != nil {
		r.cfg.Logger.Warn("can't note how a resource stands, so it may be put back as it last stood while the control plane is away",
			slog.String("resource", ref.String()), slog.String("error", err.Error()))
	}
}

// noted notes how each resource the agent keeps a record of stands, from a list of
// the server's containers read at at, as the health monitor reads one every few
// seconds, while a stream is up too: what someone stops while the control plane
// is there isn't made again while it's away (ADR 0054). A resource with a step
// under way is the step's to say.
func (r *reconciler) noted(ctx context.Context, at time.Time, list []docker.ContainerSummary) {
	byLabels := make(map[[2]string]*docker.ContainerSummary, len(list))
	for i := range list {
		c := &list[i]
		key := [2]string{c.Labels[docker.LabelProject], c.Labels[docker.LabelService]}
		if c.Labels[docker.LabelOneoff] == "True" || c.Number() > 1 {
			continue
		}
		byLabels[key] = c
	}
	for _, ref := range r.cfg.Desired.Refs() {
		if ref.Kind != "service" && ref.Kind != "container" || r.held.busy(ref) {
			continue
		}
		c := byLabels[[2]string{docker.Project(ref.Scope), ref.Name}]
		was, ok := r.cfg.Desired.Stands(ref)
		if c == nil || !ok {
			continue // gone: it stays as it was last seen
		}
		st := resource.RunState{Exists: true, Running: c.State == "running" || c.State == "restarting" || c.State == "paused", Volumes: map[string]string{}}
		for _, m := range c.Mounts {
			if m.Type == "volume" && docker.AnonymousVolume(m.Name) {
				st.Volumes[m.Destination] = m.Name
			}
		}
		if !st.Running && !was.Stopped && r.cfg.Docker != nil {
			// Only Docker's own state says whether it couldn't start it, which is
			// read only as it's seen to stop.
			look, cancel := context.WithTimeout(ctx, lookTimeout)
			full, err := r.cfg.Docker.InspectContainer(look, c.ID)
			cancel()
			if err != nil {
				continue
			}
			st.Failed = full.State.Error
		}
		r.seen(ref, st, at)
	}
}

// make makes a resource that's gone again from its record, as a step creating it
// would, and writes down how it went, and note, what was done to its image.
func (r *reconciler) make(ctx context.Context, rec desired.Record, spec proto.Message, note string) {
	ref := rec.Ref
	// Labelled with the revision and change it ran, as the step that made it was,
	// so its logs and what the control plane reads of it are that deploy's.
	ctx = resource.WithDeploy(ctx, resource.Deploy{Revision: int(rec.Revision), Change: rec.Change})
	steps, err := r.local.Plan(ctx, []resource.Desired{{Ref: ref, Spec: spec}})
	switch {
	case err != nil:
		r.journal(ref, desired.Waiting, fmt.Sprintf("It can't be planned on this server (%v), so the agent can't make it again. Apply it again from the control plane once it's back", err))
		return
	case len(steps) != 1 || steps[0].Action != resource.ActionCreate:
		return // it came back, or differs: drift, for the control plane
	}
	step := steps[0]
	step.Pull = false
	e, ok := r.beginAction(ref, desired.Made, note, "")
	if !ok {
		return
	}
	deploy := resource.DeployOf(ctx)
	deploy.Reconcile = e.ID
	ctx = resource.WithDeploy(ctx, deploy)
	if err := r.local.Apply(ctx, step, spec); err != nil {
		r.journal(ref, desired.Waiting, fmt.Sprintf("Making it again failed (%v). The agent tries again in a minute", err))
		return
	}
	r.endAction(e, desired.Made, note)
}

// start starts a container Docker couldn't, at most startTries times each boot,
// waiting longer between each.
func (r *reconciler) start(ctx context.Context, rv resource.Reviver, ref resource.Ref, failed, target string) {
	if !r.canStart(ref, failed) {
		return
	}
	e, ok := r.beginAction(ref, desired.Started, "", target)
	if !ok || !r.tryStart(ctx, rv, ref) {
		return
	}
	r.endAction(e, desired.Started, "")
}

// canStart and tryStart share the retry limit with the first start of a creation
// interrupted by an agent kill.
func (r *reconciler) canStart(ref resource.Ref, failed string) bool {
	t := r.tries[ref]
	if t == nil {
		t = &tries{}
		r.tries[ref] = t
	}
	now := r.now()
	switch {
	case t.n >= startTries:
		r.journal(ref, desired.Waiting, fmt.Sprintf("Docker couldn't start it (%s), nor could the agent in %d tries since this server or the agent started. "+
			"Fix what stops it, then start it from the control plane", failed, startTries))
		return false
	case now.Before(t.next):
		return false
	}
	return true
}

func (r *reconciler) tryStart(ctx context.Context, rv resource.Reviver, ref resource.Ref) bool {
	t := r.tries[ref]
	now := r.now()
	t.n++
	t.next = now.Add(min(firstRetry<<(t.n-1), lastRetry))
	if err := rv.Start(ctx, ref); err != nil {
		r.journal(ref, desired.Waiting, fmt.Sprintf("Docker couldn't start it, nor could the agent (%v). The agent tries again in %s", err, human.Duration(t.next.Sub(now))))
		return false
	}
	return true
}

// image makes sure the image a resource is made from is the one its record says:
// a tag that moved is moved back to it, and an image that's gone is pulled by its
// digest, never by a tag, which may have moved. With neither the image's ID nor its
// digest, the tag can't be told to name it, so it waits. It returns what it did,
// for the journal, or why it waits.
func (r *reconciler) image(ctx context.Context, rec desired.Record, spec proto.Message) (note, why string) {
	// A kind with no driver here, or one that names no image, runs none.
	d, _ := r.local.Drivers.Driver(rec.Ref.Kind)
	images, isNamer := d.(resource.ImageNamer)
	if !isNamer || r.cfg.Docker == nil {
		return "", ""
	}
	ref := images.ImageOf(spec)
	if ref == "" {
		return "", ""
	}
	c := r.cfg.Docker
	look, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	if strings.Contains(ref, "@") {
		// Pinned by digest, so it names one image wherever it's pulled from.
		if _, err := c.InspectImage(look, ref); err == nil {
			return "", ""
		}
		if err := r.pull(ctx, ref); err != nil {
			return "", fmt.Sprintf("Its image, %s, is gone, and pulling it again failed (%v). The agent tries again in a minute", ref, err)
		}
		return "Pulled its image, " + ref + ", again, as it was gone.", ""
	}
	img, err := c.InspectImage(look, ref)
	if err != nil && !docker.IsNotFound(err) {
		return "", fmt.Sprintf("Docker can't say which image %s names (%v), so the agent leaves it. It looks again every minute", ref, err)
	}
	switch {
	case err == nil && rec.Image.ID != "" && img.ID == rec.Image.ID:
		return "", ""
	case err == nil && rec.Image.ID == "" && rec.Image.Digest != "" && img.Digest(ref) == digestOf(rec.Image.Digest):
		return "", ""
	}
	if rec.Image.ID != "" {
		if _, err := c.InspectImage(look, rec.Image.ID); err == nil {
			if err := c.TagImage(look, rec.Image.ID, ref); err != nil {
				return "", fmt.Sprintf("%s names another image now, and it can't be moved back to the one this ran (%v). Apply it again from the control plane once it's back", ref, err)
			}
			return ref + " named another image, as after a pull by hand, so the agent moved it back to the one this ran.", ""
		}
	}
	if rec.Image.Digest == "" {
		return "", fmt.Sprintf("Its image is gone, or %s names another one now, and its record has no digest to pull it by. "+
			"A pull by tag may get another image, so the agent leaves it. Apply it again from the control plane once it's back", ref)
	}
	if err := r.pull(ctx, rec.Image.Digest); err != nil {
		return "", fmt.Sprintf("Its image is gone, or %s names another one now, and pulling it by its digest, %s, failed (%v). The agent tries again in a minute", ref, rec.Image.Digest, err)
	}
	if err := c.TagImage(look, rec.Image.Digest, ref); err != nil {
		return "", fmt.Sprintf("Pulled %s, and can't name it %s (%v). Apply it again from the control plane once it's back", rec.Image.Digest, ref, err)
	}
	return fmt.Sprintf("Pulled its image by its digest, %s, as %s was gone or named another one.", rec.Image.Digest, ref), ""
}

// digestOf is the digest part of a reference by digest, such as sha256:ab12…
// from nginx@sha256:ab12…, or the whole of one with none.
func digestOf(ref string) string {
	if _, d, ok := strings.Cut(ref, "@"); ok {
		return d
	}
	return ref
}

// pull pulls ref, for at most pullTimeout, and stops as a stream opens, which
// destroys nothing: the control plane has the say then.
func (r *reconciler) pull(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	go func() {
		select {
		case <-r.presence.opening():
			cancel()
		case <-ctx.Done():
		}
	}()
	return r.cfg.Docker.PullImage(ctx, ref, nil)
}

// wait writes down that a resource can't be put back, if the agent acts now.
func (r *reconciler) wait(ref resource.Ref, act bool, why string) {
	if act {
		r.journal(ref, desired.Waiting, why)
	}
}

// journal writes down what the agent did, and logs it.
func (r *reconciler) journal(ref resource.Ref, what, reason string) {
	level := slog.LevelInfo
	if what == desired.Waiting {
		level = slog.LevelWarn
	}
	r.cfg.Logger.Log(context.Background(), level, "put back what this server should run, on its own while the control plane is away",
		slog.String("resource", ref.String()), slog.String("did", what), slog.String("why", reason))
	if r.cfg.Journal == nil {
		return
	}
	if err := r.cfg.Journal.Add(ref, what, reason, r.now()); err != nil {
		r.cfg.Logger.Warn("can't write down what the agent did on its own, so the control plane won't hear of it",
			slog.String("resource", ref.String()), slog.String("error", err.Error()))
	}
}

// localActions are what the journal's entries say, as the stream carries them.
var localActions = map[string]agentv1.LocalAction{
	desired.Made: agentv1.LocalAction_LOCAL_ACTION_MADE, desired.Started: agentv1.LocalAction_LOCAL_ACTION_STARTED,
	desired.Waiting: agentv1.LocalAction_LOCAL_ACTION_WAITING,
}

// reconciledProto is the journal as the stream carries it.
func reconciledProto(entries []desired.Entry, dropped int, droppedTo string) *agentv1.Reconciled {
	out := &agentv1.Reconciled{Dropped: uint32(min(dropped, 1<<31)), DroppedTo: droppedTo} //nolint:gosec // at most 1<<31
	for i := range entries {
		e := &entries[i]
		out.Entries = append(out.Entries, &agentv1.LocalEntry{
			Id: e.ID, Resource: resourcepb.Ref(e.Ref), Action: localActions[e.What], Reason: e.Reason, Time: timestamppb.New(e.At),
		})
	}
	return out
}
