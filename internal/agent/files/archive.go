// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Compressing a selection into a .zip archive on disk and extracting an
// existing .zip, .tar.gz or .tgz archive on disk (ADR 0045). Both run as
// background jobs in their own 2-slot pool (separate from the 4 listing/read
// slots and 3 transfer slots, with transfer GC tuning) so long archives never
// block browsing and outlive proxy request timeouts. Preflight checks run under
// attempt() so hung mounts time out cleanly instead of freezing the Files page.

// MaxCompressPaths is the most top-level paths one Compress or DownloadMany
// call accepts.
const MaxCompressPaths = 1000

// maxJobSourcePaths is how many resolved source paths an ArchiveJob keeps for
// display and Activity logging. SourceCount holds the full count.
const maxJobSourcePaths = 10

// archiveJob is one running or recently finished background compress/extract job.
type archiveJob struct {
	id           string
	record       string
	owner        string
	kind         dockwellv1.ArchiveJobKind
	state        dockwellv1.ArchiveJobState
	path         string
	unpackedTo   string
	sourcePaths  []string
	sourceCount  uint32
	entries      uint64
	totalEntries uint64
	bytes        uint64
	totalBytes   uint64
	cancel       context.CancelFunc
	cancelled    bool
	committing   bool
	skippedLinks uint64
	leftOut      uint64
	file         *dockwellv1.FileEntry
	wayBackIn    bool
	errMsg       string
	errReason    string
	startTime    time.Time
	finishTime   time.Time
	done         chan struct{}
}

func (j *archiveJob) proto() *dockwellv1.ArchiveJob {
	out := &dockwellv1.ArchiveJob{
		JobId:        j.id,
		Kind:         j.kind,
		State:        j.state,
		Path:         j.path,
		UnpackedTo:   j.unpackedTo,
		SourcePaths:  slices.Clone(j.sourcePaths),
		SourceCount:  j.sourceCount,
		Entries:      j.entries,
		TotalEntries: j.totalEntries,
		Bytes:        j.bytes,
		TotalBytes:   j.totalBytes,
		SkippedLinks: j.skippedLinks,
		LeftOut:      j.leftOut,
		File:         j.file,
		WayBackIn:    j.wayBackIn,
		Error:        j.errMsg,
		ErrorReason:  j.errReason,
		StartTime:    timestamppb.New(j.startTime),
	}
	if !j.finishTime.IsZero() {
		out.FinishTime = timestamppb.New(j.finishTime)
	}
	return out
}

// ArchiveKind reports the unpack format of an archive by its file name, or
// UNPACK_UNSPECIFIED when its name doesn't end in .zip, .tar.gz or .tgz.
func ArchiveKind(name string) dockwellv1.Unpack {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip") && len(lower) > len(".zip"):
		return dockwellv1.Unpack_UNPACK_ZIP
	case strings.HasSuffix(lower, ".tar.gz") && len(lower) > len(".tar.gz"):
		return dockwellv1.Unpack_UNPACK_TAR_GZ
	case strings.HasSuffix(lower, ".tgz") && len(lower) > len(".tgz"):
		return dockwellv1.Unpack_UNPACK_TAR_GZ
	default:
		return dockwellv1.Unpack_UNPACK_UNSPECIFIED
	}
}

// CheckPaths validates a list of top-level paths for Compress or DownloadMany:
// at least one, at most MaxCompressPaths, each a valid path and name, and with
// distinct base names so archive entries never collide.
func CheckPaths(paths []string) error {
	if len(paths) == 0 {
		return errors.New("choose at least one file or folder")
	}
	if len(paths) > MaxCompressPaths {
		return fmt.Errorf("that's %d items, and one archive takes %d at most. Compress a folder instead", len(paths), MaxCompressPaths)
	}
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if err := CheckPath(p); err != nil {
			return err
		}
		if err := CheckName(p); err != nil {
			return err
		}
		base := path.Base(strings.TrimRight(p, "/"))
		if _, dup := seen[base]; dup {
			return fmt.Errorf("two items are both called %s, so they can't sit at the top of one archive", base)
		}
		seen[base] = struct{}{}
	}
	return nil
}

// CheckZipArchivePath checks that archivePath is a valid path ending in .zip.
func CheckZipArchivePath(archivePath string) error {
	if err := CheckPath(archivePath); err != nil {
		return err
	}
	if err := CheckName(archivePath); err != nil {
		return err
	}
	name := path.Base(strings.TrimRight(archivePath, "/"))
	if ArchiveKind(name) != dockwellv1.Unpack_UNPACK_ZIP {
		return errors.New("an archive's name has to end in .zip, such as site.zip")
	}
	return nil
}

