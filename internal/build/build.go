// SPDX-License-Identifier: AGPL-3.0-only

// Package build builds a service's image from Git on the server it runs on, with
// BuildKit in a container of its own for each build, through the server's Docker.
// The container has a container's limits, so a build can't take a small server
// down, and a build that fails or is cancelled has touched nothing that runs. See
// docs/adr/0046-builds-from-git.md.
package build

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// BuildKit's image, by the digest of its multi-platform index, so whatever serves
// it, Docker checks it is this one, as Caddy's is pinned (ADR 0024). Pin a new one
// to a release at least 14 days old. The tag is what the digest was read from.
const (
	Repository = "moby/buildkit"
	Tag        = "v0.34.1"
	Digest     = "sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c"
	Image      = Repository + "@" + Digest
)

// CacheVolume is the volume BuildKit keeps its layer cache in, which every build on
// the server shares.
const CacheVolume = "dockwell-build-cache"

// What a build may use (ADR 0046).
const (
	// keepForRunning is the memory a build leaves for what runs.
	keepForRunning = 256 << 20
	minMemory      = 512 << 20
	maxMemory      = 8 << 30
	// cpuShares is a quarter of an ordinary container's weight.
	cpuShares = 256
	pidsLimit = 4096
	// cache bounds what BuildKit keeps: a tenth of the disk, within these.
	minCache = 1 << 30
	maxCache = 10 << 30
	// keepBuilt is how many built images a service keeps beside the one it runs.
	keepBuilt = 3
	// cleanupWait bounds removing a builder once its build ended.
	cleanupWait = time.Minute
)

// RuntimeDir is where a build's secrets wait as files, in memory, for its builder.
const RuntimeDir = "/run/dockwell/builds"

// Builder builds services' images on this server, one at a time.
type Builder struct {
	Client *docker.Client
	Logger *slog.Logger
	// Image is BuildKit's image: Image unless set.
	Image string
	// SecretsDir is where secrets go as files: RuntimeDir unless set. It must be in
	// memory, as a tmpfs is.
	SecretsDir string
	// Memory says what memory the server has available and in all, in bytes. It
	// reads /proc/meminfo unless set.
	Memory func() (available, total int64, err error)
	// CPUs is how many CPUs the server has: what Docker says unless set.
	CPUs func() int
	// DiskSize says how large the disk path is on is, and DiskFree how much of it
	// is free, in bytes: statfs unless set.
	DiskSize, DiskFree func(path string) (int64, error)
	// WatchEvery is how often a build checks the disk it writes to: every 250 ms
	// unless set.
	WatchEvery time.Duration
	// LookupIP finds where a name leads: the server's resolver unless set.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	// AppArmorProfiles reads the AppArmor profiles the server has loaded:
	// /sys/kernel/security/apparmor/profiles unless set.
	AppArmorProfiles func() ([]byte, error)
	// InMemory says whether path is in memory, as a tmpfs: statfs unless set.
	InMemory func(path string) bool
	// Now is the clock.
	Now func() time.Time
	// StateDir is where the agent keeps its own files: when each build cache was
	// last used, and the key for sums of a build's secrets. Empty keeps the first
	// in memory, and makes a key each time the agent starts.
	StateDir string

	once sync.Once
	slot chan struct{}
	// reading holds a place for each reading of a commit under way.
	reading chan struct{}

	railpackMu    sync.Mutex
	railpackReady bool
	miseSeeded    bool
	seededAt      time.Time
	keyOnce       sync.Once
	hashKey       []byte

	mu      sync.Mutex
	running map[key]*run

	cacheMu      sync.Mutex
	cacheUse     map[string]int64
	dirty        map[string]bool
	dirtyAll     bool
	networkReady bool
}

// key names a build: the change it is a step of, and the service.
type key struct {
	change  string
	service resource.Ref
}

// run is a build that waits or runs, which someone can cancel.
type run struct {
	cancel context.CancelCauseFunc
	by     string
}

// errCancelled is why a build someone cancelled stopped.
var errCancelled = errors.New("cancelled")

var _ service.Builder = (*Builder)(nil)

func (b *Builder) init() {
	b.once.Do(func() {
		b.slot = make(chan struct{}, 1)
		b.reading = make(chan struct{}, detectsAtOnce)
		b.running = map[key]*run{}
	})
}

