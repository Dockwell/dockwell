// SPDX-License-Identifier: AGPL-3.0-only

package bump

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/registry"
)

// The waits: how long a version is out before a bump takes it by itself (ADR
// 0100). A major waits as long as a minor, though it never merges by itself.
const (
	PatchWait = 3 * 24 * time.Hour
	MinorWait = 7 * 24 * time.Hour
)

// Lane names: the newest step within the levels, and the next step beyond them.
const (
	LaneWithin = "within"
	LaneBeyond = "beyond"
)

// Planner works out an entry's bumps.
type Planner struct {
	Sources Sources
	Now     time.Time
	// Seen is when the bump first saw each version where its registry doesn't
	// say when it was pushed, by the image's repository, tag and digest (SeenKey),
	// as an open bump's pull request records it.
	Seen map[string]time.Time
}

// Entry is an entry's file, read for a bump.
type Entry struct {
	ID string
	// Path is its file, in the repository, such as entries/wordpress.yaml.
	Path      string
	Text      []byte
	Version   string
	Release   int
	AppSource string
	// Images are each service's image as the Compose file writes it, by service.
	Images map[string]string
}

// ReadEntry reads entries/<id>.yaml of a catalog checkout.
func ReadEntry(checkout, id string) (*Entry, error) {
	rel := filepath.ToSlash(filepath.Join("entries", id+".yaml"))
	text, err := os.ReadFile(filepath.Join(checkout, rel)) //nolint:gosec // a file of the checkout the person running it names
	if err != nil {
		return nil, err
	}
	e, err := catalog.Decode(text)
	if err != nil {
		return nil, err
	}
	release, err := Release(text)
	if err != nil {
		return nil, err
	}
	var compose struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(e.GetCompose()), &compose); err != nil {
		return nil, fmt.Errorf("reading its Compose file: %w", err)
	}
	out := &Entry{ID: e.GetId(), Path: rel, Text: text, Version: e.GetVersion(), Release: release, AppSource: e.GetAppSource(), Images: map[string]string{}}
	for name, s := range compose.Services {
		if s.Image != "" {
			out.Images[name] = s.Image
		}
	}
	return out, nil
}

// Step is one image's move in a bump.
type Step struct {
	Service string
	From    string
	To      string
	FromTag string
	ToTag   string
	Kind    registry.Kind
	// Out is when the version came out: when it was pushed, where its registry
	// says, else when the bump first saw it, which Seen records.
	Out time.Time
	// Pushed says Out is when it was pushed; otherwise it's when it was first seen.
	Pushed bool
	// Corroborated is what says it's the upstream's own, where something does.
	Corroborated string
	// Waits are why it can't merge by itself, each a line.
	Waits []string
	// Saw is when the bump first saw each version it read of this image, as SeenKey
	// names them, where its registry gives no push time: every candidate, not only
	// the one it took, so one passed over for a newer keeps its time (#726's review,
	// M3). A body records them for the next run.
	Saw map[string]time.Time
	// digest is the digest the version was seen at, where it was read.
	digest string
}

// Lane is an entry's bump in one lane: a pull request, on its branch.
type Lane struct {
	Entry   string
	Lane    string
	Branch  string
	Title   string
	Steps   []Step
	Version string
	Release int
	Patch   []byte
	// Labels say why it waits for a person, as a pull request's labels.
	Labels []string
}

// Result is what a bump found of one entry.
type Result struct {
	Entry string
	Lanes []Lane
	// Notes say why an image, or the entry, gets no bump, for a person.
	Notes []string
	// Unread are the images it couldn't read, with why.
	Unread []string
}

