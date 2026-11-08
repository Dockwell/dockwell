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
// railpack binary too, which a builder copies out once, into a volume, to write the
// plan with. Pinned by the digest of its multi-platform index; pin a new one to a
// release at least 14 days old, with the version it is.
const (
	RailpackVersion  = "0.43.0"
	RailpackFrontend = "ghcr.io/railwayapp/railpack-frontend@sha256:355ef61fae6f2551103cd8b10c710cb07167dd353d561f15c69b8d7ac1c676cc"
	// RailpackVolume holds the railpack binary and the mise it fetches to read
	// versions with, which every build on the server shares.
	RailpackVolume = "dockwell-railpack"
)

// railpackMount is where RailpackVolume is in a builder: where Railpack keeps
// mise, /tmp/railpack/mise, and the binary beside it.
const (
	railpackMount = "/tmp/railpack"
	railpackBin   = railpackMount + "/bin/railpack-" + RailpackVersion
)

// What reading a commit may use. It writes a plan and nothing more, so it gets
// less than a build.
const (
	detectMemory = 1 << 30
	detectPids   = 1024
	// detectWait bounds it, the first on a server fetching mise as well.
	detectWait = 3 * time.Minute
)

// copyRailpack is what a container of the frontend's image runs to put the binary
// in the volume, if it isn't there.
const copyRailpack = `set -eu
if [ -x "` + railpackBin + `" ]; then exit 0; fi
mkdir -p "$(dirname "` + railpackBin + `")"
cp /railpack "` + railpackBin + `.part"
chmod 0755 "` + railpackBin + `.part"
mv "` + railpackBin + `.part" "` + railpackBin + `"
`

// ensureRailpack has the railpack binary in RailpackVolume, once each time the
// agent starts.
func (b *Builder) ensureRailpack(ctx context.Context) error {
	b.railpackMu.Lock()
	defer b.railpackMu.Unlock()
	if b.railpackReady {
		return nil
	}
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
			Mounts:      []docker.Mount{{Type: "volume", Source: RailpackVolume, Target: railpackMount}},
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
	b.railpackReady = true
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
// Railpack say what it makes of the source, or says it needs Railpack first. Its
// arguments are Railpack's -e flags, one for each build argument. Every value comes
// from the environment or the arguments, never from the script's words.
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
if [ ! -x "` + railpackBin + `" ]; then say fail need-railpack; exit 4; fi
mkdir -p /work
set -- prepare "$ctx" --plan-out /work/railpack-plan.json --info-out /work/railpack-info.json --hide-pretty-plan "$@"
if [ -n "$DW_BUILD_CMD" ]; then set -- "$@" --build-cmd "$DW_BUILD_CMD"; fi
if [ -n "$DW_START_CMD" ]; then set -- "$@" --start-cmd "$DW_START_CMD"; fi
"` + railpackBin + `" "$@" >/tmp/railpack.log 2>&1 || true
if [ -s /work/railpack-info.json ]; then say railpack-info "$(base64 </work/railpack-info.json | tr -d '\n')"; fi
if [ -s /work/railpack-plan.json ]; then say railpack-plan "$(base64 </work/railpack-plan.json | tr -d '\n')"; fi
if [ ! -s /work/railpack-info.json ]; then tail -n 5 /tmp/railpack.log >&2; say fail railpack; fi
exit 0
`

// Detect reads a commit as a build would, and says how it would build: with its
// Dockerfile, or with Railpack and what Railpack found. It builds nothing, and runs
// beside a build.
func (b *Builder) Detect(ctx context.Context, req service.BuildRequest) (*resource.BuildDetection, error) {
	b.init()
	if err := checkRef(req.Ref); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, detectWait)
	defer cancel()
	if err := b.ensureImage(ctx); err != nil {
		return nil, err
	}
	if req.Builder == resource.BuilderRailpack {
		if err := b.ensureRailpack(ctx); err != nil {
			return nil, err
		}
	}
	d, need, err := b.detect(ctx, req)
	if err == nil && need {
		if err = b.ensureRailpack(ctx); err == nil {
			d, _, err = b.detect(ctx, req)
		}
	}
	return d, err
}

// detect runs a detect container once, and says whether it needs Railpack first.
func (b *Builder) detect(ctx context.Context, req service.BuildRequest) (*resource.BuildDetection, bool, error) {
	memory, cpus, err := b.limits(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := b.ensureNetwork(ctx); err != nil {
		return nil, false, err
	}
	var cmd []string
	for _, k := range slices.Sorted(mapsKeys(req.Args)) {
		cmd = append(cmd, "-e", k+"="+req.Args[k])
	}
	create := &docker.CreateRequest{
		Config: docker.Config{
			Image: b.image(), Entrypoint: []string{"/bin/sh"}, Cmd: append([]string{"-c", detectScript, "dockwell-detect"}, cmd...),
			Env: env(req, 0), Labels: map[string]string{docker.LabelBuilder: "detect"},
		},
		HostConfig: docker.HostConfig{
			// On the build network, as what it reads comes from the same repository.
			NetworkMode: Network,
			Memory:      min(memory, detectMemory), MemorySwap: min(memory, detectMemory), NanoCPUs: cpus * 1e9,
			CPUShares: cpuShares, OomScoreAdj: 1000, PidsLimit: detectPids,
			LogConfig: &docker.LogConfig{Type: "none"},
			Mounts:    []docker.Mount{{Type: "volume", Source: RailpackVolume, Target: railpackMount}},
		},
	}
	var said bytes.Buffer
	code, err := b.runOnce(ctx, "dockwell-detect-"+randomHex(), create, &said)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, false, resource.PlainlyBecause(resource.ReasonBuild, "Reading the commit took longer than "+human.Duration(detectWait)+". Check the server reaches the repository, and GitHub, where Railpack fetches mise", err)
		}
		return nil, false, err
	}
	if strings.Contains(said.String(), marker+"fail need-railpack") {
		return nil, true, nil
	}
	d, err := readDetection(req, said.String(), code)
	return d, false, err
}

// readDetection reads what a detect container said.
func readDetection(req service.BuildRequest, said string, code int) (*resource.BuildDetection, error) {
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
	switch failed {
	case "fetch", "context", "dockerfile":
		return nil, failure(req, f, 3, false)
	}
	if code != 0 {
		return nil, resource.PlainlyBecause(resource.ReasonBuild,
			fmt.Sprintf("Reading the commit failed, with code %d: %q", code, cmpOr(lastWords(tail), "it said nothing")),
			fmt.Errorf("the detect container exited with %d", code))
	}
	if hasDockerfile {
		return &resource.BuildDetection{
			Builder: resource.BuilderDockerfile, Dockerfile: filepath.Join(req.Context, dockerfile(req.Dockerfile)), BaseImage: baseImage(string(from)),
		}, nil
	}
	d := railpackDetection(info, plan)
	if failed == "railpack" && d.Error == "" {
		d.Error = "Railpack didn't say what it found: " + cmpOr(lastWords(tail), "it said nothing")
	}
	d.Builder = resource.BuilderRailpack
	return d, nil
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
