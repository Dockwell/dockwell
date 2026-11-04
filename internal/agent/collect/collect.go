// SPDX-License-Identifier: AGPL-3.0-only

// Package collect reads what a Linux server is and how busy it is, from
// /proc, /etc/os-release, statfs and the Docker Engine socket. It reads
// files directly rather than through a metrics library, which keeps the
// agent small (its budget is 40 MB of memory at idle).
package collect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Collector samples one server. It is not safe for concurrent use: Stats
// keeps the previous sample to compute rates.
type Collector struct {
	// Root reads files such as "proc/stat". Defaults to the real root.
	Root fs.FS
	// Statfs reports a filesystem's size and use. Defaults to syscall.Statfs.
	Statfs func(path string) (size, used uint64, err error)
	// DockerSocket is where the Docker Engine listens.
	DockerSocket string
	// Hostname and Addresses default to the real ones.
	Hostname  func() (string, error)
	Addresses func() ([]net.Addr, error)

	prev *sample
}

type sample struct {
	at       time.Time
	cpuBusy  uint64
	cpuTotal uint64
	rx, tx   uint64
	haveNet  bool
	haveCPU  bool
}

// New returns a collector for this server.
func New() *Collector {
	return &Collector{
		Root:         os.DirFS("/"),
		Statfs:       statfs,
		DockerSocket: "/var/run/docker.sock",
		Hostname:     os.Hostname,
		Addresses:    net.InterfaceAddrs,
	}
}

// Inventory reports what the server is.
func (c *Collector) Inventory(ctx context.Context) *dockwellv1.ServerInventory {
	inv := &dockwellv1.ServerInventory{Architecture: runtime.GOARCH}
	inv.Hostname, _ = c.Hostname()
	release := parseKeyValues(c.read("etc/os-release"))
	if len(release) == 0 {
		release = parseKeyValues(c.read("usr/lib/os-release"))
	}
	inv.OsName = cmpOr(release["PRETTY_NAME"], release["NAME"])
	inv.OsId, inv.OsVersion = release["ID"], release["VERSION_ID"]
	inv.Kernel = strings.TrimSpace(string(c.read("proc/sys/kernel/osrelease")))
	inv.CpuModel, inv.CpuCount = cpuInfo(c.read("proc/cpuinfo"))
	if inv.CpuCount == 0 {
		inv.CpuCount = uint32(runtime.NumCPU()) //nolint:gosec // CPU counts fit in 32 bits
	}
	mem := parseMeminfo(c.read("proc/meminfo"))
	inv.MemoryBytes = mem["MemTotal"]
	for _, m := range c.mounts() {
		size, _, err := c.Statfs(m.point)
		if err != nil || size == 0 {
			continue
		}
		inv.Disks = append(inv.Disks, &dockwellv1.Disk{MountPoint: m.point, Device: m.device, Filesystem: m.fstype, SizeBytes: size})
	}
	slices.SortStableFunc(inv.Disks, func(a, b *dockwellv1.Disk) int {
		switch {
		case a.GetSizeBytes() > b.GetSizeBytes():
			return -1
		case a.GetSizeBytes() < b.GetSizeBytes():
			return 1
		}
		return 0
	})
	inv.DockerVersion = c.dockerVersion(ctx)
	if addrs, err := c.Addresses(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				inv.Addresses = append(inv.Addresses, ipn.IP.String())
			}
		}
	}
	return inv
}

// Stats samples the server's resource use. Rates (CPU, network) cover the
// time since the previous call; the first call primes them and reports 0.
func (c *Collector) Stats(now time.Time) *dockwellv1.ServerStats {
	st := &dockwellv1.ServerStats{Time: timestamppb.New(now)}
	cur := &sample{at: now}

	if busy, total, ok := cpuTimes(c.read("proc/stat")); ok {
		cur.cpuBusy, cur.cpuTotal, cur.haveCPU = busy, total, true
		if p := c.prev; p != nil && p.haveCPU && total > p.cpuTotal {
			st.CpuPercent = clamp(float64(busy-p.cpuBusy) / float64(total-p.cpuTotal) * 100)
		}
	}

	mem := parseMeminfo(c.read("proc/meminfo"))
	st.MemoryTotalBytes = mem["MemTotal"]
	if avail, ok := mem["MemAvailable"]; ok && avail <= st.MemoryTotalBytes {
		st.MemoryUsedBytes = st.MemoryTotalBytes - avail
	}

	if f := strings.Fields(string(c.read("proc/loadavg"))); len(f) >= 3 {
		st.Load1, _ = strconv.ParseFloat(f[0], 64)
		st.Load5, _ = strconv.ParseFloat(f[1], 64)
		st.Load15, _ = strconv.ParseFloat(f[2], 64)
	}
	if f := strings.Fields(string(c.read("proc/uptime"))); len(f) >= 1 {
		if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
			st.Uptime = durationpb.New(time.Duration(secs * float64(time.Second)))
		}
	}

	if rx, tx, ok := netTotals(c.read("proc/net/dev")); ok {
		cur.rx, cur.tx, cur.haveNet = rx, tx, true
		if p := c.prev; p != nil && p.haveNet {
			if secs := now.Sub(p.at).Seconds(); secs > 0 && rx >= p.rx && tx >= p.tx {
				st.NetworkRxBytesPerSecond = uint64(float64(rx-p.rx) / secs)
				st.NetworkTxBytesPerSecond = uint64(float64(tx-p.tx) / secs)
			}
		}
	}

	for _, m := range c.mounts() {
		size, used, err := c.Statfs(m.point)
		if err != nil || size == 0 {
			continue
		}
		st.Disks = append(st.Disks, &dockwellv1.DiskUsage{MountPoint: m.point, UsedBytes: used, SizeBytes: size})
	}

	c.prev = cur
	return st
}

