// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
)

// What a read of images keeps, and how often it may ask Docker Hub for what it
// counts as a pull (ADR 0062).
const (
	// imageReadFor is how long a tag's digest, a repository's tags and a search
	// are kept.
	imageReadFor = 10 * time.Minute
	// imageIdleFor is how long without a read before all of it goes, so an idle
	// agent keeps none of it.
	imageIdleFor = 30 * time.Minute
	// maxConfigs bounds the configs kept by digest, the oldest read going first.
	maxConfigs = 64
	// hubReads and hubReadWindow bound the manifest GETs reads make of Docker Hub,
	// beside what plans make (internal/resource/container/registry.go).
	hubReads      = 10
	hubReadWindow = 6 * time.Hour
	// silentImagesFor is how long a registry that didn't answer goes unasked.
	silentImagesFor = 5 * time.Minute
	// searchLimit is the most images a search returns.
	searchLimit = 25
	// imageReadTimeout bounds a whole read (ADR 0062).
	imageReadTimeout = 8 * time.Second
)

// ImageReads says how the agent reads images from their registries.
type ImageReads struct {
	// Test, in a test, is the registry every read goes to, such as the fake
	// engine's: nothing else is reached.
	Test string
	// Off says why images aren't read here, such as a test with no registry of its
	// own. Empty reads them.
	Off string
}

type kept[T any] struct {
	value T
	at    time.Time
}

// images reads images for forms, and keeps what it read for a while.
type images struct {
	docker *docker.Client
	o      ImageReads
	now    func() time.Time

	mu       sync.Mutex
	client   *registry.Client
	platform registry.Platform
	digests  map[string]kept[string]
	tags     map[string]kept[[]registry.Tag]
	searches map[string]kept[[]docker.Found]
	configs  map[string]kept[*registry.Config]
	silent   map[string]time.Time
	hub      []time.Time
	idle     *time.Timer
}

func newImages(d *docker.Client, o ImageReads, now func() time.Time) *images {
	if now == nil {
		now = time.Now
	}
	return &images{docker: d, o: o, now: now}
}

// read answers an image call.
func (im *images) read(ctx context.Context, call *agentv1.ImageCall) (*agentv1.ImageResult, error) {
	if im.o.Off != "" {
		return nil, errors.New(im.o.Off)
	}
	if im.docker == nil {
		return nil, errors.New("this agent can't read images, as it has no Docker driver")
	}
	ctx, cancel := context.WithTimeout(ctx, imageReadTimeout)
	defer cancel()
	client, platform := im.ready(ctx)
	switch {
	case call.GetSearch() != nil:
		res := im.search(ctx, call.GetSearch().GetTerm())
		return &agentv1.ImageResult{Answer: &agentv1.ImageResult_Search{Search: res}}, nil
	case call.GetTags() != nil:
		res, err := im.listTags(ctx, client, call.GetTags().GetRepository())
		if err != nil {
			return nil, err
		}
		return &agentv1.ImageResult{Answer: &agentv1.ImageResult_Tags{Tags: res}}, nil
	case call.GetImage() != nil:
		res, err := im.image(ctx, client, platform, call.GetImage().GetReference())
		if err != nil {
			return nil, err
		}
		return &agentv1.ImageResult{Answer: &agentv1.ImageResult_Image{Image: res}}, nil
	}
	return nil, errors.New("the call asks to read nothing an agent knows of; update the agent")
}

// ready makes the registry client as the server's Docker reaches registries,
// the first time, and puts off forgetting it all. A read goes on with the
// client it returns, whatever forget does meanwhile.
func (im *images) ready(ctx context.Context) (*registry.Client, registry.Platform) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.idle == nil {
		im.idle = time.AfterFunc(imageIdleFor, im.forget)
	} else {
		im.idle.Reset(imageIdleFor)
	}
	if im.client != nil {
		return im.client, im.platform
	}
	o := registry.Options{Test: im.o.Test, Now: im.now}
	if info, err := im.docker.Info(ctx); err == nil && info.RegistryConfig != nil {
		o.Mirrors, o.Insecure = info.RegistryConfig.Mirrors, info.RegistryConfig.Insecure
	} else {
		o.Insecure = (*docker.RegistryConfig)(nil).Insecure
	}
	im.client = registry.New(o)
	im.platform = registry.Platform{OS: "linux"}
	if arch, err := im.docker.Architecture(ctx); err == nil {
		im.platform.Architecture = arch
		if arch == "arm" {
			im.platform.Variant = "v7"
		}
	}
	im.digests = map[string]kept[string]{}
	im.tags = map[string]kept[[]registry.Tag]{}
	im.searches = map[string]kept[[]docker.Found]{}
	im.configs = map[string]kept[*registry.Config]{}
	im.silent = map[string]time.Time{}
	return im.client, im.platform
}

