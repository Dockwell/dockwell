// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
)

// Sources reads what a build's branch or tag names now, as git ls-remote does
// (package gitref).
type Sources interface {
	// Resolve gives the commit ref names in the repository at url, or with ref
	// empty its default branch's, and that branch or tag by its full name, such
	// as refs/heads/main. A commit's ID is itself, with no name.
	Resolve(ctx context.Context, url, ref string) (commit, full string, err error)
}

// SetSources has the engine read what a build's branch names with s.
func (e *Engine) SetSources(s Sources) { e.sources = s }

// Detector reads how a commit would build, on a server, building nothing (package
// builds). b names the commit in full.
type Detector interface {
	Detect(ctx context.Context, server string, b *resourcev1.ImageBuild) (*resource.BuildDetection, error)
}

// SetDetector has the engine read how a build's commit builds with d.
func (e *Engine) SetDetector(d Detector) { e.detector = d }

// detectWait bounds reading how the commits a plan builds would build, which the
// first time on a server fetches Railpack and mise.
const detectWait = 3 * time.Minute

// detectBuilds reads how each commit the plan builds with Railpack, or may, would
// build, on the server that builds it, so the plan says what Railpack found before
// anything builds. One Railpack can't build is refused here, before anything is
// done. A build that names its Dockerfile isn't read, and nor is one whose server
// can't be asked: its build reads it as it starts, and says so in its log.
func (e *Engine) detectBuilds(ctx context.Context, plan *Plan, items []item) error {
	if e.detector == nil {
		return nil
	}
	specs := map[resource.Ref]*resourcev1.Service{}
	for i := range items {
		if svc, ok := items[i].spec.(*resourcev1.Service); ok && svc.GetBuild() != nil {
			specs[items[i].ref] = svc
		}
	}
	ctx, cancel := context.WithTimeout(ctx, detectWait)
	defer cancel()
	for i := range plan.Servers {
		sp := &plan.Servers[i]
		if sp.Offline || sp.Removed {
			continue
		}
		for j := range sp.Steps {
			st := &sp.Steps[j]
			svc := specs[st.Ref]
			if st.Build == nil || st.Build.Builder == resource.BuilderDockerfile || svc == nil {
				continue
			}
			d, err := e.detector.Detect(ctx, sp.Server, svc.GetBuild())
			if err != nil {
				e.logger.WarnContext(ctx, "can't read how a build's commit builds, so its plan doesn't say; the build reads it as it starts",
					slog.String("service", st.Ref.Name), slog.String("server", sp.Name), slog.String("error", err.Error()))
				continue
			}
			if d.Builder == resource.BuilderRailpack && d.Error != "" {
				why := strings.TrimSpace(d.Error)
				if k := strings.IndexByte(why, '\n'); k >= 0 {
					why = strings.TrimSpace(why[:k])
				}
				return fault.New(ErrInvalid, "Railpack can't tell how to build %s at %s for %s: %s. Add a Dockerfile, or set the build's start command",
					shownRepository(st.Build.URL), shortCommit(st.Build.Commit), st.Ref.Name, strconv.Quote(why))
			}
			st.Build.Detection = d
		}
	}
	return nil
}

// shownRepository is a repository's URL as people write it, with no https://.
func shownRepository(url string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), ".git")
}

// shortCommit is a commit's first seven hex digits.
func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// pinBuilds writes, into each service the change builds, the commit it builds and
// the image it makes, under a tag of its own, before anything plans it, so the plan
// says what it builds, its fingerprint covers it, and the revision names the image
// before it is made (ADR 0046). A change builds when the app's image is empty, as a
// new app, a deploy or a form leaves it, and when it changes the build's settings or
// commit and keeps the image it had, as a script that edits the spec would. An undo,
// a rollback and adopting drift build nothing: they carry the image a revision had.
func (e *Engine) pinBuilds(ctx context.Context, items []item, changeID string) error {
	for i := range items {
		it := &items[i]
		svc, ok := it.spec.(*resourcev1.Service)
		if !ok || svc.GetBuild() == nil || it.reverses || it.observed {
			continue
		}
		was, _ := it.current.(*resourcev1.Service)
		g := svc.GetBuild().GetGit()
		image := svc.GetApp().GetImage()
		// What another service's build made is that service's code (WriterOf).
		if image != "" && docker.Repository(image) != docker.BuiltRepository(it.id) {
			return fault.New(ErrInvalid, "%s builds its own image, so it can't run %s, which isn't one of its builds. Leave the image out to build it", it.ref.Name, image)
		}
		builds := image == ""
		if !builds && was != nil && image == was.GetApp().GetImage() {
			if g.GetCommit() == "" {
				g.Commit = was.GetBuild().GetGit().GetCommit()
			}
			builds = !proto.Equal(svc.GetBuild(), was.GetBuild())
		}
		if !builds {
			continue
		}
		if n := len(it.servers); n > 1 {
			return fault.New(ErrInvalid, "%s builds its image from Git, so it runs on one server for now, and this places it on %d. Place it on one: copying a build to other servers comes with build servers", it.ref.Name, n)
		}
		// The build fetches its branch or tag by its full name, as Git's short names
		// reach further, as pull/7/head reaches refs/pull/7/head, and a tag of a
		// branch's name comes first; so the plan pins the full name, and the commit
		// unless the edit names one.
		if g.GetCommit() == "" || !fullRef(g.GetRef()) {
			if e.sources == nil {
				return fmt.Errorf("this control plane can't read what %s's branch names, so it can't plan its build", it.ref.Name)
			}
			commit, full, err := e.sources.Resolve(ctx, g.GetUrl(), g.GetRef())
			if err != nil {
				return fault.New(ErrInvalid, "%w", err)
			}
			if g.GetCommit() == "" {
				g.Commit = commit
			}
			if full != "" {
				g.Ref = full
			}
		}
		svc.GetApp().Image = docker.BuiltImage(it.id, g.GetCommit(), changeID)
		encoded, err := resource.Encode(svc)
		if err != nil {
			return err
		}
		it.encoded = encoded
	}
	return nil
}

// fullRef reports a branch or a tag by its full name, or a commit's ID, which the
// build fetches as it is.
func fullRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") || commitRef.MatchString(strings.ToLower(ref))
}

var commitRef = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