// refusalReason maps a FileRefusal.Kind to the API reason string used by the
// control plane.
func refusalReason(k agentv1.FileRefusal_Kind) string {
	switch k {
	case agentv1.FileRefusal_KIND_NOT_FOUND:
		return "not_found"
	case agentv1.FileRefusal_KIND_REFUSED:
		return "file_refused"
	case agentv1.FileRefusal_KIND_NOT_A_DIRECTORY:
		return "not_a_directory"
	case agentv1.FileRefusal_KIND_NOT_A_FILE:
		return "not_a_file"
	case agentv1.FileRefusal_KIND_NOT_ANSWERING:
		return "path_not_answering"
	case agentv1.FileRefusal_KIND_CHANGED:
		return "file_changed"
	case agentv1.FileRefusal_KIND_EXISTS:
		return "file_exists"
	case agentv1.FileRefusal_KIND_TOO_LARGE:
		return "too_large"
	case agentv1.FileRefusal_KIND_UNPACK_REFUSED:
		return "unpack_refused"
	case agentv1.FileRefusal_KIND_BUSY:
		return "transfers_busy"
	case agentv1.FileRefusal_KIND_NO_SPACE:
		return "disk_full"
	default:
		return "file_denied"
	}
}

func (s *Server) registerArchiveJob(j *archiveJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneArchiveJobsLocked()
	s.archiveJobs[j.id] = j
}

func (s *Server) pruneArchiveJobsLocked() {
	now := s.now()
	cutoff := now.Add(-keepArchiveJobs)
	var finished []*archiveJob
	for id, j := range s.archiveJobs {
		if j.state == dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING {
			continue
		}
		if j.finishTime.Before(cutoff) {
			delete(s.archiveJobs, id)
			continue
		}
		finished = append(finished, j)
	}
	if len(finished) >= maxKeptArchiveJobs {
		slices.SortFunc(finished, func(a, b *archiveJob) int {
			return a.finishTime.Compare(b.finishTime)
		})
		for _, j := range finished[:len(finished)-maxKeptArchiveJobs+1] {
			delete(s.archiveJobs, j.id)
		}
	}
}

func (s *Server) finishArchiveJob(j *archiveJob, mutate func()) {
	s.mu.Lock()
	mutate()
	j.finishTime = s.now().UTC()
	snap := j.proto()
	s.archiveEnded = append(s.archiveEnded, &agentv1.ArchiveJobEnded{Job: snap, Record: j.record})
	close(j.done)
	s.mu.Unlock()
	select {
	case s.archiveEndedCh <- struct{}{}:
	default:
	}
}

// archiveStage records a compress job's staging file so an agent restart can
// remove the hidden .dockwell-upload-<id> file left behind by a crashed or
// abandoned compress job.
type archiveStage struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

func (s *Server) archiveStagingDir() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "agent", "archive-staging")
}

func (s *Server) saveArchiveStage(st *archiveStage) error {
	dir := s.archiveStagingDir()
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	text, err := json.Marshal(st)
	if err != nil {
		return err
	}
	next := filepath.Join(dir, st.ID+".json.next")
	if err := os.WriteFile(next, text, 0o600); err != nil {
		return err
	}
	return os.Rename(next, filepath.Join(dir, st.ID+".json"))
}

func (s *Server) dropArchiveStage(id string) {
	if dir := s.archiveStagingDir(); dir != "" {
		_ = os.Remove(filepath.Join(dir, id+".json"))
	}
}

func (s *Server) archiveJobRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.archiveJobs[id]
	return ok && j.state == dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING
}

// SweepArchiveStaging removes stray compress staging files left behind when
// the agent restarted or a compress job was abandoned.
func (s *Server) SweepArchiveStaging(ctx context.Context) {
	dir := s.archiveStagingDir()
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				id, ok := strings.CutSuffix(e.Name(), ".json")
				if !ok || ctx.Err() != nil || !deletedID().MatchString(id) {
					continue
				}
				if s.archiveJobRunning(id) || !s.claim("archive-stage:"+id) {
					continue
				}
				s.sweepOneArchiveStage(ctx, dir, id)
				s.unclaim("archive-stage:" + id)
			}
		}
	}
	if tmp := s.tmpDir(); tmp != "" {
		if entries, err := os.ReadDir(tmp); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "dockwell-zipcd-") {
					_ = os.Remove(filepath.Join(tmp, e.Name()))
				}
			}
		}
	}
}

func (s *Server) sweepOneArchiveStage(ctx context.Context, dir, id string) {
	if s.archiveJobRunning(id) {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".json")) //nolint:gosec // id matched deletedID, inside agent data dir
	if err != nil {
		return
	}
	var st archiveStage
	if err := json.Unmarshal(raw, &st); err != nil || st.ID != id || !arriving(st.Name) {
		s.dropArchiveStage(id)
		return
	}
	w, refusal := s.guard(ctx).walk(&trail{s: s}, path.Dir(st.Path))
	switch {
	case refusal.GetKind() == agentv1.FileRefusal_KIND_NOT_ANSWERING:
		return
	case refusal == nil:
		if rerr := removeAll(w.last().fd, st.Name, st.Dev, st.Ino, 0); rerr != nil {
			s.logger.Warn("can't remove a stray archive staging file", slog.String("path", st.Path), slog.String("error", rerr.Error()))
		}
		w.close()
	}
	s.dropArchiveStage(id)
}

