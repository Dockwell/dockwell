// SPDX-License-Identifier: AGPL-3.0-only

// Package serviceupdates knows what's newer than what each service runs (ADR
// 0079): it checks every service every 6 hours, through the agent of a server
// it runs on, keeps what it found in the store, and says it on ListServices and
// ServiceUpdateService. A catalog install's is read from its catalog when it's
// asked for. Nothing here changes what runs.
package serviceupdates

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	// Every is how often each server's services are checked, give or take a
	// tenth so a fleet's servers don't ask at once.
	Every = 6 * time.Hour
	// startAfter is the least time after the control plane starts before a server
	// is first checked, so a restart asks nothing at once.
	startAfter = 2 * time.Minute
	// nowEvery is how often Check now checks a service or a server again.
	nowEvery = time.Minute
	// maxSkipped is how many skipped versions a service keeps, the oldest going
	// first.
	maxSkipped = 16
	// maxSnooze is the longest a snooze lasts.
	maxSnooze = 90 * 24 * time.Hour
	// catalogFor is how long what the catalog says of its installs is kept,
	// as a list of services asks it often.
	catalogFor = 30 * time.Second
)

// Agents reaches the fleet's agents.
type Agents interface {
	// Connected reports a server whose agent is connected now.
	Connected(server string) bool
	// ImageUpdates asks a server's agent what's newer than an image it runs.
	ImageUpdates(ctx context.Context, server string, call *agentv1.ImageUpdatesCall) (*agentv1.ImageUpdatesResult, error)
}

// CatalogInstall is what a catalog install can move to.
type CatalogInstall struct {
	// FromVersion is the app's version it runs, and ToVersion and ToRelease the
	// newer release's, both empty and 0 when it's up to date.
	FromVersion, ToVersion string
	ToRelease              uint32
	// Release is the release of its entry it runs.
	Release uint32
	// Notes is where the app's releases are, an https address, or empty.
	Notes string
}

// Keeper knows what's newer than what each service runs.
type Keeper struct {
	DB      *store.DB
	Secrets *secrets.Keeper
	Agents  Agents
	// Catalog says, of each catalog install, by project ID, what it can move to.
	// Nil knows no catalog.
	Catalog func(ctx context.Context) (map[string]CatalogInstall, error)
	// Sources reads what a Git repository's branch names now. Nil checks no app
	// built from Git.
	Sources revision.Sources
	Logger  *slog.Logger
	// Now is the clock; nil takes time.Now.
	Now func() time.Time
	// Every is how often a server is checked: the package's Every unless set.
	Every time.Duration

	mu sync.Mutex
	// next is when each server is checked next, and running says which are now.
	next    map[string]time.Time
	running map[string]bool
	// askedAt is when Check now last checked each service or server.
	askedAt map[string]time.Time
	// limits holds off a registry from a server, by server and registry.
	limits map[string]holdOff
	// counted is what Counting found last, and when.
	counted   []Counted
	countedAt time.Time
	// catalog is what the catalog said last, and when.
	catalog   map[string]CatalogInstall
	catalogAt time.Time
	started   time.Time
}

// holdOff is a registry a server leaves alone until a time, and the step to
// wait the next time it says no.
type holdOff struct {
	until time.Time
	step  time.Duration
	why   dockwellv1.UpdateProblem
}

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Keeper) every() time.Duration {
	if k.Every > 0 {
		return k.Every
	}
	return Every
}

// jittered is d give or take a tenth.
func jittered(d time.Duration) time.Duration {
	tenth := int64(d / 10)
	if tenth <= 0 {
		return d
	}
	return d - time.Duration(tenth) + time.Duration(rand.Int64N(2*tenth+1)) //nolint:gosec // spreading checks needs no secret randomness
}

// found is one newer version as the store keeps it: the AvailableUpdate, as
// JSON, without skipped, which is read from what a person skipped.
type found = json.RawMessage

