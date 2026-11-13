// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/netguard"
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

	mu     sync.Mutex
	client *registry.Client
	// public reads a registry for a plan's digests that the server's Docker doesn't
	// name, at public addresses alone (ADR 0078), and named are the registries its
	// configuration names, which client reads as a pull would.
	public   *registry.Client
	named    map[string]bool
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
	case call.GetDigests() != nil:
		res, err := im.digestsOf(ctx, client, call.GetDigests().GetReferences())
		if err != nil {
			return nil, err
		}
		return &agentv1.ImageResult{Answer: &agentv1.ImageResult_Digests{Digests: res}}, nil
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
	named := map[string]bool{registry.Hub: true}
	if info, err := im.docker.Info(ctx); err == nil && info.RegistryConfig != nil {
		o.Mirrors, o.Insecure = info.RegistryConfig.Mirrors, info.RegistryConfig.Insecure
		for host := range info.RegistryConfig.IndexConfigs {
			named[host] = true
		}
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
	// Over HTTPS, at public addresses alone, wherever a name leads.
	guard := netguard.Client(netguard.Options{MaxBody: registry.MaxBody, Timeout: registry.RequestTimeout})
	public := im.newClient(registry.Options{Test: im.o.Test, Now: im.now, Secure: guard, Plain: guard, Public: guard, Insecure: func(string) bool { return false }})
	im.mu.Lock()
	defer im.mu.Unlock()
	switch {
	case im.client != nil:
		return im.client, im.platform
	case platform.Architecture == "":
		return client, platform
	}
	im.client, im.platform, im.public, im.named = client, platform, public, named
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
	im.public, im.named = nil, nil
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
	case errors.Is(err, registry.ErrSignIn), errors.Is(err, registry.ErrMoved):
		return dockwellv1.ImageAnswer_IMAGE_ANSWER_SIGN_IN_FAILED
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
	key := ref.Registry + "/" + ref.Repository + signedInAs(ctx)
	// Docker Hub's tags come from its API, which a mirror doesn't stand in for.
	endpoint := ref.Registry
	if ref.Registry == registry.Hub && signedInAs(ctx) == "" {
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
// digest is the digest an image's tag points at, as its registry answers a HEAD of
// its manifest, which no registry counts as a pull, kept for imageReadFor. take
// waits for a turn to reach the registry, which only a lookup that isn't kept needs.
func (im *images) digest(ctx context.Context, client *registry.Client, ref registry.Ref, take func() error) (string, dockwellv1.ImageAnswer, error) {
	key := ref.String() + signedInAs(ctx)
	im.mu.Lock()
	digest, ok := im.digests.get(key, im.now())
	im.mu.Unlock()
	if ok {
		return digest, dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, nil
	}
	if err := take(); err != nil {
		return "", 0, err
	}
	digest, err := client.Head(ctx, ref)
	im.heard(ref.Registry, err)
	if err != nil {
		return "", answer(err), nil
	}
	if digest != "" {
		im.mu.Lock()
		im.digests.put(key, digest, im.now())
		im.mu.Unlock()
	}
	return digest, dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, nil
}

// mostDigests is how many images one call asks the digests of: an app's handful.
const mostDigests = 20

// digestsOf answers each image's digest, as a plan pins it (ADR 0078), in the order
// asked. One reference that can't be read says so in its answer, and no other's. A
// registry the server's Docker doesn't name is read at public addresses alone, as
// what asks is a store's file, whatever its registry's name resolves to.
func (im *images) digestsOf(ctx context.Context, client *registry.Client, refs []string) (*agentv1.ImageDigests, error) {
	im.mu.Lock()
	public, named := im.public, im.named
	im.mu.Unlock()
	if public == nil {
		// Made without the server's architecture, for this read alone: guard it all.
		guard := netguard.Client(netguard.Options{MaxBody: registry.MaxBody, Timeout: registry.RequestTimeout})
		public = im.newClient(registry.Options{Test: im.o.Test, Now: im.now, Secure: guard, Plain: guard, Public: guard, Insecure: func(string) bool { return false }})
	}
	if len(refs) > mostDigests {
		return nil, fmt.Errorf("the call asks for %d digests, more than the %d an agent reads at once", len(refs), mostDigests)
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
	out := &agentv1.ImageDigests{}
	for _, reference := range refs {
		d := &agentv1.ImageDigest{Reference: reference}
		out.Digests = append(out.Digests, d)
		ref, err := registry.Parse(reference)
		if err != nil {
			d.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND
			continue
		}
		if im.isSilent(ref.Registry) {
			d.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
			continue
		}
		via := public
		if named[ref.Registry] {
			via = client
		}
		digest, said, err := im.digest(ctx, via, ref, take)
		if err != nil {
			return nil, err
		}
		d.Answer, d.Digest = said, digest
	}
	return out, nil
}

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
	digest, said, err := im.digest(ctx, client, ref, take)
	switch {
	case err != nil:
		return nil, err
	case said != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND:
		res.Answer = said
		return res, nil
	}
	res.Answer, res.Digest = dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, digest
	if digest != "" {
		im.mu.Lock()
		cfg, ok := im.configs.get(digest+signedInAs(ctx), im.now())
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
				Version: registry.VersionOf(c.Labels),
			}
			if configSize(cfg) <= registry.MaxConfig {
				im.mu.Lock()
				im.configs.put(digest+signedInAs(ctx), cfg, im.now())
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
		im.configs.put(digest+signedInAs(ctx), read, im.now())
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
		Healthcheck: c.Healthcheck, Platform: c.Platform.String(), Version: c.Version,
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
	n := configStruct + strs(c.Volumes) + strs(c.Cmd) + strs(c.Entrypoint) + strs(c.Healthcheck) + len(c.User) + len(c.WorkingDir) + len(c.Version) + 8*len(c.Ports)
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

// signedInAs tells what a read signed in with a login keeps from what a read
// without it, or with another, keeps: nothing a login read is given to a read
// without it (ADR 0086).
func signedInAs(ctx context.Context) string {
	a, ok := registry.AuthOf(ctx)
	if !ok {
		return ""
	}
	return " as " + a.Registry + "/" + a.Name + "/" + a.Username
}

// challenge refuses a login whose registry would send it to a token service other
// than the one it was saved with, before Docker is handed it, as Docker follows
// whatever realm a registry names (ADR 0086). A registry that doesn't answer says
// nothing either way: the login isn't handed to Docker, and what the server has
// stands.
func (im *images) challenge(ctx context.Context, a registry.Auth) (bool, error) {
	return im.challengeFor(ctx, a, true)
}

// challengeFor is challenge for Docker's pull, or with docker false, for a build's
// BuildKit, which reads none of Docker's mirrors.
func (im *images) challengeFor(ctx context.Context, a registry.Auth, viaDocker bool) (bool, error) {
	if im.o.Off != "" || im.docker == nil {
		return false, nil
	}
	// Docker sends a Docker Hub login to the mirrors its daemon.json names too, so
	// with any, Docker is handed none: what it pulls from Docker Hub, it pulls as
	// anyone. Dockwell's own reads go to Docker Hub alone, with it.
	if viaDocker && a.Registry == registry.Hub {
		if info, err := im.docker.Info(ctx); err != nil || info.RegistryConfig != nil && len(info.RegistryConfig.Mirrors) > 0 {
			return false, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, registry.RequestTimeout)
	defer cancel()
	client, _ := im.ready(ctx)
	err := client.Challenge(ctx, a)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, registry.ErrMoved):
		return false, fmt.Errorf("%s now sends clients to another host for its tokens, or to its own over plain HTTP, so Dockwell won't sign in there with the registry login %s. Check the login again", a.Registry, a.Name)
	case errors.Is(err, registry.ErrUnreadChallenge):
		return false, fmt.Errorf("%s asks for a sign-in in a way Dockwell can't read, so Dockwell won't hand Docker the registry login %s, as Docker could send it where Dockwell can't see", a.Registry, a.Name)
	}
	return false, nil
}

// forBuild keeps the logins a build may hand BuildKit: none for a registry on the
// loopback, by its name or what it resolves to, as a build's steps share the
// builder's, and none whose registry's challenge doesn't answer; one that moved its
// token service refuses the build (ADR 0086).
func (im *images) forBuild(ctx context.Context, auths []registry.Auth, logger *slog.Logger) ([]registry.Auth, error) {
	var kept []registry.Auth
	for _, a := range auths {
		// A test's registries are the test's, and a test resolves no name.
		loopback := registry.OnLoopback(a.Registry)
		if !loopback && im.o.Test == "" {
			loopback = registry.ResolvesToLoopback(ctx, a.Registry)
		}
		if loopback {
			logger.Warn("a build isn't given a registry login whose registry is on this server's loopback, which a build's steps share", "login", a.Name, "registry", a.Registry)
			continue
		}
		ok, err := im.challengeFor(ctx, a, false)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, a)
		}
	}
	return kept, nil
}

// checkLogin signs in at a login's registry, as a pull would, and keeps nothing.
// first lets a login not yet saved go to the token service the registry names.
func (im *images) checkLogin(ctx context.Context, a registry.Auth, first bool) *agentv1.CheckRegistryLoginResult {
	out := &agentv1.CheckRegistryLoginResult{}
	if im.o.Off != "" || im.docker == nil {
		out.State, out.Detail = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_UNREACHABLE, "This agent reads no registry"
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, imageReadTimeout)
	defer cancel()
	client, _ := im.ready(ctx)
	host, err := client.SignIn(ctx, a, first)
	switch {
	case err == nil:
		out.State, out.TokenHost = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_SIGNED_IN, host
	case errors.Is(err, registry.ErrSignIn):
		out.State = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_REFUSED
	case errors.Is(err, registry.ErrMoved):
		out.State, out.TokenHost = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_MOVED, host
	default:
		out.State, out.Detail = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_UNREACHABLE, line(err.Error())
	}
	return out
}

// probeRegistry asks a registry's /v2/ with no login, as the panel does before a
// token typed for it goes there (ADR 0086).
func (im *images) probeRegistry(ctx context.Context, host string) *agentv1.CheckRegistryLoginResult {
	out := &agentv1.CheckRegistryLoginResult{}
	if im.o.Off != "" || im.docker == nil {
		out.State, out.Detail = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_UNREACHABLE, "This agent reads no registry"
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, imageReadTimeout)
	defer cancel()
	client, _ := im.ready(ctx)
	asks, tokenHost, err := client.Probe(ctx, host)
	switch {
	case err != nil:
		out.State, out.Detail = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_UNREACHABLE, line(err.Error())
	case asks:
		out.State, out.TokenHost = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_ASKS, tokenHost
	default:
		out.State = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_SIGNED_IN
	}
	return out
}
