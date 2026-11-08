// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// BuildKitRepository is BuildKit's image, whose containers the fake engine runs as
// a stand-in for a build, as internal/build runs one (ADR 0046).
const BuildKitRepository = "moby/buildkit"

// The stand-in runs a Dockerfile's steps as words, not commands. A RUN step:
//   - prints what each echo in it says;
//   - prints a build secret it mounts and cats, so a test sees it masked;
//   - sleeps as long as a sleep in it says, up to a minute, so a test sees a build
//     under way, and stops when the builder is removed;
//   - fails with the code of an exit in it, or 1 for false;
//   - runs out of memory where it says oom.
//
// It checks out the commit with the real git, from what the build names, so a
// test needs a Git server, as a real build does.
var (
	echoWords   = regexp.MustCompile(`echo\s+("[^"]*"|'[^']*'|[^&;|]+)`)
	sleepFor    = regexp.MustCompile(`sleep\s+(\d+)`)
	exitWith    = regexp.MustCompile(`exit\s+(\d+)`)
	catSecret   = regexp.MustCompile(`cat\s+/run/secrets/([A-Za-z0-9_.-]+)`)
	secretMount = regexp.MustCompile(`--mount=type=secret,id=([A-Za-z0-9_.-]+)`)
)

// fakeBuild stands in for a build in a container of BuildKit's image: it does what
// the builder's script and BuildKit would, and says so as they would, and writes
// the image it made to stdout as an image archive. It reports the container's exit
// code.
func (e *Engine) fakeBuild(c *container, stdout, stderr io.Writer) (code int) {
	envs := map[string]string{}
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		envs[k] = v
	}
	say := func(s string) { _, _ = fmt.Fprintf(stderr, "dockwell:%s\n", s) }
	args := c.Config.Cmd
	for i, a := range args {
		if a == "dockwell-build" {
			args = args[i+1:]
			break
		}
	}
	buildArgs, secrets := map[string]string{}, map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--opt":
			if kv, ok := strings.CutPrefix(args[i+1], "build-arg:"); ok {
				k, v, _ := strings.Cut(kv, "=")
				buildArgs[k] = v
			}
		case "--secret":
			var id, src string
			for _, part := range strings.Split(args[i+1], ",") {
				k, v, _ := strings.Cut(part, "=")
				switch k {
				case "id":
					id = v
				case "src":
					src = v
				}
			}
			if host := e.hostPathOf(c, src); host != "" {
				if b, err := os.ReadFile(host); err == nil { //nolint:gosec // a test's own file
					secrets[id] = string(b)
				}
			}
		}
		i++
	}

	say("phase fetch")
	src, err := os.MkdirTemp("", "dockertest-build")
	if err != nil {
		say("fail fetch")
		return 3
	}
	defer os.RemoveAll(src)
	git := func(args ...string) error {
		cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // a test's own repository
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			_, _ = stderr.Write(out)
		}
		return err
	}
	commit, ref := envs["DW_COMMIT"], envs["DW_REF"]
	if ref == "" {
		ref = "HEAD"
	}
	if git("init", "-q") != nil {
		say("fail fetch")
		return 3
	}
	// As the script does: the branch, and the commit only if the branch has it.
	if ref == commit {
		if git("fetch", "-q", "--depth", "1", envs["DW_URL"], commit) != nil {
			say("fail fetch")
			return 3
		}
	} else {
		if git("fetch", "-q", envs["DW_URL"], ref) != nil {
			say("fail fetch")
			return 3
		}
		if exec.CommandContext(context.Background(), "git", "-C", src, "merge-base", "--is-ancestor", commit, "FETCH_HEAD").Run() != nil { //nolint:gosec // a test's own repository
			say("fail notonref")
			return 3
		}
	}
	if git("-c", "advice.detachedHead=false", "checkout", "-q", commit) != nil {
		say("fail fetch")
		return 3
	}
	author, _ := exec.CommandContext(context.Background(), "git", "-C", src, "log", "-1", "--format=%an").Output() //nolint:gosec // as above
	subject, _ := exec.CommandContext(context.Background(), "git", "-C", src, "log", "-1", "--format=%s").Output() //nolint:gosec // as above
	say("commit " + base64.StdEncoding.EncodeToString(bytes.TrimSpace(author)) + " " + base64.StdEncoding.EncodeToString(bytes.TrimSpace(subject)))
	ctxDir := filepath.Join(src, envs["DW_CONTEXT"])
	if st, err := os.Stat(ctxDir); err != nil || !st.IsDir() {
		say("fail context")
		return 3
	}
	dockerfile, err := os.ReadFile(filepath.Join(ctxDir, envs["DW_DOCKERFILE"])) //nolint:gosec // as above
	if err != nil {
		say("fail dockerfile")
		return 3
	}
	say("phase start")
	say("phase build")

	all := parseDockerfile(string(dockerfile))
	// BuildKit makes a step of what changes the filesystem, and none of what only
	// says how the image runs, such as EXPOSE or CMD.
	var steps, meta []step
	for _, st := range all {
		switch st.instr {
		case "FROM", "RUN", "COPY", "ADD", "WORKDIR":
			steps = append(steps, st)
		default:
			meta = append(meta, st)
		}
	}
	status := func(s map[string]any) {
		b, _ := json.Marshal(s)
		_, _ = stderr.Write(append(b, '\n'))
	}
	cfg := docker.Config{Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}}
	// BuildKit says what the whole build is before it starts any of it.
	digests := make([]string, len(steps))
	prev := sha256.Sum256(nil)
	var graph []any
	for i, st := range steps {
		h := sha256.New()
		h.Write(prev[:])
		h.Write([]byte(st.line))
		for k, v := range buildArgs {
			h.Write([]byte(k + "=" + v))
		}
		if st.instr == "COPY" || st.instr == "ADD" {
			h.Write([]byte(treeHash(ctxDir)))
		}
		copy(prev[:], h.Sum(nil))
		digests[i] = "sha256:" + hex.EncodeToString(prev[:])
		graph = append(graph, map[string]any{"Digest": digests[i], "Name": fmt.Sprintf("[%d/%d] %s", i+1, len(steps), st.line)})
	}
	status(map[string]any{"Vertexes": graph})
	for i, st := range steps {
		name := fmt.Sprintf("[%d/%d] %s", i+1, len(steps), st.line)
		digest := digests[i]
		now := time.Now().UTC()
		e.mu.Lock()
		cached := e.buildCache[digest]
		e.mu.Unlock()
		vertex := map[string]any{"Digest": digest, "Name": name, "Started": now}
		if cached && st.instr != "RUN" || cached && !strings.Contains(st.args, "sleep") {
			vertex["Cached"], vertex["Completed"] = true, now
			status(map[string]any{"Vertexes": []any{vertex}})
			applyStep(&cfg, st)
			continue
		}
		status(map[string]any{"Vertexes": []any{vertex}})
		logLine := func(text string) {
			status(map[string]any{"Logs": []any{map[string]any{"Vertex": digest, "Stream": 1, "Data": []byte(text + "\n"), "Timestamp": time.Now().UTC()}}})
		}
		fail := func(code int, oom bool) int {
			err := fmt.Sprintf("process \"/bin/sh -c %s\" did not complete successfully: exit code: %d", st.args, code)
			vertex["Error"], vertex["Completed"] = err, time.Now().UTC()
			status(map[string]any{"Vertexes": []any{vertex}})
			_, _ = fmt.Fprintf(stderr, "error: failed to solve: %s\n", err)
			if oom {
				e.mu.Lock()
				c.State.OOMKilled = true
				e.mu.Unlock()
			}
			return code
		}
		if st.instr == "RUN" {
			if m := secretMount.FindStringSubmatch(st.args); m != nil {
				if cm := catSecret.FindStringSubmatch(st.args); len(cm) > 1 && cm[1] == m[1] {
					logLine(secrets[m[1]])
				}
			}
			for _, m := range echoWords.FindAllStringSubmatch(st.args, -1) {
				logLine(expand(strings.Trim(strings.TrimSpace(m[1]), `"'`), buildArgs))
			}
			if m := sleepFor.FindStringSubmatch(st.args); m != nil {
				n, _ := strconv.Atoi(m[1])
				deadline := time.Now().Add(time.Duration(min(n, 60)) * time.Second)
				for time.Now().Before(deadline) {
					if e.gone(c) {
						return 137
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			switch {
			case strings.Contains(st.args, "oom"):
				logLine("Killed")
				return fail(137, true)
			case exitWith.MatchString(st.args):
				n, _ := strconv.Atoi(exitWith.FindStringSubmatch(st.args)[1])
				if n != 0 {
					return fail(n, false)
				}
			case strings.Contains(" "+st.args+" ", " false "):
				return fail(1, false)
			}
		}
		applyStep(&cfg, st)
		vertex["Completed"] = time.Now().UTC()
		status(map[string]any{"Vertexes": []any{vertex}})
		e.mu.Lock()
		e.buildCache[digest] = true
		e.mu.Unlock()
	}
	for _, st := range meta {
		applyStep(&cfg, st)
	}
	export := "sha256:" + hex.EncodeToString(prev[:4]) + "export"
	now := time.Now().UTC()
	status(map[string]any{"Vertexes": []any{map[string]any{"Digest": export, "Name": "exporting to docker image format", "Started": now, "Completed": now}}})
	if err := writeArchive(stdout, envs["DW_IMAGE"], cfg); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: can't write the image: %v\n", err)
		return 1
	}
	say("phase prune")
	say("pruned")
	return 0
}

// gone reports a container that was removed, as a build that is stopped.
func (e *Engine) gone(c *container) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.containers[c.ID]
	return !ok
}