func (s *Server) updateArchiveJob(mutate func()) {
	s.mu.Lock()
	mutate()
	s.mu.Unlock()
}

func (j *archiveJob) ownedBy(owner, downloadOwner string) bool {
	if j.kind == dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_URL_DOWNLOAD {
		owner = downloadOwner
	}
	return subtle.ConstantTimeCompare([]byte(j.owner), []byte(owner)) == 1
}

// GetArchiveJob returns the current state of one archive job owned by owner.
func (s *Server) GetArchiveJob(id, owner, downloadOwner string) (*agentv1.GetArchiveJobResult, error) {
	if owner == "" {
		return nil, errNoOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.archiveJobs[id]
	if !ok || !j.ownedBy(owner, downloadOwner) {
		return &agentv1.GetArchiveJobResult{Refusal: &agentv1.FileRefusal{
			Kind:    agentv1.FileRefusal_KIND_NOT_FOUND,
			Message: fmt.Sprintf("there's no archive job %s on this server. It may have finished earlier", id),
		}}, nil
	}
	return &agentv1.GetArchiveJobResult{Job: j.proto()}, nil
}

// ListArchiveJobs returns active and recent archive jobs owned by owner, newest first.
func (s *Server) ListArchiveJobs(owner, downloadOwner string) (*agentv1.ListArchiveJobsResult, error) {
	if owner == "" {
		return nil, errNoOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneArchiveJobsLocked()
	var jobs []*dockwellv1.ArchiveJob
	for _, j := range s.archiveJobs {
		if !j.ownedBy(owner, downloadOwner) {
			continue
		}
		jobs = append(jobs, j.proto())
	}
	slices.SortFunc(jobs, func(a, b *dockwellv1.ArchiveJob) int {
		return b.GetStartTime().AsTime().Compare(a.GetStartTime().AsTime())
	})
	return &agentv1.ListArchiveJobsResult{Jobs: jobs}, nil
}

// WaitArchiveJob waits until job id finishes (for tests).
func (s *Server) WaitArchiveJob(ctx context.Context, id string) (*dockwellv1.ArchiveJob, error) {
	s.mu.Lock()
	j, ok := s.archiveJobs[id]
	s.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	select {
	case <-j.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return j.proto(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Extract starts a background job that unpacks an existing .zip, .tar.gz or .tgz
// archive at p into a new folder beside it, never over what's there.
// confirmedArchive and confirmedDst are the archive and destination folder paths
// a passkey confirmed, where extracting there is a way back in.
func (s *Server) Extract(ctx context.Context, p, confirmedArchive, confirmedDst, owner string) (*agentv1.ExtractArchiveResult, error) {
	return s.ExtractWithRecord(ctx, p, confirmedArchive, confirmedDst, owner, "", "")
}

// ExtractWithRecord starts a background extract job using a caller-supplied
// jobID (when valid) and audit record ID echoed back on ArchiveJobEnded.
func (s *Server) ExtractWithRecord(ctx context.Context, p, confirmedArchive, confirmedDst, owner, jobID, record string) (*agentv1.ExtractArchiveResult, error) {
	if owner == "" {
		return nil, errNoOwner
	}
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if err := CheckName(p); err != nil {
		return nil, err
	}
	as := ArchiveKind(path.Base(strings.TrimRight(p, "/")))
	if as == dockwellv1.Unpack_UNPACK_UNSPECIFIED {
		return nil, errors.New("only .zip, .tar.gz and .tgz archives can be extracted")
	}
	if !deletedID().MatchString(jobID) {
		jobID = rand.Text()
	}
	release, busyRefusal := s.tryTakeArchive()
	if busyRefusal != nil {
		return &agentv1.ExtractArchiveResult{Refusal: busyRefusal}, nil
	}

	type prepared struct {
		src     *target
		dst     *target
		archive *os.File
		ast     unix.Stat_t
		dfd     int
		dst0    unix.Stat_t
		wayIn   bool
	}
	var prep *prepared
	refusal, err := attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.FileRefusal, error) {
		g := s.guard(ctx)
		src, r := s.targetOf(t, g, p)
		if r != nil {
			return r, nil
		}
		if r := t.touch(src.where); r != nil {
			src.w.close()
			return r, nil
		}
		st, serr := src.stat()
		if serr != nil {
			where := src.where
			src.w.close()
			return failed(where, serr), nil
		}
		if _, why := g.enter(src.w.last().place, src.name, &st); why != "" {
			where := src.where
			src.w.close()
			return refused(where, why), nil
		}
		if why := checkCredentials(src.where); why != "" {
			where := src.where
			src.w.close()
			return refused(where, why), nil
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			where := src.where
			src.w.close()
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_NOT_A_FILE, Path: where,
				Message: fmt.Sprintf("%s is %s, not an archive file, so there's nothing to extract", where, kindOf(&st)),
			}, nil
		}
		if st.Nlink != 1 {
			where := src.where
			src.w.close()
			return refused(where, hardLinkedFile), nil
		}
		dstName := UnpackedName(src.name)
		dst := &target{w: src.w, name: dstName, where: joined(src.w.path(), dstName)}
		if r := t.touch(dst.where); r != nil {
			src.w.close()
			return r, nil
		}
		switch _, serr := dst.stat(); {
		case serr == nil:
			where := dst.where
			src.w.close()
			return exists(where, "nothing was extracted"), nil
		case !errors.Is(serr, unix.ENOENT):
			where := dst.where
			src.w.close()
			return failed(where, serr), nil
		}
		if why := checkCredentials(dst.where); why != "" {
			where := dst.where
			src.w.close()
			return refused(where, why), nil
		}
		wayIn := s.wayInAt(dst, nil, true)
		if wayIn && (confirmedArchive != src.where || confirmedDst != dst.where) {
			r := needsPasskey(dst.where, fmt.Sprintf("extracting %s into it", src.where))
			r.Message = fmt.Sprintf("extracting %s into %s can let someone back onto this server, so it takes your passkey, and nothing was extracted. Confirm with it, then try again", src.where, dst.where)
			src.w.close()
			return r, nil
		}
		afd, oerr := unix.Openat(src.dir(), src.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if oerr != nil {
			where := src.where
			src.w.close()
			return failed(where, oerr), nil
		}
		archive := os.NewFile(uintptr(afd), src.where)
		if !sameAs(afd, &st, unix.S_IFREG) {
			_ = archive.Close()
			where := src.where
			src.w.close()
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_CHANGED, Path: where,
				Message: where + " changed as the agent opened it. Try again",
			}, nil
		}
		mask := archiveMask(st.Mode)
		dirMode := extractDirMode(madeDirMode, false, mask, &src.w.st)
		if merr := unix.Mkdirat(src.dir(), dst.name, dirMode&0o777); merr != nil {
			_ = archive.Close()
			where := dst.where
			src.w.close()
			if errors.Is(merr, unix.EEXIST) {
				return exists(where, "nothing was extracted"), nil
			}
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
				Message: fmt.Sprintf("the agent can't make %s: %v. %s", where, merr, fixFor(merr)),
			}, nil
		}
		dfd, derr := unix.Openat(src.dir(), dst.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if derr != nil {
			_ = archive.Close()
			srcWhere, dstWhere := src.where, dst.where
			src.w.close()
			return extractRefused(srcWhere, fmt.Sprintf("the agent can't open %s: %v", dstWhere, derr)), nil
		}
		var dst0 unix.Stat_t
		if ferr := unix.Fstat(dfd, &dst0); ferr != nil || dst0.Uid != uint32(unix.Geteuid()) || !emptyDir(dfd) { //nolint:gosec // a user ID fits
			_ = unix.Close(dfd)
			_ = archive.Close()
			srcWhere, dstWhere := src.where, dst.where
			src.w.close()
			return extractRefused(srcWhere, dstWhere+" changed as the agent made it"), nil //nolint:nilerr // refusal is returned to caller
		}
		ownWithMode(dfd, &src.w.st, dirMode)
		prep = &prepared{
			src: src, dst: dst, archive: archive, ast: st,
			dfd: dfd, dst0: dst0, wayIn: wayIn,
		}
		return nil, nil
	}, func(r *agentv1.FileRefusal) *agentv1.FileRefusal {
		r.Message += ". Nothing was extracted"
		return r
	})
	if err != nil {
		release()
		return nil, err
	}
	if refusal != nil {
		release()
		return &agentv1.ExtractArchiveResult{Refusal: refusal}, nil
	}

	job := &archiveJob{
		id:         jobID,
		record:     record,
		owner:      owner,
		kind:       dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_EXTRACT,
		state:      dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING,
		path:       prep.src.where,
		unpackedTo: prep.dst.where,
		wayBackIn:  prep.wayIn,
		startTime:  s.now().UTC(),
		done:       make(chan struct{}),
	}
	s.registerArchiveJob(job)
	snap := job.proto()

	go func() { //nolint:gosec // G118: background archive job intentionally outlives the request context
		defer release()
		defer prep.src.w.close()
		defer prep.archive.Close()
		defer unix.Close(prep.dfd)

		ways := s.waysIn()
		u := &unpacker{
			s: s, ctx: context.Background(), root: prep.dfd, where: prep.dst.where,
			folder: &prep.src.w.st, dev: prep.dst0.Dev,
			ways: ways, dstWayIn: prep.wayIn, dstWritten: ways.asWritten(prep.dst.w.steps, "/"+prep.dst.name), mask: archiveMask(prep.ast.Mode),
			buf: make([]byte, FrameBytes), limit: cmp.Or(s.unpackLimit, maxUnpacked),
			statfs: s.statfs,
			onTotal: func(total uint64) {
				s.updateArchiveJob(func() { job.totalEntries = total })
			},
			onProgress: func(entries, written, skipped uint64) {
				s.updateArchiveJob(func() {
					job.entries = entries
					job.bytes = written
					job.skippedLinks = skipped
				})
			},
		}
		if u.statfs == nil {
			u.statfs = unix.Fstatfs
		}
		var uerr error
		switch as {
		case dockwellv1.Unpack_UNPACK_ZIP:
			uerr = u.zip(prep.archive, prep.ast.Size)
		case dockwellv1.Unpack_UNPACK_TAR_GZ:
			uerr = u.tarGz(prep.archive)
		default:
			uerr = errors.New("only .zip, .tar.gz and .tgz archives can be extracted")
		}
		if uerr != nil {
			// All or nothing: remove what it made by device and inode.
			msg := uerr.Error()
			if rerr := removeAll(prep.src.dir(), prep.dst.name, prep.dst0.Dev, prep.dst0.Ino, 0); rerr != nil {
				msg += ", and what it made is left in " + prep.dst.where
			}
			r := extractRefused(prep.src.where, msg)
			reason := refusalReason(r.GetKind())
			if u.full != nil {
				reason = "disk_full"
			}
			s.finishArchiveJob(job, func() {
				job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED
				job.errMsg = r.GetMessage()
				job.errReason = reason
			})
			return
		}
		s.finishArchiveJob(job, func() {
			job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE
			job.entries = u.count
			job.bytes = u.written
			job.skippedLinks = u.skippedLinks
		})
	}()

	return &agentv1.ExtractArchiveResult{Job: snap}, nil
}

func extractRefused(archive, why string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_UNPACK_REFUSED, Path: archive,
		Message: archive + " wasn't extracted: " + why + ". Extract it over SSH",
	}
}

// Compress starts a background job that packs paths into a new .zip archive at
// archivePath, never over what's there. confirmed is the archivePath a passkey
// confirmed, where making it is a way back in.
func (s *Server) Compress(ctx context.Context, paths []string, archivePath, confirmed, owner string) (*agentv1.CompressFilesResult, error) {
	return s.CompressWithRecord(ctx, paths, archivePath, confirmed, owner, "", "")
}

// CompressWithRecord starts a background compress job using a caller-supplied
// jobID (when valid) and audit record ID echoed back on ArchiveJobEnded.
func (s *Server) CompressWithRecord(ctx context.Context, paths []string, archivePath, confirmed, owner, jobID, record string) (*agentv1.CompressFilesResult, error) {
	if owner == "" {
		return nil, errNoOwner
	}
	if err := CheckPaths(paths); err != nil {
		return nil, err
	}
	if err := CheckZipArchivePath(archivePath); err != nil {
		return nil, err
	}
	if !deletedID().MatchString(jobID) {
		jobID = rand.Text()
	}
	release, busyRefusal := s.tryTakeArchive()
	if busyRefusal != nil {
		return &agentv1.CompressFilesResult{Refusal: busyRefusal}, nil
	}

	type prepared struct {
		dst         *target
		tmpName     string
		tmpFd       int
		tmpSt       unix.Stat_t
		tmpFile     *os.File
		bw          *boundedZipWriter
		archiveMode uint32
		wayIn       bool
		sources     []string
		sourceCount uint32
	}
	var prep *prepared
	refusal, err := attempt(ctx, s, archivePath, func(ctx context.Context, t *trail) (*agentv1.FileRefusal, error) {
		g := s.guard(ctx)
		dst, r := s.targetOf(t, g, archivePath)
		if r != nil {
			return r, nil
		}
		if r := t.touch(dst.where); r != nil {
			dst.w.close()
			return r, nil
		}
		switch _, serr := dst.stat(); {
		case serr == nil:
			where := dst.where
			dst.w.close()
			return exists(where, "nothing was compressed"), nil
		case !errors.Is(serr, unix.ENOENT):
			where := dst.where
			dst.w.close()
			return failed(where, serr), nil
		}
		if why := checkCredentials(dst.where); why != "" {
			where := dst.where
			dst.w.close()
			return refused(where, why), nil
		}
		wayIn := s.wayInAt(dst, nil, false)
		if wayIn && confirmed != dst.where {
			r := needsPasskey(dst.where, "compressing into it")
			r.Message = fmt.Sprintf("compressing into %s can let someone back onto this server, so it takes your passkey, and nothing was compressed. Confirm with it, then try again", dst.where)
			dst.w.close()
			return r, nil
		}

		// Preflight every top-level source path inside attempt() so missing,
		// refused, or hung-mount sources fail cleanly before the job starts.
		// Archive permissions start at 0600 and tighten if a source folder is tighter.
		archiveMode := uint32(0o600)
		resolved := make([]string, 0, min(len(paths), maxJobSourcePaths))
		var preflightBuf []byte
		for _, p := range paths {
			o, or := s.openDownload(ctx, t, p)
			if or != nil {
				dst.w.close()
				return or, nil
			}
			if len(resolved) < maxJobSourcePaths {
				resolved = append(resolved, o.where)
			}
			// Tighten archive mode if the source folder (or parent folder of a
			// selected file) has tighter owner rw bits than 0600 (e.g. 0500 -> 0400).
			var folderMode uint32
			if o.dir {
				folderMode = o.w.st.Mode
			} else if len(o.w.steps) >= 2 {
				var pst unix.Stat_t
				if unix.Fstat(o.w.steps[len(o.w.steps)-2].fd, &pst) == nil {
					folderMode = pst.Mode
				}
			}
			if folderMode != 0 {
				tighter := uint32(0o600) & folderMode
				if tighter&0o400 == 0 {
					tighter |= 0o400
				}
				archiveMode &= tighter
			}
			if !o.dir {
				if preflightBuf == nil {
					preflightBuf = make([]byte, FrameBytes)
				}
				firstLen, why, seeked, _, rerr := scanFileHead(o.f, preflightBuf)
				if rerr != nil {
					where := o.where
					o.close()
					dst.w.close()
					return denied(where, rerr), nil
				}
				if why != "" {
					where := o.where
					o.close()
					dst.w.close()
					return refused(where, why), nil
				}
				if !seeked && firstLen > 0 {
					if r := (&keyScan{}).check(o.where, preflightBuf[:firstLen]); r != nil {
						o.close()
						dst.w.close()
						return r, nil
					}
				}
			}
			o.close()
		}

		// Stage under uploadPrefix so arriving() keeps it out of listings and archives while writing.
		tmpName := uploadPrefix + rand.Text()
		tmpFd, oerr := unix.Openat(dst.dir(), tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if oerr != nil {
			where := dst.where
			dst.w.close()
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
				Message: fmt.Sprintf("the agent can't make %s: %v. %s", where, oerr, fixFor(oerr)),
			}, nil
		}
		var tmpSt unix.Stat_t
		if ferr := unix.Fstat(tmpFd, &tmpSt); ferr != nil {
			_ = unix.Close(tmpFd)
			_ = unix.Unlinkat(dst.dir(), tmpName, 0)
			where := dst.where
			dst.w.close()
			return denied(where, ferr), nil
		}
		tmpFile := os.NewFile(uintptr(tmpFd), dst.where)
		statfs := s.statfs
		if statfs == nil {
			statfs = unix.Fstatfs
		}
		bw := &boundedZipWriter{w: tmpFile, fd: tmpFd, where: dst.where, statfs: statfs}
		if rerr := bw.checkRoom(); rerr != nil {
			_ = tmpFile.Close()
			_ = removeAll(dst.dir(), tmpName, tmpSt.Dev, tmpSt.Ino, 0)
			where := dst.where
			dst.w.close()
			return &agentv1.FileRefusal{ //nolint:nilerr // refusal is returned to caller
				Kind: agentv1.FileRefusal_KIND_NO_SPACE, Path: where,
				Message: rerr.Error() + ". Free some space first",
			}, nil
		}
		if serr := s.saveArchiveStage(&archiveStage{ID: jobID, Path: dst.where, Name: tmpName, Dev: tmpSt.Dev, Ino: tmpSt.Ino}); serr != nil {
			_ = tmpFile.Close()
			_ = removeAll(dst.dir(), tmpName, tmpSt.Dev, tmpSt.Ino, 0)
			where := dst.where
			dst.w.close()
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
				Message: fmt.Sprintf("the agent can't note the archive staging file: %v. Check the free space on Dockwell's data directory", serr),
			}, nil
		}
		prep = &prepared{
			dst: dst, tmpName: tmpName, tmpFd: tmpFd, tmpSt: tmpSt, tmpFile: tmpFile,
			bw: bw, archiveMode: archiveMode, wayIn: wayIn,
			sources: resolved, sourceCount: uint32(len(paths)), //nolint:gosec // <= MaxCompressPaths
		}
		return nil, nil
	}, func(r *agentv1.FileRefusal) *agentv1.FileRefusal {
		r.Message += ". Nothing was compressed"
		return r
	})
	if err != nil {
		release()
		return nil, err
	}
	if refusal != nil {
		release()
		return &agentv1.CompressFilesResult{Refusal: refusal}, nil
	}

	pathsCopy := slices.Clone(paths)
	job := &archiveJob{
		id:          jobID,
		record:      record,
		owner:       owner,
		kind:        dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS,
		state:       dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING,
		path:        prep.dst.where,
		sourcePaths: prep.sources,
		sourceCount: prep.sourceCount,
		wayBackIn:   prep.wayIn,
		startTime:   s.now().UTC(),
		done:        make(chan struct{}),
	}
	s.registerArchiveJob(job)
	snap := job.proto()

	go func() { //nolint:gosec // G118: background archive job intentionally outlives the request context
		defer release()
		defer prep.dst.w.close()
		placed := false
		defer func() {
			_ = prep.tmpFile.Close()
			if !placed {
				_ = removeAll(prep.dst.dir(), prep.tmpName, prep.tmpSt.Dev, prep.tmpSt.Ino, 0)
			}
			s.dropArchiveStage(job.id)
		}()

		failJob := func(r *agentv1.FileRefusal) {
			s.finishArchiveJob(job, func() {
				job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED
				job.errMsg = r.GetMessage()
				job.errReason = refusalReason(r.GetKind())
			})
		}

		bgCtx := context.Background()
		zw, zerr := newZipWriter(prep.bw, s.tmpDir())
		if zerr != nil {
			failJob(compressError(prep.dst.where, prep.bw, zerr))
			return
		}
		defer zw.Abort()

		t := &trail{s: s}
		g := s.guard(bgCtx)
		a := &archiver{
			ctx:  bgCtx,
			z:    zw,
			g:    g,
			root: path.Dir(prep.dst.where),
			buf:  make([]byte, FrameBytes),
			onProgress: func(entries, leftOut uint64) {
				s.updateArchiveJob(func() {
					job.entries = entries
					job.leftOut = leftOut
					job.bytes = prep.bw.written
				})
			},
		}
		for _, p := range pathsCopy {
			r, perr := s.archivePath(bgCtx, t, a, p)
			if r != nil {
				failJob(r)
				return
			}
			if perr != nil {
				failJob(compressError(prep.dst.where, prep.bw, perr))
				return
			}
		}
		if cerr := zw.Close(); cerr != nil {
			failJob(compressError(prep.dst.where, prep.bw, cerr))
			return
		}
		if serr := unix.Fsync(prep.tmpFd); serr != nil {
			failJob(compressError(prep.dst.where, prep.bw, serr))
			return
		}
		ownWithMode(prep.tmpFd, &prep.dst.w.st, prep.archiveMode)
		if rerr := unix.Renameat2(prep.dst.dir(), prep.tmpName, prep.dst.dir(), prep.dst.name, unix.RENAME_NOREPLACE); rerr != nil {
			if errors.Is(rerr, unix.EEXIST) {
				failJob(exists(prep.dst.where, "nothing was compressed"))
				return
			}
			failJob(&agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: prep.dst.where,
				Message: fmt.Sprintf("the agent can't put %s in place: %v. %s", prep.dst.where, rerr, fixFor(rerr)),
			})
			return
		}
		var finalSt unix.Stat_t
		if ferr := unix.Fstatat(prep.dst.dir(), prep.dst.name, &finalSt, unix.AT_SYMLINK_NOFOLLOW); ferr != nil || finalSt.Dev != prep.tmpSt.Dev || finalSt.Ino != prep.tmpSt.Ino {
			_ = unix.Renameat2(prep.dst.dir(), prep.dst.name, prep.dst.dir(), prep.tmpName, unix.RENAME_NOREPLACE)
			failJob(gone(prep.dst.where))
			return
		}
		placed = true
		fe := entry(prep.dst.name, &finalSt, owners{})
		s.finishArchiveJob(job, func() {
			job.state = dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE
			job.file = fe
			job.entries = uint64(max(a.count, 0))
			job.leftOut = a.leftOut
			job.bytes = uint64(max(finalSt.Size, 0))
		})
	}()

	return &agentv1.CompressFilesResult{Job: snap}, nil
}

