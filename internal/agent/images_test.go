// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/registry"
)

// imageRegistry stands in for every registry: it has library/nginx and
// acme/web, and counts the manifest GETs Docker Hub would count as pulls. Down,
// nothing reaches it; a tag called broken answers 500.
type imageRegistry struct {
	srv   *httptest.Server
	gets  atomic.Int32
	heads atomic.Int32
	down  atomic.Bool
	// hold, when set, holds every request until it's closed.
	hold atomic.Pointer[chan struct{}]
}

func newImageRegistry(t *testing.T) *imageRegistry {
	f := &imageRegistry{}
	config := `{"os":"linux","architecture":"amd64","config":{"ExposedPorts":{"80/tcp":{}},"Env":["NGINX_VERSION=1.27.3"],"Volumes":{"/data":{}}}}`
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hold := f.hold.Load(); hold != nil {
			<-*hold
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/manifests/") && (strings.Contains(path, "/library/nginx/") || strings.Contains(path, "/acme/web/")):
			ref := path[strings.LastIndex(path, "/")+1:]
			switch ref {
			case "nope":
				http.NotFound(w, r)
				return
			case "broken":
				http.Error(w, "broken", http.StatusInternalServerError)
				return
			}
			// Each tag is an image of its own, so each config is read once.
			w.Header().Set("Docker-Content-Digest", fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(path))))
			if r.Method == http.MethodHead {
				f.heads.Add(1)
			}
			if r.Method == http.MethodGet {
				f.gets.Add(1)
				_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%064d","size":%d}}`, 1, len(config))
			}
		case strings.Contains(path, "/blobs/"):
			_, _ = w.Write([]byte(config))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// to sends every request to the stand-in, whatever registry it was for, as a
// server's network would take it to the registry it names, and fails as a
// connection does while it's down.
type to struct {
	base *url.URL
	down *atomic.Bool
}

func (d to) Do(req *http.Request) (*http.Response, error) {
	if d.down != nil && d.down.Load() {
		return nil, errors.New("dial tcp: connection refused")
	}
	req.URL.Scheme, req.URL.Host = d.base.Scheme, d.base.Host
	return http.DefaultClient.Do(req)
}

func newTestImages(t *testing.T, f *imageRegistry, now func() time.Time) (*images, *dockertest.Engine) {
	t.Helper()
	e := dockertest.New(t)
	base, _ := url.Parse(f.srv.URL)
	im := newImages(e.Client(), ImageReads{}, now)
	// Made as ready makes it, with every request sent to the stand-in.
	im.newClient = func(o registry.Options) *registry.Client {
		stand := to{base, &f.down}
		o.Secure, o.Plain, o.Public = stand, stand, stand
		return registry.New(o)
	}
	return im, e
}

func imageCall(ref string) *agentv1.ImageCall {
	return &agentv1.ImageCall{Ask: &agentv1.ImageCall_Image{Image: &dockwellv1.GetImageRequest{Reference: ref}}}
}

func TestImagesReadAnImageOnceAndKeepIt(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	ctx := context.Background()
	got, err := im.read(ctx, imageCall("ghcr.io/acme/web:1.2"))
	if err != nil {
		t.Fatal(err)
	}
	res := got.GetImage()
	if res.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || res.GetDigest() == "" {
		t.Fatalf("answer %v, digest %q", res.GetAnswer(), res.GetDigest())
	}
	c := res.GetConfig()
	if len(c.GetPorts()) != 1 || c.GetPorts()[0].GetPort() != 80 || len(c.GetVolumes()) != 1 || c.GetEnv()[0].GetName() != "NGINX_VERSION" {
		t.Errorf("config = %v", c)
	}
	if _, err := im.read(ctx, imageCall("ghcr.io/acme/web:1.2")); err != nil || f.gets.Load() != 1 {
		t.Errorf("a second read made %d manifest GETs in all, want 1 (its config is kept): %v", f.gets.Load(), err)
	}
	got, _ = im.read(ctx, imageCall("ghcr.io/acme/web:nope"))
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND {
		t.Errorf("a missing tag reads %v", got.GetImage().GetAnswer())
	}
}