// hostPathOf is where a path in a container is on this machine, through its binds.
func (e *Engine) hostPathOf(c *container, p string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range c.HostConfig.Mounts {
		if m.Type == "bind" && (p == m.Target || strings.HasPrefix(p, m.Target+"/")) {
			return filepath.Join(m.Source, strings.TrimPrefix(p, m.Target))
		}
	}
	return ""
}

// step is one instruction of a Dockerfile.
type step struct {
	instr, args, line string
}

// parseDockerfile reads a Dockerfile's instructions, joining lines that go on.
func parseDockerfile(s string) []step {
	var out []step
	var cur strings.Builder
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			cur.WriteString(strings.TrimSuffix(line, "\\") + " ")
			continue
		}
		cur.WriteString(line)
		full := strings.Join(strings.Fields(cur.String()), " ")
		cur.Reset()
		instr, args, _ := strings.Cut(full, " ")
		out = append(out, step{instr: strings.ToUpper(instr), args: args, line: strings.ToUpper(instr) + " " + args})
	}
	return out
}

// applyStep has what an instruction says of the image go into its config.
func applyStep(cfg *docker.Config, st step) {
	words := func() []string {
		var w []string
		if json.Unmarshal([]byte(st.args), &w) == nil {
			return w
		}
		return []string{"/bin/sh", "-c", st.args}
	}
	switch st.instr {
	case "CMD":
		cfg.Cmd = words()
	case "ENTRYPOINT":
		cfg.Entrypoint = words()
	case "WORKDIR":
		cfg.WorkingDir = st.args
	case "ENV":
		k, v, ok := strings.Cut(st.args, "=")
		if !ok {
			k, v, _ = strings.Cut(st.args, " ")
		}
		cfg.Env = append(cfg.Env, k+"="+strings.Trim(v, `"`))
	case "EXPOSE":
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range strings.Fields(st.args) {
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			cfg.ExposedPorts[p] = struct{}{}
		}
	case "USER":
		cfg.User = st.args
	}
}