func (b *Builder) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Builder) image() string {
	if b.Image != "" {
		return b.Image
	}
	return Image
}

// Has reports whether the server has the image.
func (b *Builder) Has(ctx context.Context, image string) (bool, error) {
	_, err := b.Client.InspectImage(ctx, image)
	switch {
	case err == nil:
		return true, nil
	case docker.IsNotFound(err):
		return false, nil
	}
	return false, err
}

// Build makes req.Image from its source, unless the server has it, then removes the
// service's older built images. It waits for any other build on the server first.
func (b *Builder) Build(ctx context.Context, req service.BuildRequest) (err error) {
	defer func() {
		// Nothing a repository holds may take the agent down: a build that goes
		// wrong this way fails, and says so.
		if p := recover(); p != nil {
			b.logger().Error("a build failed unexpectedly", "change", req.Change, "service", req.Service.Name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			err = b.ended(ctx, req, facts{}, unexpected(p))
		}
	}()
	return b.build(ctx, req)
}

func (b *Builder) build(ctx context.Context, req service.BuildRequest) error {
	b.init()
	if has, err := b.Has(ctx, req.Image); err != nil || has {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	k := key{req.Change, req.Service}
	r := &run{cancel: cancel}
	b.mu.Lock()
	b.running[k] = r
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.running[k] == r {
			delete(b.running, k)
		}
		b.mu.Unlock()
	}()

	resource.ReportBuild(ctx, resource.BuildUpdate{State: resource.BuildQueued, Image: req.Image})
	select {
	case b.slot <- struct{}{}:
	default:
		resource.Progress(ctx, "Waiting for another build on this server to finish")
		select {
		case b.slot <- struct{}{}:
		case <-ctx.Done():
			return b.ended(ctx, req, facts{}, b.stopped(ctx, r, req))
		}
	}
	defer func() { <-b.slot }()

	if err := checkRef(req.Ref); err != nil {
		return b.ended(ctx, req, facts{}, err)
	}
	if err := b.checkPlain(ctx, req.URL); err != nil {
		return b.ended(ctx, req, facts{}, err)
	}
	root, volume := b.dockerRoot(ctx), cacheVolume(req)
	if err := b.checkDisk(root); err != nil {
		// What a build that stopped wrote is in its cache, which its next build
		// would prune, if it could start: under the floor, the cache goes first.
		if !b.pruneFirst(volume) || !b.dropCache(ctx, volume) {
			return b.ended(ctx, req, facts{}, err)
		}
		if err := b.checkDisk(root); err != nil {
			return b.ended(ctx, req, facts{}, err)
		}
	}
	stopWatching := b.watchDisk(root, cancel)
	rep := newReport(ctx, req, b.now)
	builder, plan := req.Builder, []byte(nil)
	var f facts
	var err error
	if builder != resource.BuilderDockerfile {
		builder, plan, err = b.readFor(ctx, req, rep)
	}
	if err == nil {
		f, err = b.run(ctx, req, rep, builder, plan)
	} else {
		// What the reading said is in the log too.
		rep.send()
		f = rep.facts()
	}
	stopWatching()
	if err != nil {
		if ctx.Err() != nil {
			err = b.stopped(ctx, r, req)
		}
		var low diskLow
		if errors.As(context.Cause(ctx), &low) && b.dropCache(context.WithoutCancel(ctx), volume) {
			// What the build wrote is in its cache, not its container, so removing
			// the container freed none of it.
			plain, _ := resource.Explain(err)
			err = resource.PlainlyBecause(resource.ReasonBuild,
				fmt.Sprintf("%s. Dockwell removed this environment's build cache, %s, which held what it wrote, so the next build starts from nothing", strings.TrimSuffix(plain, "."), volume),
				low)
		}
		return b.ended(ctx, req, f, err)
	}
	_ = b.ended(ctx, req, f, nil)
	b.prune(ctx, req)
	return nil
}

// stopped is why a build stopped before it ended on its own: someone cancelled it,
// or the step's own time, or its call, ran out.
func (b *Builder) stopped(ctx context.Context, r *run, _ service.BuildRequest) error {
	var low diskLow
	switch cause := context.Cause(ctx); {
	case errors.As(cause, &low):
		return resource.PlainlyBecause(resource.ReasonBuild,
			fmt.Sprintf("The build stopped when Docker's disk had %s free, under the %s a build leaves, so nothing was deployed. Free some disk, or build the image in CI and deploy that", human.Size(low.free), human.Size(low.floor)),
			low)
	case errors.Is(cause, errCancelled):
		who := "someone"
		if r.by != "" {
			who = r.by
		}
		return &resource.Failure{Plain: "The build was cancelled by " + who + ", so nothing was deployed", Err: errCancelled, Reason: resource.ReasonCancelled}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return resource.PlainlyBecause(resource.ReasonBuild,
			"The build ran out of time: it has "+human.Duration(resource.BuildBudget)+" on top of what any step gets. Make it faster, or build the image in CI and deploy that",
			ctx.Err())
	}
	return ctx.Err()
}

// ended tells whoever follows the build how it ended, and gives the error the step
// fails with, if it does.
func (b *Builder) ended(ctx context.Context, req service.BuildRequest, f facts, err error) error {
	u := resource.BuildUpdate{State: resource.BuildDone, End: b.now(), Steps: f.steps, Cached: f.cached, Image: req.Image}
	// What a step printed reaches the error only masked, as it goes to the build's
	// record and the change's.
	err = hideIn(err, secretForms(req.Secrets))
	if err != nil {
		u.State = resource.BuildFailed
		if resource.ReasonOf(err) == resource.ReasonCancelled {
			u.State = resource.BuildCancelled
		}
		u.Error, _ = resource.Explain(err)
		u.Step, u.StepName, u.FailedLine = f.step, f.stepName, f.failedLine
	}
	// The step's own context may be over by now, and this is what says so.
	resource.ReportBuild(context.WithoutCancel(ctx), u)
	return err
}

// guardEnv says what the builder's script does beside the build: prune the cache
// first when its last build didn't, and run each step under AppArmor's profile for
// Docker where the server has it.
func (b *Builder) guardEnv(volume string) []string {
	var out []string
	if b.pruneFirst(volume) {
		out = append(out, "DW_PRUNE_FIRST=1")
	}
	if p := b.appArmor(); p != "" {
		out = append(out, "DW_APPARMOR="+p)
	}
	return out
}

// hideIn is err with forms masked in its words and its detail, its reason kept.
func hideIn(err error, forms []string) error {
	if err == nil || len(forms) == 0 {
		return err
	}
	plain, detail := resource.Explain(err)
	hidPlain, hidDetail := binding.Hide(plain, forms), binding.Hide(detail, forms)
	switch {
	case hidPlain == plain && hidDetail == detail:
		// Nothing to mask: the error stays what it is, as a deadline does.
		return err
	case detail == "":
		return errors.New(hidPlain)
	}
	return &resource.Failure{Plain: hidPlain, Err: errors.New(hidDetail), Reason: resource.ReasonOf(err)}
}

// Cancel stops a build of service for change that waits or runs, and says whether
// there was one. by is who cancelled it, for its log.
func (b *Builder) Cancel(change string, svc resource.Ref, by string) bool {
	b.init()
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.running[key{change, svc}]
	if r == nil {
		return false
	}
	r.by = by
	r.cancel(errCancelled)
	return true
}

// run builds in a builder, and loads what it makes into Docker. rep hears what the
// builder says.
// readFor reads the commit before a build that may use Railpack, as a plan does,
// and says what builds it: with no Dockerfile, Railpack, whose plan the reading
// wrote, unprivileged, for the builder to build. The builder never runs Railpack,
// nor mounts what it runs from (ADR 0046). The reading has the build's secrets'
// names, and none of their values.
func (b *Builder) readFor(ctx context.Context, req service.BuildRequest, rep *report) (string, []byte, error) {
	rep.reading()
	read := req
	read.Secrets, read.SecretIDs = nil, slices.Sorted(mapsKeys(req.Secrets))
	// It holds the build slot already, so it takes no reading's place.
	d, plan, err := b.read(ctx, read, true)
	if err != nil {
		return "", nil, err
	}
	if d == nil {
		return "", nil, errors.New("the reading said nothing of how the commit builds")
	}
	if d.Builder == resource.BuilderDockerfile {
		rep.detectedAs(d)
		return resource.BuilderDockerfile, nil, nil
	}
	if err := service.RailpackNames(req.Args, read.SecretIDs); err != nil {
		return "", nil, resource.PlainlyBecause(resource.ReasonBuild, sentence(err.Error()), err)
	}
	rep.detectedAs(d)
	if d.Error != "" {
		return "", nil, railpackFailure(req, d)
	}
	if len(plan) == 0 {
		return "", nil, resource.PlainlyBecause(resource.ReasonBuild, "Railpack read the commit and wrote no plan, so there's nothing to build. Deploy again, and report it if it happens again",
			errors.New("the reading wrote no plan"))
	}
	return resource.BuilderRailpack, plan, nil
}

// railpackFailure is a build Railpack can't make, in words chosen by what it said.
func railpackFailure(req service.BuildRequest, d *resource.BuildDetection) error {
	where := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(req.URL, "https://"), "http://"), ".git")
	short := req.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	return resource.PlainlyBecause(resource.ReasonBuild, resource.RailpackRefusal(d, where+" at "+short),
		fmt.Errorf("railpack can't build %s at %s: %s", req.URL, req.Commit, firstLine(d.Error)))
}

