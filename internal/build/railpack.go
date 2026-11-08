// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Railpack (railpack.com) builds an app that has no Dockerfile: it reads what the
// source is, such as a Node app from its package.json, writes a plan, and its
// BuildKit frontend builds the plan (ADR 0046). Its frontend's image holds the
// railpack binary too, which the agent copies out once, into a volume, and runs
// only to read a commit, unprivileged: the builder builds the plan it wrote. Pinned
// by the digest of its multi-platform index; pin a new one to a release at least 14
// days old, with the version it is.
const (
	RailpackVersion  = "0.43.0"
	RailpackFrontend = "ghcr.io/railwayapp/railpack-frontend@sha256:355ef61fae6f2551103cd8b10c710cb07167dd353d561f15c69b8d7ac1c676cc"
	// RailpackVolume holds the railpack binary and the mise it fetches to read
	// versions with. Only the agent's own containers write it, with nothing of
	// anyone's repository in them; a reading mounts it read-only, and a builder
	// not at all.
	RailpackVolume = "dockwell-railpack"
)

// Where RailpackVolume is. The containers that fill it mount it where Railpack keeps
// mise and its binary, /tmp/railpack; a reading mounts it read-only apart, at
// railpackRO, and keeps /tmp/railpack in memory, so nothing a reading does outlives
// it.
const (
	railpackHome = "/tmp/railpack"
	railpackBin  = railpackHome + "/bin/railpack-" + RailpackVersion
	railpackRO   = "/opt/dockwell-railpack"
	detectBin    = railpackRO + "/bin/railpack-" + RailpackVersion
)

// What reading a commit may use. It writes a plan and nothing more, so it gets
// less than a build, and keeps all it writes in memory, counted against that: a
// commit too large to check out there fails to read, rather than fill the disk the
// server's databases share.
const (
	detectMemory   = 1 << 30
	detectPids     = 1024
	detectCheckout = 512 << 20
	detectTmp      = 256 << 20
	detectWork     = 16 << 20
	// detectWait bounds it, the first on a server fetching mise as well.
	detectWait = 3 * time.Minute
	// detectsAtOnce is how many readings run at once on a server.
	detectsAtOnce = 2
	// seedAgain is how long the agent waits before it tries again to fetch mise,
	// which failed.
	seedAgain = 10 * time.Minute
)

// secretPlaceholder stands in for a secret's value when Railpack reads a commit:
// its plan names a build's secrets and holds none of their values, so a reading
// needs only their names (service.RailpackNames).
const secretPlaceholder = "dockwell-secret"

// copyRailpack is what a container of the frontend's image runs to put the binary
// in the volume, if it isn't there.
const copyRailpack = `set -eu
if [ -x "` + railpackBin + `" ]; then exit 0; fi
mkdir -p "$(dirname "` + railpackBin + `")"
cp /railpack "` + railpackBin + `.part"
chmod 0755 "` + railpackBin + `.part"
mv "` + railpackBin + `.part" "` + railpackBin + `"
`

// seedMise is what a container of BuildKit's image runs, with RailpackVolume where
// Railpack keeps mise, to have Railpack fetch mise into it: it reads a package.json
// of its own, so nothing of anyone's repository runs while the volume can be
// written. Railpack fetches mise from GitHub by its version, over TLS: it isn't
// pinned by a digest (ADR 0046).
const seedMise = `set -eu
for f in ` + railpackHome + `/mise/mise-*; do if [ -f "$f" ]; then exit 0; fi; done
mkdir -p /seed /work
printf '{"name":"seed","engines":{"node":"22"}}' >/seed/package.json
"` + railpackBin + `" prepare /seed --plan-out /work/plan.json --info-out /work/info.json >/work/railpack.log 2>&1 || true
for f in ` + railpackHome + `/mise/mise-*; do if [ -f "$f" ]; then exit 0; fi; done
tail -n 3 /work/railpack.log >&2
exit 1
`

