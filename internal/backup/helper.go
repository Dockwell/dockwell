// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/volumetool"
)

const (
	helperRepository = "dockwell-volume-tool"
	// labelHelper marks a helper, so it's never listed as something of a person's,
	// and one a crash left can be found.
	labelHelper = docker.LabelVolumeTool
	// helperMount is where a helper has the volume.
	helperMount = "/volume"
	// helperBinary is the binary in its image.
	helperBinary = "/dockwell"
	// helperCleanup bounds removing a helper and its image, once its run ended.
	helperCleanup = time.Minute
)

// tool runs the volume tool on a volume, in a throwaway container made for it
// from this agent's own binary. The volume is read-only unless the tool writes.
func (r *Runner) tool(ctx context.Context, t volume.Target, verb string, s docker.Streams) (volumetool.Stats, error) {
	var stats volumetool.Stats
	run := randomHex()
	image := helperRepository + ":" + run
	if err := r.importHelper(ctx, image); err != nil {
		return stats, err
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), helperCleanup)
	defer cancel()
	defer func() {
		if err := r.Client.RemoveImage(cleanup, image); err != nil {
			r.logger().Warn("can't remove a helper's image; the agent removes it when it next starts", "image", image, "error", err.Error())
		}
	}()
	req := &docker.CreateRequest{
		Config: docker.Config{
			Image: image, Entrypoint: []string{helperBinary}, Cmd: []string{volumetool.Command, verb, helperMount},
			User: "0:0", Labels: map[string]string{labelHelper: "1"},
			OpenStdin: s.Stdin != nil, StdinOnce: s.Stdin != nil,
		},
		HostConfig: docker.HostConfig{
			NetworkMode: "none", ReadonlyRootfs: true,
			// What it prints is an archive, and Docker would keep a copy of it.
			LogConfig: &docker.LogConfig{Type: "none"},
			Mounts:    []docker.Mount{{Type: "volume", Source: t.Name, Target: helperMount, ReadOnly: verb == "pack"}},
		},
	}
	id, err := r.Client.CreateContainer(ctx, helperRepository+"-"+run, req)
	if err != nil {
		return stats, fmt.Errorf("docker can't make the helper that reads the volume: %w", err)
	}
	defer func() {
		if err := r.Client.RemoveContainer(cleanup, id, true); err != nil && !docker.IsNotFound(err) {
			r.logger().Warn("can't remove a helper; the agent removes it when it next starts", "container", id, "error", err.Error())
		}
	}()
	report := &toolReport{ctx: ctx, volume: t.Key, verb: verb}
	s.Stderr = report
	code, err := r.Client.RunAttached(ctx, id, s)
	report.flush()
	stats = report.final()
	switch {
	case err != nil:
		return stats, errors.Join(err, report.failure())
	case code != 0:
		if said := report.failure(); said != nil {
			return stats, said
		}
		return stats, fmt.Errorf("the helper exited with code %d and said nothing. See what Docker says of it on this server", code)
	case !stats.Done:
		return stats, errors.New("the helper ended without saying it had finished")
	}
	return stats, nil
}

// importHelper makes an image of one file, this agent's binary. It's static, so
// it runs with nothing else there, and it comes from no registry (ADR 0033).
func (r *Runner) importHelper(ctx context.Context, image string) error {
	path := r.Binary
	if path == "" {
		path = "/proc/self/exe"
	}
	f, err := os.Open(path) //nolint:gosec // this process's own binary
	if err != nil {
		return fmt.Errorf("can't read the agent's own binary: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("can't read the agent's own binary: %w", err)
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(helperBinary, "/"), Typeflag: tar.TypeReg, Mode: 0o755, Size: fi.Size()})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		if err == nil {
			err = tw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	defer pr.Close()
	if err := r.Client.ImportImage(ctx, image, pr); err != nil {
		return fmt.Errorf("docker can't import the helper that reads the volume: %w", err)
	}
	return nil
}

// toolReport reads what the tool says on stderr: lines of JSON on how it's going,
// and a failure in words.
type toolReport struct {
	ctx    context.Context //nolint:containedctx // for its progress, for the length of a call
	volume string
	verb   string

	mu    sync.Mutex
	buf   bytes.Buffer
	stats volumetool.Stats
	said  []string
}

// maxSaid bounds what's kept of a failure.
const maxSaid = 8

func (t *toolReport) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(b)
	for {
		line, err := t.buf.ReadString('\n')
		if err != nil {
			t.buf.Reset()
			t.buf.WriteString(line)
			return len(b), nil
		}
		t.line(strings.TrimSpace(line))
	}
}

func (t *toolReport) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := bufio.NewScanner(&t.buf); s.Scan() {
		t.line(strings.TrimSpace(s.Text()))
	}
}

func (t *toolReport) line(line string) {
	if line == "" {
		return
	}
	var s volumetool.Stats
	if json.Unmarshal([]byte(line), &s) != nil {
		if len(t.said) < maxSaid {
			t.said = append(t.said, line)
		}
		return
	}
	t.stats = s
	if !s.Done && s.Bytes > 0 {
		what := "Copying "
		if t.verb == "unpack" {
			what = "Restoring "
		}
		resource.Progress(t.ctx, what+t.volume+": "+human.Size(s.Bytes)+" so far")
	}
}

func (t *toolReport) final() volumetool.Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

func (t *toolReport) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.said) == 0 {
		return nil
	}
	return errors.New(strings.Join(t.said, " "))
}
