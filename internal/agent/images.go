// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"container/list"
	"context"
	"errors"
	"maps"
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
	// imageReadFor is how long an answer is kept.
	imageReadFor = 10 * time.Minute
	// imageKeptFor is how long after the first read everything kept goes,
	// however often it's read meanwhile, so nothing stays for good.
	imageKeptFor = 30 * time.Minute
	// What each kind of answer may take of the agent's memory, in bytes, the
	// first kept going first: a config is a few kilobytes, and one over
	// registry.MaxConfig is never read.
	configBytes = 1 << 20
	tagBytes    = 512 << 10
	searchBytes = 128 << 10
	digestBytes = 64 << 10
	// maxSilent is the most endpoints remembered as not answering.
	maxSilent = 64
	// hubReads and hubReadWindow bound the manifest GETs reads make of Docker Hub,
	// beside what plans make (internal/resource/container/registry.go). A read
	// spends two, an index and its platform's manifest, as most of Docker Hub's
	// images have both.
	hubReads      = 10
	hubPerRead    = 2
	hubReadWindow = 6 * time.Hour
	// silentImagesFor is how long an endpoint that didn't answer goes unasked.
	silentImagesFor = 5 * time.Minute
	// searchLimit is the most images a search returns.
	searchLimit = 25
	// imageReadTimeout bounds a whole read (ADR 0062).
	imageReadTimeout = 8 * time.Second
	// imageCalls is how many reads may reach registries at once. Another waits
	// for its turn for as long as it may take, as a form sends its search, its
	// check and its tags close together.
	imageCalls = 2
)

// What a read says when it doesn't go.
var (
	errBusy     = errors.New("this server is reading two images from their registries already; try again in a moment")
	errHostTerm = errors.New("only Docker Hub is searched, so a search names no registry; type a name, such as nginx, or the whole reference")
)