// sentence is an error's words as a sentence starts.
func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (b *Builder) run(ctx context.Context, req service.BuildRequest, rep *report, builder string, plan []byte) (facts, error) {
	if err := b.ensureImage(ctx); err != nil {
		return facts{}, err
	}
	memory, cpus, err := b.limits(ctx)
	if err != nil {
		return facts{}, err
	}
	volume := cacheVolume(req)
	keep, err := b.cacheShare(ctx, volume, b.cacheBound(ctx))
	if err != nil {
		return facts{}, err
	}
	if err := b.ensureNetwork(ctx); err != nil {
		return facts{}, err
	}
	// What builds it is what the reading said, for a build that may use Railpack.
	railpack := builder == resource.BuilderRailpack
	req.Builder = builder
	files := map[string]string{}
	for id, v := range req.Secrets {
		files[secretFile(id)] = v
	}
	if railpack {
		// Railpack takes build arguments as it takes secrets: as BuildKit secrets.
		for k, v := range req.Args {
			files["a-"+k] = v
		}
	}
	secrets, cleanup, err := b.writeSecrets(files, len(req.Secrets) > 0)
	if err != nil {
		return facts{}, err
	}
	defer cleanup()
	planDir, planCleanup := "", func() {}
	if railpack {
		if planDir, planCleanup, err = b.writeSecrets(map[string]string{planFile: string(plan)}, false); err != nil {
			return facts{}, err
		}
	}
	defer planCleanup()

	cmd := append([]string{"-c", script, "dockwell-build"}, args(req)...)
	labels := map[string]string{docker.LabelBuilder: req.Change}
	mounts := []docker.Mount{{Type: "volume", Source: volume, Target: cacheMount}}
	if secrets != "" {
		mounts = append(mounts, docker.Mount{Type: "bind", Source: secrets, Target: secretsMount, ReadOnly: true})
	}
	environment := env(req, keep)
	if railpack {
		mounts = append(mounts, docker.Mount{Type: "bind", Source: planDir, Target: planMount, ReadOnly: true})
		environment = append(environment, "DW_SECRETS_HASH="+b.secretsHash(req), "DW_CACHE_KEY="+b.cacheKey(req))
	}
	create := &docker.CreateRequest{
		Config: docker.Config{
			Image: b.image(), Entrypoint: []string{"/bin/sh"}, Cmd: cmd,
			Env: append(environment, b.guardEnv(volume)...), Labels: labels,
		},
		HostConfig: docker.HostConfig{
			// A network of its own, on which no RUN step reaches another container.
			NetworkMode: Network,
			// BuildKit's sandbox for each RUN step, runc, makes namespaces and
			// cgroups. What a Dockerfile runs stays in it (ADR 0046).
			Privileged: true,
			Memory:     memory, MemorySwap: memory, NanoCPUs: cpus * 1e9,
			CPUShares: cpuShares, OomScoreAdj: 1000, PidsLimit: pidsLimit,
			// What it prints is the image, and Docker would keep a copy of it.
			LogConfig: &docker.LogConfig{Type: "none"},
			Mounts:    mounts,
		},
	}
	name := "dockwell-build-" + randomHex()
	id, err := b.Client.CreateContainer(ctx, name, create)
	if err != nil {
		return facts{}, fmt.Errorf("docker can't make the container a build runs in: %w", err)
	}
	defer func() {
		cleanup, cancel := resource.Finishing(ctx, cleanupWait)
		defer cancel()
		if err := b.Client.RemoveContainer(cleanup, id, true); err != nil && !docker.IsNotFound(err) {
			b.logger().Warn("can't remove a build's container; the agent removes it when it next starts", "container", id, "error", err.Error())
		}
	}()

	stopFollowing := rep.follow()
	pr, pw := io.Pipe()
	type load struct {
		names []string
		err   error
	}
	loaded := make(chan load, 1)
	go func() {
		names, err := b.Client.LoadImage(ctx, bufio.NewReaderSize(pr, 64<<10))
		_ = pr.CloseWithError(err)
		loaded <- load{names, err}
	}()
	code, runErr := b.Client.RunAttached(ctx, id, docker.Streams{Stdout: pw, Stderr: rep})
	_ = pw.Close()
	got := <-loaded
	loadErr := got.err
	stopFollowing()
	f := rep.facts()
	f.memory = memory
	b.endedCache(volume, f.pruned)
	b.usedCache(context.WithoutCancel(ctx), volume)

	oom := false
	if c, err := b.Client.InspectContainer(context.WithoutCancel(ctx), id); err == nil && c != nil {
		oom = c.State.OOMKilled
	}
	switch {
	case ctx.Err() != nil:
		return f, ctx.Err()
	case runErr != nil:
		return f, fmt.Errorf("docker stopped answering about the build's container: %w", runErr)
	case code != 0 || oom:
		return f, failure(req, f, code, oom)
	case loadErr != nil:
		return f, resource.PlainlyBecause(resource.ReasonBuild, "Docker couldn't take the image the build made: "+loadErr.Error(), loadErr)
	}
	// What Docker took is the image the build names, and nothing else.
	for _, name := range got.names {
		if name != req.Image && docker.WithTag(name) != req.Image {
			if _, err := b.Client.RemoveUnusedImage(context.WithoutCancel(ctx), name); err != nil {
				b.logger().Warn("can't remove an image a build loaded that it didn't name", "image", name, "error", err.Error())
			}
			return f, resource.PlainlyBecause(resource.ReasonBuild,
				fmt.Sprintf("The build gave Docker an image named %s, not %s, so Dockwell removed it and deployed nothing. Deploy again, and report it if it happens again", name, req.Image),
				fmt.Errorf("docker load took %s for a build of %s", name, req.Image))
		}
	}
	if has, err := b.Has(ctx, req.Image); err != nil {
		return f, err
	} else if !has {
		return f, resource.PlainlyBecause(resource.ReasonBuild, "The build ended, and Docker hasn't the image it made. Deploy again to build it again",
			fmt.Errorf("the build exited 0 and %s isn't on the server", req.Image))
	}
	return f, nil
}

