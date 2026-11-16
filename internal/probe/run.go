// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// What the probe's image holds: the agent's binary, and the server's CA
// certificates, as a container made of nothing else has none to check TLS by.
const (
	binaryIn = "/dockwell"
	certsIn  = "/etc/ssl/certs/ca-certificates.crt"
)

// certFiles are where Linux distributions keep their CA bundle, as Go looks for it.
var certFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// The limits of the probe's container, which runs one small check.
const (
	probeMemory = 64 << 20
	probePids   = 64
	// cleanupWait bounds removing what a check made, once it ended.
	cleanupWait = 30 * time.Second
	// maxAnswer bounds what the probe's output reads.
	maxAnswer = 64 << 10
)

// MaxChecks is how many checks an agent runs at once: each makes a container, and
// maybe a network, so a loop of them can't keep the server's Docker busy (#710).
const MaxChecks = 2

// ErrBusy is a check refused as the agent runs as many as it takes at once.
var ErrBusy = errors.New("this server is running as many connection checks as it takes at once. Try again in a moment")

// Runner runs checks in the probe's container, on this server's Docker.
type Runner struct {
	Client *docker.Client
	// Binary is the agent's own; /proc/self/exe unless set.
	Binary string
	// Certs is the CA bundle the probe trusts; the first of certFiles there unless
	// set.
	Certs string
	// Own are the server's own addresses, which the probe never reaches: each of
	// its interfaces' unless set.
	Own    func() ([]netip.Prefix, error)
	Logger *slog.Logger
	// Start is what this agent's checks are labelled with, and their names begin
	// with, so Clean takes only what an earlier start left: random unless set.
	Start string
	start sync.Once

	// slots holds a place for each check running, MaxChecks at most.
	slots     chan struct{}
	slotsOnce sync.Once
	// made is the image of this binary, once known, which every check runs, and
	// layer the DiffID of its one layer, which an image by that name must have.
	madeMu sync.Mutex
	made   string
	layer  string
}

// started is what this agent's checks are labelled with.
func (r *Runner) started() string {
	r.start.Do(func() {
		if r.Start == "" {
			r.Start = randomHex()
		}
	})
	return r.Start
}

