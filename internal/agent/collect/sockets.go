// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"bufio"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const (
	// MaxSockets bounds how many live sockets ListSockets returns.
	MaxSockets = 1000
	// MaxProcessSockets bounds how many open sockets GetProcess returns for one process.
	MaxProcessSockets = 256
	// socketReadBudget bounds how long walking /proc/<pid>/fd and container
	// network namespaces may take, so a box with tens of thousands of fds never
	// stalls the agent.
	socketReadBudget = 2 * time.Second
	// fdReadPage bounds how many /proc/<pid>/fd entries are read per batch so
	// the deadline is checked inside a single process's fd table too.
	fdReadPage = 256
	// maxSocketTableBytes caps how many bytes are streamed from any single
	// /proc/.../net/{tcp,tcp6,udp,udp6} file, preventing a container with
	// tens of thousands of TIME_WAIT entries from churning agent memory.
	maxSocketTableBytes = 1 << 20 // 1 MiB (~6,900 rows)
	// maxSocketTotalBytes caps total bytes streamed across all namespace socket
	// tables in a single Sockets or processSockets call.
	maxSocketTotalBytes = 4 << 20 // 4 MiB (~27,000 rows)
)

// tcpState maps Linux /proc/net/tcp hex state codes to ss(8) state names.
var tcpStates = map[string]string{
	"01": "ESTABLISHED",
	"02": "SYN_SENT",
	"03": "SYN_RECV",
	"04": "FIN_WAIT1",
	"05": "FIN_WAIT2",
	"06": "TIME_WAIT",
	"07": "CLOSE",
	"08": "CLOSE_WAIT",
	"09": "LAST_ACK",
	"0A": "LISTEN",
	"0B": "CLOSING",
	"0C": "NEW_SYN_RECV",
}

// socketState maps Linux /proc/net/{tcp,udp} hex state codes to ss(8) state names.
func socketState(proto, hexState string) string {
	if strings.HasPrefix(proto, "udp") {
		if hexState == "01" {
			return "ESTABLISHED"
		}
		return "UNCONN"
	}
	if s, ok := tcpStates[hexState]; ok {
		return s
	}
	return hexState
}

// rawSocket is one row parsed from /proc/<pid>/net/{tcp,tcp6,udp,udp6}.
type rawSocket struct {
	protocol   string
	localAddr  string
	localPort  uint32
	remoteAddr string
	remotePort uint32
	state      string
	inode      string
	uid        uint32
	netns      string
}

// socketOwner is the process owning a socket inode.
type socketOwner struct {
	pid         uint32
	startTicks  uint64
	name        string
	uid         uint32
	containerID string
	unit        string
}

type procNetEntry struct {
	pid   uint32
	netns string
}

// socketScanBudget tracks the time deadline and total byte allowance across
// socket table reads so neither CPU time nor memory spikes on busy hosts.
type socketScanBudget struct {
	ctx            context.Context
	deadline       time.Time
	remainingBytes int64
	truncated      bool
}

func (b *socketScanBudget) expired() bool {
	if b.ctx.Err() != nil {
		b.truncated = true
		return true
	}
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		b.truncated = true
		return true
	}
	return false
}

// socketTopK keeps at most limit best sockets according to compareRawSocket,
// stored as a max-heap so items[0] is always the worst kept socket.
type socketTopK struct {
	limit     int
	items     []rawSocket
	total     uint32
	truncated bool
}

func newSocketTopK(limit int) *socketTopK {
	return &socketTopK{limit: limit}
}

func (h *socketTopK) full() bool { return len(h.items) >= h.limit }

func (h *socketTopK) worstStateRank() int {
	if len(h.items) == 0 {
		return 100
	}
	return stateRank(h.items[0].state)
}

func (h *socketTopK) countDropped() {
	h.total++
	h.truncated = true
}

func (h *socketTopK) push(s rawSocket) {
	h.total++
	if len(h.items) < h.limit {
		h.items = append(h.items, s)
		h.siftUp(len(h.items) - 1)
		return
	}
	h.truncated = true
	if h.limit == 0 || compareRawSocket(s, h.items[0]) >= 0 {
		return
	}
	h.items[0] = s
	h.siftDown(0)
}

func (h *socketTopK) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if compareRawSocket(h.items[i], h.items[p]) <= 0 {
			break
		}
		h.items[i], h.items[p] = h.items[p], h.items[i]
		i = p
	}
}

