// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func symlink(target string) *fstest.MapFile {
	return &fstest.MapFile{Mode: fs.ModeSymlink, Data: []byte(target)}
}

func TestSocketsAndProcessDetail(t *testing.T) {
	t.Parallel()
	cid := strings.Repeat("a", 64)
	cidBridge := strings.Repeat("b", 64)
	// Host namespace sockets:
	//   :80 LISTEN (inode 1001, nginx host proc)
	//   :8080->:50000 ESTABLISHED (inode 1002, host-network container app)
	//   :443 TIME_WAIT (inode 0, kernel)
	hostTCP := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0 100 0 0 10 0",
		"   1: 0100007F:1F90 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 1002 1 0 100 0 0 10 0",
		"   2: 0100007F:01BB 0100007F:C351 06 00000000:00000000 00:00000000 00000000     0        0    0 0 0",
	}, "\n") + "\n"
	hostUDP := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 1003 2 0 0\n"

	// Bridge-networked container namespace (net:[4026539999]): postgres listening on 5432 (0x1538), inode 2001.
	// Absent from host /proc/net/tcp, present only in /proc/300/net/tcp (M2).
	bridgeTCP := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 2001 1 0 100 0 0 10 0\n"

	stat1 := "1 (systemd) S 0 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 10 0 100\n"
	stat100 := "100 (nginx) S 1 100 100 0 -1 4194560 0 0 0 0 10 20 0 0 20 0 4 0 500 0 256\n"
	stat200 := "200 (app) R 100 200 200 0 -1 4194560 0 0 0 0 30 40 0 0 20 0 8 0 600 0 512\n"
	stat300 := "300 (postgres) S 1 300 300 0 -1 4194560 0 0 0 0 5 5 0 0 20 0 2 0 700 0 1024\n"

	longSecret := "s3cr3t_value_never_shown"
	longArg := strings.Repeat("x", 800)
	root := fstest.MapFS{
		"etc/passwd":       {Data: []byte("root:x:0:0:root:/root:/bin/sh\napp:x:1000:1000::/home/app:/bin/sh\n")},
		"proc/uptime":      {Data: []byte("1000.00 500.00\n")},
		"proc/self/ns/net": symlink("net:[4026531992]"),
		"proc/net/tcp":     {Data: []byte(hostTCP)},
		"proc/net/udp":     {Data: []byte(hostUDP)},
		"proc/1/stat":      {Data: []byte(stat1)},
		"proc/1/status":    {Data: []byte("Tgid:\t1\nUid:\t0\t0\t0\t0\n")},
		"proc/1/ns/net":    symlink("net:[4026531992]"),
		"proc/100/stat":    {Data: []byte(stat100)},
		"proc/100/status":  {Data: []byte("Tgid:\t100\nUid:\t0\t0\t0\t0\n")},
		"proc/100/cmdline": {Data: []byte("/usr/sbin/nginx\x00-g\x00daemon off;\x00")},
		"proc/100/cgroup":  {Data: []byte("0::/system.slice/nginx.service\n")},
		"proc/100/ns/net":  symlink("net:[4026531992]"),
		"proc/100/net/tcp": {Data: []byte(hostTCP)},
		"proc/100/net/udp": {Data: []byte(hostUDP)},
		"proc/100/fd/3":    symlink("socket:[1001]"),
		"proc/100/fd/4":    symlink("socket:[1003]"),
		"proc/200/stat":    {Data: []byte(stat200)},
		"proc/200/status":  {Data: []byte("Tgid:\t200\nUid:\t1000\t1000\t1000\t1000\n")},
		"proc/200/cmdline": {Data: []byte("node\x00server.js\x00--api-key\x00" + longSecret + "\x00DB_PASSWORD=hunter2\x00--arg\x00" + longArg + "\x00")},
		"proc/200/environ": {Data: []byte("TOP_SECRET_ENV=must_never_leak\x00")},
		"proc/200/cgroup":  {Data: []byte("0::/system.slice/docker-" + cid + ".scope\n")},
		"proc/200/ns/net":  symlink("net:[4026531992]"),
		"proc/200/net/tcp": {Data: []byte(hostTCP)},
		"proc/200/fd/5":    symlink("socket:[1002]"),
		// Regular (non-symlink) file under fd/ must NEVER be opened/read (M3).
		"proc/200/fd/9":    {Data: []byte("socket:[1001]")},
		"proc/300/stat":    {Data: []byte(stat300)},
		"proc/300/status":  {Data: []byte("Tgid:\t300\nUid:\t999\t999\t999\t999\n")},
		"proc/300/cmdline": {Data: []byte("postgres\x00-D\x00/var/lib/postgresql/data\x00")},
		"proc/300/cgroup":  {Data: []byte("0::/system.slice/docker-" + cidBridge + ".scope\n")},
		"proc/300/ns/net":  symlink("net:[4026539999]"),
		"proc/300/net/tcp": {Data: []byte(bridgeTCP)},
		"proc/300/fd/6":    symlink("socket:[2001]"),
	}

	c := &Collector{Root: root}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	byContainer := func(id string) *dockwellv1.ProcessContainer {
		switch id {
		case cid:
			return &dockwellv1.ProcessContainer{Id: cid, Name: "web-1"}
		case cidBridge:
			return &dockwellv1.ProcessContainer{Id: cidBridge, Name: "db-1"}
		default:
			return nil
		}
	}

	// ListSockets covers BOTH host namespace and bridge-networked container namespace (M2).
	res := c.Sockets(t.Context(), now, byContainer, nil)
	if res.GetTotal() != 5 || len(res.GetConnections()) != 5 || res.GetTruncated() {
		t.Fatalf("Sockets = total %d len %d truncated %v, want 5/5/false", res.GetTotal(), len(res.GetConnections()), res.GetTruncated())
	}
	// LISTEN sockets sorted by port: :80 (nginx) then :5432 (postgres in db-1).
	s0 := res.GetConnections()[0]
	if s0.GetState() != "LISTEN" || s0.GetLocalPort() != 80 || s0.GetPid() != 100 || s0.GetProcessName() != "nginx" || s0.GetUnit() != "nginx.service" {
		t.Errorf("first socket = %+v, want LISTEN :80 nginx (PID 100, nginx.service)", s0)
	}
	s1 := res.GetConnections()[1]
	if s1.GetState() != "LISTEN" || s1.GetLocalPort() != 5432 || s1.GetPid() != 300 || s1.GetProcessName() != "postgres" || s1.GetContainer().GetName() != "db-1" {
		t.Errorf("second socket = %+v, want LISTEN :5432 postgres (PID 300, db-1)", s1)
	}
	// Third socket is ESTABLISHED :8080->:50000 owned by app (PID 200, container web-1).
	s2 := res.GetConnections()[2]
	if s2.GetState() != "ESTABLISHED" || s2.GetLocalPort() != 8080 || s2.GetRemotePort() != 50000 || s2.GetPid() != 200 || s2.GetContainer().GetName() != "web-1" {
		t.Errorf("third socket = %+v, want ESTABLISHED :8080->:50000 app (PID 200, web-1)", s2)
	}
	// Fourth socket is UDP UNCONN :53 owned by nginx.
	s3 := res.GetConnections()[3]
	if s3.GetProtocol() != "udp" || s3.GetState() != "UNCONN" || s3.GetLocalPort() != 53 || s3.GetPid() != 100 {
		t.Errorf("fourth socket = %+v, want udp UNCONN :53 PID 100", s3)
	}
	// Fifth is TIME_WAIT with inode 0 (kernel).
	s4 := res.GetConnections()[4]
	if s4.GetState() != "TIME_WAIT" || s4.GetPid() != 0 {
		t.Errorf("fifth socket = %+v, want TIME_WAIT PID 0", s4)
	}

	// ProcessDetail for PID 200: full redacted command, parent=nginx, threads=8,
	// ONLY 1 open socket (port 8080; fd/9 is a regular file so M3 never reads it).
	detail, ok := c.ProcessDetail(t.Context(), now, 200, 600, 999, byContainer, nil)
	if !ok {
		t.Fatal("ProcessDetail(200) returned !ok")
	}
	if detail.GetParentName() != "nginx" || detail.GetThreads() != 8 || detail.GetState() != "running" {
		t.Errorf("detail metadata: parent=%q threads=%d state=%q, want nginx/8/running", detail.GetParentName(), detail.GetThreads(), detail.GetState())
	}
	cmd := detail.GetFullCommand()
	if strings.Contains(cmd, longSecret) || strings.Contains(cmd, "hunter2") || strings.Contains(cmd, "TOP_SECRET_ENV") || strings.Contains(cmd, "must_never_leak") {
		t.Errorf("ProcessDetail leaked a secret or env var: %q", cmd)
	}
	if !strings.Contains(cmd, "--api-key "+Hidden) || !strings.Contains(cmd, "DB_PASSWORD="+Hidden) || !strings.Contains(cmd, longArg) {
		t.Errorf("ProcessDetail full_command = %q, want redacted secrets + full 800-char arg", cmd)
	}
	if detail.GetSocketsTotal() != 1 || len(detail.GetSockets()) != 1 || detail.GetSockets()[0].GetLocalPort() != 8080 {
		t.Errorf("ProcessDetail sockets = %+v (total %d), want 1 socket on 8080 (fd/9 regular file ignored)", detail.GetSockets(), detail.GetSocketsTotal())
	}

	// ProcessDetail for PID 300 (bridge-networked postgres container) reads /proc/300/net/tcp (M2).
	pgDetail, ok := c.ProcessDetail(t.Context(), now, 300, 700, 999, byContainer, nil)
	if !ok {
		t.Fatal("ProcessDetail(300) returned !ok")
	}
	if pgDetail.GetSocketsTotal() != 1 || len(pgDetail.GetSockets()) != 1 || pgDetail.GetSockets()[0].GetLocalPort() != 5432 {
		t.Errorf("bridge container ProcessDetail sockets = %+v (total %d), want 1 socket on 5432", pgDetail.GetSockets(), pgDetail.GetSocketsTotal())
	}

	// Mismatched start_ticks must return !ok.
	if _, ok := c.ProcessDetail(t.Context(), now, 200, 999, 999, byContainer, nil); ok {
		t.Error("ProcessDetail with mismatched start_ticks should return !ok")
	}
}