// An image only the server has, or a part of an image's ID, reads as not found,
// whatever it holds: which images a server has is the install's to know (ADR
// 0048), and the answer is the registry's alone (ADR 0062).
func TestImagesSayNothingOfWhatOnlyTheServerHas(t *testing.T) {
	f := newImageRegistry(t)
	im, e := newTestImages(t, f, nil)
	e.Registry = map[string]dockertest.Image{"otherteam/billing:2": {Config: docker.Config{
		Env: []string{"STRIPE_KEY=sk_test_example"}, Cmd: []string{"billing", "--db", "postgres://billing:pw@db/billing"},
	}}}
	if err := e.Client().PullImage(context.Background(), "otherteam/billing:2", nil); err != nil {
		t.Fatal(err)
	}
	local, err := e.Client().InspectImage(context.Background(), "otherteam/billing:2")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(local.ID, "sha256:")
	for _, ref := range []string{"otherteam/billing:2", id[:12], id} {
		got, err := im.read(context.Background(), imageCall(ref))
		if err != nil {
			t.Fatal(err)
		}
		res := got.GetImage()
		if res.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND || res.GetConfig() != nil {
			t.Errorf("%s, which only the server has, reads %v", ref, res)
		}
	}
}

// What the registry names, the server's copy of says what it declares, with no
// manifest read of Docker Hub's allowance.
func TestImagesReadTheServersCopyOfWhatTheRegistryNamed(t *testing.T) {
	f := newImageRegistry(t)
	im, e := newTestImages(t, f, nil)
	// The stand-in's digest of nginx:1.27, which the server pulled.
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("/v2/library/nginx/manifests/1.27")))
	e.Registry = map[string]dockertest.Image{"nginx:1.27": {Digest: digest, Config: docker.Config{
		ExposedPorts: map[string]struct{}{"8080/tcp": {}},
	}}}
	if err := e.Client().PullImage(context.Background(), "nginx:1.27", nil); err != nil {
		t.Fatal(err)
	}
	got, err := im.read(context.Background(), imageCall("nginx:1.27"))
	if err != nil {
		t.Fatal(err)
	}
	res := got.GetImage()
	if res.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || res.GetDigest() != digest || res.GetConfig().GetPorts()[0].GetPort() != 8080 {
		t.Errorf("an image the registry named and the server has reads %v", res)
	}
	if f.gets.Load() != 0 {
		t.Errorf("reading the server's copy made %d manifest GETs", f.gets.Load())
	}
}

func TestImagesSpendDockerHubsAllowanceOnlyOnWhatNoCacheHad(t *testing.T) {
	f := newImageRegistry(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	im, _ := newTestImages(t, f, clock)
	ctx := context.Background()
	for i := range hubReads / hubPerRead {
		got, err := im.read(ctx, imageCall(fmt.Sprintf("nginx:1.%d", i)))
		if err != nil || got.GetImage().GetConfig() == nil {
			t.Fatalf("read %d: %v, %v", i, got, err)
		}
	}
	// Read again, it's kept, and costs nothing.
	if got, _ := im.read(ctx, imageCall("nginx:1.0")); got.GetImage().GetConfig() == nil {
		t.Errorf("a kept config wasn't read from what's kept")
	}
	got, _ := im.read(ctx, imageCall("nginx:2.0"))
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || got.GetImage().GetUnread() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED || got.GetImage().GetConfig() != nil {
		t.Errorf("past the allowance, a read says %v", got.GetImage())
	}
	if f.gets.Load() != hubReads/hubPerRead {
		t.Errorf("Docker Hub saw %d counted GETs, want %d", f.gets.Load(), hubReads/hubPerRead)
	}
	// Six hours on, there's an allowance again.
	now = now.Add(hubReadWindow)
	if got, _ := im.read(ctx, imageCall("nginx:2.0")); got.GetImage().GetConfig() == nil {
		t.Errorf("after six hours, a read still says %v", got.GetImage())
	}
}

func TestImagesLeaveASilentRegistryAloneForAWhile(t *testing.T) {
	f := newImageRegistry(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	im, _ := newTestImages(t, f, func() time.Time { return now })
	f.down.Store(true)
	got, _ := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2"))
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Fatalf("a registry that's down reads %v", got.GetImage().GetAnswer())
	}
	f.down.Store(false)
	if got, _ := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Errorf("a registry that didn't answer was asked again at once")
	}
	now = now.Add(silentImagesFor)
	if got, _ := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Errorf("after a while, it reads %v", got.GetImage().GetAnswer())
	}
}

