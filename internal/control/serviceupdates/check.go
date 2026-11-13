// SPDX-License-Identifier: AGPL-3.0-only

package serviceupdates

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

const (
	// tickEvery is how often the loop looks for a server that's due, and for a
	// service that's new or runs another image, which it checks then.
	tickEvery = 2 * time.Minute
	// limitedFirst is how long a registry that limits how often it's asked is
	// left alone the first time, doubling each time to limitedMost.
	limitedFirst = time.Hour
	limitedMost  = 24 * time.Hour
	// silentFor is how long a registry that didn't answer is left alone.
	silentFor = time.Hour
	// pageSize is how many services a load reads at a time.
	pageSize = 500
)

// commitRe is a commit's 40 hex digits, which a ref names for good.
var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// subject is a service as a check reads it.
type subject struct {
	Subject
	// Name is the service's name.
	Name string
	// home is the server it's checked through: the first, by ID, that its latest
	// revision runs on. Empty when it runs nowhere yet.
	home string
	// row is what was kept of it, and kept whether there was a row.
	row  store.ServiceUpdate
	kept bool
	// install says it's a catalog install's, which its catalog says of.
	install bool
	// pinned says Dockwell picks and pins its image itself, and moves it with its
	// own releases, as a website's web and PHP servers' (ADR 0039): nothing is
	// checked for it, and it reads as kept current by Dockwell.
	pinned bool
}

// checksImage says a check reads what's newer than the subject's image from its
// registry: any service's but a catalog install's, whose catalog says what's newer,
// unless it runs an image by a tag that moves, such as latest, whose new builds
// only its registry knows of (ADR 0085).
func (s *subject) checksImage() bool {
	return !s.install || catalog.Moving(imageOf(s.Spec))
}

// Run checks each server's services every Every, until ctx ends, and a
// service that's new or runs another image soon after it does. Nothing is
// checked in the first minutes after it starts.
func (k *Keeper) Run(ctx context.Context) {
	k.mu.Lock()
	k.started = k.now()
	k.mu.Unlock()
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := k.tick(ctx); err != nil && ctx.Err() == nil && k.Logger != nil {
			k.Logger.WarnContext(ctx, "can't check the apps' updates; it tries again in a few minutes", slog.String("error", err.Error()))
		}
	}
}

// tick checks each connected server that's due, and on the rest each service
// that's new or runs another image, and forgets services that are gone.
func (k *Keeper) tick(ctx context.Context) error {
	subjects, gone, err := k.load(ctx)
	if err != nil {
		return err
	}
	if len(gone) > 0 {
		if err := k.DB.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.DeleteServiceUpdates(ctx, gone)
			return err
		}); err != nil {
			return err
		}
	}
	now := k.now()
	k.mu.Lock()
	if k.started.IsZero() {
		k.started = now
	}
	warm := now.Sub(k.started) >= startAfter
	k.mu.Unlock()
	if !warm {
		return nil
	}
	byHome := map[string][]subject{}
	var marking []subject
	for i := range subjects {
		s := &subjects[i]
		switch {
		case s.pinned:
			// Said once, and again as it runs another image: no registry is asked.
			if !s.kept || s.row.Problem != keptByDockwell || s.row.Image != checkedAs(s.Spec) {
				marking = append(marking, *s)
			}
		case s.home != "" && s.checksImage():
			byHome[s.home] = append(byHome[s.home], *s)
		}
	}
	if err := k.markKept(ctx, marking); err != nil {
		return err
	}
	for _, server := range slices.Sorted(maps.Keys(byHome)) {
		list := byHome[server]
		if !k.Agents.Connected(server) {
			continue
		}
		if k.due(server, list, now) {
			k.pass(ctx, server, list)
			continue
		}
		var fresh []subject
		for i := range list {
			if s := &list[i]; !s.kept || s.row.Image != checkedAs(s.Spec) {
				fresh = append(fresh, *s)
			}
		}
		if len(fresh) > 0 {
			k.check(ctx, server, fresh)
		}
	}
	return nil
}