// ensureRailpack has the railpack binary in RailpackVolume, once each time the
// agent starts, and mise beside it, fetched once (seedMise). Without mise there, a
// reading fetches it into its own memory, and the agent tries again after a while.
func (b *Builder) ensureRailpack(ctx context.Context) error {
	b.railpackMu.Lock()
	defer b.railpackMu.Unlock()
	if !b.railpackReady {
		if err := b.copyRailpack(ctx); err != nil {
			return err
		}
		b.railpackReady = true
	}
	if !b.miseSeeded && (b.seededAt.IsZero() || b.now().Sub(b.seededAt) >= seedAgain) {
		b.seededAt = b.now()
		if err := b.seed(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.logger().Warn("can't fetch mise for Railpack once for all readings, so each fetches its own; check the server reaches github.com", "error", err.Error())
		} else {
			b.miseSeeded = true
		}
	}
	return nil
}

// seed has Railpack fetch mise into RailpackVolume (seedMise).
func (b *Builder) seed(ctx context.Context) error {
	if err := b.ensureNetwork(ctx); err != nil {
		return err
	}
	create := &docker.CreateRequest{
		Config: docker.Config{
			Image: b.image(), Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", seedMise, "dockwell-railpack-seed"},
			Labels: map[string]string{docker.LabelBuilder: "railpack-seed"},
		},
		HostConfig: docker.HostConfig{
			// mise comes from GitHub.
			NetworkMode: Network,
			Memory:      detectMemory, MemorySwap: detectMemory, PidsLimit: detectPids, OomScoreAdj: 1000,
			LogConfig: &docker.LogConfig{Type: "none"},
			Mounts:    []docker.Mount{{Type: "volume", Source: RailpackVolume, Target: railpackHome}},
		},
	}
	var said bytes.Buffer
	code, err := b.runOnce(ctx, "dockwell-railpack-seed-"+randomHex(), create, &said)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("fetching mise exited with %d: %q", code, strings.TrimSpace(lastLine(said.String())))
	}
	return nil
}

// copyRailpack puts the railpack binary from its frontend's image in RailpackVolume.
func (b *Builder) copyRailpack(ctx context.Context) error {
	if _, err := b.Client.InspectImage(ctx, RailpackFrontend); docker.IsNotFound(err) {
		resource.Progress(ctx, "Getting Railpack, which builds an app with no Dockerfile")
		if err := b.Client.PullImage(ctx, RailpackFrontend, func(p string) { resource.Progress(ctx, "Getting Railpack: "+p) }); err != nil {
			if ctx.Err() != nil {
				return err
			}
			return resource.PlainlyBecause(resource.ReasonBuild, "This server can't get Railpack's image, ghcr.io/railwayapp/railpack-frontend "+RailpackVersion+", which builds an app with no Dockerfile: "+err.Error()+". Check the server reaches ghcr.io, or add a Dockerfile", err)
		}
	} else if err != nil {
		return err
	}
	create := &docker.CreateRequest{
		Config: docker.Config{
			Image: RailpackFrontend, Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", copyRailpack},
			Labels: map[string]string{docker.LabelBuilder: "railpack"},
		},
		HostConfig: docker.HostConfig{
			// It copies a file, and reaches nothing.
			NetworkMode: "none",
			LogConfig:   &docker.LogConfig{Type: "none"},
			Mounts:      []docker.Mount{{Type: "volume", Source: RailpackVolume, Target: railpackHome}},
		},
	}
	var said bytes.Buffer
	code, err := b.runOnce(ctx, "dockwell-railpack-"+randomHex(), create, &said)
	if err != nil {
		return err
	}
	if code != 0 {
		return resource.PlainlyBecause(resource.ReasonBuild,
			fmt.Sprintf("Dockwell couldn't put Railpack in the volume %s: %q. Check the server's disk has room", RailpackVolume, strings.TrimSpace(lastLine(said.String()))),
			fmt.Errorf("copying railpack exited with %d", code))
	}
	return nil
}

// runOnce runs a container to its end, says what it printed to out, and removes it.
func (b *Builder) runOnce(ctx context.Context, name string, create *docker.CreateRequest, out *bytes.Buffer) (int, error) {
	id, err := b.Client.CreateContainer(ctx, name, create)
	if err != nil {
		return 0, fmt.Errorf("docker can't make the container %s: %w", name, err)
	}
	defer func() {
		cleanup, cancel := resource.Finishing(ctx, cleanupWait)
		defer cancel()
		if err := b.Client.RemoveContainer(cleanup, id, true); err != nil && !docker.IsNotFound(err) {
			b.logger().Warn("can't remove a container a build used; the agent removes it when it next starts", "container", id, "error", err.Error())
		}
	}()
	w := &capped{buf: out, max: 1 << 20}
	code, err := b.Client.RunAttached(ctx, id, docker.Streams{Stdout: w, Stderr: w})
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("docker stopped answering about %s: %w", name, err)
	}
	return code, nil
}