// failure is a build that failed, in words that say why and what to do.
func failure(req service.BuildRequest, f facts, code int, oom bool) error {
	short := req.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	where := strings.TrimPrefix(strings.TrimPrefix(req.URL, "https://"), "http://")
	err := fmt.Errorf("the build of %s at %s exited with %d", req.URL, req.Commit, code)
	plain := func(s string) error { return resource.PlainlyBecause(resource.ReasonBuild, s, err) }
	switch {
	case oom || code == 137 || strings.Contains(f.stepError, "exit code: 137"):
		return plain(fmt.Sprintf("The build ran out of the %s it may use on this server. Free some memory on the server, or build the image in CI and deploy that", human.Size(f.memory)))
	case f.failed == "fetch":
		why := lastWords(f.tail)
		if why == "" {
			why = "git said nothing more"
		}
		return plain(fmt.Sprintf("Dockwell can't fetch %s of %s: %s. Check the repository is public and the commit is still there", short, where, strconv.Quote(why)))
	case f.failed == "notonref":
		ref := cmpOr(req.Ref, "its default branch")
		return plain(fmt.Sprintf("%s isn't on %s of %s, so Dockwell won't build it: a build's commit has to be on the branch or tag it names. Deploy again to build what %s names now", short, ref, where, ref))
	case f.failed == "context":
		return plain(fmt.Sprintf("%s has no folder %s at %s. Check the build's context", where, req.Context, short))
	case f.failed == "dockerfile":
		return plain(fmt.Sprintf("%s has no Dockerfile at %s at %s. Check the build's Dockerfile path, add one, or build with Railpack", where, filepath.Join(req.Context, dockerfile(req.Dockerfile)), short))
	case f.failed == "noplan":
		return plain("The builder had no plan from Railpack to build, so it built nothing. Deploy again, and report it if it happens again")
	case f.failed == "buildkitd":
		return plain("BuildKit didn't start on this server: " + strconv.Quote(cmpOr(lastWords(f.tail), "it said nothing")) + ". Check Docker can run privileged containers here")
	case f.step > 0 && f.stepName != "":
		// The step in quotes, as its words are the Dockerfile's. What it printed
		// last is log content, so it is the build's failed line, for whoever may
		// read logs, and not in an error anyone reading changes reads (ADR 0025).
		return plain(fmt.Sprintf("The build failed at step %d of %d, %s: its log marks what it printed last. Fix it in the repository, then deploy again", f.step, f.steps, strconv.Quote(f.stepName)))
	case f.reachedBuildKit:
		return plain("The build failed: " + strconv.Quote(cmpOr(lastWords(f.said), lastWords(f.tail), fmt.Sprintf("BuildKit exited with %d", code))) + ". Fix it in the repository, then deploy again")
	}
	return plain(fmt.Sprintf("The build failed before it began, with code %d: %s", code, strconv.Quote(cmpOr(lastWords(f.tail), "it said nothing"))))
}