// boundedZipWriter writes a zip archive to disk while enforcing MaxUploadBytes
// and a free-space margin.
type boundedZipWriter struct {
	w       io.Writer
	fd      int
	where   string
	written uint64
	checked uint64
	statfs  func(fd int, st *unix.Statfs_t) error
	full    error
	tooBig  bool
}

func (b *boundedZipWriter) checkRoom() error { return b.checkRoomFor(0) }

func (b *boundedZipWriter) checkRoomFor(needed uint64) error {
	b.checked = b.written
	var st unix.Statfs_t
	if err := b.statfs(b.fd, &st); err != nil {
		return nil //nolint:nilerr // a disk that can't say goes on, as a write that fills it stops it anyway
	}
	bsize := uint64(st.Bsize) //nolint:gosec // a block size is positive
	margin := min(uint64(1)<<30, st.Blocks*bsize/20)
	if free := st.Bavail * bsize; free < margin || free-margin < needed {
		b.full = fmt.Errorf("the disk %s is on has %s free now, so it stopped short of filling it", path.Dir(b.where), sizeOf(free))
		return b.full
	}
	return nil
}

func (b *boundedZipWriter) Write(p []byte) (int, error) {
	if b.written+uint64(len(p)) > MaxUploadBytes {
		b.tooBig = true
		return 0, fmt.Errorf("that archive grew past the %s limit", sizeOf(MaxUploadBytes))
	}
	n, err := b.w.Write(p)
	b.written += uint64(max(n, 0))
	if err == nil && b.written-b.checked >= roomEvery {
		err = b.checkRoom()
	}
	return n, err
}