func TestImagesSearchDockerHubThroughTheEngine(t *testing.T) {
	f := newImageRegistry(t)
	im, e := newTestImages(t, f, nil)
	e.Hub = []docker.Found{{Name: "bitnami/postgresql", Stars: 300}, {Name: "postgres", Official: true, Stars: 14000}, {Name: "nginx", Official: true}}
	got, err := im.read(context.Background(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Search{Search: &dockwellv1.SearchImagesRequest{Term: "POST"}}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got.GetSearch().GetMatches() {
		names = append(names, m.GetName())
	}
	if strings.Join(names, ",") != "postgres,bitnami/postgresql" {
		t.Errorf("search found %v, want the official one first", names)
	}
}

func TestImagesForgetEverythingWhenIdle(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	if _, err := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); err != nil {
		t.Fatal(err)
	}
	im.forget()
	im.mu.Lock()
	kept := im.configs.len() + im.digests.len()
	im.mu.Unlock()
	if kept != 0 || im.client != nil {
		t.Errorf("an idle reader keeps %d answers", kept)
	}
}

func TestImagesSayWhyTheyAreOff(t *testing.T) {
	im := newImages(nil, ImageReads{Off: "this is a test, so Dockwell reads no image from a registry here"}, nil)
	if _, err := im.read(context.Background(), imageCall("nginx")); err == nil || !strings.Contains(err.Error(), "this is a test") {
		t.Errorf("a reader that's off says %v", err)
	}
}

// configRegistry stands in for a registry whose every image's config holds
// about size bytes of Env.
func configRegistry(t *testing.T, size int) *httptest.Server {
	var cfg strings.Builder
	cfg.WriteString(`{"os":"linux","architecture":"amd64","config":{"Env":[`)
	for i := 0; cfg.Len() < size; i++ {
		if i > 0 {
			cfg.WriteByte(',')
		}
		fmt.Fprintf(&cfg, `"V%d=%s"`, i, strings.Repeat("x", 200))
	}
	cfg.WriteString(`]}}`)
	config := cfg.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/manifests/"):
			w.Header().Set("Docker-Content-Digest", fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(p[:strings.LastIndex(p, "/manifests/")]))))
			if r.Method == http.MethodGet {
				// A registry may say a config is small and send a large one.
				_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%064d","size":%d}}`, 1, min(len(config), 1000))
			}
		case strings.Contains(p, "/blobs/"):
			_, _ = w.Write([]byte(config))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func imagesAt(t *testing.T, srv *httptest.Server) *images {
	base, _ := url.Parse(srv.URL)
	im := newImages(dockertest.New(t).Client(), ImageReads{}, nil)
	im.newClient = func(o registry.Options) *registry.Client {
		o.Secure, o.Plain, o.Public = to{base: base}, to{base: base}, to{base: base}
		return registry.New(o)
	}
	return im
}