// expand puts build arguments' values in for ${NAME} and $NAME.
func expand(s string, args map[string]string) string {
	return os.Expand(s, func(k string) string { return args[k] })
}

// treeHash sums the files under dir, so a COPY of what changed isn't cached.
func treeHash(dir string) string {
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() && d.Name() == ".git" {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(p) //nolint:gosec // a test's own checkout
			rel, _ := filepath.Rel(dir, p)
			h.Write([]byte(rel))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// writeArchive writes an image as docker save does: a manifest naming its config
// and its tag, and no layers, which the fake engine has no use for.
func writeArchive(w io.Writer, tag string, cfg docker.Config) error {
	config, err := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "config": cfg, "created": time.Now().UTC()})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(config)
	name := hex.EncodeToString(sum[:]) + ".json"
	manifest, err := json.Marshal([]map[string]any{{"Config": name, "RepoTags": []string{tag}, "Layers": []string{}}})
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, f := range []struct {
		name string
		body []byte
	}{{name, config}, {"manifest.json", manifest}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(f.body); err != nil {
			return err
		}
	}
	return tw.Close()
}

// loadImage takes an image archive into the engine's images, as docker load does.
func (e *Engine) loadImage(w http.ResponseWriter, r *http.Request) {
	files := map[string][]byte{}
	tr := tar.NewReader(r.Body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(w, http.StatusBadRequest, "archive/tar: %v", err)
			return
		}
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(h.Name, ".json") {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		files[h.Name] = b
	}
	var manifest []struct {
		Config   string
		RepoTags []string
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil || len(manifest) == 0 {
		fail(w, http.StatusBadRequest, "invalid tar file: no manifest.json")
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Requests = append(e.Requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	for _, m := range manifest {
		var config struct {
			Config  docker.Config `json:"config"`
			Created time.Time     `json:"created"`
		}
		if err := json.Unmarshal(files[m.Config], &config); err != nil {
			_ = enc.Encode(map[string]any{"error": "invalid config", "errorDetail": map[string]string{"message": "invalid config " + m.Config}})
			return
		}
		img := &image{id: "sha256:" + newID(), created: config.Created, Image: Image{Config: config.Config}}
		e.images[img.id] = img
		for _, tag := range m.RepoTags {
			img.repo = repoOf(tag)
			e.images[tag] = img
			_ = enc.Encode(map[string]string{"stream": "Loaded image: " + tag + "\n"})
		}
		e.Loads++
	}
}