// Plan works out an entry's bumps under its policy.
func (p *Planner) Plan(ctx context.Context, e *Entry, policy *Policy) Result {
	r := Result{Entry: e.ID}
	services := slices.Sorted(maps.Keys(e.Images))
	if err := policy.Check(services); err != nil {
		r.Notes = append(r.Notes, "Its policy in updates/"+e.ID+".yaml can't be followed: "+err.Error()+".")
		return r
	}
	gives := policy.Gives
	if gives == "" && len(services) == 1 {
		gives = services[0]
	}
	if gives == "" {
		r.Notes = append(r.Notes, "It has more than one image and no gives in its policy, so nothing says which gives its version. Name it in updates/"+e.ID+".yaml.")
		return r
	}
	if have := tagVersionOf(e.Images[gives]); have != strings.TrimPrefix(e.Version, "v") {
		r.Notes = append(r.Notes, fmt.Sprintf("Its version is %s and %s's tag says %s. Make them agree first.", e.Version, gives, have))
		return r
	}
	within, beyond := map[string]*Step{}, map[string]*Step{}
	for _, svc := range services {
		a, b, note, unread := p.service(ctx, e, svc, svc == gives, policy.For(svc, mustParse(e.Images[svc])))
		if note != "" {
			r.Notes = append(r.Notes, note)
		}
		if unread != "" {
			r.Unread = append(r.Unread, unread)
		}
		if a != nil {
			within[svc] = a
		}
		if b != nil {
			beyond[svc] = b
		}
	}
	for _, lane := range []struct {
		name  string
		steps map[string]*Step
	}{{LaneWithin, within}, {LaneBeyond, beyond}} {
		if len(lane.steps) == 0 {
			continue
		}
		l, err := p.lane(e, lane.name, lane.steps, gives)
		if err != nil {
			r.Notes = append(r.Notes, "Its file can't be edited: "+err.Error()+".")
			continue
		}
		r.Lanes = append(r.Lanes, l)
	}
	return r
}

func mustParse(image string) registry.Ref {
	r, _ := registry.Parse(image)
	return r
}

func tagVersionOf(image string) string {
	r, err := registry.Parse(image)
	if err != nil {
		return ""
	}
	return registry.TagVersion(r.Tag)
}

// service works out one service's step in each lane.
func (p *Planner) service(ctx context.Context, e *Entry, svc string, gives bool, eff Effective) (within, beyond *Step, note, unread string) {
	image := e.Images[svc]
	if eff.Level == LevelStays {
		return nil, nil, "", ""
	}
	ref, err := registry.Parse(image)
	if err != nil {
		return nil, nil, fmt.Sprintf("%s's image %s can't be read as one: %v.", svc, image, err), ""
	}
	if !registry.IsVersion(ref.Tag) {
		return nil, nil, fmt.Sprintf("%s's tag %s isn't a version, which Dockwell's own catalog doesn't allow. Pin a version.", svc, ref.Tag), ""
	}
	if ref.Digest == "" {
		return nil, nil, fmt.Sprintf("%s's image %s isn't pinned by its digest. Pin it.", svc, image), ""
	}
	tags, err := p.Sources.Tags(ctx, ref)
	if err != nil {
		return nil, nil, "", fmt.Sprintf("%s's image %s: %v", svc, ref.Name(), err)
	}
	names := make([]string, len(tags))
	byName := map[string]registry.Tag{}
	for i, t := range tags {
		names[i] = t.Name
		byName[t.Name] = t
	}
	var cands []registry.NewerTag
	for _, n := range registry.AllNewer(ref.Tag, names) {
		if eff.Held(registry.TagVersion(n.Tag)) != "" {
			continue
		}
		// A step that moves a database's data to a new series is a major,
		// whatever its numbers say (ADR 0080).
		if eff.Engine != nil && eff.Engine.MovesBySeries() &&
			database.Series(eff.Engine, ref.Name()+":"+ref.Tag) != database.Series(eff.Engine, ref.Name()+":"+n.Tag) {
			n.Kind = registry.KindMajor
		}
		cands = append(cands, n)
	}
	var in, out []registry.NewerTag
	for _, c := range cands {
		if eff.Level.covers(c.Kind) {
			in = append(in, c)
		} else {
			out = append(out, c)
		}
	}
	// The next step beyond goes by the first number, or for a database whose data
	// moves by series, by the series: MariaDB 12.3 to 12.4 is a major of its own.
	keyOf := func(tag string) []int {
		if eff.Engine != nil && eff.Engine.MovesBySeries() {
			if k := seriesKey(database.Series(eff.Engine, ref.Name()+":"+tag)); k != nil {
				return k
			}
		}
		m, _ := registry.MajorOf(tag)
		return []int{m}
	}
	beyondAll := len(out)
	out, majorWait := nextBeyond(out, ref.Tag, keyOf, eff.MajorsInOrder)
	if beyondAll > 0 && len(out) == 0 {
		// Never dropped without a word.
		note = fmt.Sprintf("%s has %d newer tags past its level, %s, and none reads as the next step after %s, so none is offered.", svc, beyondAll, eff.Level, ref.Tag)
	}
	pick := func(list []registry.NewerTag, beyondLevel bool) *Step {
		if len(list) == 0 {
			return nil
		}
		s := p.choose(ctx, list, byName, ref)
		if beyondLevel {
			s.Waits = append(s.Waits, fmt.Sprintf("A %s is past %s's level, %s, so a person moves it", s.Kind, svc, eff.Level))
			if majorWait != "" {
				s.Waits = append(s.Waits, majorWait)
			}
		}
		p.check(ctx, s, e, svc, ref, byName[s.ToTag], gives, eff)
		return s
	}
	return pick(in, false), pick(out, true), note, ""
}

