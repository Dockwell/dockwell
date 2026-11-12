// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// How often the control plane looks at the branch each source follows, and how it
// waits after a look that failed (docs/adr/0077-dockwells-catalog-in-its-own-repository.md).
var (
	// FollowEvery is the wait between looks at a branch, give or take a tenth, so
	// installs started together don't all ask a host at once.
	FollowEvery = time.Hour
	// FollowFirst is the most the first look after the control plane starts waits,
	// so a restart doesn't ask every host at once either.
	FollowFirst = 5 * time.Minute
	// followRetry is the wait after a look that failed, doubled after each failure
	// in a row up to followRetryMost.
	followRetry     = 5 * time.Minute
	followRetryMost = 6 * time.Hour
	// FollowTick is how often the follower sees which sources are due.
	FollowTick = time.Minute
)

// settingFollowed is the setting that holds the commit Dockwell last moved a source
// to, by the source's name after it, so a look on the hour never moves a source back
// to a commit a person undid.
const settingFollowed = "catalog.follow."

// errNotFollowing and errNoSource are why a look can't be made.
var (
	errNoSource     = errors.New("no such source")
	errNotFollowing = errors.New("not following a branch")
)

// Follower moves each source that follows a branch to the branch's commit as it
// moves, by Dockwell's own change, applied at once, with the notes a person's move
// has. It looks every FollowEvery, and when asked. A commit it can't read, or whose
// entries all fail the catalog's checks, leaves the source where it is, and its
// state says why. It never touches what's installed: an install whose entry has a
// newer release shows an upgrade, which a person plans.
type Follower struct {
	h      *Handler
	engine *revision.Engine

	// looking holds the one look made at a time, as each may fetch a commit.
	looking sync.Mutex
	mu      sync.Mutex
	state   map[string]*followState
}

// followState is what the follower knows of a source since the control plane started.
type followState struct {
	// checked is when it last looked, and next when it looks again.
	checked, next time.Time
	// failures is how many looks in a row failed, since the first of them.
	failures int
	since    time.Time
	// problem says why the source isn't where its branch is after the last look.
	problem string
	// branch is the commit the branch was at.
	branch string
}

// Follow gives the handler a follower that moves sources through engine, for the
// control plane to run, and to look when asked through CheckCatalogSource.
func (h *Handler) Follow(engine *revision.Engine) *Follower {
	h.follower = &Follower{h: h, engine: engine, state: map[string]*followState{}}
	return h.follower
}

