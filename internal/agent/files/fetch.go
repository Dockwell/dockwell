// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	maxURLDownload       = 1 << 30
	urlDownloadTime      = 10 * time.Minute
	maxDownloadRedirects = 5
)

// DownloadDestination validates the URL and chooses a destination name from the
// original URL when none was given. It never logs a URL with a query or path.
func DownloadDestination(folder, rawURL, name string) (string, error) {
	if err := CheckPath(folder); err != nil {
		return "", err
	}
	u, err := downloadURL(rawURL)
	if err != nil {
		return "", err
	}
	if name == "" {
		_, end := path.Split(u.EscapedPath())
		name = downloadName(end)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") ||
		Printable(name) != name || len(name) > MaxName || arriving(name) || strings.HasPrefix(name, asidePrefix) {
		return "", errors.New("enter a single valid filename for the download")
	}
	dest := path.Join(folder, name)
	return dest, CheckPath(dest)
}

func downloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || u == nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("enter an HTTP or HTTPS URL without a username, password or fragment")
	}
	return u, nil
}

// Use conventional names and extensions. Encoded names, signed path segments,
// and long opaque words fall back to a generic filename.
var (
	downloadFilename = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}(\.[A-Za-z0-9]{1,8}){1,3}$`)
	downloadNameWord = regexp.MustCompile(`[A-Za-z0-9]+`)
)

func downloadName(end string) string {
	if !downloadFilename.MatchString(end) {
		return "download"
	}
	for _, word := range downloadNameWord.FindAllString(strings.ToLower(end), -1) {
		// Long and mixed alphanumeric identifiers are common in signed paths.
		if len(word) >= 16 || len(word) >= 10 && strings.ContainsAny(word, "0123456789") {
			return "download"
		}
		switch word {
		case "token", "secret", "signature", "signed", "auth", "credential":
			return "download"
		}
	}
	return end
}

// DownloadOrigin is the URL information that can go in a download's record.
func DownloadOrigin(raw string) string {
	u, err := downloadURL(raw)
	if err != nil {
		return ""
	}
	return Printable(u.Scheme + "://" + u.Host)
}

func downloadHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func (s *Server) downloadOptions(u *url.URL, original string, local []netip.Prefix, within time.Duration) netguard.Options {
	host := downloadHost(u.Hostname())
	allowed := host == original && slices.ContainsFunc(s.DownloadHosts, func(entry string) bool {
		return host == downloadHost(entry)
	})
	options := netguard.Options{Private: allowed, Loopback: allowed, MaxBody: maxURLDownload, Timeout: within}
	if !allowed {
		options.Refuse = local
	}
	return options
}

func downloadLocalAddresses() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, errors.New("the agent can't check this server's addresses. Try the download again")
	}
	local := make([]netip.Prefix, 0, len(addrs))
	for _, addr := range addrs {
		ip, _, _ := strings.Cut(addr.String(), "/")
		if parsed, err := netip.ParseAddr(ip); err == nil {
			parsed = parsed.Unmap().WithZone("")
			local = append(local, netip.PrefixFrom(parsed, parsed.BitLen()))
		}
	}
	return local, nil
}

func (s *Server) downloadHTTP(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := downloadURL(rawURL)
	if err != nil {
		return nil, err
	}
	original := downloadHost(u.Hostname())
	local, err := downloadLocalAddresses()
	if err != nil {
		return nil, err
	}
	for hop := 0; hop <= maxDownloadRedirects; hop++ {
		deadline, _ := ctx.Deadline()
		options := s.downloadOptions(u, original, local, time.Until(deadline))
		if why := netguard.Forbidden(u.Hostname(), options); why != "" {
			return nil, fmt.Errorf("the download host is %s. Choose a public download URL", why)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
		if err != nil {
			return nil, errors.New("the download URL could not be requested. Check the URL")
		}
		// Progress and the length check count bytes on disk. Keep encoded
		// representations as served instead of automatically decompressing.
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := netguard.Client(options).Do(req)
		if err != nil {
			return nil, fmt.Errorf("the download host could not be reached: %s", netguard.Words(err))
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			next, err := resp.Location()
			_ = resp.Body.Close()
			if err != nil {
				return nil, errors.New("the download host gave an invalid redirect. Check the URL")
			}
			to, err := downloadURL(next.String())
			if err != nil || u.Scheme == "https" && to.Scheme != "https" {
				return nil, errors.New("the download host gave an unsafe redirect. Check the URL")
			}
			u = to
		case http.StatusOK:
			return resp, nil
		default:
			_ = resp.Body.Close()
			return nil, fmt.Errorf("the download host returned HTTP %d. Check the URL", resp.StatusCode)
		}
	}
	return nil, errors.New("the download host redirected too many times. Use the final URL")
}

// DownloadFileFromURL starts a guarded background job. The partial file stays
// in an agent-owned staging directory until the completed inode is published.
func (s *Server) DownloadFileFromURL(ctx context.Context, call *agentv1.DownloadFileFromURLCall) (*agentv1.DownloadFileFromURLResult, error) {
	p, rawURL := call.GetPath(), call.GetUrl()
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if checked, err := DownloadDestination(path.Dir(p), rawURL, path.Base(p)); err != nil || checked != p {
		return nil, errors.New("choose a valid download URL and destination filename")
	}
	if call.GetOwner() == "" || !deletedID().MatchString(call.GetJobId()) {
		return nil, errors.New("a download needs an owner and a job ID")
	}
	refused := func(r *agentv1.FileRefusal) *agentv1.DownloadFileFromURLResult {
		return &agentv1.DownloadFileFromURLResult{Refusal: r}
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.DownloadFileFromURLResult, error) {
		dst, r := s.downloadTarget(ctx, t, p)
		if r != nil {
			return refused(r), nil
		}
		defer dst.w.close()
		folder := dst.w.st
		digest := sha256.Sum256(fmt.Appendf(nil, "%d:%d\x00%s", folder.Dev, folder.Ino, rawURL))
		target := fmt.Sprintf("%s\n%x", dst.where, digest)
		wayIn := s.wayInAt(dst, nil, false)
		if call.GetConfirmed() != "" && call.GetConfirmed() != target {
			return refused(gone(dst.where)), nil
		}
		if wayIn && call.GetConfirmed() != target {
			return &agentv1.DownloadFileFromURLResult{Refusal: needsPasskey(dst.where, "downloading into it"), Confirmation: target}, nil
		}
		release, busy := s.tryTakeArchive()
		if busy != nil {
			return refused(busy), nil
		}
		stage, err := s.stageDownload(dst, call.GetJobId())
		if err != nil {
			release()
			return refused(&agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: dst.where,
				Message: fmt.Sprintf("the agent can't prepare a download in %s: %v. Choose another folder", path.Dir(dst.where), err),
			}), nil
		}
		if ctx.Err() != nil {
			s.removeDownloadStage(stage)
			release()
			return refused(notAnswering(dst.where)), nil
		}
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), urlDownloadTime)
		job := &archiveJob{
			id: call.GetJobId(), record: call.GetRecord(), owner: call.GetOwner(), path: dst.where,
			kind: dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_URL_DOWNLOAD, state: dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING,
			wayBackIn: wayIn, startTime: s.now().UTC(), done: make(chan struct{}), cancel: cancel,
		}
		snap := job.proto()
		s.registerArchiveJob(job)
		go func() {
			defer release()
			defer cancel()
			result, fail := s.fetchDownload(bg, job, stage, folder, dst.where, rawURL)
			s.finishArchiveJob(job, func() {
				switch {
				case result != nil:
					job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE
					job.file = result
				case job.cancelled:
					job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_CANCELLED
				default:
					job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED
					job.errMsg, job.errReason = fail.GetMessage(), refusalReason(fail.GetKind())
				}
			})
		}()
		return &agentv1.DownloadFileFromURLResult{Job: snap}, nil
	}, refused)
}

type downloadStage struct {
	archiveStage
	parent, dir int
	file        *os.File
}

const downloadStageFile = "partial"

// stageDownload uses a private directory beside the file, including on
// filesystems without O_TMPFILE. Its ownership and mode are checked on its
// descriptor. Callers publish the open file itself and never trust its name.
func (s *Server) stageDownload(dst *target, id string) (*downloadStage, error) {
	parent, err := unix.FcntlInt(uintptr(dst.dir()), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	stage := &downloadStage{
		archiveStage: archiveStage{ID: id, Path: dst.where, Name: uploadPrefix + rand.Text()},
		parent:       parent, dir: -1,
	}
	if !s.claim("archive-stage:" + id) {
		_ = unix.Close(parent)
		return nil, errors.New("a staging directory already exists for this download")
	}
	if err := stage.open(); err != nil {
		s.removeDownloadStage(stage)
		return nil, err
	}
	s.mu.Lock()
	s.downloadStages[identity{stage.Dev, stage.Ino}] = true
	s.mu.Unlock()
	if err := s.saveArchiveStage(&stage.archiveStage); err != nil {
		s.removeDownloadStage(stage)
		return nil, err
	}
	return stage, nil
}

func (stage *downloadStage) open() error {
	if err := unix.Mkdirat(stage.parent, stage.Name, 0o700); err != nil {
		return err
	}
	dir, err := unix.Openat(stage.parent, stage.Name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	stage.dir = dir
	var st unix.Stat_t
	if err := unix.Fstat(dir, &st); err != nil {
		return err
	}
	if st.Uid != uint32(unix.Geteuid()) || st.Mode&0o777 != 0o700 { //nolint:gosec // the current effective UID
		return errors.New("the staging directory changed before it could be opened")
	}
	stage.Dev, stage.Ino = st.Dev, st.Ino
	fd, err := unix.Openat(dir, downloadStageFile, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	stage.file = os.NewFile(uintptr(fd), stage.Path)
	return nil
}

func (s *Server) removeDownloadStage(stage *downloadStage) {
	if stage.file != nil {
		_ = stage.file.Close()
		_ = unix.Unlinkat(stage.dir, downloadStageFile, 0)
	}
	if stage.dir >= 0 {
		_ = unix.Close(stage.dir)
	}
	if stage.Ino != 0 {
		var current unix.Stat_t
		err := unix.Fstatat(stage.parent, stage.Name, &current, unix.AT_SYMLINK_NOFOLLOW)
		id := identity{stage.Dev, stage.Ino}
		if err == nil && identify(&current) == id {
			err = unix.Unlinkat(stage.parent, stage.Name, unix.AT_REMOVEDIR)
		}
		if err == nil || errors.Is(err, unix.ENOENT) {
			s.dropArchiveStage(stage.ID)
		}
		s.mu.Lock()
		delete(s.downloadStages, id)
		s.mu.Unlock()
	}
	_ = unix.Close(stage.parent)
	s.unclaim("archive-stage:" + stage.ID)
}

func (s *Server) downloadTarget(ctx context.Context, t *trail, p string) (*target, *agentv1.FileRefusal) {
	dst, r := s.targetOf(t, s.guard(ctx), p)
	if r != nil {
		return nil, r
	}
	if why := credentials(dst.where); why != "" {
		dst.w.close()
		return nil, refused(dst.where, why)
	}
	if slices.ContainsFunc(dst.w.steps, func(step step) bool { return Printable(step.name) != step.name }) {
		dst.w.close()
		return nil, refused(dst.where, "a path Dockwell cannot change")
	}
	if r := t.touch(dst.where); r != nil {
		dst.w.close()
		return nil, r
	}
	_, err := dst.stat()
	switch {
	case err == nil:
		dst.w.close()
		return nil, exists(dst.where, "the download could not be put in place")
	case !errors.Is(err, unix.ENOENT):
		dst.w.close()
		return nil, failed(dst.where, err)
	}
	return dst, nil
}

type downloadOutcome struct {
	file    *dockwellv1.FileEntry
	refusal *agentv1.FileRefusal
}

func (s *Server) fetchDownload(ctx context.Context, job *archiveJob, stage *downloadStage, folder unix.Stat_t, dest, rawURL string) (*dockwellv1.FileEntry, *agentv1.FileRefusal) {
	room := &boundedZipWriter{w: stage.file, fd: int(stage.file.Fd()), where: dest, statfs: s.statfs}
	if room.statfs == nil {
		room.statfs = unix.Fstatfs
	}
	fail := s.streamDownload(ctx, job, room, rawURL)
	var finished atomic.Pointer[downloadOutcome]
	// This download already owns the staging descriptors. The worker must take
	// them even if the job was cancelled or another path has stopped answering.
	out, _ := attemptOwned(context.WithoutCancel(ctx), s, dest, func(bound context.Context, t *trail) (downloadOutcome, error) {
		defer s.removeDownloadStage(stage)
		result := downloadOutcome{refusal: fail}
		if fail == nil {
			result.file, result.refusal = s.publishDownload(ctx, bound, t, job, stage.file, folder, dest, room)
		}
		finished.Store(&result)
		return result, nil
	}, func(r *agentv1.FileRefusal) downloadOutcome {
		if result := finished.Load(); result != nil {
			return *result
		}
		s.mu.Lock()
		committing := job.committing
		s.mu.Unlock()
		if committing {
			r.Message += ". The download may still be put in place when the folder answers"
		}
		return downloadOutcome{refusal: r}
	})
	return out.file, out.refusal
}

func (s *Server) streamDownload(ctx context.Context, job *archiveJob, room *boundedZipWriter, rawURL string) *agentv1.FileRefusal {
	dest := room.where
	deny := func(msg string) *agentv1.FileRefusal {
		return &agentv1.FileRefusal{Kind: agentv1.FileRefusal_KIND_DENIED, Path: dest, Message: msg}
	}
	tooLarge := &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: dest,
		Message: "the download exceeds 1 GB. Choose a smaller file",
	}
	if err := room.checkRoom(); err != nil {
		return compressError(dest, room, err)
	}
	resp, err := s.downloadHTTP(ctx, rawURL)
	if err != nil {
		return deny(err.Error())
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxURLDownload {
		return tooLarge
	}
	if resp.ContentLength > 0 {
		if err := room.checkRoomFor(uint64(resp.ContentLength)); err != nil {
			return compressError(dest, room, err)
		}
		s.updateArchiveJob(func() { job.totalBytes = uint64(resp.ContentLength) }) //nolint:gosec // checked positive
	}
	buf := make([]byte, 64<<10)
	for {
		if ctx.Err() != nil {
			return deny("the download stopped before the file was complete. Try again")
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := room.Write(buf[:n]); err != nil {
				return compressError(dest, room, err)
			}
			s.updateArchiveJob(func() { job.bytes = room.written })
		}
		if errors.Is(rerr, netguard.ErrTooLarge) {
			return tooLarge
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return deny("the download ended before the file was complete. Check the URL and try again")
		}
	}
	return nil
}

func (s *Server) publishDownload(ctx, bound context.Context, t *trail, job *archiveJob, file *os.File, folder unix.Stat_t, dest string, room *boundedZipWriter) (*dockwellv1.FileEntry, *agentv1.FileRefusal) {
	if r := t.touch(dest); r != nil {
		return nil, r
	}
	if ctx.Err() != nil || bound.Err() != nil {
		return nil, notAnswering(dest)
	}
	fd := int(file.Fd())
	if unix.Geteuid() == 0 {
		if err := unix.Fchown(fd, int(folder.Uid), int(folder.Gid)); err != nil {
			return nil, accessError(dest, err)
		}
	}
	if err := unix.Fchmod(fd, madeFileMode); err != nil {
		return nil, accessError(dest, err)
	}
	if err := file.Sync(); err != nil {
		return nil, compressError(dest, room, err)
	}
	st := unix.Stat_t{}
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, denied(dest, err)
	}
	dst, r := s.downloadTarget(bound, t, dest)
	if r != nil {
		return nil, r
	}
	defer dst.w.close()
	if dst.where != dest || identify(&dst.w.st) != identify(&folder) || dst.w.st.Uid != folder.Uid || dst.w.st.Gid != folder.Gid ||
		s.wayInAt(dst, nil, false) && !job.wayBackIn {
		return nil, gone(dest)
	}
	// Past this point cancel cannot win, and publication is a single atomic
	// link of the checked inode that fails when the destination exists.
	s.mu.Lock()
	canCommit := ctx.Err() == nil && bound.Err() == nil && !job.cancelled
	job.committing = canCommit
	s.mu.Unlock()
	if !canCommit {
		return nil, notAnswering(dest)
	}
	if err := unix.Linkat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(fd), dst.dir(), dst.name, unix.AT_SYMLINK_FOLLOW); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, exists(dest, "the download could not be put in place")
		}
		return nil, compressError(dest, room, err)
	}
	return entry(dst.name, &st, owners{}), nil
}

// CancelFileDownload stops a running download owned by the calling session.
func (s *Server) CancelFileDownload(id, owner string) (*agentv1.CancelFileDownloadResult, error) {
	if owner == "" {
		return nil, errNoOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.archiveJobs[id]
	if j == nil || j.kind != dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_URL_DOWNLOAD ||
		subtle.ConstantTimeCompare([]byte(j.owner), []byte(owner)) != 1 {
		return &agentv1.CancelFileDownloadResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_FOUND, Message: "there's no download job for this session",
		}}, nil
	}
	if j.state == dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING && !j.committing {
		j.cancelled = true
		j.cancel()
	}
	return &agentv1.CancelFileDownloadResult{Job: j.proto()}, nil
}
