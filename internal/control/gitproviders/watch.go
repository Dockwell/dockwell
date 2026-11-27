// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Whether GitHub reaches the panel is read from the App's own deliveries every 10
// minutes. While it doesn't, Dockwell checks each app's branch itself, once a
// minute, at most 30 times a minute for each installation, a third of the least
// GitHub gives one, and backs off when GitHub asks (ADR 0098).
const (
	deliveriesEvery = 10 * time.Minute
	pollEvery       = time.Minute
	pollCap         = 30
	backoffFirst    = time.Minute
	backoffMost     = 15 * time.Minute
)

// Pusher deploys a push Dockwell found by checking a branch, as a delivery of it
// would: githooks.
type Pusher interface {
	Polled(ctx context.Context, hookID, ref, commit string) error
}

// SetPusher has polling hand what it finds to p.
func (h *Handler) SetPusher(p Pusher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pusher = p
}

// watching is what Dockwell knows of whether an App's deliveries reach the panel.
type watching struct {
	state   dockwellv1.GitHubDelivery_State
	checked time.Time
	// backoffUntil is when checks start again, and backoffStep how long the
	// last back-off for a secondary limit was, which doubles each time.
	backoffUntil time.Time
	backoffStep  time.Duration
	// every is how often each app's branch is checked, past the cap, by
	// installation, as each takes its own turns.
	every map[int64]time.Duration
	// turns is where each installation's turns are, by its ID.
	turns map[int64]int
	// refused says the panel refused GitHub's newest delivery: the secret GitHub
	// signs with isn't Dockwell's.
	refused bool
}

// branch is what the last check of a hook's branch said.
type branch struct {
	etag, commit string
}

// watchOf is the App's state, made where there's none. The caller holds mu.
func (h *Handler) watchOf(appID string) *watching {
	w, ok := h.watch[appID]
	if !ok {
		w = &watching{state: dockwellv1.GitHubDelivery_STATE_UNKNOWN, turns: map[int64]int{}, every: map[int64]time.Duration{}}
		h.watch[appID] = w
	}
	return w
}

// Delivered says one of the App's deliveries reached the panel: polling stops.
func (h *Handler) Delivered(appID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.watchOf(appID)
	w.state, w.checked, w.refused = dockwellv1.GitHubDelivery_STATE_REACHED, h.now(), false
}

// deliveryProto is what the API says of an App's deliveries.
func (h *Handler) deliveryProto(appID string) *dockwellv1.GitHubDelivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, ok := h.watch[appID]
	if !ok {
		return &dockwellv1.GitHubDelivery{State: dockwellv1.GitHubDelivery_STATE_UNKNOWN}
	}
	out := &dockwellv1.GitHubDelivery{State: w.state, SecretRefused: w.refused}
	if !w.checked.IsZero() {
		out.CheckedTime = timestamppb.New(w.checked)
	}
	if w.state == dockwellv1.GitHubDelivery_STATE_POLLING || w.state == dockwellv1.GitHubDelivery_STATE_BACKING_OFF {
		// The slowest installation's, so no app's line says more often than it is.
		slowest := pollEvery
		for _, e := range w.every {
			slowest = max(slowest, e)
		}
		out.CheckEverySeconds = int32(slowest.Seconds())
	}
	if w.state == dockwellv1.GitHubDelivery_STATE_BACKING_OFF {
		out.BackoffUntil = timestamppb.New(w.backoffUntil)
	}
	return out
}

// CheckDeliveries reads each App's latest deliveries, as the App: GitHub says
// what the panel answered the newest. One it took stops polling; one it didn't
// starts it, and one it refused for its signature says the secrets differ.
func (h *Handler) CheckDeliveries(ctx context.Context) {
	var apps []kept
	if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		apps, err = h.all(ctx, tx)
		return err
	}); err != nil {
		h.cfg.Logger.WarnContext(ctx, "can't read the GitHub Apps to check their deliveries", slog.String("error", err.Error()))
		return
	}
	for _, k := range apps {
		app, err := h.acting(k)
		if err != nil {
			continue
		}
		if self, err := app.Self(ctx); err == nil {
			h.setAppPermissions(k.r.ID, self.Permissions)
		}
		var ins []store.GitHubInstallation
		if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			ins, err = tx.GitHubInstallationsOf(ctx, k.r.ID)
			return err
		}); err == nil {
			for i := range ins {
				if ins[i].RemovedAt == nil && (len(ins[i].Permissions) == 0 || !github.HasChecksWrite(ins[i].Permissions)) {
					_, _ = h.Refresh(ctx, k, ins[i].ID)
				}
			}
		}
		deliveries, err := app.Deliveries(ctx)
		h.noted(k.r.ID, err)
		if err != nil {
			h.cfg.Logger.WarnContext(ctx, "can't read a GitHub App's deliveries", slog.String("app", k.r.Name), slog.String("error", err.Error()))
			continue
		}
		h.mu.Lock()
		w := h.watchOf(k.r.ID)
		w.checked = h.now()
		switch {
		case len(deliveries) == 0:
			// Nothing sent yet: as it was.
		case deliveries[0].Reached():
			w.state, w.refused = dockwellv1.GitHubDelivery_STATE_REACHED, false
		default:
			// Refused, or answered for by something before the panel: it delivered
			// nothing, so Dockwell checks for pushes itself.
			w.refused = deliveries[0].Refused()
			if w.state != dockwellv1.GitHubDelivery_STATE_BACKING_OFF {
				w.state = dockwellv1.GitHubDelivery_STATE_POLLING
			}
		}
		h.mu.Unlock()
	}
}