// forget lets go of everything read, after imageIdleFor without a read.
func (im *images) forget() {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.client, im.digests, im.tags, im.searches, im.configs, im.silent = nil, nil, nil, nil, nil, nil
	im.idle = nil
}

// answer says what an error from a registry says of an image.
func answer(err error) dockwellv1.ImageAnswer {
	switch {
	case err == nil:
		return dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND
	case errors.Is(err, registry.ErrNotFound):
		return dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND
	case errors.Is(err, registry.ErrLimited):
		return dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED
	}
	return dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
}

// fresh is a kept value read within imageReadFor.
func fresh[T any](m map[string]kept[T], key string, now time.Time) (T, bool) {
	k, ok := m[key]
	if !ok || now.Sub(k.at) >= imageReadFor {
		var zero T
		return zero, false
	}
	return k.value, true
}

// isSilent reports a registry that didn't answer a while ago, and notes one
// that didn't answer now.
func (im *images) isSilent(host string) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.now().Before(im.silent[host])
}

func (im *images) heard(host string, err error) {
	if err == nil || answer(err) != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.silent != nil {
		im.silent[host] = im.now().Add(silentImagesFor)
	}
}

func (im *images) search(ctx context.Context, term string) *dockwellv1.SearchImagesResponse {
	term = strings.ToLower(strings.TrimSpace(term))
	res := &dockwellv1.SearchImagesResponse{}
	if term == "" {
		return res
	}
	im.mu.Lock()
	found, ok := fresh(im.searches, term, im.now())
	im.mu.Unlock()
	if !ok {
		if im.isSilent(registry.Hub) {
			res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			return res
		}
		var err error
		found, err = im.docker.SearchImages(ctx, term, searchLimit)
		if err != nil {
			im.heard(registry.Hub, registry.ErrUnreachable)
			res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			return res
		}
		slices.SortStableFunc(found, func(a, b docker.Found) int {
			if a.Official != b.Official {
				if a.Official {
					return -1
				}
				return 1
			}
			return int(b.Stars - a.Stars)
		})
		im.mu.Lock()
		if im.searches != nil {
			im.searches[term] = kept[[]docker.Found]{found, im.now()}
		}
		im.mu.Unlock()
	}
	res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND
	for _, f := range found {
		res.Matches = append(res.Matches, &dockwellv1.ImageMatch{Name: f.Name, Description: f.Description, Official: f.Official, Stars: f.Stars})
	}
	return res
}

func (im *images) listTags(ctx context.Context, client *registry.Client, repository string) (*dockwellv1.ListImageTagsResponse, error) {
	ref, err := registry.Parse(repository)
	if err != nil {
		return nil, err
	}
	key := ref.Registry + "/" + ref.Repository
	res := &dockwellv1.ListImageTagsResponse{}
	im.mu.Lock()
	list, ok := fresh(im.tags, key, im.now())
	im.mu.Unlock()
	if !ok {
		if im.isSilent(ref.Registry) {
			res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			return res, nil
		}
		list, err = client.Tags(ctx, ref)
		im.heard(ref.Registry, err)
		if err != nil {
			res.Answer = answer(err)
			return res, nil
		}
		im.mu.Lock()
		if im.tags != nil {
			im.tags[key] = kept[[]registry.Tag]{list, im.now()}
		}
		im.mu.Unlock()
	}
	res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND
	for _, t := range list {
		tag := &dockwellv1.ImageTag{Name: t.Name, Digest: t.Digest, Latest: t.Latest}
		if !t.Pushed.IsZero() {
			tag.PushTime = timestamppb.New(t.Pushed)
		}
		res.Tags = append(res.Tags, tag)
	}
	return res, nil
}

