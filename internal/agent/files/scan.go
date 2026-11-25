// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/mounts"
)

const (
	scanDepth     = 24
	scanEntries   = 20_000
	scanTime      = 4 * time.Second
	searchBytes   = 32 << 20
	searchResults = 200
)

const (
	sizeDocker    = dockwellv1.FolderSizeOmission_FOLDER_SIZE_OMISSION_DOCKER_DATA
	sizeHardLinks = dockwellv1.FolderSizeOmission_FOLDER_SIZE_OMISSION_HARD_LINKS
	sizeRefused   = dockwellv1.FolderSizeOmission_FOLDER_SIZE_OMISSION_REFUSED_ENTRIES
	sizeLimit     = dockwellv1.FolderSizeOmission_FOLDER_SIZE_OMISSION_SCAN_LIMIT
)

type treeScan struct {
	ctx        context.Context
	trail      *trail
	guard      *guard
	mounts     map[string]bool
	entries    int
	skipped    uint64
	limited    bool
	incomplete bool
	access     bool
	omissions  *atomic.Uint32
	hardLinks  map[identity]bool
}

func (s *Server) tree(ctx context.Context, t *trail, g *guard) *treeScan {
	mounted := map[string]bool{}
	for line := range strings.Lines(s.mounts()) {
		fields := strings.Fields(line)
		if len(fields) >= 5 {
			mounted[mounts.Unescape(fields[4])] = true
		}
	}
	return &treeScan{ctx: ctx, trail: t, guard: g, mounts: mounted}
}

func (s *treeScan) omit(reason dockwellv1.FolderSizeOmission) {
	if s.omissions != nil {
		s.omissions.Or(1 << reason)
	}
}

// below walks child descriptors, without following links or crossing a mount.
// Children are visited before their parent so a chmod can remove traversal bits.
func (s *treeScan) below(w *walked, depth int, visit func(*walked) bool) bool {
	where, at := w.path(), w.last()
	if s.ctx.Err() != nil {
		s.limited = true
		s.omit(sizeLimit)
		return false
	}
	if depth >= scanDepth {
		s.limited = true
		s.omit(sizeLimit)
		s.skipped++
		return true
	}
	if s.trail.touch(where) != nil {
		s.skipped++
		s.incomplete = true
		s.omit(sizeRefused)
		return true
	}
	fd, err := unix.Openat(at.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		s.skipped++
		s.incomplete = true
		s.omit(sizeRefused)
		return true
	}
	dir := os.NewFile(uintptr(fd), where)
	defer dir.Close()
	for {
		names, err := dir.Readdirnames(128)
		for _, name := range names {
			if s.entries >= scanEntries || s.ctx.Err() != nil {
				s.limited = true
				s.omit(sizeLimit)
				return false
			}
			s.entries++
			if !s.child(w, where, name, depth, visit) {
				return false
			}
		}
		if err != nil {
			if err != io.EOF {
				s.skipped++
				s.incomplete = true
				s.omit(sizeRefused)
			}
			return true
		}
	}
}

func (s *treeScan) child(w *walked, where, name string, depth int, visit func(*walked) bool) bool {
	child := joined(where, name)
	if arriving(name) || strings.HasPrefix(name, asidePrefix) {
		s.omit(sizeRefused)
		return true
	}
	if CheckPath(child) != nil || Printable(name) != name || !s.access && credentials(child) != "" {
		s.skipped++
		s.omit(sizeRefused)
		return true
	}
	if s.mounts[child] || s.trail.touch(child) != nil {
		s.skipped++
		s.incomplete = true
		s.omit(sizeRefused)
		return true
	}
	at := w.last()
	fd, err := unix.Openat2(at.fd, name, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_XDEV,
	})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		fd, err = unix.Openat(at.fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == nil {
			mount := descriptorMount(fd)
			if mount == "" || mount != descriptorMount(at.fd) {
				_ = unix.Close(fd)
				err = unix.EXDEV
			}
		}
	}
	if err != nil {
		s.skipped++
		s.incomplete = true
		s.omit(sizeRefused)
		return true
	}
	var st unix.Stat_t
	parent := w.st
	if err := unix.Fstat(fd, &st); err != nil || st.Dev != parent.Dev {
		_ = unix.Close(fd)
		s.skipped++
		s.incomplete = true
		s.omit(sizeRefused)
		return true
	}
	typ := st.Mode & unix.S_IFMT
	place, why := s.guard.enter(at.place, name, &st)
	if why != "" || typ != unix.S_IFDIR && typ != unix.S_IFREG {
		_ = unix.Close(fd)
		s.skipped++
		if why == whyDocker || why == whyVolumeOpts {
			s.omit(sizeDocker)
		} else {
			s.omit(sizeRefused)
		}
		return true
	}
	if typ == unix.S_IFREG && st.Nlink != 1 {
		id := identify(&st)
		if s.hardLinks == nil || s.hardLinks[id] {
			_ = unix.Close(fd)
			s.skipped++
			s.omit(sizeHardLinks)
			return true
		}
		s.hardLinks[id] = true
		s.omit(sizeHardLinks)
	}
	if place == free && typ == unix.S_IFDIR && dockerShaped(fd) {
		place = dockerRoot
	}
	w.steps = append(w.steps, step{fd: fd, name: name, place: place, id: identify(&st)})
	w.st = st
	defer func() {
		w.up()
		w.st = parent
	}()
	if isDir(&st) && !s.below(w, depth+1, visit) {
		return false
	}
	return visit(w)
}