// capped keeps up to max bytes of what is written to it.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// detectScript reads a commit as a build would, in BuildKit's image, which has
// git: it checks out the commit, and says whether the Dockerfile is where the build
// says, with its FROM lines. Without one, or when the build names Railpack, it has
// Railpack say what it makes of the source and write the plan a build builds, or
// says it needs Railpack first. Railpack runs from RailpackVolume, read-only, with
// mise from there too, and keeps all it writes in memory. Its arguments are
// Railpack's: an -e flag for each build argument, and each secret by its name, and
// --error-missing-start. Every value comes from the environment or the arguments,
// never from the script's words.
const detectScript = `set -eu
say() { printf '` + marker + `%s\n' "$*" >&2; }
export GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false
` + fetchScript + `
if [ "$DW_BUILDER" = dockerfile ] && [ -z "$df" ]; then say fail dockerfile; exit 3; fi
if [ -n "$df" ] && [ "$DW_BUILDER" != railpack ]; then
  say dockerfile yes
  say from "$(grep -i '^[[:space:]]*from[[:space:]]' "$df" | head -n 50 | base64 | tr -d '\n')"
  exit 0
fi
if [ ! -x "` + detectBin + `" ]; then say fail need-railpack; exit 4; fi
mkdir -p ` + railpackHome + `/mise /work
for f in ` + railpackRO + `/mise/mise-*; do if [ -f "$f" ]; then ln -sf "$f" ` + railpackHome + `/mise/; fi; done
set -- prepare "$ctx" --plan-out /work/railpack-plan.json --info-out /work/railpack-info.json --hide-pretty-plan "$@"
if [ -n "$DW_BUILD_CMD" ]; then set -- "$@" --build-cmd "$DW_BUILD_CMD"; fi
if [ -n "$DW_START_CMD" ]; then set -- "$@" --start-cmd "$DW_START_CMD"; fi
"` + detectBin + `" "$@" >/tmp/railpack.log 2>&1 || true
if [ -s /work/railpack-info.json ]; then say railpack-info "$(base64 </work/railpack-info.json | tr -d '\n')"; fi
if [ -s /work/railpack-plan.json ]; then say railpack-plan "$(base64 </work/railpack-plan.json | tr -d '\n')"; fi
if [ ! -s /work/railpack-info.json ]; then tail -n 5 /tmp/railpack.log >&2; say fail railpack; fi
exit 0
`

// Detect reads a commit as a build would, and says how it would build: with its
// Dockerfile, or with Railpack and what Railpack found. It builds nothing, and runs
// beside a build.
func (b *Builder) Detect(ctx context.Context, req service.BuildRequest) (*resource.BuildDetection, error) {
	d, _, err := b.read(ctx, req)
	return d, err
}

// read reads a commit, as Detect does, and gives the plan Railpack wrote, when it
// builds with Railpack. A server reads detectsAtOnce commits at a time.
func (b *Builder) read(ctx context.Context, req service.BuildRequest) (*resource.BuildDetection, []byte, error) {
	b.init()
	if err := checkRef(req.Ref); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, detectWait)
	defer cancel()
	select {
	case b.reading <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, b.readTimedOut(ctx, ctx.Err())
	}
	defer func() { <-b.reading }()
	if err := b.checkPlain(ctx, req.URL); err != nil {
		return nil, nil, err
	}
	if err := b.ensureImage(ctx); err != nil {
		return nil, nil, err
	}
	if req.Builder == resource.BuilderRailpack {
		if err := b.ensureRailpack(ctx); err != nil {
			return nil, nil, err
		}
	}
	d, plan, need, err := b.detect(ctx, req)
	if err == nil && need {
		if err = b.ensureRailpack(ctx); err == nil {
			d, plan, _, err = b.detect(ctx, req)
		}
	}
	return d, plan, err
}

