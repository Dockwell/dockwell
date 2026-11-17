// SPDX-License-Identifier: AGPL-3.0-only

package tunnels

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/dockwell/dockwell/internal/lazyre"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
)

// Finder finds the Cloudflare tunnels cloudflared runs on a server: on the host, or
// in a container, as /proc shows every process either way.
type Finder struct {
	// Proc is the server's /proc.
	Proc fs.FS
	// ReadIn reads a file inside a process's own root, for its token or config file,
	// as a regular file of 4 KB at most; nil reads nothing.
	ReadIn func(pid int, start uint64, name string) ([]byte, error)
	// ReadyIn asks cloudflared's /ready at addr from inside the process's own
	// network; nil asks nothing, so no connector ID is found.
	ReadyIn func(ctx context.Context, pid int, addr string) (Ready, error)
}

// maxEnviron is as much of a process's environment as is read.
const maxEnviron = 64 << 10

// MaxFile is the most of a token or config file that's read: a connector token is
// about 250 bytes.
const MaxFile = 4 << 10

// kept are the only entries of a process's environment that are read: the token
// and where it's kept, where its metrics are, and its home, for its default config.
// A container's environment holds its other secrets too, which are never kept.
var kept = []string{"TUNNEL_TOKEN", "TUNNEL_TOKEN_FILE", "TUNNEL_METRICS", "HOME"}

// Reaches says how a tunnel reaches the server's proxy (ADR 0030): from the host's
// own network, or from a network of its own.
const (
	ReachesFromHost    = "http://127.0.0.1:80"
	ReachesFromNetwork = "http://host.docker.internal:80"
)

// Find lists the tunnels cloudflared runs now, each with the ID of its run where its
// /ready answers. A tunnel's token is decoded in memory for its account and ID, and
// its secret is never kept: nothing returned holds it.
func (f *Finder) Find(ctx context.Context) []*dockwellv1.CloudflaredTunnel {
	entries, err := fs.ReadDir(f.Proc, ".")
	if err != nil {
		return nil
	}
	ownNet := link(f.Proc, "self/ns/net")
	var out []*dockwellv1.CloudflaredTunnel
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if t := f.one(ctx, pid, ownNet); t != nil {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.CloudflaredTunnel) int { return int(a.GetPid()) - int(b.GetPid()) })
	return out
}

// one reads the process pid, if it's cloudflared running a tunnel.
func (f *Finder) one(ctx context.Context, pid int, ownNet string) *dockwellv1.CloudflaredTunnel {
	dir := strconv.Itoa(pid)
	name, start, ok := stat(f.Proc, dir)
	if !ok || name != "cloudflared" {
		return nil
	}
	args := splitNUL(readAtMost(f.Proc, dir+"/cmdline", 16<<10))
	a, ok := parseArgs(args)
	if !ok {
		return nil
	}
	environ := readAtMost(f.Proc, dir+"/environ", maxEnviron)
	env := keptEnv(environ)
	clear(environ)
	defer clear(env)

	t := &dockwellv1.CloudflaredTunnel{Pid: int32(pid)} //nolint:gosec // PIDs fit in 32 bits
	token := cmpOr(a.token, env["TUNNEL_TOKEN"])
	tokenFile := cmpOr(a.tokenFile, env["TUNNEL_TOKEN_FILE"])
	if token == "" && tokenFile != "" && f.ReadIn != nil {
		if b, err := f.ReadIn(pid, start, tokenFile); err == nil {
			token = string(bytes.TrimSpace(b))
			clear(b)
		}
	}
	// A run whose tunnel can't be read is reported all the same, with no tunnel ID:
	// cloudflared runs here, so the server's address is never published by default
	// (the review's M1).
	switch {
	case token != "":
		if account, id, ok := decodeToken(token); ok {
			t.AccountTag, t.TunnelId = account, id
		}
	case tokenFile != "":
		// Its token is in a file the agent can't read.
	default:
		// A locally managed tunnel: its ID from its config, or the command line.
		t.ManagedInFile = true
		t.TunnelId = f.localID(pid, start, a, env["HOME"])
	}
	// What's read is the process's only while its PID is: one that changed hands
	// as it was read is dropped.
	if _, again, ok := stat(f.Proc, dir); !ok || again != start {
		return nil
	}
	t.ContainerId = collect.ContainerInCgroup(readAtMost(f.Proc, dir+"/cgroup", 16<<10))
	if theirs := link(f.Proc, dir+"/ns/net"); theirs == "" || theirs == ownNet {
		t.Reaches = ReachesFromHost
	} else {
		t.Reaches = ReachesFromNetwork
		t.NetworkRange = ownRange(readAtMost(f.Proc, dir+"/net/route", 64<<10))
	}
	if f.ReadyIn != nil {
		for _, addr := range metricsAddrs(cmpOr(a.metrics, env["TUNNEL_METRICS"])) {
			if r, err := f.ReadyIn(ctx, pid, addr); err == nil {
				t.ConnectorId = r.ConnectorID
				break
			}
		}
	}
	return t
}

