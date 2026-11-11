// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// What reading how a commit builds keeps, so a form's reading and the plan that
// follows it read the commit once; and how many readings one caller may have under
// way at once, as a server reads few at a time for everyone.
const (
	detectedFor  = 15 * time.Minute
	detectedKept = 64
	readsEach    = 2
)

// ErrServerOffline says a server's agent isn't connected, so it couldn't read how a
// commit builds.
var ErrServerOffline = errors.New("the server's agent isn't connected")

// detected is a reading kept.
type detected struct {
	at time.Time
	d  *resource.BuildDetection
}

// callerKey holds who asked for a reading through DetectBuild, by the audit log's
// name, whose readings under way are counted.
type callerKey struct{}

// errTooManyReadings refuses a caller a reading beyond readsEach under way.
var errTooManyReadings = authapi.ReasonError(connect.CodeResourceExhausted,
	fmt.Errorf("you have %d readings under way already. Wait for one to end, then read again", readsEach), "too_many_readings")

// reading is a reading under way, which the same request waits for rather than
// read the commit again (H2 of #336's review: a reading runs as you type).
type reading struct {
	done chan struct{}
	d    *resource.BuildDetection
	err  error
}

// SetSources has the handler read what a build's branch names with s.
func (h *Handler) SetSources(s revision.Sources) { h.sources = s }

var _ revision.Detector = (*Handler)(nil)

// maxBranches is how many of a repository's branches one answer lists, and
// listingsAtOnce how many listings the control server makes at once, for everyone:
// each may read up to gitref's bound of a repository's refs into memory.
const (
	maxBranches    = 500
	listingsAtOnce = 2
)

// errTooManyListings refuses a listing beyond listingsAtOnce under way.
var errTooManyListings = authapi.ReasonError(connect.CodeResourceExhausted,
	fmt.Errorf("the control server is reading %d repositories' branches already. Check again in a moment", listingsAtOnce), "too_many_readings")

// branchLister is a Sources that lists a repository's branches, as gitref's does.
type branchLister interface {
	Branches(ctx context.Context, url string) (head string, branches []string, err error)
}

// ListRepositoryBranches asks a repository for its branches, from the control
// server, under the guard a plan's own question has (gitref.Guard).
func (h *Handler) ListRepositoryBranches(ctx context.Context, req *connect.Request[dockwellv1.ListRepositoryBranchesRequest]) (*connect.Response[dockwellv1.ListRepositoryBranchesResponse], error) {
	// The URL is held to what a build's own may be, before anything asks it, and
	// asked as a build would fetch it: github.com/acme/web, as people paste it,
	// is https://github.com/acme/web (ADR 0046).
	url, err := service.CheckRepository(req.Msg.GetUrl())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	lister, ok := h.sources.(branchLister)
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane can't read a repository's branches"))
	}
	select {
	case h.listing <- struct{}{}:
		defer func() { <-h.listing }()
	default:
		return nil, errTooManyListings
	}
	head, branches, err := lister.Branches(ctx, url)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "unreadable")
	}
	res := &dockwellv1.ListRepositoryBranchesResponse{DefaultBranch: fit(head, 255)}
	if len(branches) > maxBranches {
		res.Total = int32(len(branches)) //nolint:gosec // a repository's answer is bounded (gitref)
		branches = branches[:maxBranches]
	}
	for _, b := range branches {
		if v := fit(b, 255); v != "" {
			res.Branches = append(res.Branches, v)
		}
	}
	return connect.NewResponse(res), nil
}

// Detect reads how a commit would build on a server, building nothing, and keeps
// what it found for a while. b names the commit in full. The same request while
// one is under way waits for it, and reads nothing more.
func (h *Handler) Detect(ctx context.Context, server string, b *resourcev1.ImageBuild, ownCommand bool) (*resource.BuildDetection, error) {
	key, err := proto.MarshalOptions{Deterministic: true}.Marshal(b)
	if err != nil {
		return nil, err
	}
	k := fmt.Sprintf("%s\x00%t\x00%s", server, ownCommand, key)
	for {
		h.mu.Lock()
		if got, ok := h.detected[k]; ok && h.now().Sub(got.at) < detectedFor {
			h.mu.Unlock()
			return got.d, nil
		}
		r, ok := h.reading[k]
		if !ok {
			break // with h.mu held
		}
		h.mu.Unlock()
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// The one that read went away, as a form does when you type on: read anew.
		if !errors.Is(r.err, context.Canceled) {
			return r.d, r.err
		}
	}
	// A new reading counts against its caller's, as one already under way doesn't.
	caller, _ := ctx.Value(callerKey{}).(string)
	if caller != "" {
		if h.readers[caller] >= readsEach {
			h.mu.Unlock()
			return nil, errTooManyReadings
		}
		h.readers[caller]++
		defer func() {
			h.mu.Lock()
			h.readers[caller]--
			if h.readers[caller] <= 0 {
				delete(h.readers, caller)
			}
			h.mu.Unlock()
		}()
	}
	r := &reading{done: make(chan struct{})}
	h.reading[k] = r
	h.mu.Unlock()
	r.d, r.err = h.read(ctx, server, b, ownCommand)
	h.mu.Lock()
	delete(h.reading, k)
	if r.err == nil {
		h.keep(k, r.d)
	}
	h.mu.Unlock()
	close(r.done)
	return r.d, r.err
}