// L4: A cancelled context aborts paged fd walking cleanly and marks sockets truncated.
func TestProcessSocketsRespectsContextDeadline(t *testing.T) {
	t.Parallel()
	root := fstest.MapFS{
		"proc/10/stat":    {Data: []byte("10 (busy) S 1 10 10 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 100\n")},
		"proc/10/status":  {Data: []byte("Tgid:\t10\nUid:\t0\t0\t0\t0\n")},
		"proc/10/cmdline": {Data: []byte("busy\x00")},
		"proc/10/net/tcp": {Data: []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 9001 1 0\n")},
		"proc/10/fd/3":    symlink("socket:[9001]"),
	}
	c := &Collector{Root: root}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	detail, ok := c.ProcessDetail(ctx, time.Now(), 10, 100, 999, nil, nil)
	if !ok {
		t.Fatal("ProcessDetail(10) returned !ok")
	}
	if !detail.GetSocketsTruncated() {
		t.Errorf("expected SocketsTruncated=true when context is cancelled")
	}
}

func TestSocketsRowBound(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i := range MaxSockets + 25 {
		fmt.Fprintf(&b, "%4d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 0\n", i, 1000+i)
	}
	c := &Collector{Root: fstest.MapFS{"proc/net/tcp": {Data: []byte(b.String())}}}
	res := c.Sockets(t.Context(), time.Now(), nil, nil)
	if len(res.GetConnections()) != MaxSockets || res.GetTotal() != MaxSockets+25 || !res.GetTruncated() {
		t.Fatalf("got len=%d total=%d truncated=%v, want %d/%d/true", len(res.GetConnections()), res.GetTotal(), res.GetTruncated(), MaxSockets, MaxSockets+25)
	}
}

type byteCountingFS struct {
	base     fstest.MapFS
	mu       sync.Mutex
	perFile  map[string]int64
	totalNet int64
}

func (f *byteCountingFS) Open(name string) (fs.File, error) {
	inner, err := f.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &byteCountingFile{File: inner, fs: f, name: name}, nil
}

func (f *byteCountingFS) ReadLink(name string) (string, error) {
	return f.base.ReadLink(name)
}

func (f *byteCountingFS) Lstat(name string) (fs.FileInfo, error) {
	return f.base.Lstat(name)
}

type byteCountingFile struct {
	fs.File
	fs   *byteCountingFS
	name string
}

func (cf *byteCountingFile) Read(p []byte) (int, error) {
	n, err := cf.File.Read(p)
	if n > 0 && strings.Contains(cf.name, "/net/") {
		cf.fs.mu.Lock()
		if cf.fs.perFile == nil {
			cf.fs.perFile = map[string]int64{}
		}
		cf.fs.perFile[cf.name] += int64(n)
		cf.fs.totalNet += int64(n)
		cf.fs.mu.Unlock()
	}
	return n, err
}

func (cf *byteCountingFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if rdf, ok := cf.File.(fs.ReadDirFile); ok {
		return rdf.ReadDir(n)
	}
	return nil, io.EOF
}

// N2: Sockets bounds bytes streamed per socket table file and in total across
// namespaces, never buffering entire multi-megabyte tables into memory.
func TestSocketsRespectsPerFileAndTotalByteCaps(t *testing.T) {
	t.Parallel()
	// Build a ~2 MiB tcp table (18,000 TIME_WAIT rows) with 2 LISTEN rows first.
	var big strings.Builder
	big.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	big.WriteString("   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0 100 0 0 10 0\n")
	big.WriteString("   1: 00000000:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00:00000000     0        0 1002 1 0 100 0 0 10 0\n")
	for i := range 18_000 {
		fmt.Fprintf(&big, "%5d: 0100007F:%04X 0100007F:%04X 06 00000000:00000000 00:00000000 00000000     0        0    0 0 0 100 0 0 10 0\n", i+2, 1000+(i%50000), 2000+(i%50000))
	}
	tableBytes := []byte(big.String())
	if len(tableBytes) <= maxSocketTableBytes {
		t.Fatalf("fixture size %d must exceed maxSocketTableBytes (%d)", len(tableBytes), maxSocketTableBytes)
	}

	mfs := fstest.MapFS{
		"proc/self/ns/net": symlink("net:[1]"),
		"proc/net/tcp":     {Data: tableBytes},
	}
	// Add 6 distinct container namespaces, each with a >2 MiB tcp table (~14 MiB total).
	for ns := 1; ns <= 6; ns++ {
		pid := strconv.Itoa(100 + ns)
		mfs["proc/"+pid+"/ns/net"] = symlink(fmt.Sprintf("net:[%d]", 1000+ns))
		mfs["proc/"+pid+"/net/tcp"] = &fstest.MapFile{Data: tableBytes}
	}

	cfs := &byteCountingFS{base: mfs}
	c := &Collector{Root: cfs}
	res := c.Sockets(t.Context(), time.Now(), nil, nil)

	if !res.GetTruncated() {
		t.Error("expected Truncated=true when tables exceed byte caps")
	}
	if len(res.GetConnections()) != MaxSockets {
		t.Errorf("kept %d connections, want %d", len(res.GetConnections()), MaxSockets)
	}
	if res.GetConnections()[0].GetState() != "LISTEN" || res.GetConnections()[0].GetLocalPort() != 80 {
		t.Errorf("first socket = %+v, want LISTEN :80", res.GetConnections()[0])
	}
	for name, n := range cfs.perFile {
		if n > maxSocketTableBytes+1 {
			t.Errorf("read %d bytes from %s, exceeds per-file cap %d", n, name, maxSocketTableBytes)
		}
	}
	// Each capped file may read 1 sentinel byte to detect truncation.
	if cfs.totalNet > maxSocketTotalBytes+int64(len(cfs.perFile)) {
		t.Errorf("read %d total net bytes across %d files, exceeds total cap %d", cfs.totalNet, len(cfs.perFile), maxSocketTotalBytes)
	}
}

// N2: Bounded top-K heap retains LISTEN sockets appearing after thousands of
// TIME_WAIT rows without holding every parsed row in memory.
func TestSocketsKeepsBestRowsUnderHeavyTimeWait(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i := range 4000 {
		fmt.Fprintf(&b, "%4d: 0100007F:%04X 0100007F:%04X 06 00000000:00000000 00:00000000 00000000 0 0 0\n", i, 10000+(i%40000), 20000+(i%40000))
	}
	for i := range 5 {
		fmt.Fprintf(&b, "%4d: 00000000:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 0\n", 4000+i, 80+i)
	}
	c := &Collector{Root: fstest.MapFS{"proc/net/tcp": {Data: []byte(b.String())}}}
	res := c.Sockets(t.Context(), time.Now(), nil, nil)
	if len(res.GetConnections()) != MaxSockets || !res.GetTruncated() || res.GetTotal() != 4005 {
		t.Fatalf("got len=%d total=%d truncated=%v, want %d/4005/true", len(res.GetConnections()), res.GetTotal(), res.GetTruncated(), MaxSockets)
	}
	for i := range 5 {
		got := res.GetConnections()[i]
		if got.GetState() != "LISTEN" || got.GetLocalPort() != uint32(80+i) {
			t.Errorf("socket[%d] = %s:%d, want LISTEN:%d", i, got.GetState(), got.GetLocalPort(), 80+i)
		}
	}
}

// N2: Cancelled context stops before reading container namespaces.
func TestSocketsChecksDeadlineBeforeNamespaceRead(t *testing.T) {
	t.Parallel()
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00:00000000 0 0 1001\n"
	cfs := &byteCountingFS{base: fstest.MapFS{
		"proc/self/ns/net": symlink("net:[1]"),
		"proc/net/tcp":     {Data: []byte(tcp)},
		"proc/200/ns/net":  symlink("net:[2]"),
		"proc/200/net/tcp": {Data: []byte(tcp)},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := &Collector{Root: cfs}
	res := c.Sockets(ctx, time.Now(), nil, nil)
	if !res.GetTruncated() {
		t.Error("expected Truncated=true on cancelled context")
	}
	if cfs.totalNet != 0 {
		t.Errorf("expected 0 net bytes read on cancelled context, got %d", cfs.totalNet)
	}
}