// args is what a cloudflared command line says of its tunnel.
type args struct {
	token, tokenFile, config, metrics string
	// named is what follows "run", a tunnel's name or ID, for a locally managed one.
	named string
}

// parseArgs reads a cloudflared command line: false for one that runs no tunnel,
// such as "cloudflared access" or "cloudflared tunnel list".
func parseArgs(argv []string) (args, bool) {
	var a args
	var sawTunnel, sawRun bool
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		value := func() string {
			if _, v, ok := strings.Cut(arg, "="); ok {
				return v
			}
			if i+1 < len(argv) {
				i++
				return argv[i]
			}
			return ""
		}
		flag := strings.TrimLeft(arg, "-")
		if before, _, ok := strings.Cut(flag, "="); ok {
			flag = before
		}
		switch {
		case !strings.HasPrefix(arg, "-"):
			switch {
			case arg == "tunnel" && !sawTunnel:
				sawTunnel = true
			case arg == "run" && sawTunnel && !sawRun:
				sawRun = true
			case sawRun && a.named == "":
				a.named = arg
			}
		case flag == "token":
			a.token = value()
		case flag == "token-file":
			a.tokenFile = value()
		case flag == "config":
			a.config = value()
		case flag == "metrics":
			a.metrics = value()
		case strings.Contains(arg, "="):
			// A flag with its value joined, such as --loglevel=info: nothing to skip.
		case takesValue[flag]:
			i++
		}
	}
	return a, sawTunnel && sawRun
}

// takesValue are cloudflared's flags that take a value apart, which parseArgs skips
// past so it isn't read as a tunnel's name.
var takesValue = map[string]bool{
	"loglevel": true, "transport-loglevel": true, "logfile": true, "log-directory": true, "pidfile": true,
	"origincert": true, "credentials-file": true, "credentials-contents": true, "edge-ip-version": true,
	"protocol": true, "region": true, "grace-period": true, "retries": true, "label": true, "url": true,
	"hostname": true, "name": true, "edge-bind-address": true, "management-hostname": true,
	"icmpv4-src": true, "icmpv6-src": true, "tag": true, "ha-connections": true,
}

// decodeToken reads a connector token's account tag and tunnel ID. The token is
// base64 JSON {"a": account, "t": tunnel, "s": secret}: only "a" and "t" are read
// into anything, and the decoded bytes are cleared.
func decodeToken(token string) (account, tunnel string, ok bool) {
	var raw []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(token); err == nil {
			raw = b
			break
		}
	}
	if raw == nil {
		return "", "", false
	}
	defer clear(raw)
	var got struct {
		A string `json:"a"`
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || !uuidLike(got.T) || got.A == "" {
		return "", "", false
	}
	return got.A, strings.ToLower(got.T), true
}