// nextBeyond keeps, of the steps past a service's level, the next step alone: the
// smallest kind there is, and for a major, the next major only, never two at once.
// keyOf says which major a tag is in: its first number, or a database's series, so
// MariaDB 12.4 comes before 13.0 (#726's review, M1). Where majors must go in order,
// a next major that isn't the one right after the current one waits for a person,
// with why.
func nextBeyond(out []registry.NewerTag, currentTag string, keyOf func(string) []int, inOrder bool) ([]registry.NewerTag, string) {
	if len(out) == 0 {
		return nil, ""
	}
	smallest := registry.KindMajor
	for _, c := range out {
		smallest = min(smallest, c.Kind)
	}
	current := keyOf(currentTag)
	var next []int
	for _, c := range out {
		if c.Kind != smallest || smallest != registry.KindMajor {
			continue
		}
		if k := keyOf(c.Tag); slices.Compare(k, current) > 0 && (next == nil || slices.Compare(k, next) < 0) {
			next = k
		}
	}
	var kept []registry.NewerTag
	for _, c := range out {
		if c.Kind == smallest && (smallest != registry.KindMajor || slices.Equal(keyOf(c.Tag), next)) {
			kept = append(kept, c)
		}
	}
	// In order goes by the first number: a series of a database is next where
	// it's the smallest above, which it is here.
	if smallest == registry.KindMajor && inOrder && len(next) == 1 && len(current) == 1 && next[0] != current[0]+1 {
		return kept, fmt.Sprintf("Its majors go in order, and its next one among the tags read is %d, not %d", next[0], current[0]+1)
	}
	return kept, ""
}