// due reports a server whose turn has come: Every after its services were
// last checked, as the store says the first time, and Every after its last
// pass after that.
func (k *Keeper) due(server string, list []subject, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.next == nil {
		k.next = map[string]time.Time{}
	}
	next, ok := k.next[server]
	if !ok {
		var oldest time.Time
		for i := range list {
			s := &list[i]
			if s.row.TriedAt.IsZero() {
				oldest = time.Time{}
				break
			}
			if oldest.IsZero() || s.row.TriedAt.Before(oldest) {
				oldest = s.row.TriedAt
			}
		}
		next = now
		if !oldest.IsZero() {
			next = oldest.Add(jittered(k.every()))
		}
		k.next[server] = next
	}
	return !now.Before(next)
}

// pass checks every service on a server, once at a time per server, and sets
// when it's next due.
func (k *Keeper) pass(ctx context.Context, server string, list []subject) {
	k.mu.Lock()
	if k.running == nil {
		k.running = map[string]bool{}
	}
	if k.running[server] {
		k.mu.Unlock()
		return
	}
	k.running[server] = true
	if k.next == nil {
		k.next = map[string]time.Time{}
	}
	k.next[server] = k.now().Add(jittered(k.every()))
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		delete(k.running, server)
		k.mu.Unlock()
	}()
	k.check(ctx, server, list)
}

// nextOf is when a server is next due, as things stand.
func (k *Keeper) nextOf(server string, now time.Time) time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	if next, ok := k.next[server]; ok && next.After(now) {
		return next
	}
	return now.Add(k.every())
}