// firstLine is s up to its first line's end.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func cmpOr(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// ensureImage pulls BuildKit's image if the server hasn't it.
func (b *Builder) ensureImage(ctx context.Context) error {
	_, err := b.Client.InspectImage(ctx, b.image())
	if !docker.IsNotFound(err) {
		return err
	}
	resource.Progress(ctx, "Getting BuildKit, which builds the image")
	if err := b.Client.PullImage(ctx, b.image(), func(p string) { resource.Progress(ctx, "Getting BuildKit: "+p) }); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return resource.PlainlyBecause(resource.ReasonBuild, "This server can't get BuildKit's image, "+Repository+":"+Tag+", which builds the image: "+err.Error()+". Check the server reaches Docker Hub, or its mirror", err)
	}
	return nil
}

// limits are the memory and CPUs a build may use: what the server has available,
// less what runs keeps, within bounds, and every CPU but one, as Docker counts them,
// as Docker refuses more than it has.
func (b *Builder) limits(ctx context.Context) (memory, cpus int64, err error) {
	read := b.Memory
	if read == nil {
		read = meminfo
	}
	available, _, err := read()
	if err != nil {
		return 0, 0, fmt.Errorf("can't read how much memory this server has free: %w", err)
	}
	memory = min(max(available-keepForRunning, minMemory), maxMemory)
	n := runtime.NumCPU()
	switch {
	case b.CPUs != nil:
		n = b.CPUs()
	default:
		if info, err := b.Client.Info(ctx); err == nil && info.NCPU > 0 {
			n = info.NCPU
		}
	}
	return memory, int64(max(n-1, 1)), nil
}