func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// What's kept stays within its budget of bytes, however many images are read,
// and a config larger than any image's isn't read at all.
func TestImagesKeepWhatTheyReadWithinABudget(t *testing.T) {
	ctx := context.Background()
	im := imagesAt(t, configRegistry(t, 200<<10))
	before := heapInUse()
	for i := range 64 {
		got, err := im.read(ctx, imageCall(fmt.Sprintf("registry.example.com/acme/app%d:1", i)))
		if err != nil || got.GetImage().GetConfig() == nil {
			t.Fatalf("read %d: %v %v", i, got, err)
		}
	}
	for i := range 2000 {
		if _, err := im.read(ctx, imageCall(fmt.Sprintf("registry.example.com/acme/web:t%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	im.mu.Lock()
	configs, digests := im.configs.bytes, im.digests.bytes
	im.mu.Unlock()
	if configs > configBytes || digests > digestBytes {
		t.Errorf("configs take %d bytes of %d, digests %d of %d", configs, configBytes, digests, digestBytes)
	}
	grew := int64(heapInUse()) - int64(before)
	t.Logf("64 configs of 200 KiB and 2,000 more images read: configs keep %d KiB, digests %d KiB, the heap grew %d KiB", configs>>10, digests>>10, grew>>10)
	if grew > 8<<20 {
		t.Errorf("what's kept grew the heap by %d MiB", grew>>20)
	}

	large := imagesAt(t, configRegistry(t, 4<<20))
	got, err := large.read(ctx, imageCall("registry.example.com/acme/large:1"))
	if err != nil {
		t.Fatal(err)
	}
	if res := got.GetImage(); res.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || res.GetConfig() != nil || res.GetUnread() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Errorf("an image whose config is 4 MiB reads %v", res)
	}
	if large.isSilent("registry.example.com") {
		t.Errorf("one image's config silenced its registry")
	}
}

// Everything goes 30 minutes after the first read, however often it's read
// meanwhile: the client, its tokens and what went unanswered with it.
func TestImagesForgetThirtyMinutesAfterTheFirstRead(t *testing.T) {
	f := newImageRegistry(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	im, _ := newTestImages(t, f, func() time.Time { return now })
	read := func() *registry.Client {
		t.Helper()
		if _, err := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); err != nil {
			t.Fatal(err)
		}
		im.mu.Lock()
		defer im.mu.Unlock()
		return im.client
	}
	first := read()
	for range 5 {
		now = now.Add(5 * time.Minute)
		if read() != first {
			t.Fatalf("a read %s after the first started afresh", now.Sub(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)))
		}
	}
	now = now.Add(5 * time.Minute)
	if read() == first {
		t.Errorf("30 minutes of reads, five minutes apart, kept what the first read made")
	}
}

// Only an endpoint that didn't answer goes unasked: not Docker Hub after its
// search failed, nor a registry after one image's answer was odd.
func TestImagesSilenceOnlyWhatDidNotAnswer(t *testing.T) {
	f := newImageRegistry(t)
	im, e := newTestImages(t, f, nil)
	ctx := context.Background()
	e.RegistryDown = true
	got, err := im.read(ctx, &agentv1.ImageCall{Ask: &agentv1.ImageCall_Search{Search: &dockwellv1.SearchImagesRequest{Term: "ngin"}}})
	if err != nil || got.GetSearch().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Fatalf("a failed search reads %v, %v", got, err)
	}
	e.RegistryDown = false
	if got, _ := im.read(ctx, imageCall("nginx:1.27")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Errorf("after a failed search, nginx:1.27 reads %v", got.GetImage().GetAnswer())
	}
	if got, _ := im.read(ctx, imageCall("ghcr.io/acme/web:broken")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Errorf("a tag the registry answers 500 for reads %v", got.GetImage().GetAnswer())
	}
	if got, _ := im.read(ctx, imageCall("ghcr.io/acme/web:1.2")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Errorf("after one odd answer, the registry's next image reads %v", got.GetImage().GetAnswer())
	}
}

// Two reads reach registries at once; a third waits for its turn, as a form
// sends its search, its check and its tags close together, and is told to try
// again only when its own time runs out first.
func TestImagesReadTwoAtOnce(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	done := make(chan error, imageCalls+1)
	for i := range imageCalls {
		go func() {
			_, err := im.read(context.Background(), imageCall(fmt.Sprintf("ghcr.io/acme/web:%d", i)))
			done <- err
		}()
	}
	// Both hold their place until their registry answers.
	deadline := time.Now().Add(5 * time.Second)
	for len(im.calls) < imageCalls && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := im.read(short, imageCall("ghcr.io/acme/web:3")); !errors.Is(err, errBusy) {
		t.Errorf("a third read whose time ran out waiting = %v, want errBusy", err)
	}
	var third *agentv1.ImageResult
	go func() {
		got, err := im.read(context.Background(), imageCall("ghcr.io/acme/web:4"))
		third = got
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	f.hold.Store(nil)
	close(hold)
	for range imageCalls + 1 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if third.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Errorf("a third read that waited its turn reads %v", third.GetImage())
	}
}

// A read that ends before its registry answers, as its caller left or its time
// ran out, says nothing of the registry, which goes on being asked.
func TestImagesLeaveARegistryAskedWhenAReadEndsFirst(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	left, leave := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		leave()
	}()
	if got, err := im.read(left, imageCall("nginx:1.2")); err == nil && got.GetImage().GetAnswer() == dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Fatalf("a read whose caller left reads %v", got.GetImage())
	}
	late, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, _ = im.read(late, imageCall("ghcr.io/acme/web:1.2"))
	f.hold.Store(nil)
	close(hold)
	for _, host := range []string{registry.Hub, "ghcr.io"} {
		if im.isSilent(host) {
			t.Errorf("%s went unasked after a read of it ended first", host)
		}
	}
	if got, _ := im.read(context.Background(), imageCall("nginx:1.27")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Errorf("the next read reads %v", got.GetImage().GetAnswer())
	}
}