// Run checks req from a container on networks, the app's, as the app would reach
// what it names. With none, as for a connection no app uses yet, or an app on the
// host's network, it runs on a network made for it. The server's own addresses
// are refused whatever networks it's on.
func (r *Runner) Run(ctx context.Context, req Request, networks []string) (Result, error) {
	if req.S3 != nil && s3.CheckPrefix(req.S3.Prefix) != nil {
		return Result{}, fmt.Errorf("the check's prefix isn't folders in a bucket, so nothing ran: %w", s3.ErrPrefix)
	}
	r.slotsOnce.Do(func() { r.slots = make(chan struct{}, MaxChecks) })
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return Result{}, ErrBusy
	}
	start := r.started()
	run := start + "-" + randomHex()
	name := docker.ProbeRepository + "-" + run
	own, err := r.own()
	if err != nil {
		return Result{}, err
	}
	req.Refuse = append(req.Refuse, own...)
	if len(networks) == 0 {
		if _, err := r.Client.CreateNetwork(ctx, &docker.NetworkCreate{
			Name: name, CheckDuplicate: true, Labels: map[string]string{docker.LabelProbe: start},
		}); err != nil {
			return Result{}, fmt.Errorf("docker can't make the network the check runs on: %w", err)
		}
		defer r.cleanup(ctx, "network", name, func(ctx context.Context) error { return r.Client.RemoveNetwork(ctx, name) })
		networks = []string{name}
	} else {
		// What the app reaches on its own networks, the probe reaches too.
		for _, n := range networks {
			got, err := r.Client.InspectNetwork(ctx, n)
			if err != nil {
				return Result{}, fmt.Errorf("can't read the app's network %s: %w", n, err)
			}
			for _, c := range got.IPAM.Config {
				if p, err := netip.ParsePrefix(c.Subnet); err == nil {
					req.Allow = append(req.Allow, p)
				}
			}
		}
	}
	image, err := r.image(ctx)
	if err != nil {
		return Result{}, err
	}
	id, err := r.Client.CreateContainer(ctx, name, &docker.CreateRequest{
		Config: docker.Config{
			Image: image, Entrypoint: []string{binaryIn}, Cmd: []string{Command},
			User: "65534:65534", Labels: map[string]string{docker.LabelProbe: start},
			OpenStdin: true, StdinOnce: true,
		},
		HostConfig: docker.HostConfig{
			NetworkMode: networks[0], ReadonlyRootfs: true,
			// Its input holds the secrets, and Docker would keep what it prints.
			LogConfig:   &docker.LogConfig{Type: "none"},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Memory:      probeMemory, MemorySwap: probeMemory, PidsLimit: probePids,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("docker can't make the check's container: %w", err)
	}
	defer r.cleanup(ctx, "container", id, func(ctx context.Context) error {
		if err := r.Client.RemoveContainer(ctx, id, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
		return nil
	})
	for _, n := range networks[1:] {
		if err := r.Client.ConnectNetwork(ctx, n, id, nil); err != nil {
			return Result{}, fmt.Errorf("docker can't join the check to the app's network %s: %w", n, err)
		}
	}
	in, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	wait, cancel := context.WithTimeout(ctx, CheckWait+ConnectWait)
	defer cancel()
	out, errs := &capped{max: maxAnswer}, &capped{max: 4 << 10}
	code, err := r.Client.RunAttached(wait, id, docker.Streams{Stdin: bytes.NewReader(in), Stdout: out, Stderr: errs})
	switch {
	case err != nil && wait.Err() != nil && ctx.Err() == nil:
		return Result{Step: "connect", Code: CodeTimeout}, nil
	case err != nil:
		return Result{}, fmt.Errorf("can't run the check's container: %w", err)
	case code != 0:
		// What it printed stays in the agent's log, without the request's keys: a
		// panic or a future error could carry what it was given, and the caller
		// gets the code alone.
		if r.Logger != nil {
			r.Logger.Warn("a connection's check stopped before it finished", "code", code, "said", withoutKeys(strings.TrimSpace(errs.String()), req))
		}
		return Result{}, fmt.Errorf("the check stopped before it finished, with code %d. Try again, or read the agent's log on the server", code)
	}
	var res Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return Result{}, fmt.Errorf("can't read what the check found: %w", err)
	}
	return res, nil
}

// image is the probe's image for this agent's binary, tagged by its layer's digest:
// made the first time a check needs it, again if it's gone, and kept, so a check
// imports nothing (#710). An image by that name is run only if its one layer is the
// one this agent would import, so one made by other hands, which would be given a
// connection's keys, is made again instead. Clean removes another build's.
func (r *Runner) image(ctx context.Context) (string, error) {
	r.madeMu.Lock()
	defer r.madeMu.Unlock()
	tag, err := r.tag()
	if err != nil {
		return "", err
	}
	tags, err := r.Client.ImageTags(ctx, docker.ProbeRepository)
	if err != nil {
		return "", fmt.Errorf("docker can't list the check's images: %w", err)
	}
	if slices.Contains(tags, tag) {
		got, err := r.Client.InspectImage(ctx, tag)
		if err != nil {
			return "", fmt.Errorf("docker can't read the check's image: %w", err)
		}
		if slices.Equal(got.RootFS.Layers, []string{r.layer}) {
			return tag, nil
		}
		if err := r.Client.RemoveImage(ctx, tag); err != nil {
			return "", fmt.Errorf("the check's image isn't this agent's, and docker can't remove it: %w", err)
		}
	}
	if err := r.Client.ImportFiles(ctx, tag, r.files()); err != nil {
		return "", fmt.Errorf("docker can't import the check's container: %w", err)
	}
	return tag, nil
}

// tag is the probe image's name for this binary, worked out once: the repository
// and the start of the DiffID of the layer ImportFiles writes of its files, which
// layer keeps whole. Callers hold madeMu.
func (r *Runner) tag() (string, error) {
	if r.made != "" {
		return r.made, nil
	}
	sum := sha256.New()
	if err := docker.WriteFiles(sum, r.files()); err != nil {
		return "", fmt.Errorf("can't read the agent's own binary: %w", err)
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	r.made, r.layer = docker.ProbeRepository+":"+digest[:16], "sha256:"+digest
	return r.made, nil
}

// files are what the probe's image holds.
func (r *Runner) files() []docker.ImageFile {
	binary := r.Binary
	if binary == "" {
		binary = "/proc/self/exe"
	}
	out := []docker.ImageFile{{Name: binaryIn, Path: binary, Mode: 0o755}}
	certs := r.Certs
	if certs == "" {
		for _, f := range certFiles {
			if _, err := os.Stat(f); err == nil {
				certs = f
				break
			}
		}
	}
	if certs != "" {
		out = append(out, docker.ImageFile{Name: certsIn, Path: certs, Mode: 0o644})
	}
	return out
}

// own are the server's own addresses: each of its interfaces', Docker's bridges
// and loopback among them.
func (r *Runner) own() ([]netip.Prefix, error) {
	if r.Own != nil {
		return r.Own()
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("can't read this server's own addresses: %w", err)
	}
	out := make([]netip.Prefix, 0, len(addrs))
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if addr, ok := netip.AddrFromSlice(n.IP); ok {
			addr = addr.Unmap()
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return out, nil
}

// cleanup removes what a check made once it ended, however it ended, logging what
// it can't: Clean removes it at the agent's next start.
func (r *Runner) cleanup(ctx context.Context, what, name string, remove func(context.Context) error) {
	ctx, cancel := resource.Finishing(ctx, cleanupWait)
	defer cancel()
	if err := remove(ctx); err != nil && r.Logger != nil {
		r.Logger.Warn("can't remove what a connection's check made; the agent removes it when it next starts", what, name, "error", err.Error())
	}
}

// Clean removes what a check an agent's crash cut short left: its containers and its
// networks, only what an earlier start of the agent made, by their label and name,
// so a check this start runs, which may begin while Clean does, is never cut short;
// and the probe images of other builds, as an upgrade leaves them.
func (r *Runner) Clean(ctx context.Context) error {
	start := r.started()
	list, err := r.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProbe}})
	if err != nil {
		return err
	}
	var errs []error
	for i := range list {
		if list[i].Labels[docker.LabelProbe] == start {
			continue
		}
		if err := r.Client.RemoveContainer(ctx, list[i].ID, true); err != nil && !docker.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	nets, err := r.Client.ListNetworks(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for i := range nets {
		if was, ok := nets[i].Labels[docker.LabelProbe]; ok && was != start {
			errs = append(errs, r.Client.RemoveNetwork(ctx, nets[i].Name))
		}
	}
	// One image per build: this one's stays, and another's, as an upgrade leaves it, goes.
	r.madeMu.Lock()
	keep, err := r.tag()
	r.madeMu.Unlock()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	tags, err := r.Client.ImageTags(ctx, docker.ProbeRepository)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, tag := range tags {
		if tag != keep {
			errs = append(errs, r.Client.RemoveImage(ctx, tag))
		}
	}
	return errors.Join(errs...)
}

// capped keeps what's written up to max bytes.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// withoutKeys is what the probe printed with its request's keys taken out: as they
// are, and as a URL, base64 or JSON would write them. What's left of one where
// the cap cut it short goes too.
func withoutKeys(said string, req Request) string {
	if req.S3 == nil {
		return said
	}
	for _, key := range []string{req.S3.SecretKey, req.S3.AccessKey} {
		if key == "" {
			continue
		}
		for _, form := range keyForms(key) {
			said = strings.ReplaceAll(said, form, "[key]")
			for n := len(form) - 1; n >= minKeyCut; n-- {
				if strings.HasSuffix(said, form[:n]) {
					said = strings.TrimSuffix(said, form[:n]) + "[key]"
					break
				}
			}
		}
	}
	return said
}

// minKeyCut is the shortest start of a key, at the end of what the probe printed,
// that's taken for one the cap cut short.
const minKeyCut = 4

// keyForms are the ways a key may be written out.
func keyForms(key string) []string {
	escaped, _ := json.Marshal(key)
	forms := []string{
		key, url.QueryEscape(key), url.PathEscape(key), strings.Trim(string(escaped), `"`),
		base64.StdEncoding.EncodeToString([]byte(key)), base64.RawStdEncoding.EncodeToString([]byte(key)),
		base64.URLEncoding.EncodeToString([]byte(key)), base64.RawURLEncoding.EncodeToString([]byte(key)),
	}
	// The longest first, so a form that holds another goes whole, and the same
	// form once.
	slices.SortFunc(forms, func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	return slices.Compact(forms)
}