// cacheBound is the most BuildKit's cache keeps: a tenth of the disk Docker keeps
// its data on, within bounds.
func (b *Builder) cacheBound(ctx context.Context) int64 {
	size := b.DiskSize
	if size == nil {
		size = statfsSize
	}
	root := "/var/lib/docker"
	if info, err := b.Client.Info(ctx); err == nil && info.DockerRootDir != "" {
		root = info.DockerRootDir
	}
	total, err := size(root)
	if err != nil || total <= 0 {
		return 2 << 30
	}
	return min(max(total/10, minCache), maxCache)
}

// writeSecrets writes a build's secrets, and with Railpack its build arguments and
// its plan, to files, by name, in a directory of their own, which goes when cleanup
// is called. With a secret among them, it must be in memory. With none, an agent
// that can't write RuntimeDir, as one that doesn't run as root, writes them in the
// system's temporary folder. None gives no directory.
func (b *Builder) writeSecrets(files map[string]string, secret bool) (dir string, cleanup func(), err error) {
	cleanup = func() {}
	if len(files) == 0 {
		return "", cleanup, nil
	}
	parent := b.SecretsDir
	if parent == "" {
		parent = RuntimeDir
	}
	dir, err = b.folderIn(parent, secret)
	if err != nil && !secret {
		// A folder of its own, made now, and none it finds there, which anyone
		// could have made, or pointed elsewhere.
		dir, err = os.MkdirTemp(os.TempDir(), "dockwell-build-")
		if err != nil {
			err = secretsRefused(os.TempDir(), err)
		}
	}
	if err != nil {
		return "", cleanup, err
	}
	cleanup = func() {
		if err := os.RemoveAll(dir); err != nil {
			b.logger().Warn("can't remove a build's secrets", "dir", dir, "error", err.Error())
		}
	}
	for _, name := range slices.Sorted(mapsKeys(files)) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(files[name]), 0o600); err != nil {
			cleanup()
			return "", func() {}, secretsRefused(parent, err)
		}
	}
	return dir, cleanup, nil
}