// seriesKey reads a database's series, such as "12.4" or "16", as numbers to
// order by, or nil where there's none.
func seriesKey(series string) []int {
	if series == "" {
		return nil
	}
	var out []int
	for p := range strings.SplitSeq(series, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// choose takes the newest version that's aged past its wait, else the newest.
// Where the registry gives no push time, a version's wait runs from when the bump
// first saw it at its digest: a tag pushed again with another image starts again
// (#726's review, M4), and every version read is kept in Saw (M3).
func (p *Planner) choose(ctx context.Context, list []registry.NewerTag, byName map[string]registry.Tag, ref registry.Ref) *Step {
	saw := map[string]time.Time{}
	var first, chosen *Step
	for _, c := range list {
		s := &Step{ToTag: c.Tag, Kind: c.Kind, FromTag: ref.Tag}
		t := byName[c.Tag]
		if !t.Pushed.IsZero() {
			s.Out, s.Pushed, s.digest = t.Pushed, true, t.Digest
		} else {
			s.digest = t.Digest
			if s.digest == "" {
				at := ref
				at.Tag, at.Digest = c.Tag, ""
				if d, err := p.Sources.Digest(ctx, at); err == nil {
					s.digest = d
				}
			}
			s.Out = p.Now
			if s.digest != "" {
				key := SeenKey(ref, c.Tag, s.digest)
				if seen, ok := p.Seen[key]; ok && !seen.After(p.Now) {
					s.Out = seen
				} else if p.pushedAgain(ref, c.Tag, s.digest) {
					s.Waits = append(s.Waits, "Its tag was pushed again with another image since the bump first saw it, so its wait starts again")
				}
				saw[key] = s.Out
			}
		}
		if first == nil {
			first = s
		}
		if chosen == nil && p.Now.Sub(s.Out) >= waitFor(s.Kind) {
			chosen = s
		}
	}
	if chosen == nil {
		chosen = first
		chosen.Waits = append(chosen.Waits, fmt.Sprintf("It's within its wait: %s out for %s of %s", chosen.ToTag, days(p.Now.Sub(chosen.Out)), days(waitFor(chosen.Kind))))
	}
	if len(saw) > 0 {
		chosen.Saw = saw
	}
	return chosen
}

// pushedAgain says the bump saw tag at another digest before.
func (p *Planner) pushedAgain(ref registry.Ref, tag, digest string) bool {
	prefix := seenTag(ref, tag) + "@"
	for k := range p.Seen {
		if strings.HasPrefix(k, prefix) && k != prefix+digest {
			return true
		}
	}
	return false
}

// SeenKey is how Seen names a version of an image: its repository, the tag, and
// the digest it was seen at.
func SeenKey(ref registry.Ref, tag, digest string) string {
	return seenTag(ref, tag) + "@" + digest
}

func seenTag(ref registry.Ref, tag string) string {
	return ref.Registry + "/" + ref.Repository + ":" + tag
}

func waitFor(k registry.Kind) time.Duration {
	if k == registry.KindPatch {
		return PatchWait
	}
	return MinorWait
}

func days(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// check finds the step's digest, and says why it waits where it isn't
// corroborated, isn't checked for its signature, or drops a platform.
func (p *Planner) check(ctx context.Context, s *Step, e *Entry, svc string, ref registry.Ref, tag registry.Tag, gives bool, eff Effective) {
	s.Service = svc
	s.From = e.Images[svc]
	to := ref
	to.Tag, to.Digest = s.ToTag, cmp.Or(tag.Digest, s.digest)
	if to.Digest == "" {
		d, err := p.Sources.Digest(ctx, to)
		if err != nil {
			s.Waits = append(s.Waits, fmt.Sprintf("Its digest can't be read: %v", err))
			return
		}
		to.Digest = d
	}
	s.To = imageString(s.From, to)
	if before, err := p.Sources.Platforms(ctx, ref); err != nil {
		s.Waits = append(s.Waits, fmt.Sprintf("Its platforms before can't be read: %v", err))
	} else if after, err := p.Sources.Platforms(ctx, to); err != nil {
		s.Waits = append(s.Waits, fmt.Sprintf("Its platforms can't be read: %v", err))
	} else if dropped := platformsDropped(before, after); len(dropped) > 0 {
		s.Waits = append(s.Waits, "It drops "+strings.Join(dropped, " and ")+", which the image ran on before")
	}
	switch {
	case gives && GitHubRepository(e.AppSource) == "":
		s.Waits = append(s.Waits, "Its appSource isn't on GitHub, so nothing corroborates the version")
	case gives:
		version := registry.TagVersion(s.ToTag)
		ok, said, err := p.Sources.Released(ctx, GitHubRepository(e.AppSource), version)
		switch {
		case err != nil:
			s.Waits = append(s.Waits, fmt.Sprintf("Its release can't be read: %v", err))
		case ok:
			s.Corroborated = said
		default:
			s.Waits = append(s.Waits, "Nothing corroborates it: "+said)
		}
	case eff.Official:
		name := strings.TrimPrefix(ref.Repository, "library/")
		ok, err := p.Sources.OfficialTag(ctx, name, s.ToTag)
		switch {
		case err != nil:
			s.Waits = append(s.Waits, fmt.Sprintf("docker-library's file of %s can't be read: %v", name, err))
		case ok:
			s.Corroborated = "docker-library's official-images builds " + name + ":" + s.ToTag
		default:
			s.Waits = append(s.Waits, "Nothing corroborates it: docker-library's official-images names no "+name+":"+s.ToTag)
		}
	default:
		s.Waits = append(s.Waits, "Nothing corroborates it: it neither gives the entry's version nor is a Docker Official Image")
	}
	if eff.SignedBy != nil {
		s.Waits = append(s.Waits, "Its signature by "+eff.SignedBy.String()+" isn't checked yet")
	}
}

// imageString is the image as the entry writes it, moved to ref's tag and digest:
// its name kept as written, as Docker Hub's library/ never is.
func imageString(from string, ref registry.Ref) string {
	name := from
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	return name + ":" + ref.Tag + "@" + ref.Digest
}

// lane edits the entry for one lane's steps, and says why it waits.
func (p *Planner) lane(e *Entry, name string, steps map[string]*Step, gives string) (Lane, error) {
	l := Lane{Entry: e.ID, Lane: name, Branch: "bump/" + e.ID, Release: e.Release + 1, Version: e.Version}
	if name == LaneBeyond {
		l.Branch += "-major"
	}
	edit := Edit{Images: map[string]string{}, Release: l.Release}
	labels := map[string]bool{}
	for _, svc := range slices.Sorted(maps.Keys(steps)) {
		s := steps[svc]
		if s.To == "" {
			// Its digest couldn't be read: nothing to write for it.
			labels["unread"] = true
			l.Steps = append(l.Steps, *s)
			continue
		}
		edit.Images[s.From] = s.To
		if svc == gives {
			l.Version = registry.TagVersion(s.ToTag)
			edit.Version = l.Version
		}
		for _, w := range s.Waits {
			labels[labelFor(w)] = true
		}
		if s.Kind == registry.KindMajor {
			labels["major"] = true
		}
		l.Steps = append(l.Steps, *s)
	}
	if len(edit.Images) == 0 {
		return l, errors.New("no step of it could be read")
	}
	after, err := Apply(e.Text, edit)
	if err != nil {
		return l, err
	}
	if l.Patch, err = Diff(e.Path, e.Text, after); err != nil {
		return l, err
	}
	l.Labels = slices.Sorted(maps.Keys(labels))
	l.Title = title(e, &l)
	return l, nil
}

// labelFor is the label a reason a bump waits gives its pull request.
func labelFor(why string) string {
	switch {
	case strings.Contains(why, "pushed again"):
		return "tag pushed again"
	case strings.Contains(why, "past "):
		return "past its level"
	case strings.Contains(why, "within its wait"):
		return "within its wait"
	case strings.Contains(why, "corroborates"):
		return "not corroborated"
	case strings.Contains(why, "drops "):
		return "drops a platform"
	case strings.Contains(why, "signature"):
		return "signature unchecked"
	case strings.Contains(why, "majors go in order"):
		return "majors in order"
	}
	return "unread"
}

// title is a lane's pull request's title: the app's new version, or the images
// that move.
func title(e *Entry, l *Lane) string {
	if l.Version != e.Version {
		return fmt.Sprintf("Move %s to %s", e.ID, l.Version)
	}
	var moves []string
	for i := range l.Steps {
		if s := &l.Steps[i]; s.To != "" {
			moves = append(moves, s.Service+" to "+s.ToTag)
		}
	}
	sort.Strings(moves)
	return fmt.Sprintf("Move %s's %s", e.ID, strings.Join(moves, " and "))
}