// check checks services on a server: one call to its agent for each image they
// run, shared by every service that runs it, and one read of each repository
// branch the apps built from Git follow. It keeps what it found, and returns it.
func (k *Keeper) check(ctx context.Context, server string, list []subject) {
	now := k.now()
	next := k.nextOf(server, now)
	images := map[string]*agentv1.ImageUpdatesResult{}
	failed := map[string]error{}
	commits := map[string]string{}
	var rows []store.ServiceUpdate
	for i := range list {
		s := &list[i]
		if ctx.Err() != nil {
			break
		}
		row := store.ServiceUpdate{ServiceID: s.ID, ServerID: server, Image: checkedAs(s.Spec), TriedAt: now, NextAt: next}
		if s.pinned {
			row.State, row.Problem = notChecked, keptByDockwell
			rows = append(rows, row)
			continue
		}
		var prev []*dockwellv1.AvailableUpdate
		if s.kept && s.row.Image == row.Image {
			prev, row.CheckedAt, row.Found = decodeFound(s.row.Found), s.row.CheckedAt, s.row.Found
		}
		if g := s.Spec.GetBuild().GetGit(); g != nil {
			k.checkCommit(ctx, g, &row, prev, commits, failed, now)
			rows = append(rows, row)
			continue
		}
		image := imageOf(s.Spec)
		ref, err := registry.Parse(image)
		if image == "" || err != nil {
			row.State, row.Problem = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED), int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_LOCAL_IMAGE)
			rows = append(rows, row)
			continue
		}
		res, err := k.readImage(ctx, server, ref, image, s, images, failed, now)
		k.compose(&row, s, ref, image, res, err, prev, now)
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return
	}
	err := k.DB.Update(ctx, func(tx *store.Tx) error {
		for i := range rows {
			if err := tx.PutServiceUpdateCheck(ctx, rows[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && k.Logger != nil && ctx.Err() == nil {
		k.Logger.WarnContext(ctx, "can't keep what the check of the apps' updates found", slog.String("server", server), slog.String("error", err.Error()))
	}
}

// What a service Dockwell pins reads as.
var (
	notChecked     = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED)
	keptByDockwell = int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_KEPT_BY_DOCKWELL)
)

// markKept notes services whose image Dockwell pins as kept current by Dockwell,
// with nothing checked, wherever they run, connected or not.
func (k *Keeper) markKept(ctx context.Context, list []subject) error {
	if len(list) == 0 {
		return nil
	}
	now := k.now()
	return k.DB.Update(ctx, func(tx *store.Tx) error {
		for i := range list {
			s := &list[i]
			row := store.ServiceUpdate{
				ServiceID: s.ID, ServerID: s.home, Image: checkedAs(s.Spec), TriedAt: now, NextAt: now.Add(k.every()),
				State: notChecked, Problem: keptByDockwell,
			}
			if err := tx.PutServiceUpdateCheck(ctx, row); err != nil {
				return err
			}
		}
		return nil
	})
}

// pinnedBy says Dockwell picks and pins a service's image itself: a website's web
// server, which runs the image the ingress pins, and its PHP, whose version is
// picked on the website's page (ADR 0039).
func pinnedBy(websites map[string]bool, env, name string) bool {
	return websites[env] && (name == website.WebService || name == website.PHPService)
}

// websiteEnvironments are the environments of the projects made as websites.
func (k *Keeper) websiteEnvironments(ctx context.Context, tx *store.Tx) (map[string]bool, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range tree.Projects {
		r, err := tx.GetResource(ctx, string(scope.ProjectKind), tree.Organization.ID, p.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		spec, err := k.Secrets.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			continue // what can't be read is checked as any other's
		}
		if project, ok := spec.(*resourcev1.Project); ok && project.GetWebsite() != nil {
			for _, e := range p.Environments {
				out[e.ID] = true
			}
		}
	}
	return out, nil
}

// readImage asks a server's agent what's newer than an image, once for every
// service that runs it, unless the registry is being left alone.
func (k *Keeper) readImage(ctx context.Context, server string, ref registry.Ref, image string, s *subject, images map[string]*agentv1.ImageUpdatesResult, failed map[string]error, now time.Time) (*agentv1.ImageUpdatesResult, error) {
	key := ref.Name() + ":" + ref.Tag
	if res, ok := images[key]; ok {
		return res, nil
	}
	if err, ok := failed[key]; ok {
		return nil, err
	}
	if h, ok := k.heldOff(server, ref.Registry, now); ok {
		err := &heldOffError{h}
		failed[key] = err
		return nil, err
	}
	// A catalog install moves on by its catalog's releases, and by a new build of a
	// tag that moves, never to another tag.
	call := &agentv1.ImageUpdatesCall{Reference: image, Tags: s.Spec.GetRuntime() == nil && !s.install}
	if d := s.Digests[server]; d != "" {
		call.Running = ref.Name() + "@" + d
	}
	res, err := k.Agents.ImageUpdates(ctx, server, call)
	if err != nil {
		failed[key] = err
		return nil, err
	}
	k.heard(server, ref.Registry, res.GetAnswer(), now)
	images[key] = res
	return res, nil
}

// heldOffError is a registry the server is leaving alone.
type heldOffError struct{ h holdOff }

func (e *heldOffError) Error() string {
	return "it's asked again after " + e.h.until.UTC().Format(time.RFC3339)
}

// heldOff says whether a server is leaving a registry alone now.
func (k *Keeper) heldOff(server, reg string, now time.Time) (holdOff, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h, ok := k.limits[server+" "+reg]
	return h, ok && now.Before(h.until)
}

// heard notes what a registry answered a server: one that limits how often
// it's asked is left alone an hour, then two, doubling to a day, and one that
// didn't answer an hour. One that answered is asked as usual again.
func (k *Keeper) heard(server, reg string, a dockwellv1.ImageAnswer, now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.limits == nil {
		k.limits = map[string]holdOff{}
	}
	key := server + " " + reg
	switch a {
	case dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED:
		h := k.limits[key]
		h.step = min(max(h.step*2, limitedFirst), limitedMost)
		h.until, h.why = now.Add(h.step), dockwellv1.UpdateProblem_UPDATE_PROBLEM_RATE_LIMITED
		k.limits[key] = h
	case dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE:
		h := k.limits[key]
		h.until, h.why = now.Add(silentFor), dockwellv1.UpdateProblem_UPDATE_PROBLEM_UNREACHABLE
		k.limits[key] = h
	case dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND, dockwellv1.ImageAnswer_IMAGE_ANSWER_UNSPECIFIED:
		delete(k.limits, key)
	}
}

// compose writes into row what a read of an image says of one service.
func (k *Keeper) compose(row *store.ServiceUpdate, s *subject, ref registry.Ref, image string, res *agentv1.ImageUpdatesResult, err error, prev []*dockwellv1.AvailableUpdate, now time.Time) {
	failedWith := func(p dockwellv1.UpdateProblem, detail string) {
		row.State, row.Problem, row.Detail = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_FAILED), int32(p), detail
	}
	var held *heldOffError
	switch {
	case errors.As(err, &held):
		failedWith(held.h.why, "")
		row.NextAt = held.h.until
		return
	case errors.Is(err, agentapi.ErrOlderAgent):
		failedWith(dockwellv1.UpdateProblem_UPDATE_PROBLEM_AGENT_OUTDATED, "")
		return
	case err != nil:
		failedWith(dockwellv1.UpdateProblem_UPDATE_PROBLEM_UNREACHABLE, line(err.Error()))
		return
	}
	digest := s.Digests[row.ServerID]
	switch res.GetAnswer() {
	case dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND:
	case dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND:
		if digest == "" && ref.Digest == "" {
			// Run, with no digest a registry gave it: built or loaded on the server.
			row.State, row.Problem = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED), int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_LOCAL_IMAGE)
			return
		}
		failedWith(dockwellv1.UpdateProblem_UPDATE_PROBLEM_PRIVATE_OR_NOT_FOUND, res.GetDetail())
		return
	case dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED:
		failedWith(dockwellv1.UpdateProblem_UPDATE_PROBLEM_RATE_LIMITED, res.GetDetail())
		if h, ok := k.heldOff(row.ServerID, ref.Registry, now); ok {
			row.NextAt = h.until
		}
		return
	default:
		failedWith(dockwellv1.UpdateProblem_UPDATE_PROBLEM_UNREACHABLE, res.GetDetail())
		return
	}
	found := Offers(image, res, digest)
	carryFound(found, prev, now)
	row.CheckedAt, row.Found, row.Problem, row.Detail = now, encodeFound(found), 0, ""
	row.State = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE)
	if len(found) > 0 {
		row.State = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE)
	}
}