func (h *socketTopK) siftDown(i int) {
	n := len(h.items)
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		worst := l
		if r := l + 1; r < n && compareRawSocket(h.items[r], h.items[l]) > 0 {
			worst = r
		}
		if compareRawSocket(h.items[i], h.items[worst]) >= 0 {
			break
		}
		h.items[i], h.items[worst] = h.items[worst], h.items[i]
		i = worst
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// Sockets reads live TCP and UDP (IPv4 and IPv6) sockets across the host and
// container network namespaces (deduplicated by /proc/<pid>/ns/net), maps them
// to owning processes and containers within socketReadBudget and byte bounds,
// and returns at most MaxSockets rows ordered with LISTEN and ESTABLISHED first.
func (c *Collector) Sockets(ctx context.Context, now time.Time, container ContainerOf, userIn UserIn) *dockwellv1.ListSocketsResponse {
	deadline := time.Now().Add(socketReadBudget)
	budget := &socketScanBudget{
		ctx:            ctx,
		deadline:       deadline,
		remainingBytes: maxSocketTotalBytes,
	}
	top := newSocketTopK(MaxSockets)

	rlfs, canReadLink := c.Root.(fs.ReadLinkFS)
	readLink := func(name string) string {
		if !canReadLink {
			return ""
		}
		link, err := rlfs.ReadLink(name)
		if err != nil {
			return ""
		}
		return link
	}

	// Pass 1: stream host namespace (/proc/net) plus each distinct container
	// network namespace (/proc/<pid>/net) deduplicated by /proc/<pid>/ns/net,
	// checking deadline and byte budgets before each namespace read.
	seenNS := map[string]bool{}
	hostNS := readLink("proc/self/ns/net")
	if hostNS != "" {
		seenNS[hostNS] = true
	}
	c.scanSocketsFrom("proc/net", hostNS, budget, top, nil)

	var pids []procNetEntry
	if !budget.expired() {
		if entries, err := fs.ReadDir(c.Root, "proc"); err == nil {
			for i, e := range entries {
				if i&31 == 0 && budget.expired() {
					break
				}
				if !e.IsDir() {
					continue
				}
				pid64, err := strconv.ParseUint(e.Name(), 10, 32)
				if err != nil {
					continue
				}
				pid := uint32(pid64)
				pidDir := path.Join("proc", e.Name())
				ns := readLink(path.Join(pidDir, "ns/net"))
				pids = append(pids, procNetEntry{pid: pid, netns: ns})
				if ns != "" && !seenNS[ns] {
					seenNS[ns] = true
					if budget.expired() {
						break
					}
					if budget.remainingBytes <= 0 {
						budget.truncated = true
						continue
					}
					c.scanSocketsFrom(path.Join(pidDir, "net"), ns, budget, top, nil)
				}
			}
		}
	}

	total := top.total
	truncated := budget.truncated || top.truncated
	if len(top.items) == 0 {
		return &dockwellv1.ListSocketsResponse{Time: timestamppb.New(now), Truncated: truncated}
	}

	kept := top.items
	sortRawSockets(kept)

	wantInodes := make(map[string]struct{}, len(kept))
	wantNS := map[string]bool{}
	for _, s := range kept {
		if s.inode != "" && s.inode != "0" {
			wantInodes[s.inode] = struct{}{}
			wantNS[s.netns] = true
		}
	}

	owners, ownerTimedOut := c.findSocketOwners(ctx, pids, wantInodes, wantNS, deadline)
	if ownerTimedOut {
		truncated = true
	}

	users := parsePasswd(readAtMost(c.Root, "etc/passwd", maxPasswd))
	byContainer := map[string]*dockwellv1.ProcessContainer{}
	out := make([]*dockwellv1.SocketConnection, 0, len(kept))
	for _, s := range kept {
		conn := &dockwellv1.SocketConnection{
			Protocol:      s.protocol,
			LocalAddress:  s.localAddr,
			LocalPort:     s.localPort,
			RemoteAddress: s.remoteAddr,
			RemotePort:    s.remotePort,
			State:         s.state,
		}
		if owner, ok := owners[s.inode]; ok {
			conn.Pid = owner.pid
			conn.StartTicks = owner.startTicks
			conn.ProcessName = owner.name
			conn.Unit = owner.unit
			if owner.containerID != "" {
				pc, seen := byContainer[owner.containerID]
				if !seen {
					pc = &dockwellv1.ProcessContainer{Id: owner.containerID}
					if container != nil {
						if known := container(owner.containerID); known != nil {
							pc = known
						}
					}
					byContainer[owner.containerID] = pc
				}
				conn.Container = pc
				if userIn != nil {
					conn.User = userIn(owner.containerID, owner.pid, owner.uid)
				}
			} else {
				conn.User = users[owner.uid]
			}
			if conn.User == "" {
				conn.User = formatUID(owner.uid)
			}
		} else if name := users[s.uid]; name != "" {
			conn.User = name
		}
		out = append(out, conn)
	}
	return &dockwellv1.ListSocketsResponse{
		Time:        timestamppb.New(now),
		Connections: out,
		Total:       total,
		Truncated:   truncated,
	}
}

// processSockets returns the open network sockets held by a single process pid,
// streaming from that process's own network namespace (/proc/<pid>/net/*)
// within socketReadBudget and byte bounds, bounded at MaxProcessSockets.
func (c *Collector) processSockets(ctx context.Context, pid uint32) (conns []*dockwellv1.SocketConnection, total uint32, truncated bool) {
	deadline := time.Now().Add(socketReadBudget)
	inodes, timedOut := c.pidSocketInodes(ctx, pid, deadline)
	if len(inodes) == 0 {
		return nil, 0, timedOut
	}
	budget := &socketScanBudget{
		ctx:            ctx,
		deadline:       deadline,
		remainingBytes: maxSocketTotalBytes,
		truncated:      timedOut,
	}
	top := newSocketTopK(MaxProcessSockets)
	pidNetDir := path.Join("proc", strconv.FormatUint(uint64(pid), 10), "net")
	c.scanSocketsFrom(pidNetDir, "", budget, top, inodes)

	truncated = budget.truncated || top.truncated
	if len(top.items) == 0 {
		return nil, 0, truncated
	}
	matched := top.items
	sortRawSockets(matched)
	total = top.total
	conns = make([]*dockwellv1.SocketConnection, 0, len(matched))
	for _, s := range matched {
		conns = append(conns, &dockwellv1.SocketConnection{
			Protocol:      s.protocol,
			LocalAddress:  s.localAddr,
			LocalPort:     s.localPort,
			RemoteAddress: s.remoteAddr,
			RemotePort:    s.remotePort,
			State:         s.state,
			Pid:           pid,
		})
	}
	return conns, total, truncated
}

// scanSocketsFrom streams /proc/.../net/{tcp,tcp6,udp,udp6} through a
// byte-capped bufio.Scanner, pushing directly into top without buffering the
// whole file or holding more than top.limit sockets in memory.
func (c *Collector) scanSocketsFrom(netDir, netns string, budget *socketScanBudget, top *socketTopK, wantInodes map[string]struct{}) {
	for _, proto := range []string{"tcp", "tcp6", "udp", "udp6"} {
		if budget.expired() {
			return
		}
		if budget.remainingBytes <= 0 {
			budget.truncated = true
			return
		}
		f, err := c.Root.Open(path.Join(netDir, proto))
		if err != nil {
			continue
		}
		fileCap := min(int64(maxSocketTableBytes), budget.remainingBytes)
		// Allow reading up to fileCap bytes plus 1 sentinel byte so we can tell
		// whether the file was truncated at the cap without exceeding fileCap+1.
		cr := &countingReader{r: io.LimitReader(f, fileCap+1)}
		sc := bufio.NewScanner(cr)
		// Skip header line.
		if sc.Scan() {
			line := 0
			for sc.Scan() {
				line++
				if line&255 == 0 && budget.expired() {
					break
				}
				if cr.n > fileCap {
					budget.truncated = true
					break
				}
				fields := strings.Fields(sc.Text())
				if len(fields) < 10 {
					continue
				}
				inode := fields[9]
				if wantInodes != nil {
					if _, ok := wantInodes[inode]; !ok {
						continue
					}
				}
				state := socketState(proto, strings.ToUpper(fields[3]))
				if top.full() && stateRank(state) > top.worstStateRank() {
					top.countDropped()
					continue
				}
				lAddr, lPort, ok1 := parseProcEndpoint(fields[1])
				rAddr, rPort, ok2 := parseProcEndpoint(fields[2])
				if !ok1 || !ok2 {
					continue
				}
				uid, _ := strconv.ParseUint(fields[7], 10, 32)
				top.push(rawSocket{
					protocol:   proto,
					localAddr:  lAddr,
					localPort:  lPort,
					remoteAddr: rAddr,
					remotePort: rPort,
					state:      state,
					inode:      inode,
					uid:        uint32(uid),
					netns:      netns,
				})
			}
		}
		if cr.n > fileCap {
			budget.truncated = true
		}
		spent := min(cr.n, fileCap)
		budget.remainingBytes -= spent
		if budget.remainingBytes < 0 {
			budget.remainingBytes = 0
		}
		_ = f.Close()
	}
}

// parseProcEndpoint decodes Linux /proc/net's "HEXIP:HEXPORT" format.
func parseProcEndpoint(s string) (string, uint32, bool) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	b, err := hex.DecodeString(hexAddr)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return "", 0, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	addr, ok := netip.AddrFromSlice(b)
	if !ok {
		return "", 0, false
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return "", 0, false
	}
	return addr.String(), uint32(port), true
}