// read asks the server's agent to read a commit.
func (h *Handler) read(ctx context.Context, server string, b *resourcev1.ImageBuild, ownCommand bool) (*resource.BuildDetection, error) {
	session, ok := h.agents.SessionPastMove(ctx, server, agentapi.MoveGrace)
	if !ok {
		return nil, ErrServerOffline
	}
	p, err := session.DetectBuild(ctx, &agentv1.DetectBuildCall{Build: b, OwnCommand: ownCommand})
	if err != nil {
		return nil, err
	}
	d := fitDetection(resourcepb.DetectionFrom(p))
	if d == nil {
		return nil, errors.New("the server said nothing of how the commit builds")
	}
	return d, nil
}

// keep keeps a reading, and forgets the oldest past detectedKept. h.mu is held.
func (h *Handler) keep(k string, d *resource.BuildDetection) {
	now := h.now()
	for key, got := range h.detected {
		if now.Sub(got.at) >= detectedFor {
			delete(h.detected, key)
		}
	}
	if len(h.detected) >= detectedKept {
		oldest := ""
		for key, got := range h.detected {
			if oldest == "" || got.at.Before(h.detected[oldest].at) {
				oldest = key
			}
		}
		delete(h.detected, oldest)
	}
	h.detected[k] = detected{at: now, d: d}
}

// DetectBuild reads how a commit would build, on a server, building nothing.
func (h *Handler) DetectBuild(ctx context.Context, req *connect.Request[dockwellv1.DetectBuildRequest]) (*connect.Response[dockwellv1.DetectBuildResponse], error) {
	_, caller := h.caller(ctx)
	ctx = context.WithValue(ctx, callerKey{}, caller)
	m := req.Msg
	var b *resourcev1.ImageBuild
	var placed []string
	ownCommand := false
	if m.GetId() != "" {
		svc, name, servers, err := h.services.ServiceOf(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
		if err != nil {
			return nil, err
		}
		got := svc.GetBuild()
		if got == nil {
			return nil, authapi.Invalid(fmt.Errorf("%s runs an image, so it builds nothing. Read build settings for it by passing them instead", name))
		}
		ownCommand = len(svc.GetApp().GetCommand()) > 0 || len(svc.GetApp().GetEntrypoint()) > 0
		b = proto.Clone(got).(*resourcev1.ImageBuild) //nolint:forcetypeassert // a clone is its own type
		// What a deploy would build: what the branch names now.
		if g := b.GetGit(); g != nil && g.GetRef() != g.GetCommit() {
			g.Commit = ""
		}
		placed = servers
	} else {
		if m.GetBuild() == nil {
			return nil, authapi.Invalid(errors.New("the request names no service and no build settings. Pass a service's ID or name, or the build settings to read"))
		}
		b = proto.Clone(m.GetBuild()).(*resourcev1.ImageBuild) //nolint:forcetypeassert // as above
	}
	if err := service.CheckBuild(b); err != nil {
		return nil, authapi.Invalid(err)
	}
	server, name, err := h.reader(ctx, m.GetServer(), placed)
	if err != nil {
		return nil, err
	}
	g := b.GetGit()
	ref := g.GetRef()
	full := strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") || ref != "" && ref == g.GetCommit()
	if g.GetCommit() == "" || !full {
		if h.sources == nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane can't read what a branch names"))
		}
		commit, branch, err := h.sources.Resolve(ctx, g.GetUrl(), g.GetRef())
		if err != nil {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "unreadable")
		}
		if g.GetCommit() == "" {
			g.Commit = commit
		}
		ref = branch
		if branch != "" {
			// The agent fetches the branch or tag by its full name, as a build does.
			g.Ref = branch
		}
	}
	d, err := h.Detect(ctx, server, b, ownCommand)
	switch {
	case errors.Is(err, errTooManyReadings):
		return nil, errTooManyReadings
	case errors.Is(err, ErrServerOffline):
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s's agent isn't connected, so it can't read how the commit builds. Try again once it reconnects, or name another server", name), "server_offline")
	case err != nil:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s couldn't read how the commit builds: %w", name, err), "unreadable")
	}
	return connect.NewResponse(&dockwellv1.DetectBuildResponse{Commit: g.GetCommit(), Ref: ref, ServerName: name, Detection: resourcepb.Detection(d)}), nil
}

