// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// A plan asks an image's registry for an image the server hasn't got, through
// Docker's /distribution, so a change that names one no pull can get is refused
// before anything stops (checkImage). That answer is worth little next to what
// the asking costs, so the agent bounds it:
//
//   - Docker answers an image a registry has with a manifest GET, which Docker
//     Hub counts as a pull, 100 in 6 hours from an address that doesn't sign in.
//     So an answer that it can be pulled is kept until the server pulls it, or for
//     pullableFor: the plan a change is proposed with asks, and the plan it is
//     applied with doesn't. An answer that it can't isn't kept, as a registry that
//     hasn't it answers the HEAD before, which costs nothing, and the fix is to
//     push it and plan again.
//   - Docker Hub is asked hubAsks times at most in hubWindow, as one file from a
//     token could otherwise spend a server's allowance. Past that a file's image
//     goes unasked, and the pull finds out.
//   - Docker pings every endpoint it might pull from, mirrors and Docker Hub alike,
//     so a registry it can't reach costs the whole pullableWait for each image. One
//     that doesn't answer in that time isn't asked again for silentFor, so a file
//     of seven images on it costs one wait, not seven.
//   - A plan waits on registries for half the time it had when it began at most,
//     in all, however many it asks, so it never runs out of time waiting.
//
// None of that says an image is there. An image that went unasked is its own answer
// (notAsked), and a deploy or a rollback, which names one image, asks past the first
// two limits, as a pin it kept for want of asking could only fail.
const (
	// pullableWait bounds asking a registry for one image while planning.
	pullableWait = 10 * time.Second
	// pullableFor is how long an answer that an image can be pulled is kept.
	pullableFor = 15 * time.Minute
	// silentFor is how long a registry that didn't answer goes unasked.
	silentFor = 5 * time.Minute
	// hubAsks and hubWindow bound asking Docker Hub: a fifth of what it allows an
	// address that doesn't sign in.
	hubAsks   = 20
	hubWindow = 6 * time.Hour
	// maxPullable bounds how many answers are kept, the oldest going first.
	maxPullable = 256
)

// registries is what the agent keeps of asking registries.
type registries struct {
	mu sync.Mutex
	// pullable are the images a registry said it would give, and when.
	pullable map[string]time.Time
	// silent are the registries that didn't answer, until when they go unasked.
	silent map[string]time.Time
	// hub are the times Docker Hub was asked, within hubWindow.
	hub []time.Time
}

// registryOf is the registry an image reference names, as Docker reads one:
// its first part when that is a host, and Docker Hub otherwise.
func registryOf(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	first, _, hasPath := strings.Cut(name, "/")
	if !hasPath || !strings.ContainsAny(first, ".:") && first != "localhost" && first == strings.ToLower(first) {
		return "docker.io"
	}
	if first == "index.docker.io" || first == "registry-1.docker.io" {
		return "docker.io"
	}
	return first
}

// now is the agent's clock.
func (d *Docker) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// registryAnswer is what a plan knows of whether a pull can get an image.
type registryAnswer int

const (
	// notAsked says a limit here held the question back, so nothing is known of the
	// image. It never means the image is there.
	notAsked registryAnswer = iota
	// saidNothing says the registry was asked, and is down, slow or limiting how
	// often it's asked, which says nothing of the image.
	saidNothing
	// canPull says the registry will give it.
	canPull
	// cantPull says the registry won't give it: it hasn't got it, or wants
	// credentials.
	cantPull
)

// askRegistry asks an image's registry for it, within the limits above. A deploy or a
// rollback names one image someone asked for, and a pin rests on the answer, so it
// asks past Docker Hub's limit and a registry's silence, which are there for files.
// It still counts against Docker Hub's limit, and waits within the plan's time.
func (d *Docker) askRegistry(ctx context.Context, image string) registryAnswer {
	host, now, named := registryOf(image), d.now(), resource.DeployOf(ctx).Run
	// What a pull with a login may get says nothing of one without it (ADR 0086).
	ref := image
	if login := loginOf(ctx); login != "" {
		ref += " as " + login
	}
	wait := pullableWait
	if until, ok := resource.WaitsUntil(ctx); ok {
		wait = min(wait, time.Until(until))
	}
	r := &d.registries
	r.mu.Lock()
	switch {
	case !r.pullable[ref].IsZero() && now.Sub(r.pullable[ref]) < pullableFor:
		r.mu.Unlock()
		return canPull
	case wait <= 0, !named && now.Before(r.silent[host]):
		r.mu.Unlock()
		return notAsked
	case host == "docker.io":
		r.hub = trimBefore(r.hub, now.Add(-hubWindow))
		if !named && len(r.hub) >= hubAsks {
			r.mu.Unlock()
			return notAsked
		}
		r.hub = append(r.hub, now)
	}
	r.mu.Unlock()

	asking, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	err := d.Client.CheckPullable(asking, image)

	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err == nil:
		if r.pullable == nil {
			r.pullable = map[string]time.Time{}
		}
		if len(r.pullable) >= maxPullable {
			forgetOldest(r.pullable)
		}
		r.pullable[ref] = now
		return canPull
	case docker.NotPullable(err):
		return cantPull
	case ctx.Err() == nil && (asking.Err() != nil || errors.Is(err, context.DeadlineExceeded)):
		if r.silent == nil {
			r.silent = map[string]time.Time{}
		}
		// A file names its registries, so this goes no further than they're silent.
		maps.DeleteFunc(r.silent, func(_ string, until time.Time) bool { return !now.Before(until) })
		r.silent[host] = d.now().Add(silentFor)
	}
	return saidNothing
}

// pulled forgets what a registry said of an image the server has now pulled, so
// a plan from here on asks again if it's gone, rather than take an old answer.
func (d *Docker) pulled(ref string) {
	d.registries.mu.Lock()
	defer d.registries.mu.Unlock()
	maps.DeleteFunc(d.registries.pullable, func(asked string, _ time.Time) bool {
		return asked == ref || strings.HasPrefix(asked, ref+" as ")
	})
}

// trimBefore drops the times before from, from a list kept oldest first.
func trimBefore(times []time.Time, from time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(from) {
		i++
	}
	return times[i:]
}

// forgetOldest drops the oldest answer.
func forgetOldest(m map[string]time.Time) {
	var oldest string
	for ref, at := range m {
		if oldest == "" || at.Before(m[oldest]) {
			oldest = ref
		}
	}
	delete(m, oldest)
}