// Offers is what's newer than an image a service runs, by what its server's
// agent found: a new build of its tag, where the digest that runs, or the one
// it's pinned to, isn't the one its tag points at now, and each newer tag the
// agent found, smallest step first.
func Offers(image string, res *agentv1.ImageUpdatesResult, digest string) []*dockwellv1.AvailableUpdate {
	ref, err := registry.Parse(image)
	if err != nil {
		return nil
	}
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	notes := NotesOf(res.GetSource())
	from := digest
	if from == "" {
		from = ref.Digest
	}
	var out []*dockwellv1.AvailableUpdate
	if now := res.GetDigest(); from != "" && now != "" && from != now {
		// The same reference, pulled again; one pinned by digest is pinned to the new one.
		to := image
		if ref.Digest != "" {
			to = retag(image, tag) + "@" + now
		}
		out = append(out, &dockwellv1.AvailableUpdate{
			Kind: dockwellv1.UpdateKind_UPDATE_KIND_DIGEST, FromVersion: tag, ToVersion: tag, FromImage: image, ToImage: to,
			FromDigest: from, ToDigest: now, ReleaseTime: res.GetPushTime(), NotesUrl: notes,
		})
	}
	for _, n := range res.GetNewer() {
		switch n.GetKind() {
		case dockwellv1.UpdateKind_UPDATE_KIND_PATCH, dockwellv1.UpdateKind_UPDATE_KIND_MINOR, dockwellv1.UpdateKind_UPDATE_KIND_MAJOR:
		default:
			continue
		}
		out = append(out, &dockwellv1.AvailableUpdate{
			Kind: n.GetKind(), FromVersion: tag, ToVersion: n.GetName(), FromImage: image, ToImage: retag(image, n.GetName()),
			FromDigest: from, ToDigest: n.GetDigest(), ReleaseTime: n.GetPushTime(), NotesUrl: notes,
		})
	}
	sortAvailable(out)
	return out
}

// carryFound keeps when each update was first found, from what the check
// before found of the same, and is now for the rest.
func carryFound(found, prev []*dockwellv1.AvailableUpdate, now time.Time) {
	for _, a := range found {
		a.FoundTime = timestamppb.New(now)
		for _, p := range prev {
			if p.GetKind() == a.GetKind() && target(p) == target(a) && p.GetFoundTime() != nil {
				a.FoundTime = p.GetFoundTime()
				break
			}
		}
	}
}

// retag is an image as written with another tag and no digest, such as
// postgres:16.4 for postgres:16.3, or ghcr.io/acme/web:2 for ghcr.io/acme/web.
func retag(image, tag string) string {
	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name + ":" + tag
}