// Run looks at each following source when it's due, until ctx ends.
func (f *Follower) Run(ctx context.Context) {
	t := time.NewTicker(FollowTick)
	defer t.Stop()
	for {
		f.due(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// due looks at each source that follows a branch and whose time has come. One seen
// for the first time is due within FollowFirst.
func (f *Follower) due(ctx context.Context) {
	list, err := f.h.storedSources(ctx)
	if err != nil {
		if ctx.Err() == nil {
			f.h.logger.WarnContext(ctx, "can't read the catalog sources to follow their branches; it's tried again in a minute", slog.String("error", err.Error()))
		}
		return
	}
	now := f.h.sources.now()
	for _, st := range list {
		if st.spec.GetFollow() == "" {
			continue
		}
		f.mu.Lock()
		s := f.stateOf(st.name)
		if s.next.IsZero() {
			s.next = now.Add(rand.N(FollowFirst + 1)) //nolint:gosec // a spread of start times, not a secret
		}
		wait := now.Before(s.next)
		f.mu.Unlock()
		if !wait {
			_, _ = f.look(ctx, st.name, false)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// stateOf is a source's state, made empty the first time. f.mu is held.
func (f *Follower) stateOf(name string) *followState {
	s, ok := f.state[name]
	if !ok {
		s = &followState{}
		f.state[name] = s
	}
	return s
}

// look looks at the branch a source follows, and moves the source to its commit
// when that's another, returning the change that did. A look on the hour leaves a
// source at a commit other than the branch's when Dockwell moved it to the branch's
// before, as a person undid that; a look a person asks for moves it anyway. A look
// that failed returns why, which the source's state says too.
func (f *Follower) look(ctx context.Context, name string, asked bool) (string, error) {
	f.looking.Lock()
	defer f.looking.Unlock()
	list, err := f.h.storedSources(ctx)
	if err != nil {
		return "", err
	}
	var st *stored
	for i := range list {
		if list[i].name == name {
			st = &list[i]
		}
	}
	switch {
	case st == nil:
		f.forget(name)
		return "", errNoSource
	case st.spec.GetFollow() == "":
		f.forget(name)
		return "", errNotFollowing
	}
	spec, branch := st.spec, st.spec.GetFollow()
	now := f.h.sources.now()
	at, err := f.h.sources.branchAt(ctx, spec)
	if err != nil {
		return "", f.failed(ctx, name, now, "", err.Error())
	}
	if at == spec.GetCommit() {
		f.fine(name, now, at)
		return "", nil
	}
	if !asked {
		var last []byte
		err := f.h.db.View(ctx, func(tx *store.Tx) (err error) {
			last, err = tx.GetSetting(ctx, settingFollowed+name)
			if errors.Is(err, store.ErrNotFound) {
				err = nil
			}
			return err
		})
		if err != nil {
			return "", err
		}
		if string(last) == at {
			f.fine(name, now, at)
			return "", nil
		}
	}
	next := proto.CloneOf(spec)
	next.Commit = at
	stays := fmt.Sprintf("It stays at %s.", spec.GetCommit()[:12])
	plan, err := f.h.planSource(ctx, name, next, branch)
	if err != nil {
		return "", f.failed(ctx, name, now, at, sentence(err)+" "+stays)
	}
	if len(plan.read.entries) == 0 && len(plan.was.entries) > 0 {
		why := ""
		if len(plan.read.refused) > 0 {
			r := plan.read.refused[0]
			why = fmt.Sprintf(" %s: %s.", r.GetFile(), strings.TrimSuffix(r.GetReason(), "."))
		}
		return "", f.failed(ctx, name, now, at, fmt.Sprintf("None of the entries at %s, where %s is now, pass Dockwell's checks.%s %s", at[:12], branch, why, stays))
	}
	notes := append(slices.Clip(plan.notes), revision.Note{
		Reason: "catalog_source_followed", Ref: plan.ref, Names: []string{"refs/heads/" + branch},
		Message: "Dockwell moves it as " + branch + " moved. What's installed doesn't change: an install with a newer release in it shows an upgrade, which you plan.",
	})
	ref := plan.ref
	ref.Scope = st.scope
	p, err := f.engine.Propose(ctx, revision.Proposal{
		Summary: plan.summary + ", as " + branch + " moved", Actor: revision.SystemActor, Via: revision.ViaAutomatic,
		Edits: []revision.Edit{{Ref: ref, Spec: next, Base: st.revision}}, Notes: notes,
	})
	if err != nil {
		return "", f.failed(ctx, name, now, at, sentence(err)+" "+stays)
	}
	if err := f.engine.Apply(ctx, p.Change); err != nil {
		// A plan that didn't apply isn't left behind: the next look makes another.
		_ = f.engine.Discard(context.WithoutCancel(ctx), p.Change, revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic})
		return "", f.failed(ctx, name, now, at, sentence(err)+" "+stays)
	}
	if err := f.h.db.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, settingFollowed+name, []byte(at)) }); err != nil {
		return p.Change, err
	}
	f.h.logger.InfoContext(ctx, "moved a catalog source along the branch it follows", slog.String("source", name), slog.String("branch", branch), slog.String("commit", at))
	f.fine(name, now, at)
	return p.Change, nil
}

// fine records a look that left a source where its branch is.
func (f *Follower) fine(name string, now time.Time, at string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stateOf(name)
	s.checked, s.problem, s.branch, s.failures, s.since = now, "", at, 0, time.Time{}
	s.next = now.Add(FollowEvery - FollowEvery/10 + rand.N(FollowEvery/5+1)) //nolint:gosec // a spread of times, not a secret
}

// failed records a look that failed, and when to look again, later after each
// failure in a row, and returns why.
func (f *Follower) failed(ctx context.Context, name string, now time.Time, at, problem string) error {
	f.mu.Lock()
	s := f.stateOf(name)
	if s.failures == 0 {
		s.since = now
	}
	s.checked, s.problem, s.failures = now, problem, s.failures+1
	if at != "" {
		s.branch = at
	}
	wait := followRetry << min(s.failures-1, 10)
	s.next = now.Add(min(wait, followRetryMost))
	f.mu.Unlock()
	f.h.logger.WarnContext(ctx, "a catalog source can't follow its branch, so it stays where it is", slog.String("source", name), slog.String("problem", problem))
	return errors.New(problem)
}

// forget forgets a source that no longer follows a branch, or is gone.
func (f *Follower) forget(name string) {
	f.mu.Lock()
	delete(f.state, name)
	f.mu.Unlock()
}

// Standing is a source that follows a branch, as the last look found it.
type Standing struct {
	// Name is the source's, and Branch the branch it follows.
	Name, Branch string
	// Looked says the follower has looked at it since the control plane started.
	Looked bool
	// Problem says why it isn't at its branch's commit, since Since, the first of
	// the looks in a row that failed. Empty when it is.
	Problem string
	Since   time.Time
}

// Standings says how each source that follows a branch stands, in their names'
// order, for an alert.
func (f *Follower) Standings(ctx context.Context) []Standing {
	list, err := f.h.storedSources(ctx)
	if err != nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Standing
	for _, st := range list {
		if st.spec.GetFollow() == "" {
			continue
		}
		one := Standing{Name: st.name, Branch: st.spec.GetFollow()}
		if s, ok := f.state[st.name]; ok && !s.checked.IsZero() {
			one.Looked, one.Problem, one.Since = true, s.problem, s.since
		}
		out = append(out, one)
	}
	return out
}

// info writes what the follower knows of a source into what the API says of it.
func (f *Follower) info(out *dockwellv1.CatalogSourceInfo) {
	if f == nil || out.GetSource().GetFollow() == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[out.GetName()]
	if !ok {
		return
	}
	if !s.checked.IsZero() {
		out.Checked = timestamppb.New(s.checked)
	}
	if !s.next.IsZero() {
		out.NextCheck = timestamppb.New(s.next)
	}
	out.Problem, out.BranchCommit = s.problem, s.branch
}

// branchAt is the commit the branch a source follows is at now, read as a source is.
func (s *Sources) branchAt(ctx context.Context, spec *resourcev1.CatalogSource) (string, error) {
	list := s.Refs
	if list == nil {
		list = gitfetch.List
	}
	ctx, cancel := context.WithTimeout(ctx, fetchWait)
	defer cancel()
	listing, err := list(ctx, s.Client, spec.GetRepository())
	if err != nil {
		return "", fmt.Errorf("Dockwell can't read the branches of %s: %w", spec.GetRepository(), err) //nolint:staticcheck // it starts with a name
	}
	at, ok := listing.Refs["refs/heads/"+spec.GetFollow()]
	if !ok {
		return "", fmt.Errorf("%s has no branch %s now. Follow another branch, or pin the source to a commit", spec.GetRepository(), spec.GetFollow())
	}
	return at, nil
}

// sentence is an error as the end of a sentence says it, with its stop.
func sentence(err error) string {
	words := strings.TrimSpace(err.Error())
	if words != "" && !strings.HasSuffix(words, ".") {
		words += "."
	}
	return words
}

// CheckCatalogSource looks at the branch a source follows now, and moves the source
// to its commit by Dockwell's own change when it has moved.
func (h *Handler) CheckCatalogSource(ctx context.Context, req *connect.Request[dockwellv1.CheckCatalogSourceRequest]) (*connect.Response[dockwellv1.CheckCatalogSourceResponse], error) {
	if h.follower == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane doesn't follow branches, so it can't look at one. Try again once it has started"))
	}
	name := req.Msg.GetName()
	changeID, err := h.follower.look(ctx, name, true)
	switch {
	case errors.Is(err, errNoSource):
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no catalog source %s. List them to find its name", name), "not_found")
	case errors.Is(err, errNotFollowing):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the catalog %s is pinned to a commit, so there's no branch to look at. Have it follow a branch first", name), "not_following")
	case errors.Is(err, context.Canceled):
		return nil, h.internal(ctx, err)
	}
	// Any other failure is the source's problem, which its state says.
	out := &dockwellv1.CheckCatalogSourceResponse{}
	infos, err := h.sourceInfos(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	for _, info := range infos {
		if info.GetName() == name {
			out.Source = info
		}
	}
	if changeID != "" {
		r, err := h.follower.engine.Change(ctx, changeID)
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		cs, err := h.changes.Protos(ctx, r)
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		if len(cs) > 0 {
			out.Change = cs[0]
		}
	}
	return connect.NewResponse(out), nil
}