// folderIn makes a folder of a build's own in parent, which must be in memory for
// a secret.
func (b *Builder) folderIn(parent string, secret bool) (string, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", secretsRefused(parent, err)
	}
	inMemory := b.InMemory
	if inMemory == nil {
		inMemory = isTmpfs
	}
	if secret && !inMemory(parent) {
		return "", secretsRefused(parent, errors.New("it isn't in memory"))
	}
	dir, err := os.MkdirTemp(parent, "build-")
	if err != nil {
		return "", secretsRefused(parent, err)
	}
	return dir, nil
}

func secretsRefused(dir string, err error) error {
	return resource.PlainlyBecause(resource.ReasonBuild,
		"This server has nowhere in memory to keep the build's secrets, "+dir+", so it builds nothing that uses one. Check the agent runs as root under systemd, which keeps /run in memory",
		fmt.Errorf("can't keep a build's secrets in %s: %w", dir, err))
}

// prune removes a service's built images but the newest few, which it doesn't run.
func (b *Builder) prune(ctx context.Context, req service.BuildRequest) {
	refs, err := b.Client.ImageTags(ctx, imageRepository(req.Image))
	if err != nil {
		b.logger().Warn("can't list a service's built images to remove the old ones", "error", err.Error())
		return
	}
	type built struct {
		ref     string
		created time.Time
	}
	var all []built
	for _, ref := range refs {
		img, err := b.Client.InspectImage(ctx, ref)
		if err != nil {
			continue
		}
		all = append(all, built{ref: ref, created: img.Created})
	}
	slices.SortFunc(all, func(x, y built) int { return y.created.Compare(x.created) })
	for i, img := range all {
		if i < keepBuilt+1 || img.ref == req.Image {
			continue
		}
		// One a container still runs, as after a rollback to it, stays.
		if _, err := b.Client.RemoveUnusedImage(ctx, img.ref); err != nil {
			b.logger().Warn("can't remove an old built image", "image", img.ref, "error", err.Error())
		}
	}
}

// imageRepository is an image's name without its tag.
func imageRepository(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// Clean removes what a crash left of builds: their containers, and their secrets.
// A build a crash stopped didn't prune its cache, so each cache's next build prunes
// it first.
func (b *Builder) Clean(ctx context.Context) {
	if n, err := b.Client.RemoveContainersLabelled(ctx, docker.LabelBuilder); err != nil {
		b.logger().Warn("can't remove what earlier builds left", "error", err.Error())
	} else if n > 0 {
		b.cacheMu.Lock()
		b.dirtyAll = true
		b.cacheMu.Unlock()
		b.logger().Info("removed what earlier builds left", "containers", n)
	}
	dir := b.SecretsDir
	if dir == "" {
		dir = RuntimeDir
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "build-") {
				_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// meminfo reads what memory the server has available and in all.
func meminfo() (available, total int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemAvailable:":
			available = n << 10
		case "MemTotal:":
			total = n << 10
		}
	}
	if total == 0 {
		return 0, 0, errors.New("/proc/meminfo says nothing of MemTotal")
	}
	return available, total, s.Err()
}

// statfsSize is how large the filesystem path is on is.
func statfsSize(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Blocks) * st.Bsize, nil //nolint:gosec // a disk's size fits
}

// tmpfsMagic is TMPFS_MAGIC, which statfs gives for a filesystem in memory.
const tmpfsMagic = 0x01021994

// isTmpfs reports a path on a filesystem in memory.
func isTmpfs(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return int64(st.Type) == tmpfsMagic //nolint:unconvert // Type's width differs by platform
}

func randomHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