// checkCommit writes into row whether the branch an app builds from has a newer
// commit than the one it runs, from one read of the repository's refs for every
// app that follows that branch. A tag or a commit named as the ref never moves.
func (k *Keeper) checkCommit(ctx context.Context, g *resourcev1.GitSource, row *store.ServiceUpdate, prev []*dockwellv1.AvailableUpdate, commits map[string]string, failed map[string]error, now time.Time) {
	ref := g.GetRef()
	if k.Sources == nil || commitRe.MatchString(ref) || strings.HasPrefix(ref, "refs/tags/") || g.GetCommit() == "" {
		row.State = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED)
		return
	}
	key := g.GetUrl() + "#" + ref
	commit, ok := commits[key]
	if !ok {
		if err, seen := failed[key]; seen {
			row.State, row.Problem, row.Detail = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_FAILED), int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_UNREACHABLE), line(err.Error())
			return
		}
		got, full, err := k.Sources.Resolve(ctx, g.GetUrl(), ref)
		if err != nil {
			failed[key] = err
			row.State, row.Problem, row.Detail = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_FAILED), int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_UNREACHABLE), line(err.Error())
			return
		}
		if !strings.HasPrefix(full, "refs/heads/") {
			got = "" // a tag, which never moves
		}
		commit, commits[key] = got, got
	}
	row.CheckedAt, row.Problem, row.Detail, row.Found = now, 0, "", ""
	row.State = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE)
	if commit == "" || commit == g.GetCommit() {
		return
	}
	a := &dockwellv1.AvailableUpdate{
		Kind: dockwellv1.UpdateKind_UPDATE_KIND_COMMIT, FromVersion: short(g.GetCommit()), ToVersion: short(commit), ToCommit: commit,
		NotesUrl: compareURL(g.GetUrl(), g.GetCommit(), commit),
	}
	found := []*dockwellv1.AvailableUpdate{a}
	carryFound(found, prev, now)
	row.Found, row.State = encodeFound(found), int32(dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE)
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// compareURL is GitHub's page of what changed between two commits, for a
// repository there, and empty elsewhere.
func compareURL(repo, from, to string) string {
	notes := NotesOf(repo)
	if !strings.HasPrefix(notes, "https://github.com/") || !strings.HasSuffix(notes, "/releases") {
		return ""
	}
	return strings.TrimSuffix(notes, "/releases") + "/compare/" + from + "..." + to
}

// line is an error's first line, at most 200 bytes of it.
func line(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// load reads every service as a check needs it, and the services kept that
// are gone.
func (k *Keeper) load(ctx context.Context) ([]subject, []string, error) {
	var out []subject
	var gone []string
	installs := k.catalogInstalls(ctx)
	err := k.DB.View(ctx, func(tx *store.Tx) error {
		projects := map[string]string{}
		if err := environmentsProjects(ctx, tx, projects); err != nil {
			return err
		}
		websites, err := k.websiteEnvironments(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.ServiceUpdates(ctx, nil)
		if err != nil {
			return err
		}
		live := map[string]bool{}
		after := ""
		for {
			list, err := tx.PageResources(ctx, string(service.Kind), "", after, pageSize)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				break
			}
			after = list[len(list)-1].ID
			ids := make([]string, len(list))
			specs := make([]store.StoredSpec, len(list))
			for i := range list {
				ids[i] = list[i].ID
				specs[i] = store.StoredSpec{At: store.InRevision(list[i].ID, list[i].Kind, list[i].Revision), Spec: list[i].Spec}
			}
			deployments, err := tx.LatestDeployments(ctx, ids)
			if err != nil {
				return err
			}
			decoded, unreadable, err := k.Secrets.DecodeEach(ctx, tx, specs)
			if err != nil {
				return err
			}
			for i := range list {
				live[list[i].ID] = true
				if unreadable[i] != nil {
					continue
				}
				spec, ok := decoded[i].(*resourcev1.Service)
				if !ok {
					continue
				}
				s := subject{Subject: Subject{ID: list[i].ID, Environment: list[i].Scope, Spec: spec, Digests: map[string]string{}}, Name: list[i].Name}
				for j := range deployments[list[i].ID] {
					d := &deployments[list[i].ID][j]
					if d.State != store.DeploymentLive {
						continue
					}
					s.Digests[d.Server] = d.Digest
					if s.home == "" || d.Server < s.home {
						s.home = d.Server
					}
				}
				s.row, s.kept = rows[s.ID]
				_, s.install = installs[projects[s.Environment]]
				s.pinned = pinnedBy(websites, s.Environment, s.Name)
				out = append(out, s)
			}
			if len(list) < pageSize {
				break
			}
		}
		for id := range rows {
			if !live[id] {
				gone = append(gone, id)
			}
		}
		return nil
	})
	return out, gone, err
}