// image says whether an image is there, and what it declares: from the
// server's own images when it has it, which asks nothing of the network, else
// a HEAD of its manifest, then its config by digest, kept for good.
func (im *images) image(ctx context.Context, client *registry.Client, platform registry.Platform, reference string) (*dockwellv1.GetImageResponse, error) {
	ref, err := registry.Parse(reference)
	if err != nil {
		return nil, err
	}
	res := &dockwellv1.GetImageResponse{}
	if local, err := im.docker.InspectImage(ctx, ref.String()); err == nil && local.Config != nil {
		res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND
		res.Digest = digestOf(local, ref)
		c := local.Config
		res.Config = configProto(&registry.Config{
			Ports: registry.Ports(c.ExposedPorts), Env: registry.Vars(c.Env), Volumes: registry.Volumes(c.Volumes),
			Cmd: c.Cmd, Entrypoint: c.Entrypoint, User: c.User, WorkingDir: c.WorkingDir,
			Healthcheck: healthTest(c.Healthcheck), Platform: registry.Platform{OS: "linux", Architecture: local.Architecture},
		})
		return res, nil
	}
	if im.isSilent(ref.Registry) {
		res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
		return res, nil
	}
	key := ref.String()
	im.mu.Lock()
	digest, ok := fresh(im.digests, key, im.now())
	im.mu.Unlock()
	if !ok {
		digest, err = client.Head(ctx, ref)
		im.heard(ref.Registry, err)
		if err != nil {
			res.Answer = answer(err)
			return res, nil
		}
		im.mu.Lock()
		if im.digests != nil && digest != "" {
			im.digests[key] = kept[string]{digest, im.now()}
		}
		im.mu.Unlock()
	}
	res.Answer, res.Digest = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, digest
	im.mu.Lock()
	cfg, ok := im.configs[digest]
	im.mu.Unlock()
	if ok && digest != "" {
		res.Config = configProto(cfg.value)
		return res, nil
	}
	if ref.Registry == registry.Hub && im.o.Test == "" && !im.spendHub() {
		res.Unread = dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED
		return res, nil
	}
	byDigest := ref
	if digest != "" {
		byDigest.Digest = digest
	}
	read, _, err := client.Config(ctx, byDigest, platform)
	im.heard(ref.Registry, err)
	if err != nil {
		res.Unread = answer(err)
		if res.Unread == dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
			res.Unread = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
		}
		return res, nil
	}
	im.mu.Lock()
	if im.configs != nil && digest != "" {
		if len(im.configs) >= maxConfigs {
			oldest := ""
			for d, k := range im.configs {
				if oldest == "" || k.at.Before(im.configs[oldest].at) {
					oldest = d
				}
			}
			delete(im.configs, oldest)
		}
		im.configs[digest] = kept[*registry.Config]{read, im.now()}
	}
	im.mu.Unlock()
	res.Config = configProto(read)
	return res, nil
}

// spendHub takes one of the manifest GETs reads may make of Docker Hub in
// hubReadWindow, if one is left.
func (im *images) spendHub() bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	now := im.now()
	im.hub = slices.DeleteFunc(im.hub, func(at time.Time) bool { return now.Sub(at) >= hubReadWindow })
	if len(im.hub) >= hubReads {
		return false
	}
	im.hub = append(im.hub, now)
	return true
}

// digestOf is what a registry calls an image the server has: the digest the
// reference names, or its repository's among the image's.
func digestOf(local *docker.Image, ref registry.Ref) string {
	if ref.Digest != "" {
		return ref.Digest
	}
	repo := ref.Name() + "@"
	for _, rd := range local.RepoDigests {
		name, digest, _ := strings.Cut(rd, "@")
		if strings.HasPrefix(repo, name+"@") || name == "docker.io/"+ref.Repository {
			return digest
		}
	}
	return ""
}

func healthTest(h *docker.Healthcheck) []string {
	if h == nil {
		return nil
	}
	return h.Test
}

func configProto(c *registry.Config) *dockwellv1.ImageConfig {
	out := &dockwellv1.ImageConfig{
		Volumes: c.Volumes, Command: c.Cmd, Entrypoint: c.Entrypoint, User: c.User, WorkingDir: c.WorkingDir,
		Healthcheck: c.Healthcheck, Platform: c.Platform.String(),
	}
	for _, p := range c.Ports {
		out.Ports = append(out.Ports, &dockwellv1.ImagePort{Port: p.Number, Udp: p.UDP})
	}
	for _, v := range c.Env {
		out.Env = append(out.Env, &dockwellv1.ImageVariable{Name: v.Name, Value: v.Value})
	}
	return out
}