func compressError(where string, bw *boundedZipWriter, err error) *agentv1.FileRefusal {
	switch {
	case bw.tooBig:
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: where,
			Message: err.Error() + ". Compress fewer files, or compress them over SSH",
		}
	case bw.full != nil, errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		msg := err.Error()
		if bw.full == nil {
			msg = fmt.Sprintf("the disk %s is on filled while writing %s", path.Dir(where), where)
		}
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NO_SPACE, Path: where,
			Message: msg + ". Free some space first",
		}
	}
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
		Message: fmt.Sprintf("the agent can't write %s: %v. %s", where, err, fixFor(err)),
	}
}

// archivePath opens one top-level path p and packs it (file or directory) into a.
func (s *Server) archivePath(ctx context.Context, t *trail, a *archiver, p string) (*agentv1.FileRefusal, error) {
	o, refusal := s.openDownload(ctx, t, p)
	if refusal != nil {
		return refusal, nil
	}
	defer o.close()
	name := path.Base(strings.TrimRight(p, "/"))
	if name == "/" || name == "." || name == "" {
		name = o.w.last().name
	}
	a.root = o.where
	if o.dir {
		a.count++
		if a.count > maxArchived {
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_REFUSED, Path: o.where,
				Message: fmt.Sprintf("this archive holds more than %d files and folders, so it stopped. Archive a smaller selection, or use SSH", maxArchived),
			}, nil
		}
		if _, err := a.z.Create(name+"/", time.Unix(o.w.st.Mtim.Unix()), fs.ModeDir|fs.FileMode(o.w.st.Mode&0o777), zipStored); err != nil {
			return nil, err
		}
		a.progress()
		fd, err := unix.Openat(o.w.last().fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return denied(o.where, err), nil
		}
		defer unix.Close(fd)
		// Pass o.where as diskDir separately from archive prefix name so inner
		// credential paths are never doubled (e.g. /etc/shadow, never /etc/etc/shadow).
		return a.folder(fd, o.where, name, o.w.last().place, o.w.st.Dev, 0)
	}
	a.count++
	if a.count > maxArchived {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: o.where,
			Message: fmt.Sprintf("this archive holds more than %d files and folders, so it stopped. Archive a smaller selection, or use SSH", maxArchived),
		}, nil
	}
	firstLen, why, seeked, isBin, err := scanFileHead(o.f, a.buf)
	if err != nil {
		return denied(o.where, err), nil
	}
	if why != "" {
		return refused(o.where, why), nil
	}
	if seeked {
		if _, err := o.f.Seek(0, io.SeekStart); err != nil {
			return denied(o.where, err), nil
		}
		n, rerr := io.ReadFull(o.f, a.buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return denied(o.where, rerr), nil
		}
		firstLen = n
	}
	scan := &keyScan{}
	var dockerTOCTOU headScan
	checkDocker := seeked && !isBin
	headSeen := 0
	if firstLen > 0 {
		if refusal := scan.check(o.where, a.buf[:firstLen]); refusal != nil {
			return refusal, nil
		}
		if checkDocker {
			dockerTOCTOU.feedDocker(a.buf[:firstLen])
			headSeen += firstLen
			if dockerTOCTOU.sawDocker() {
				return refused(o.where, whyDockerCopy), nil
			}
		}
	}
	w, err := a.z.Create(name, time.Unix(o.w.st.Mtim.Unix()), fs.FileMode(o.w.st.Mode&0o777), zipDeflated)
	if err != nil {
		return nil, err
	}
	if firstLen > 0 {
		if _, err := w.Write(a.buf[:firstLen]); err != nil {
			return nil, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := o.f.Read(a.buf)
		if m > 0 {
			if refusal := scan.check(o.where, a.buf[:m]); refusal != nil {
				return refusal, nil
			}
			if checkDocker && headSeen < MaxFileBytes {
				take := min(m, MaxFileBytes-headSeen)
				dockerTOCTOU.feedDocker(a.buf[:take])
				headSeen += take
				if dockerTOCTOU.sawDocker() {
					return refused(o.where, whyDockerCopy), nil
				}
			}
			if _, werr := w.Write(a.buf[:m]); werr != nil {
				return nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			a.progress()
			return nil, nil
		}
		if err != nil {
			return denied(o.where, err), nil
		}
	}
}