// due is which of n branches an installation checks this minute, by index, from
// turn on, at most limit of them; the next turn; and how often each is checked.
func due(n, turn, limit int) (picked []int, next int, every time.Duration) {
	if n <= limit {
		picked = make([]int, n)
		for i := range picked {
			picked[i] = i
		}
		return picked, 0, pollEvery
	}
	turn %= n
	for i := range limit {
		picked = append(picked, (turn+i)%n)
	}
	rounds := (n + limit - 1) / limit
	return picked, (turn + limit) % n, time.Duration(rounds) * pollEvery
}

// slowDown keeps what an answer said of GitHub's limits: a secondary limit waits as
// long as GitHub asked, or twice the last wait, to 15 minutes at most; fewer than a
// tenth of the hour's calls left waits for the hour to start over; an answer that
// passed ends a back-off whose time is up, and starts the next one over.
func (w *watching) slowDown(rate github.Rate, now time.Time) {
	switch {
	case rate.Limited:
		step := max(rate.RetryAfter, w.backoffStep*2, backoffFirst)
		w.backoffStep = min(step, backoffMost)
		w.backoffUntil, w.state = now.Add(w.backoffStep), dockwellv1.GitHubDelivery_STATE_BACKING_OFF
	case rate.Limit > 0 && rate.Remaining < rate.Limit/10 && rate.Reset.After(now):
		w.backoffUntil, w.state = rate.Reset, dockwellv1.GitHubDelivery_STATE_BACKING_OFF
	default:
		w.backoffStep = 0
	}
}

// polled is a hook Dockwell checks the branch of, through an installation.
type polled struct {
	hook          string
	installation  int64
	repository    int64
	fullName, ref string
	pinned        string
	app           kept
}

// Poll checks, once, the branch of each app whose pushes come through a GitHub App
// GitHub can't reach, at most 30 for each installation, each in its turn, and hands
// a branch that moved past what the app runs to the pusher.
func (h *Handler) Poll(ctx context.Context) {
	h.mu.Lock()
	pusher := h.pusher
	h.mu.Unlock()
	if pusher == nil {
		return
	}
	targets, err := h.pollTargets(ctx)
	if err != nil {
		h.cfg.Logger.WarnContext(ctx, "can't read which apps to check for pushes", slog.String("error", err.Error()))
		return
	}
	h.forgetGone(targets)
	byInstallation := map[int64][]polled{}
	var order []int64
	for i := range targets {
		t := &targets[i]
		if _, ok := byInstallation[t.installation]; !ok {
			order = append(order, t.installation)
		}
		byInstallation[t.installation] = append(byInstallation[t.installation], *t)
	}
	now := h.now()
	for _, installation := range order {
		ts := byInstallation[installation]
		appID := ts[0].app.r.ID
		h.mu.Lock()
		w := h.watchOf(appID)
		polling := w.state == dockwellv1.GitHubDelivery_STATE_POLLING ||
			w.state == dockwellv1.GitHubDelivery_STATE_BACKING_OFF && !now.Before(w.backoffUntil)
		var picked []int
		if polling {
			if w.state == dockwellv1.GitHubDelivery_STATE_BACKING_OFF {
				w.state = dockwellv1.GitHubDelivery_STATE_POLLING
			}
			picked, w.turns[installation], w.every[installation] = due(len(ts), w.turns[installation], pollCap)
		}
		h.mu.Unlock()
		app, err := h.acting(ts[0].app)
		if err != nil {
			continue
		}
		for _, i := range picked {
			if !h.pollOne(ctx, app, appID, ts[i], pusher) {
				break // backing off: the rest wait their turn
			}
		}
	}
}