// The endpoints a read reaches beside a registry, each silent on its own: Docker
// Hub's search, through the engine, and its API, which lists its tags.
const (
	searchEndpoint = "Docker Hub's search"
	hubAPIEndpoint = "Docker Hub's API"
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

// images reads images for forms, and keeps what it read for a while.
type images struct {
	docker *docker.Client
	o      ImageReads
	now    func() time.Time
	// calls holds a place for each read that's reaching a registry.
	calls chan struct{}
	// newClient makes the registry client: registry.New, but a test's sends
	// every request to its stand-in.
	newClient func(registry.Options) *registry.Client

	mu       sync.Mutex
	client   *registry.Client
	platform registry.Platform
	digests  *shelf[string]
	tags     *shelf[[]registry.Tag]
	searches *shelf[[]docker.Found]
	configs  *shelf[*registry.Config]
	silent   map[string]time.Time
	hub      []time.Time
	// since is when the first read made the client, and forgets lets it all go
	// imageKeptFor after, even with no read to see it's time.
	since   time.Time
	forgets *time.Timer
}

func newImages(d *docker.Client, o ImageReads, now func() time.Time) *images {
	if now == nil {
		now = time.Now
	}
	return &images{docker: d, o: o, now: now, calls: make(chan struct{}, imageCalls), newClient: registry.New}
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
		res, err := im.search(ctx, call.GetSearch().GetTerm())
		if err != nil {
			return nil, err
		}
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
// the first time, and starts the clock on forgetting it all. Docker is asked
// outside the lock, so a slow engine holds up no other read, and a client made
// without the server's architecture serves this read alone, so the next asks
// again. A read goes on with the client it returns, whatever forget does
// meanwhile.
func (im *images) ready(ctx context.Context) (*registry.Client, registry.Platform) {
	im.mu.Lock()
	if im.client != nil && im.now().Sub(im.since) >= imageKeptFor {
		im.forgetLocked()
	}
	if im.client != nil {
		client, platform := im.client, im.platform
		im.mu.Unlock()
		return client, platform
	}
	im.mu.Unlock()
	o := registry.Options{Test: im.o.Test, Now: im.now}
	if info, err := im.docker.Info(ctx); err == nil && info.RegistryConfig != nil {
		o.Mirrors, o.Insecure = info.RegistryConfig.Mirrors, info.RegistryConfig.Insecure
	} else {
		o.Insecure = (*docker.RegistryConfig)(nil).Insecure
	}
	platform := registry.Platform{OS: "linux"}
	if arch, err := im.docker.Architecture(ctx); err == nil {
		platform.Architecture = arch
		if arch == "arm" {
			platform.Variant = "v7"
		}
	}
	client := im.newClient(o)
	im.mu.Lock()
	defer im.mu.Unlock()
	switch {
	case im.client != nil:
		return im.client, im.platform
	case platform.Architecture == "":
		return client, platform
	}
	im.client, im.platform = client, platform
	im.digests = newShelf(digestBytes, func(d string) int { return len(d) })
	im.tags = newShelf(tagBytes, tagsSize)
	im.searches = newShelf(searchBytes, foundSize)
	im.configs = newShelf(configBytes, configSize)
	im.silent = map[string]time.Time{}
	im.since = im.now()
	im.forgets = time.AfterFunc(imageKeptFor, im.forget)
	return im.client, im.platform
}

// forget lets go of everything read, imageKeptFor after the first read.
func (im *images) forget() {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.forgetLocked()
}

func (im *images) forgetLocked() {
	if im.forgets != nil {
		im.forgets.Stop()
	}
	im.client, im.digests, im.tags, im.searches, im.configs, im.silent = nil, nil, nil, nil, nil, nil
	im.forgets = nil
}

// turn waits for one of the imageCalls places to reach registries from, for as
// long as the read may take, and says to try again when its time runs out first.
func (im *images) turn(ctx context.Context) (func(), error) {
	select {
	case im.calls <- struct{}{}:
		return func() { <-im.calls }, nil
	case <-ctx.Done():
		return nil, errBusy
	}
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

// isSilent reports an endpoint that didn't answer a while ago.
func (im *images) isSilent(endpoint string) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.now().Before(im.silent[endpoint])
}

// heard notes an endpoint that didn't answer at all. What came back, however
// odd, says something of one image or one search, and silences nothing.
func (im *images) heard(endpoint string, err error) {
	if !errors.Is(err, registry.ErrDown) {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.silent == nil {
		return
	}
	now := im.now()
	maps.DeleteFunc(im.silent, func(_ string, until time.Time) bool { return !now.Before(until) })
	if len(im.silent) < maxSilent {
		im.silent[endpoint] = now.Add(silentImagesFor)
	}
}

func (im *images) search(ctx context.Context, term string) (*dockwellv1.SearchImagesResponse, error) {
	term = strings.ToLower(strings.TrimSpace(term))
	res := &dockwellv1.SearchImagesResponse{}
	if term == "" {
		return res, nil
	}
	// Docker would search the registry such a term names, as no reach is checked.
	if first, _, path := strings.Cut(term, "/"); path && registry.NamesAHost(first) {
		return nil, errHostTerm
	}
	im.mu.Lock()
	found, ok := im.searches.get(term, im.now())
	im.mu.Unlock()
	if !ok {
		if im.isSilent(searchEndpoint) {
			res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			return res, nil
		}
		release, err := im.turn(ctx)
		if err != nil {
			return nil, err
		}
		found, ok = im.searchHub(ctx, term)
		release()
		if !ok {
			res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			return res, nil
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
		im.searches.put(term, found, im.now())
		im.mu.Unlock()
	}
	res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND
	for _, f := range found {
		res.Matches = append(res.Matches, &dockwellv1.ImageMatch{Name: f.Name, Description: f.Description, Official: f.Official, Stars: f.Stars})
	}
	return res, nil
}

// searchHub searches Docker Hub through the engine. Its search is its own:
// when it doesn't answer, what goes unasked a while is the search alone.
func (im *images) searchHub(ctx context.Context, term string) ([]docker.Found, bool) {
	found, err := im.docker.SearchImages(ctx, term, searchLimit)
	if err != nil {
		im.heard(searchEndpoint, registry.ErrDown)
		return nil, false
	}
	return found, true
}

func (im *images) listTags(ctx context.Context, client *registry.Client, repository string) (*dockwellv1.ListImageTagsResponse, error) {
	ref, err := registry.Parse(repository)
	if err != nil {
		return nil, err
	}
	key := ref.Registry + "/" + ref.Repository
	// Docker Hub's tags come from its API, which a mirror doesn't stand in for.
	endpoint := ref.Registry
	if ref.Registry == registry.Hub {
		endpoint = hubAPIEndpoint
	}
	res := &dockwellv1.ListImageTagsResponse{}
	im.mu.Lock()
	kept, ok := im.tags.get(key, im.now())
	im.mu.Unlock()
	if !ok {
		if im.isSilent(endpoint) {
			res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			return res, nil
		}
		release, err := im.turn(ctx)
		if err != nil {
			return nil, err
		}
		kept, err = client.Tags(ctx, ref)
		release()
		im.heard(endpoint, err)
		if err != nil {
			res.Answer = answer(err)
			return res, nil
		}
		im.mu.Lock()
		im.tags.put(key, kept, im.now())
		im.mu.Unlock()
	}
	res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND
	for _, t := range kept {
		tag := &dockwellv1.ImageTag{Name: t.Name, Digest: t.Digest, Latest: t.Latest}
		if !t.Pushed.IsZero() {
			tag.PushTime = timestamppb.New(t.Pushed)
		}
		res.Tags = append(res.Tags, tag)
	}
	return res, nil
}

// image says whether an image is there, and what it declares. Its registry
// alone says whether it's there, with a HEAD of its manifest: an image only the
// server has reads as not found, as which images a server holds is the
// install's to know (ADR 0048). What it declares comes from the server's copy
// of that very image, by the digest the registry named, which asks nothing more
// of the network, else from its registry, by digest.
func (im *images) image(ctx context.Context, client *registry.Client, platform registry.Platform, reference string) (*dockwellv1.GetImageResponse, error) {
	ref, err := registry.Parse(reference)
	if err != nil {
		return nil, err
	}
	res := &dockwellv1.GetImageResponse{}
	if im.isSilent(ref.Registry) {
		res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
		return res, nil
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	take := func() error {
		if release == nil {
			r, err := im.turn(ctx)
			if err != nil {
				return err
			}
			release = r
		}
		return nil
	}
	key := ref.String()
	im.mu.Lock()
	digest, ok := im.digests.get(key, im.now())
	im.mu.Unlock()
	if !ok {
		if err := take(); err != nil {
			return nil, err
		}
		digest, err = client.Head(ctx, ref)
		im.heard(ref.Registry, err)
		if err != nil {
			res.Answer = answer(err)
			return res, nil
		}
		if digest != "" {
			im.mu.Lock()
			im.digests.put(key, digest, im.now())
			im.mu.Unlock()
		}
	}
	res.Answer, res.Digest = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, digest
	if digest != "" {
		im.mu.Lock()
		cfg, ok := im.configs.get(digest, im.now())
		im.mu.Unlock()
		if ok {
			res.Config = configProto(cfg)
			return res, nil
		}
		// By name and digest alone, never as typed: Docker finds an image by a part
		// of its ID, and this is the image the registry named, not any the server has.
		if local, err := im.docker.InspectImage(ctx, ref.Name()+"@"+digest); err == nil && local.Config != nil {
			c := local.Config
			cfg := &registry.Config{
				Ports: registry.Ports(c.ExposedPorts), Env: registry.Vars(c.Env), Volumes: registry.Volumes(c.Volumes),
				Cmd: c.Cmd, Entrypoint: c.Entrypoint, User: c.User, WorkingDir: c.WorkingDir,
				Healthcheck: healthTest(c.Healthcheck), Platform: registry.Platform{OS: "linux", Architecture: local.Architecture},
			}
			if configSize(cfg) <= registry.MaxConfig {
				im.mu.Lock()
				im.configs.put(digest, cfg, im.now())
				im.mu.Unlock()
				res.Config = configProto(cfg)
				return res, nil
			}
		}
	}
	if ref.Registry == registry.Hub && im.o.Test == "" && !im.spendHub() {
		res.Unread = dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED
		return res, nil
	}
	if err := take(); err != nil {
		return nil, err
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
	if digest != "" {
		im.mu.Lock()
		im.configs.put(digest, read, im.now())
		im.mu.Unlock()
	}
	res.Config = configProto(read)
	return res, nil
}

// spendHub takes the manifest GETs a read may make of Docker Hub, hubPerRead,
// of the hubReads it may make in hubReadWindow, if they're left.
func (im *images) spendHub() bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	now := im.now()
	im.hub = slices.DeleteFunc(im.hub, func(at time.Time) bool { return now.Sub(at) >= hubReadWindow })
	if len(im.hub)+hubPerRead > hubReads {
		return false
	}
	for range hubPerRead {
		im.hub = append(im.hub, now)
	}
	return true
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

// shelf keeps answers by key, each for imageReadFor, within a budget of bytes,
// the first kept going first when it's full. Reading one keeps it no longer.
// A nil shelf keeps nothing. The caller holds images.mu.
type shelf[T any] struct {
	budget int
	size   func(T) int
	items  map[string]*list.Element
	order  *list.List
	bytes  int
}

type shelved[T any] struct {
	key   string
	value T
	at    time.Time
	bytes int
}

func newShelf[T any](budget int, size func(T) int) *shelf[T] {
	return &shelf[T]{budget: budget, size: size, items: map[string]*list.Element{}, order: list.New()}
}

func (s *shelf[T]) get(key string, now time.Time) (T, bool) {
	var zero T
	if s == nil {
		return zero, false
	}
	e, ok := s.items[key]
	if !ok {
		return zero, false
	}
	it := e.Value.(*shelved[T])
	if now.Sub(it.at) >= imageReadFor {
		s.remove(e)
		return zero, false
	}
	return it.value, true
}

func (s *shelf[T]) put(key string, value T, now time.Time) {
	if s == nil {
		return
	}
	if e, ok := s.items[key]; ok {
		s.remove(e)
	}
	// What an entry takes beside its strings: the map's, the list's and its own.
	n := len(key) + s.size(value) + 160
	if n > s.budget {
		return
	}
	for s.bytes+n > s.budget {
		s.remove(s.order.Front())
	}
	s.items[key] = s.order.PushBack(&shelved[T]{key: key, value: value, at: now, bytes: n})
	s.bytes += n
}

func (s *shelf[T]) remove(e *list.Element) {
	it := e.Value.(*shelved[T])
	s.order.Remove(e)
	delete(s.items, it.key)
	s.bytes -= it.bytes
}

// len is how many answers it keeps.
func (s *shelf[T]) len() int {
	if s == nil {
		return 0
	}
	return len(s.items)
}

// What an answer takes, about: its strings, and a word or two beside each.
func strs(words []string) int {
	n := 0
	for _, s := range words {
		n += len(s) + 16
	}
	return n
}

// configStruct is about what a Config takes beside its strings: its slices' headers
// and its fields, so a budget of small configs means what it says.
const configStruct = 256

func configSize(c *registry.Config) int {
	n := configStruct + strs(c.Volumes) + strs(c.Cmd) + strs(c.Entrypoint) + strs(c.Healthcheck) + len(c.User) + len(c.WorkingDir) + 8*len(c.Ports)
	for _, v := range c.Env {
		n += len(v.Name) + len(v.Value) + 32
	}
	return n
}

func tagsSize(tags []registry.Tag) int {
	n := 0
	for _, t := range tags {
		n += len(t.Name) + len(t.Digest) + 64
	}
	return n
}

func foundSize(found []docker.Found) int {
	n := 0
	for _, f := range found {
		n += len(f.Name) + len(f.Description) + 48
	}
	return n
}