func stateRank(state string) int {
	switch state {
	case "LISTEN":
		return 0
	case "ESTABLISHED":
		return 1
	case "UNCONN":
		return 2
	case "SYN_RECV", "SYN_SENT":
		return 3
	case "CLOSE_WAIT", "FIN_WAIT1", "FIN_WAIT2", "LAST_ACK", "CLOSING":
		return 4
	case "TIME_WAIT":
		return 5
	default:
		return 6
	}
}

func compareRawSocket(a, b rawSocket) int {
	return cmp.Or(
		cmp.Compare(stateRank(a.state), stateRank(b.state)),
		cmp.Compare(a.localPort, b.localPort),
		cmp.Compare(a.protocol, b.protocol),
		cmp.Compare(a.localAddr, b.localAddr),
		cmp.Compare(a.remoteAddr, b.remoteAddr),
		cmp.Compare(a.remotePort, b.remotePort),
	)
}

func sortRawSockets(list []rawSocket) {
	slices.SortStableFunc(list, compareRawSocket)
}

// findSocketOwners scans /proc/<pid>/fd to map requested socket inodes to their
// owning process, stopping early once every inode is matched, ctx cancels, or
// deadline is reached.
func (c *Collector) findSocketOwners(ctx context.Context, pids []procNetEntry, want map[string]struct{}, wantNS map[string]bool, deadline time.Time) (map[string]socketOwner, bool) {
	out := make(map[string]socketOwner, len(want))
	if len(want) == 0 {
		return out, false
	}
	for i, pe := range pids {
		if i&31 == 0 && (ctx.Err() != nil || time.Now().After(deadline)) {
			return out, true
		}
		if pe.netns != "" && len(wantNS) > 0 && !wantNS[pe.netns] {
			continue
		}
		inodes, timedOut := c.pidSocketInodes(ctx, pe.pid, deadline)
		if len(inodes) > 0 {
			var matched []string
			for ino := range inodes {
				if _, ok := want[ino]; ok {
					if _, already := out[ino]; !already {
						matched = append(matched, ino)
					}
				}
			}
			if len(matched) > 0 {
				if p, ok := c.readProcess(pe.pid); ok {
					owner := socketOwner{
						pid:         p.pid,
						startTicks:  p.start,
						name:        p.name,
						uid:         p.uid,
						containerID: p.containerID,
						unit:        p.unit,
					}
					for _, ino := range matched {
						out[ino] = owner
					}
					if len(out) >= len(want) {
						return out, false
					}
				}
			}
		}
		if timedOut {
			return out, true
		}
	}
	return out, false
}