// pollOne checks one branch, and says whether to go on.
func (h *Handler) pollOne(ctx context.Context, app *github.App, appID string, t polled, pusher Pusher) bool {
	h.mu.Lock()
	was := h.branches[t.hook]
	h.mu.Unlock()
	short := strings.TrimPrefix(strings.TrimPrefix(t.ref, "refs/heads/"), "refs/tags/")
	commit, etag, rate, err := app.Head(ctx, t.installation, t.repository, t.fullName, short, was.etag)
	h.noted(appID, err)
	// GitHub failing backs off as asking too often does, rather than ask each
	// branch again a minute on (review L4).
	var e *github.Error
	if errors.As(err, &e) && e.Status >= 500 {
		rate.Limited = true
	}
	h.mu.Lock()
	w := h.watchOf(appID)
	w.slowDown(rate, h.now())
	backingOff := w.state == dockwellv1.GitHubDelivery_STATE_BACKING_OFF
	h.mu.Unlock()
	switch {
	case errors.Is(err, github.ErrNotModified):
		return !backingOff
	case err != nil:
		h.cfg.Logger.WarnContext(ctx, "can't check a branch for pushes", slog.String("repository", t.fullName), slog.String("error", err.Error()))
		return !backingOff
	}
	h.mu.Lock()
	h.branches[t.hook] = branch{etag: etag, commit: commit}
	h.mu.Unlock()
	if commit != t.pinned && commit != was.commit {
		if err := pusher.Polled(ctx, t.hook, t.ref, commit); err != nil {
			h.cfg.Logger.WarnContext(ctx, "can't deploy a push found by checking a branch", slog.String("repository", t.fullName), slog.String("error", err.Error()))
		}
	}
	return !backingOff
}

// forgetGone drops what polling keeps for hooks, installations and Apps no
// target names any more, so what it holds is bounded by what it checks (review
// L4). An App's delivery state stays while the App does.
func (h *Handler) forgetGone(targets []polled) {
	hooks, installations := map[string]bool{}, map[int64]bool{}
	for i := range targets {
		hooks[targets[i].hook], installations[targets[i].installation] = true, true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for hook := range h.branches {
		if !hooks[hook] {
			delete(h.branches, hook)
		}
	}
	for _, w := range h.watch {
		for in := range w.turns {
			if !installations[in] {
				delete(w.turns, in)
			}
		}
		for in := range w.every {
			if !installations[in] {
				delete(w.every, in)
			}
		}
	}
}

// pollTargets are the hooks through an App whose app builds a branch of a
// repository an installation of that App lists and reaches the app's project
// through, sorted by hook.
func (h *Handler) pollTargets(ctx context.Context) ([]polled, error) {
	var out []polled
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		hooks, err := tx.GitHooksVia(ctx, store.ViaGitHub)
		if err != nil {
			return err
		}
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		byName := map[string]kept{}
		for _, a := range apps {
			byName[a.r.Name] = a
		}
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for i := range hooks {
			hook := &hooks[i]
			// A hook whose maker is gone deploys nothing, so nothing is asked for it.
			if u, err := tx.GetUser(ctx, hook.UserID); err != nil || u.RemovedAt != nil {
				continue
			}
			r, err := tx.GetResourceByID(ctx, hook.ResourceID)
			if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
				continue
			} else if err != nil {
				return err
			}
			spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				continue
			}
			svc, ok := spec.(*resourcev1.Service)
			g := svc.GetBuild().GetGit()
			login := svc.GetBuild().GetGitLogin()
			if !ok || g == nil || login == "" || g.GetRef() == g.GetCommit() {
				continue // a commit named as the ref never moves
			}
			name, idText, _ := strings.Cut(login, "/")
			installation, err := strconv.ParseInt(idText, 10, 64)
			a, found := byName[name]
			if err != nil || !found {
				continue
			}
			in, err := tx.GetGitHubInstallation(ctx, installation)
			if err != nil || in.AppID != a.r.ID || !in.Usable() {
				continue
			}
			// As a delivery through it would, an installation narrowed away from the
			// app's project checks nothing for it.
			if project := projectIn(tree, r.Scope); project == "" || !githubapp.Reaches(a.spec, installation, project) {
				continue
			}
			full := fullName(h.cfg.Web, g.GetUrl())
			var repo int64
			for _, rp := range in.Repositories {
				if full != "" && strings.EqualFold(rp.FullName, full) {
					repo = rp.ID
				}
			}
			if repo == 0 {
				continue
			}
			out = append(out, polled{hook: hook.ID, installation: installation, repository: repo, fullName: full, ref: g.GetRef(), pinned: g.GetCommit(), app: a})
		}
		return nil
	})
	slices.SortFunc(out, func(a, b polled) int { return strings.Compare(a.hook, b.hook) })
	return out, err
}

// KeepWatching checks the Apps' deliveries every 10 minutes, and polls for pushes
// each minute where they don't reach the panel, until ctx ends.
func (h *Handler) KeepWatching(ctx context.Context) {
	h.CheckDeliveries(ctx)
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	deliveries := time.NewTicker(deliveriesEvery)
	defer deliveries.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deliveries.C:
			h.CheckDeliveries(ctx)
		case <-poll.C:
			h.Poll(ctx)
		}
	}
}