// reader is the server that reads a commit: the one named, by ID or by name, or
// one the service runs on, or with neither, the only server. With more than one,
// and none named, it reads nothing until someone says where the app runs: a
// reading pulls Railpack, and checks out, on a server that may never run it.
func (h *Handler) reader(ctx context.Context, named string, placed []string) (id, name string, err error) {
	var servers []store.Server
	err = h.db.View(ctx, func(tx *store.Tx) error {
		servers, err = tx.ListServers(ctx)
		return err
	})
	if err != nil {
		return "", "", h.internal(ctx, "listing servers", err)
	}
	slices.SortFunc(servers, func(a, b store.Server) int { return strings.Compare(a.Name, b.Name) })
	switch {
	case named != "":
		for i := range servers {
			if s := &servers[i]; s.ID == named || s.Name == named {
				return s.ID, s.Name, nil
			}
		}
		return "", "", authapi.Invalid(fmt.Errorf("there's no server %s in the fleet. List them with ListServers", named))
	case len(placed) > 0:
		for i := range servers {
			if s := &servers[i]; slices.Contains(placed, s.ID) {
				return s.ID, s.Name, nil
			}
		}
	case len(servers) > 1:
		return "", "", authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("there are %d servers, so say which builds the app, and Dockwell reads the commit there", len(servers)), "choose_server")
	}
	connected := h.agents.Connected()
	for i := range servers {
		if s := &servers[i]; slices.Contains(connected, s.ID) {
			return s.ID, s.Name, nil
		}
	}
	return "", "", authapi.ReasonError(connect.CodeUnavailable, errors.New("no server's agent is connected, so nothing can read how the commit builds. Try again once one reconnects"), "server_offline")
}

// Bounds on what a server's reading may put on record.
const (
	maxDetectedItems = 32
	maxDetectedError = 4 << 10
)

// fitDetection makes what a server says it found fit to keep and show: what it
// names is short, with no control characters, and there are a few of each.
func fitDetection(d *resource.BuildDetection) *resource.BuildDetection {
	if d == nil {
		return nil
	}
	if d.Builder != resource.BuilderDockerfile && d.Builder != resource.BuilderRailpack {
		d.Builder = resource.BuilderRailpack
	}
	if d.Error != "" {
		// What kind of problem it is, read here too, as an older agent doesn't say.
		d.Problem, d.ProblemTool = resource.RailpackProblem(d.Error)
		d.ProblemTool = fit(d.ProblemTool, 64)
	}
	fitAll := func(in []string, n int) []string {
		var out []string
		for _, s := range in[:min(len(in), maxDetectedItems)] {
			if v := fit(s, n); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	d.Dockerfile, d.BaseImage, d.Railpack, d.Start = short(d.Dockerfile), fit(d.BaseImage, 200), fit(d.Railpack, 32), fit(d.Start, 1024)
	d.Ports = d.Ports[:min(len(d.Ports), maxDetectedItems)]
	d.Providers, d.Warnings = fitAll(d.Providers, 64), fitAll(d.Warnings, maxShort)
	d.Error = fitLines(d.Error, maxDetectedError)
	d.Packages = d.Packages[:min(len(d.Packages), maxDetectedItems)]
	for i := range d.Packages {
		p := &d.Packages[i]
		p.Name, p.Requested, p.Resolved, p.Source = fit(p.Name, 64), fit(p.Requested, 64), fit(p.Resolved, 64), fit(p.Source, 200)
	}
	d.Steps = d.Steps[:min(len(d.Steps), maxDetectedItems)]
	for i := range d.Steps {
		d.Steps[i].Name = fit(d.Steps[i].Name, 64)
		d.Steps[i].Commands = fitAll(d.Steps[i].Commands, 1024)
	}
	return d
}

// fitLines is fit for text of more than one line, which keeps its line ends.
func fitLines(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