// readTimedOut is a reading that ran out of time, which may have waited for others.
func (b *Builder) readTimedOut(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return resource.PlainlyBecause(resource.ReasonBuild, "Reading the commit took longer than "+human.Duration(detectWait)+". Check the server reaches the repository, and GitHub, where Railpack fetches mise, then try again", err)
	}
	return err
}

// detectArgs are Railpack's arguments for a reading: each build argument, each
// secret by its name with a stand-in for its value, and --error-missing-start
// unless the service starts with its own command. A name Railpack can't take is
// left out, and a build that uses Railpack refuses it (service.RailpackNames).
func detectArgs(req service.BuildRequest) []string {
	var out []string
	for _, k := range slices.Sorted(mapsKeys(req.Args)) {
		if service.RailpackNames(map[string]string{k: ""}, nil) == nil {
			out = append(out, "-e", k+"="+req.Args[k])
		}
	}
	for _, id := range req.SecretIDs {
		if service.RailpackNames(nil, []string{id}) == nil {
			out = append(out, "-e", id+"="+secretPlaceholder)
		}
	}
	if !req.OwnCommand {
		out = append(out, "--error-missing-start")
	}
	return out
}

// detect runs a detect container once, and says whether it needs Railpack first.
func (b *Builder) detect(ctx context.Context, req service.BuildRequest) (*resource.BuildDetection, []byte, bool, error) {
	memory, cpus, err := b.limits(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	if err := b.ensureNetwork(ctx); err != nil {
		return nil, nil, false, err
	}
	create := &docker.CreateRequest{
		Config: docker.Config{
			Image: b.image(), Entrypoint: []string{"/bin/sh"}, Cmd: append([]string{"-c", detectScript, "dockwell-detect"}, detectArgs(req)...),
			Env: env(req, 0), Labels: map[string]string{docker.LabelBuilder: "detect"},
		},
		HostConfig: docker.HostConfig{
			// On the build network, as what it reads comes from the same repository.
			NetworkMode: Network,
			Memory:      min(memory, detectMemory), MemorySwap: min(memory, detectMemory), NanoCPUs: cpus * 1e9,
			CPUShares: cpuShares, OomScoreAdj: 1000, PidsLimit: detectPids,
			LogConfig: &docker.LogConfig{Type: "none"},
			Mounts:    []docker.Mount{{Type: "volume", Source: RailpackVolume, Target: railpackRO, ReadOnly: true}},
			// All it writes is in memory, counted against its own. Docker makes a
			// tmpfs noexec, as the checkout stays; /tmp runs the mise a reading
			// fetches for itself, on a server whose volume hasn't one.
			Tmpfs: map[string]string{
				sourceDir: "size=" + strconv.Itoa(detectCheckout),
				"/tmp":    "size=" + strconv.Itoa(detectTmp) + ",mode=1777,exec",
				"/work":   "size=" + strconv.Itoa(detectWork),
			},
		},
	}
	var said bytes.Buffer
	name := "dockwell-detect-" + randomHex()
	code, err := b.runOnce(ctx, name, create, &said)
	if err != nil {
		return nil, nil, false, b.readTimedOut(ctx, err)
	}
	if strings.Contains(said.String(), marker+"fail need-railpack") {
		return nil, nil, true, nil
	}
	d, plan, err := readDetection(req, said.String(), code)
	return d, plan, false, err
}

// readDetection reads what a detect container said, and the plan Railpack wrote.
func readDetection(req service.BuildRequest, said string, code int) (*resource.BuildDetection, []byte, error) {
	var info, plan, from []byte
	var failed string
	hasDockerfile := false
	var tail []string
	for _, line := range strings.Split(said, "\n") {
		rest, ok := strings.CutPrefix(line, marker)
		if !ok {
			if strings.TrimSpace(line) != "" {
				tail = append(tail, line)
				if len(tail) > 5 {
					tail = tail[1:]
				}
			}
			continue
		}
		verb, arg, _ := strings.Cut(rest, " ")
		switch verb {
		case "fail":
			failed = arg
		case "dockerfile":
			hasDockerfile = arg == "yes"
		case "railpack-info":
			info, _ = base64.StdEncoding.DecodeString(arg)
		case "railpack-plan":
			plan, _ = base64.StdEncoding.DecodeString(arg)
		case "from":
			from, _ = base64.StdEncoding.DecodeString(arg)
		}
	}
	f := facts{failed: failed, tail: tail}
	switch {
	case slices.ContainsFunc(tail, func(l string) bool { return strings.Contains(l, "No space left on device") }):
		// The fetch, or the checkout after it, which writes far more.
		return nil, nil, resource.PlainlyBecause(resource.ReasonBuild,
			fmt.Sprintf("The commit is larger than reading it may take on this server, %s, so Dockwell can't say how it builds. Choose the Dockerfile as its builder, which reads nothing first, or make the repository smaller", human.Size(detectCheckout)),
			errors.New("the checkout ran out of its room in memory"))
	case failed == "fetch", failed == "notonref", failed == "context", failed == "dockerfile":
		return nil, nil, failure(req, f, 3, false)
	case code == 137:
		return nil, nil, resource.PlainlyBecause(resource.ReasonBuild,
			fmt.Sprintf("Reading the commit ran out of the %s it may use, which holds its checkout too. Choose the Dockerfile as its builder, which reads nothing first, or make the repository smaller", human.Size(detectMemory)),
			errors.New("the detect container was killed"))
	case code != 0:
		return nil, nil, resource.PlainlyBecause(resource.ReasonBuild,
			fmt.Sprintf("Reading the commit failed, with code %d: %q", code, cmpOr(lastWords(tail), "it said nothing")),
			fmt.Errorf("the detect container exited with %d", code))
	}
	if hasDockerfile {
		return &resource.BuildDetection{
			Builder: resource.BuilderDockerfile, Dockerfile: filepath.Join(req.Context, dockerfile(req.Dockerfile)), BaseImage: baseImage(string(from)),
		}, nil, nil
	}
	d := railpackDetection(info, plan)
	if failed == "railpack" && d.Error == "" {
		d.Error = "Railpack didn't say what it found: " + cmpOr(lastWords(tail), "it said nothing")
	}
	if d.Error != "" {
		d.Problem, d.ProblemTool = resource.RailpackProblem(d.Error)
		plan = nil
	}
	d.Builder = resource.BuilderRailpack
	return d, plan, nil
}

// baseImage is the image a Dockerfile's last stage starts from, as its FROM lines
// say, through the names of the stages before it, such as node:22-alpine. It is
// empty when a build argument names it.
func baseImage(from string) string {
	stages := map[string]string{}
	last := ""
	for _, line := range strings.Split(from, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		f = f[1:]
		for len(f) > 0 && strings.HasPrefix(f[0], "--") {
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		img := f[0]
		if s, ok := stages[strings.ToLower(img)]; ok {
			img = s
		}
		if len(f) >= 3 && strings.EqualFold(f[1], "AS") {
			stages[strings.ToLower(f[2])] = img
		}
		last = img
	}
	if strings.Contains(last, "$") || strings.EqualFold(last, "scratch") {
		return ""
	}
	return cut(last, 200)
}

// railpackInfo is the part of railpack's --info-out a detection reads.
type railpackInfo struct {
	RailpackVersion  string `json:"railpackVersion"`
	ResolvedPackages map[string]struct {
		Name             string `json:"name"`
		RequestedVersion string `json:"requestedVersion"`
		ResolvedVersion  string `json:"resolvedVersion"`
		Source           string `json:"source"`
	} `json:"resolvedPackages"`
	DetectedProviders []string `json:"detectedProviders"`
	Logs              []struct {
		Level string `json:"Level"`
		Msg   string `json:"Msg"`
	} `json:"logs"`
	Success bool `json:"success"`
}

// railpackPlan is the part of railpack's --plan-out a detection reads.
type railpackPlan struct {
	Steps []struct {
		Name     string `json:"name"`
		Commands []struct {
			Cmd        string `json:"cmd"`
			CustomName string `json:"customName"`
		} `json:"commands"`
	} `json:"steps"`
	Deploy struct {
		StartCommand string `json:"startCommand"`
	} `json:"deploy"`
}

// railpackDetection is what Railpack's info and plan say: what the source is, what
// it installs, what it runs to build, and what the image starts with.
func railpackDetection(infoJSON, planJSON []byte) *resource.BuildDetection {
	d := &resource.BuildDetection{}
	var info railpackInfo
	if len(infoJSON) == 0 || json.Unmarshal(infoJSON, &info) != nil {
		return d
	}
	d.Railpack = cut(info.RailpackVersion, 32)
	for _, p := range info.DetectedProviders {
		d.Providers = append(d.Providers, cut(p, 64))
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range info.ResolvedPackages {
			if !yield(k) {
				return
			}
		}
	}) {
		p := info.ResolvedPackages[name]
		d.Packages = append(d.Packages, resource.DetectedPackage{
			Name: cut(cmpOr(p.Name, name), 64), Requested: cut(p.RequestedVersion, 64), Resolved: cut(p.ResolvedVersion, 64), Source: cut(p.Source, 200),
		})
	}
	var errs []string
	for _, l := range info.Logs {
		switch strings.ToLower(l.Level) {
		case "warn":
			d.Warnings = append(d.Warnings, cut(l.Msg, 500))
		case "error":
			errs = append(errs, l.Msg)
		}
	}
	if !info.Success {
		d.Error = cut(cmpOr(strings.Join(errs, "\n"), "Railpack couldn't say how to build the app"), 4096)
	}
	var plan railpackPlan
	if len(planJSON) > 0 && json.Unmarshal(planJSON, &plan) == nil {
		for _, st := range plan.Steps {
			// What installs the packages is said of the packages.
			if strings.HasPrefix(st.Name, "packages:") {
				continue
			}
			var cmds []string
			for _, c := range st.Commands {
				if c.Cmd != "" && !strings.HasPrefix(c.Cmd, "mkdir -p ") {
					cmds = append(cmds, cut(cmpOr(c.CustomName, c.Cmd), 1024))
				}
			}
			if len(cmds) > 0 {
				d.Steps = append(d.Steps, resource.DetectedStep{Name: cut(st.Name, 64), Commands: cmds})
			}
		}
		d.Start = cut(plan.Deploy.StartCommand, 1024)
	}
	return d
}

// secretsHash sums a Railpack build's arguments and secrets, so a step that uses
// them builds again when one changes, and a step that doesn't stays in the cache.
// It is keyed with the server's own key, so it says nothing of what it sums.
func (b *Builder) secretsHash(req service.BuildRequest) string {
	mac := hmac.New(sha256.New, b.key())
	for _, set := range []map[string]string{req.Args, req.Secrets} {
		for _, k := range slices.Sorted(mapsKeys(set)) {
			mac.Write([]byte(k))
			mac.Write([]byte{0})
			mac.Write([]byte(set[k]))
			mac.Write([]byte{0})
		}
		mac.Write([]byte{1})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// cacheKey keys a Railpack build's cache mounts, which Railpack names after it,
// as <key>-<name>: the service's own, as an HMAC of its image's repository under
// the server's key, so no Dockerfile of another service can name them.
func (b *Builder) cacheKey(req service.BuildRequest) string {
	mac := hmac.New(sha256.New, b.key())
	mac.Write([]byte("railpack-cache\x00" + imageRepository(req.Image)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// key is the server's key for sums of secrets: kept in StateDir, so a build's
// cache outlives the agent, or made anew each time it starts when there is none.
func (b *Builder) key() []byte {
	b.keyOnce.Do(func() {
		if b.StateDir != "" {
			p := filepath.Join(b.StateDir, "build-key")
			if k, err := os.ReadFile(p); err == nil && len(k) == 32 { //nolint:gosec // the agent's own file
				b.hashKey = k
				return
			}
			k := make([]byte, 32)
			_, _ = rand.Read(k)
			if err := os.WriteFile(p, k, 0o600); err != nil {
				b.logger().Warn("can't keep the key builds' sums of secrets use, so a Railpack build's steps that use one build again once the agent restarts", "path", p, "error", err.Error())
			}
			b.hashKey = k
			return
		}
		b.hashKey = make([]byte, 32)
		_, _ = rand.Read(b.hashKey)
	})
	return b.hashKey
}