func encodeFound(list []*dockwellv1.AvailableUpdate) string {
	if len(list) == 0 {
		return ""
	}
	out := make([]found, 0, len(list))
	for _, a := range list {
		b, err := protojson.Marshal(a)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func decodeFound(s string) []*dockwellv1.AvailableUpdate {
	if s == "" {
		return nil
	}
	var raw []found
	if json.Unmarshal([]byte(s), &raw) != nil {
		return nil
	}
	out := make([]*dockwellv1.AvailableUpdate, 0, len(raw))
	for _, r := range raw {
		a := &dockwellv1.AvailableUpdate{}
		if protojson.Unmarshal(r, a) == nil {
			out = append(out, a)
		}
	}
	return out
}

func decodeSkipped(s string) []string {
	var out []string
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

// target is what skipping an update names it by: its version, its new digest
// for a new build, or its commit.
func target(a *dockwellv1.AvailableUpdate) string {
	switch a.GetKind() {
	case dockwellv1.UpdateKind_UPDATE_KIND_DIGEST:
		return a.GetToDigest()
	case dockwellv1.UpdateKind_UPDATE_KIND_COMMIT:
		return a.GetToCommit()
	case dockwellv1.UpdateKind_UPDATE_KIND_UNSPECIFIED, dockwellv1.UpdateKind_UPDATE_KIND_PATCH, dockwellv1.UpdateKind_UPDATE_KIND_MINOR,
		dockwellv1.UpdateKind_UPDATE_KIND_MAJOR, dockwellv1.UpdateKind_UPDATE_KIND_RELEASE:
	}
	return a.GetToVersion()
}

// rank orders kinds smallest step first.
func rank(k dockwellv1.UpdateKind) int {
	switch k {
	case dockwellv1.UpdateKind_UPDATE_KIND_DIGEST, dockwellv1.UpdateKind_UPDATE_KIND_RELEASE, dockwellv1.UpdateKind_UPDATE_KIND_COMMIT:
		return 0
	case dockwellv1.UpdateKind_UPDATE_KIND_PATCH:
		return 1
	case dockwellv1.UpdateKind_UPDATE_KIND_MINOR:
		return 2
	case dockwellv1.UpdateKind_UPDATE_KIND_MAJOR:
		return 3
	case dockwellv1.UpdateKind_UPDATE_KIND_UNSPECIFIED:
	}
	return 4
}

func sortAvailable(list []*dockwellv1.AvailableUpdate) {
	slices.SortStableFunc(list, func(a, b *dockwellv1.AvailableUpdate) int { return rank(a.GetKind()) - rank(b.GetKind()) })
}

// Subject is a service, as a list read it, for saying what's newer than what it
// runs.
type Subject struct {
	ID string
	// Environment is its environment's ID.
	Environment string
	// Spec is its latest revision's, nil when it can't be read.
	Spec *resourcev1.Service
	// Digests are what its latest revision ran, by server, where that's known.
	Digests map[string]string
}

// running is what a service runs now: the digest of its latest revision's
// first deployment that has one.
func running(digests map[string]string) string {
	servers := make([]string, 0, len(digests))
	for s, d := range digests {
		if d != "" {
			servers = append(servers, s)
		}
	}
	slices.Sort(servers)
	if len(servers) == 0 {
		return ""
	}
	return digests[servers[0]]
}

// imageOf is the image a service's check is for, as its spec names it.
func imageOf(spec *resourcev1.Service) string {
	return strings.TrimSpace(spec.GetApp().GetImage())
}

// Describe says what's newer than what each subject runs, by its ID: what the
// last check found, with what a person skipped and snoozed, and a catalog
// install's from its catalog now. A subject never checked, and no catalog
// install, has none.
func (k *Keeper) Describe(ctx context.Context, subjects []Subject) (map[string]*dockwellv1.ServiceUpdate, error) {
	out := make(map[string]*dockwellv1.ServiceUpdate, len(subjects))
	if len(subjects) == 0 {
		return out, nil
	}
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		ids[i] = s.ID
	}
	var rows map[string]store.ServiceUpdate
	projects := map[string]string{}
	err := k.DB.View(ctx, func(tx *store.Tx) (err error) {
		if rows, err = tx.ServiceUpdates(ctx, ids); err != nil {
			return err
		}
		return environmentsProjects(ctx, tx, projects)
	})
	if err != nil {
		return nil, err
	}
	installs := k.catalogInstalls(ctx)
	now := k.now()
	for _, s := range subjects {
		row, kept := rows[s.ID]
		install, isInstall := installs[projects[s.Environment]]
		var u *dockwellv1.ServiceUpdate
		switch {
		case isInstall:
			u = catalogUpdate(install, now)
			if kept && s.Spec != nil && catalog.Moving(imageOf(s.Spec)) {
				newBuilds(u, k.keptUpdate(row, s), install)
			}
		case kept && s.Spec != nil:
			u = k.keptUpdate(row, s)
		default:
			continue
		}
		if u == nil {
			continue
		}
		applyPerson(u, row, now)
		out[s.ID] = u
	}
	return out, nil
}

// keptUpdate is what the last check of a service found, unless what it runs has
// moved on since: another image, which the next check reads afresh, or the new
// build a digest update offered.
func (k *Keeper) keptUpdate(row store.ServiceUpdate, s Subject) *dockwellv1.ServiceUpdate {
	if row.State == 0 && row.Image == "" {
		return nil // a person's skip or snooze, before any check
	}
	u := &dockwellv1.ServiceUpdate{
		Check:         dockwellv1.UpdateCheck(row.State),
		Problem:       dockwellv1.UpdateProblem(row.Problem),
		ProblemDetail: row.Detail,
		Source:        sourceOf(s.Spec),
	}
	if !row.CheckedAt.IsZero() {
		u.CheckTime = timestamppb.New(row.CheckedAt)
	}
	if !row.NextAt.IsZero() {
		u.NextCheckTime = timestamppb.New(row.NextAt)
	}
	if row.Image != checkedAs(s.Spec) {
		// It runs another image now, which the next check reads.
		u.Check, u.Problem, u.ProblemDetail, u.CheckTime = dockwellv1.UpdateCheck_UPDATE_CHECK_UNSPECIFIED, 0, "", nil
		return u
	}
	now := running(s.Digests)
	for _, a := range decodeFound(row.Found) {
		if a.GetKind() == dockwellv1.UpdateKind_UPDATE_KIND_DIGEST && now != "" && now == a.GetToDigest() {
			continue // what it offered runs now
		}
		u.Available = append(u.Available, a)
	}
	return u
}

// checkedAs is what a check of a service is kept under, so a check of another
// image, commit or runtime reads as not done.
func checkedAs(spec *resourcev1.Service) string {
	if g := spec.GetBuild().GetGit(); g != nil {
		return g.GetUrl() + "#" + g.GetRef() + "@" + g.GetCommit()
	}
	return imageOf(spec)
}

func sourceOf(spec *resourcev1.Service) dockwellv1.UpdateSource {
	switch {
	case spec.GetBuild() != nil:
		return dockwellv1.UpdateSource_UPDATE_SOURCE_GIT
	case spec.GetRuntime() != nil:
		return dockwellv1.UpdateSource_UPDATE_SOURCE_RUNTIME
	}
	return dockwellv1.UpdateSource_UPDATE_SOURCE_IMAGE
}

// catalogUpdate is what a catalog install can move to, as its catalog says now.
func catalogUpdate(c CatalogInstall, now time.Time) *dockwellv1.ServiceUpdate {
	u := &dockwellv1.ServiceUpdate{
		Source:    dockwellv1.UpdateSource_UPDATE_SOURCE_CATALOG,
		Check:     dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE,
		CheckTime: timestamppb.New(now),
	}
	if c.ToRelease == 0 {
		return u
	}
	kind := dockwellv1.UpdateKind_UPDATE_KIND_RELEASE
	switch registry.Step(c.FromVersion, c.ToVersion) {
	case registry.KindPatch:
		kind = dockwellv1.UpdateKind_UPDATE_KIND_PATCH
	case registry.KindMinor:
		kind = dockwellv1.UpdateKind_UPDATE_KIND_MINOR
	case registry.KindMajor:
		kind = dockwellv1.UpdateKind_UPDATE_KIND_MAJOR
	case registry.KindNone:
	}
	u.Available = []*dockwellv1.AvailableUpdate{{
		Kind: kind, FromVersion: c.FromVersion, ToVersion: c.ToVersion, CatalogRelease: c.ToRelease, NotesUrl: c.Notes,
	}}
	return u
}

// newBuilds adds to a catalog install's update each new build its check found of an
// image the install runs by a tag that moves, such as latest, which upgrading the
// install takes (ADR 0085). Each is offered at the release it runs, as it upgrades
// through its catalog and takes no image.
func newBuilds(u, found *dockwellv1.ServiceUpdate, install CatalogInstall) {
	if found == nil {
		return
	}
	for _, a := range found.GetAvailable() {
		if a.GetKind() != dockwellv1.UpdateKind_UPDATE_KIND_DIGEST {
			continue
		}
		a.CatalogRelease = install.Release
		u.Available = append(u.Available, a)
	}
	if len(u.Available) > 0 {
		u.Check = dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE
	}
	sortAvailable(u.Available)
}

// applyPerson marks what a person skipped, says a snooze, and sets the check's
// state by what's left.
func applyPerson(u *dockwellv1.ServiceUpdate, row store.ServiceUpdate, now time.Time) {
	skipped := decodeSkipped(row.Skipped)
	left := 0
	for _, a := range u.GetAvailable() {
		a.Skipped = slices.Contains(skipped, target(a))
		if !a.GetSkipped() {
			left++
		}
	}
	sortAvailable(u.Available)
	switch u.GetCheck() {
	case dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE, dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE:
		u.Check = dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE
		if left > 0 {
			u.Check = dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE
		}
	case dockwellv1.UpdateCheck_UPDATE_CHECK_UNSPECIFIED, dockwellv1.UpdateCheck_UPDATE_CHECK_FAILED, dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED:
	}
	if row.SnoozeUntil.After(now) {
		u.SnoozeTime = timestamppb.New(row.SnoozeUntil)
	}
}

// environmentsProjects fills each live environment's project, by environment ID.
func environmentsProjects(ctx context.Context, tx *store.Tx, out map[string]string) error {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return err
	}
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			out[e.ID] = p.ID
		}
	}
	return nil
}