var uuid = lazyre.Regexp(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func uuidLike(s string) bool { return uuid().MatchString(s) }

// configPlaces are cloudflared's default places for its config file, under a home
// for the first three.
var configPlaces = []string{"~/.cloudflared", "~/.cloudflare-warp", "~/cloudflare-warp", "/etc/cloudflared", "/usr/local/etc/cloudflared"}

// localID finds a locally managed tunnel's ID: what follows "run" where it's an ID,
// or its config's tunnel key, or the ID its credentials file is named for. The
// credentials file itself is never opened.
func (f *Finder) localID(pid int, start uint64, a args, home string) string {
	if uuidLike(a.named) {
		return strings.ToLower(a.named)
	}
	if f.ReadIn == nil {
		return ""
	}
	var files []string
	if a.config != "" {
		files = append(files, a.config)
	} else {
		for _, dir := range configPlaces {
			if strings.HasPrefix(dir, "~") {
				if home == "" || !path.IsAbs(home) {
					continue
				}
				dir = path.Join(home, strings.TrimPrefix(dir, "~"))
			}
			files = append(files, path.Join(dir, "config.yml"), path.Join(dir, "config.yaml"))
		}
	}
	for _, name := range files {
		b, err := f.ReadIn(pid, start, name)
		if err != nil {
			continue
		}
		id := configID(b)
		clear(b)
		if id != "" {
			return id
		}
	}
	return ""
}

// configID reads a cloudflared config's tunnel ID: its "tunnel" key where that's an
// ID, or the ID its "credentials-file" is named for.
func configID(b []byte) string {
	var tunnel, creds string
	for line := range strings.SplitSeq(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(key, " ") || strings.HasPrefix(key, "\t") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "tunnel":
			tunnel = value
		case "credentials-file":
			creds = strings.TrimSuffix(path.Base(value), ".json")
		}
	}
	switch {
	case uuidLike(tunnel):
		return strings.ToLower(tunnel)
	case uuidLike(creds):
		return strings.ToLower(creds)
	}
	return ""
}

// metricsAddrs are where to ask for /ready: the address named, with a host that
// listens everywhere asked on loopback, or cloudflared's defaults.
func metricsAddrs(named string) []string {
	if named == "" {
		return DefaultMetrics
	}
	host, port, err := net.SplitHostPort(named)
	if err != nil || port == "" || port == "0" {
		return DefaultMetrics
	}
	switch host {
	case "", "0.0.0.0", "::", "localhost":
		host = "127.0.0.1"
	}
	return []string{net.JoinHostPort(host, port)}
}

// ownRange reads the range a process's network comes from out of its own routing
// table, /proc/<pid>/net/route: the route on its interface that needs no gateway.
func ownRange(table []byte) string {
	for i, line := range strings.Split(string(table), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 || f[1] == "00000000" || f[2] != "00000000" {
			continue
		}
		dest, err1 := strconv.ParseUint(f[1], 16, 32)
		mask, err2 := strconv.ParseUint(f[7], 16, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		var ip, m [4]byte
		binary.LittleEndian.PutUint32(ip[:], uint32(dest))
		binary.LittleEndian.PutUint32(m[:], uint32(mask))
		ones, bits := net.IPMask(m[:]).Size()
		if bits == 0 || ones == 0 {
			continue
		}
		return (&net.IPNet{IP: net.IP(ip[:]), Mask: net.CIDRMask(ones, 32)}).String()
	}
	return ""
}

// stat reads a process's name and start time, in ticks since boot.
func stat(proc fs.FS, dir string) (name string, start uint64, ok bool) {
	b := readAtMost(proc, dir+"/stat", 4<<10)
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", 0, false
	}
	rest := strings.Fields(string(b[end+1:]))
	if len(rest) < 20 || rest[0] == "Z" {
		return "", 0, false
	}
	start, err := strconv.ParseUint(rest[19], 10, 64)
	return string(b[open+1 : end]), start, err == nil
}

// keptEnv reads only the kept entries out of an environment's NUL-separated list.
func keptEnv(environ []byte) map[string]string {
	out := map[string]string{}
	for _, entry := range bytes.Split(environ, []byte{0}) {
		key, value, ok := bytes.Cut(entry, []byte{'='})
		if ok && slices.Contains(kept, string(key)) {
			out[string(key)] = string(value)
		}
	}
	return out
}

func splitNUL(b []byte) []string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

func readAtMost(fsys fs.FS, name string, n int64) []byte {
	f, err := fsys.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, n)
	got := 0
	for got < len(b) {
		m, err := f.Read(b[got:])
		got += m
		if err != nil {
			break
		}
	}
	return b[:got]
}

// link reads a symlink in proc, as ns/net's "net:[4026531840]", which names a
// network namespace; empty where it can't.
func link(proc fs.FS, name string) string {
	if rl, ok := proc.(fs.ReadLinkFS); ok {
		target, err := rl.ReadLink(name)
		if err == nil {
			return target
		}
	}
	return ""
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