// Kernels before openat2 expose a descriptor's mount ID in procfs.
func descriptorMount(fd int) string {
	info, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return ""
	}
	for line := range bytes.Lines(info) {
		if value, ok := bytes.CutPrefix(line, []byte("mnt_id:")); ok {
			return string(bytes.TrimSpace(value))
		}
	}
	return ""
}

func directory(g *guard, t *trail, p string) (*walked, *agentv1.FileRefusal) {
	w, r := g.walk(t, p)
	if r == nil && !isDir(&w.st) {
		r = &agentv1.FileRefusal{Kind: agentv1.FileRefusal_KIND_NOT_A_DIRECTORY, Path: w.path(), Message: w.path() + " is not a folder. Choose a folder"}
		w.close()
		return nil, r
	}
	return w, r
}

// CalculateFolderSize counts logical file bytes and retains the lower bound
// even if a mount stops answering during the scan.
func (s *Server) CalculateFolderSize(ctx context.Context, p string) (*agentv1.CalculateFolderSizeResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, scanTime)
	defer cancel()
	var size atomic.Uint64
	var opened atomic.Bool
	var omitted atomic.Uint32
	result := func() *agentv1.CalculateFolderSizeResult {
		bits := omitted.Load()
		out := &dockwellv1.CalculateFolderSizeResponse{SizeBytes: size.Load(), Partial: bits != 0}
		for _, reason := range []dockwellv1.FolderSizeOmission{sizeDocker, sizeHardLinks, sizeRefused, sizeLimit} {
			if bits&(1<<reason) != 0 {
				out.Omissions = append(out.Omissions, reason)
			}
		}
		return &agentv1.CalculateFolderSizeResult{Response: out}
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.CalculateFolderSizeResult, error) {
		g := s.guard(ctx)
		w, r := directory(g, t, p)
		if r != nil {
			return &agentv1.CalculateFolderSizeResult{Refusal: r}, nil
		}
		defer w.close()
		opened.Store(true)
		scan := s.tree(ctx, t, g)
		scan.omissions = &omitted
		scan.hardLinks = map[identity]bool{}
		scan.below(w, 0, func(w *walked) bool {
			if !isDir(&w.st) {
				current := size.Load()
				size.Store(current + min(^uint64(0)-current, uint64(max(0, w.st.Size))))
			}
			return true
		})
		return result(), nil
	}, func(r *agentv1.FileRefusal) *agentv1.CalculateFolderSizeResult {
		if opened.Load() {
			omitted.Or(1 << sizeLimit)
			return result()
		}
		return &agentv1.CalculateFolderSizeResult{Refusal: r}
	})
}

// CheckSearchQuery keeps short, single-line, plain-text search terms.
func CheckSearchQuery(q string) error {
	if strings.TrimSpace(q) == "" || len(q) > 256 || strings.ContainsAny(q, "\r\n\x00") {
		return errors.New("enter a search term of up to 256 bytes on one line")
	}
	return nil
}

// SearchFiles returns paths and line numbers, with no file contents or snippets.
func (s *Server) SearchFiles(ctx context.Context, p, query string, text bool) (*agentv1.SearchFilesResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if err := CheckSearchQuery(query); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, scanTime)
	defer cancel()
	match, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
	if err != nil {
		return nil, err
	}
	var n atomic.Int32
	var opened atomic.Bool
	// A single writer publishes complete immutable results by incrementing n.
	var matches [searchResults]*dockwellv1.FileSearchMatch
	result := func(partial bool) *agentv1.SearchFilesResult {
		length := n.Load()
		return &agentv1.SearchFilesResult{Response: &dockwellv1.SearchFilesResponse{Matches: matches[:length:length], Partial: partial}}
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.SearchFilesResult, error) {
		g := s.guard(ctx)
		w, r := directory(g, t, p)
		if r != nil {
			return &agentv1.SearchFilesResult{Refusal: r}, nil
		}
		defer w.close()
		opened.Store(true)
		scan := s.tree(ctx, t, g)
		budget := searchBytes
		var scratch []byte
		scan.below(w, 0, func(w *walked) bool {
			nameMatch := match.MatchString(w.last().name)
			line := uint32(0)
			if !isDir(&w.st) && (nameMatch || text) {
				// Reserve one byte for detecting files that grew after fstat.
				if budget <= MaxFileBytes {
					scan.limited = true
					return false
				}
				if scratch == nil {
					scratch = make([]byte, MaxFileBytes+1)
				}
				data, truncated, refusal := readBytesInto(t, w, MaxFileBytes, scratch)
				if refusal != nil {
					budget -= MaxFileBytes + 1
					return true
				}
				budget -= len(data) + 1
				if heldBack(data) != "" {
					return true
				}
				if text && truncated {
					data = trimRune(data)
					scan.limited = true
				}
				if text && !isBinary(data) {
					candidate := uint32(0)
					for content := range bytes.Lines(data) {
						candidate++
						if match.Match(bytes.TrimSuffix(content, []byte{'\n'})) {
							line = candidate
							break
						}
					}
				}
			}
			if nameMatch || line != 0 {
				i := n.Load()
				if i == searchResults {
					scan.limited = true
					return false
				}
				matches[i] = &dockwellv1.FileSearchMatch{Path: w.path(), Type: typeOf(w.st.Mode), Line: line}
				n.Store(i + 1)
			}
			return true
		})
		return result(scan.limited || scan.incomplete), nil
	}, func(r *agentv1.FileRefusal) *agentv1.SearchFilesResult {
		if opened.Load() {
			return result(true)
		}
		return &agentv1.SearchFilesResult{Refusal: r}
	})
}