// catalogInstalls is what the catalog says of its installs, kept a little, as
// a list asks it often. One that can't be read is logged, and lists no installs.
func (k *Keeper) catalogInstalls(ctx context.Context) map[string]CatalogInstall {
	if k.Catalog == nil {
		return nil
	}
	k.mu.Lock()
	if k.catalog != nil && k.now().Sub(k.catalogAt) < catalogFor {
		c := k.catalog
		k.mu.Unlock()
		return c
	}
	k.mu.Unlock()
	c, err := k.Catalog(ctx)
	if err != nil {
		if k.Logger != nil && ctx.Err() == nil {
			k.Logger.WarnContext(ctx, "can't read what the catalog's installs can move to", slog.String("error", err.Error()))
		}
		return nil
	}
	k.mu.Lock()
	k.catalog, k.catalogAt = c, k.now()
	k.mu.Unlock()
	return c
}

// NotesOf is where a source repository's release notes are: GitHub's and
// GitLab's releases pages, and any other https address as itself. Empty for
// anything but https.
func NotesOf(source string) string {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch strings.ToLower(u.Host) {
	case "github.com":
		if len(parts) >= 2 {
			u.Path = "/" + parts[0] + "/" + parts[1] + "/releases"
		}
	case "gitlab.com":
		if len(parts) >= 2 {
			u.Path = "/" + strings.Join(parts, "/") + "/-/releases"
		}
	}
	return u.String()
}

// FromAgents reaches the fleet's agents through their sessions.
func FromAgents(a *agentapi.Agents) Agents { return sessions{a} }

type sessions struct{ agents *agentapi.Agents }

func (s sessions) Connected(server string) bool {
	_, ok := s.agents.Session(server)
	return ok
}

func (s sessions) ImageUpdates(ctx context.Context, server string, call *agentv1.ImageUpdatesCall) (*agentv1.ImageUpdatesResult, error) {
	session, ok := s.agents.Session(server)
	if !ok {
		return nil, errors.New("its agent isn't connected")
	}
	return session.ImageUpdates(ctx, call)
}