// Only Docker Hub is searched: a term that names another registry isn't sent.
func TestImagesSearchNoOtherRegistry(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	for _, term := range []string{"127.0.0.1:2375/x", "registry.example.com/acme", "localhost/x"} {
		_, err := im.read(context.Background(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Search{Search: &dockwellv1.SearchImagesRequest{Term: term}}})
		if !errors.Is(err, errHostTerm) {
			t.Errorf("a search for %s = %v", term, err)
		}
	}
}

// A plan pins an entry's images with one call for all of them: a HEAD each, which no
// registry counts as a pull, into the same short-lived digests a form's reads keep,
// so a form's read after it asks no registry again (ADR 0078).
func TestImagesPinDigestsWithHeadsAlone(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	ctx := context.Background()
	refs := []string{"nginx:1.27", "ghcr.io/acme/web:nope", "ghcr.io/acme/web:broken", "Not An Image"}
	got, err := im.read(ctx, &agentv1.ImageCall{Ask: &agentv1.ImageCall_Digests{Digests: &agentv1.ImageDigestsCall{References: refs}}})
	if err != nil {
		t.Fatal(err)
	}
	ds := got.GetDigests().GetDigests()
	if len(ds) != len(refs) {
		t.Fatalf("digests = %v", ds)
	}
	want := []dockwellv1.ImageAnswer{dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND, dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE, dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND}
	for i, d := range ds {
		if d.GetReference() != refs[i] || d.GetAnswer() != want[i] || (i == 0) != (d.GetDigest() != "") {
			t.Errorf("%s: %v %q, want %v", refs[i], d.GetAnswer(), d.GetDigest(), want[i])
		}
	}
	if f.gets.Load() != 0 {
		t.Errorf("pinning made %d manifest GETs, which Docker Hub counts as pulls", f.gets.Load())
	}
	heads := f.heads.Load()
	read, err := im.read(ctx, imageCall("nginx:1.27"))
	if err != nil || read.GetImage().GetDigest() != ds[0].GetDigest() || f.heads.Load() != heads {
		t.Errorf("a form's read after pinning asked again: %d HEADs, then %d, digest %q, %v", heads, f.heads.Load(), read.GetImage().GetDigest(), err)
	}
	many := make([]string, mostDigests+1)
	for i := range many {
		many[i] = "nginx:1.27"
	}
	if _, err := im.read(ctx, &agentv1.ImageCall{Ask: &agentv1.ImageCall_Digests{Digests: &agentv1.ImageDigestsCall{References: many}}}); err == nil {
		t.Error("a call for more digests than an agent reads at once went ahead")
	}
}

// A plan's digests come from a store's file, so a registry the server's Docker
// doesn't name is read at public addresses alone, wherever its name leads: an image
// on this server's own registry is never asked about, unless the server's Docker
// names that registry, as a pull would reach it then (the review of #548, its M2).
func TestImagesPinOnlyWhatDockerNamesOffThePublicInternet(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	read := func(e *dockertest.Engine) *agentv1.ImageDigest {
		t.Helper()
		im := newImages(e.Client(), ImageReads{}, nil)
		got, err := im.read(context.Background(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Digests{Digests: &agentv1.ImageDigestsCall{References: []string{host + "/x:1.0"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return got.GetDigests().GetDigests()[0]
	}
	if d := read(dockertest.New(t)); d.GetAnswer() == dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || asked.Load() != 0 {
		t.Errorf("a registry on this server that Docker doesn't name: %v %q, asked %d times", d.GetAnswer(), d.GetDigest(), asked.Load())
	}
	named := dockertest.New(t)
	named.Registries = []string{host}
	if d := read(named); d.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || asked.Load() == 0 {
		t.Errorf("a registry Docker's daemon.json names: %v, asked %d times", d.GetAnswer(), asked.Load())
	}
}

// A registry that names a digest that isn't one answers nothing a plan could pin
// (the review of #548, its L1).
func TestImagesRefuseADigestThatIsntOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:../../not-a-digest")
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	e := dockertest.New(t)
	e.Registries = []string{host}
	im := newImages(e.Client(), ImageReads{}, nil)
	got, err := im.read(context.Background(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Digests{Digests: &agentv1.ImageDigestsCall{References: []string{host + "/x:1.0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d := got.GetDigests().GetDigests()[0]; d.GetAnswer() == dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || d.GetDigest() != "" {
		t.Errorf("a digest that isn't one: %v %q", d.GetAnswer(), d.GetDigest())
	}
}