// pidSocketInodes returns the set of socket inodes open in /proc/<pid>/fd by
// reading symlink targets only (never opening or reading the file behind an fd),
// paging ReadDir in chunks of fdReadPage and checking ctx/deadline between pages.
func (c *Collector) pidSocketInodes(ctx context.Context, pid uint32, deadline time.Time) (map[string]struct{}, bool) {
	rlfs, canReadLink := c.Root.(fs.ReadLinkFS)
	if !canReadLink {
		return nil, false
	}
	fdDir := path.Join("proc", strconv.FormatUint(uint64(pid), 10), "fd")
	f, err := c.Root.Open(fdDir)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	rdf, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, false
	}
	var out map[string]struct{}
	for {
		if ctx.Err() != nil || (!deadline.IsZero() && time.Now().After(deadline)) {
			return out, true
		}
		entries, err := rdf.ReadDir(fdReadPage)
		for _, fd := range entries {
			target, linkErr := rlfs.ReadLink(path.Join(fdDir, fd.Name()))
			if linkErr != nil {
				continue
			}
			if rest, ok := strings.CutPrefix(target, "socket:["); ok {
				if ino, ok := strings.CutSuffix(rest, "]"); ok && ino != "" {
					if out == nil {
						out = map[string]struct{}{}
					}
					out[ino] = struct{}{}
				}
			}
		}
		if errors.Is(err, io.EOF) || len(entries) == 0 {
			return out, false
		}
		if err != nil {
			return out, false
		}
	}
}

func formatUID(uid uint32) string {
	if uid == 0 {
		return "root"
	}
	return strconv.FormatUint(uint64(uid), 10)
}