func (c *Collector) read(name string) []byte {
	b, err := fs.ReadFile(c.Root, name)
	if err != nil {
		return nil
	}
	return b
}

// dataFilesystems are the filesystem types that hold a server's data.
// Everything else (proc, tmpfs, overlay, squashfs and friends) is plumbing.
var dataFilesystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true,
}

type mount struct{ device, point, fstype string }

// mounts lists data filesystems, one per device: the shortest mount point
// wins, so bind mounts and Docker's view of the same disk don't repeat.
func (c *Collector) mounts() []mount {
	byDevice := map[string]mount{}
	sc := bufio.NewScanner(bytes.NewReader(c.read("proc/self/mounts")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !dataFilesystems[f[2]] {
			continue
		}
		m := mount{device: f[0], point: unescapeMount(f[1]), fstype: f[2]}
		if have, ok := byDevice[m.device]; !ok || len(m.point) < len(have.point) {
			byDevice[m.device] = m
		}
	}
	out := make([]mount, 0, len(byDevice))
	for _, m := range byDevice {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b mount) int { return strings.Compare(a.point, b.point) })
	return out
}

// unescapeMount undoes the octal escapes /proc/self/mounts uses for spaces
// and other odd characters in paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// cpuTimes reads the aggregate "cpu" line of /proc/stat: busy is total
// minus idle and iowait.
func cpuTimes(stat []byte) (busy, total uint64, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(stat))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, s := range f[1:] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals = append(vals, v)
		}
		// guest and guest_nice (fields 9 and 10) are already counted in user and nice.
		for i, v := range vals {
			if i < 8 {
				total += v
			}
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4]
		}
		return total - idle, total, true
	}
	return 0, 0, false
}

// parseMeminfo reads /proc/meminfo into bytes.
func parseMeminfo(b []byte) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		out[key] = v
	}
	return out
}

// virtualInterfaces are network devices that carry traffic already counted
// on a physical one, or none at all.
var virtualInterfaces = []string{"lo", "docker", "veth", "br-", "virbr", "cni", "flannel", "cali", "vxlan", "tun", "tap", "dummy"}

// netTotals sums received and sent bytes over physical interfaces.
func netTotals(dev []byte) (rx, tx uint64, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(dev))
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if slices.ContainsFunc(virtualInterfaces, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx, tx, ok = rx+r, tx+t, true
	}
	return rx, tx, ok
}

// cpuInfo returns the first CPU model and the count of logical CPUs.
func cpuInfo(b []byte) (model string, count uint32) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "processor":
			count++
		case "model name", "Model", "cpu model":
			if model == "" {
				model = strings.TrimSpace(val)
			}
		}
	}
	return model, count
}

// parseKeyValues reads os-release style KEY=value lines.
func parseKeyValues(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if unq, err := strconv.Unquote(val); err == nil {
			val = unq
		} else {
			val = strings.Trim(val, `"'`)
		}
		out[key] = val
	}
	return out
}

// dockerVersion asks the Docker Engine for its version over its socket. It
// returns "" when Docker isn't installed or doesn't answer within 2 s.
func (c *Collector) dockerVersion(ctx context.Context) string {
	if c.DockerSocket == "" {
		return ""
	}
	if _, err := os.Stat(c.DockerSocket); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.DockerSocket)
		},
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", http.NoBody)
	if err != nil {
		return ""
	}
	res, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var v struct{ Version string }
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&v) != nil {
		return ""
	}
	return v.Version
}

func statfs(path string) (size, used uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) //nolint:gosec // block sizes are positive
	size = st.Blocks * bsize
	// Used as df counts it: blocks not free to anyone.
	used = (st.Blocks - st.Bfree) * bsize
	return size, used, nil
}

func clamp(p float64) float64 { return max(0, min(100, p)) }

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
