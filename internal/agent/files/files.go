// SPDX-License-Identifier: AGPL-3.0-only

// Package files is what an agent reads of its server's disk for a person, for
// FileService. It runs as root, so it decides here what's refused, on what it
// actually opens: see docs/adr/0032-files-on-a-server.md and
// docs/security/files.md. A refusal is an answer, not a failure, so these calls
// return one rather than an error.
package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/mounts"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	// MaxFileBytes is the most of a file that a read returns.
	MaxFileBytes = 1 << 20
	// MaxEntries is the most entries a listing returns.
	MaxEntries = 5000
	// maxListBytes is the most a listing's names and link targets come to, as
	// each target can be 4 KiB, and an oversized message would end the stream.
	maxListBytes = 4 << 20
	// entryBytes is about what an entry costs past those.
	entryBytes = 64
	// slots is how many of these calls run at once. Each can hold a file in
	// memory, and they share the agent's stream.
	slots = 4
)

// Server reads a server's files.
type Server struct {
	// AnswerWithin is how long a path has to answer. Zero means answerWithin.
	AnswerWithin time.Duration
	// Touching hears of each path a call is about to touch, on the goroutine that
	// touches it. A test blocks in it to stand in for a mount that stopped
	// answering, which is otherwise hard to come by. Nil hears nothing.
	Touching func(path string)
	// Writing hears of each save about to write its file, past where it can still
	// stop, as Touching does. Nil hears nothing.
	Writing func(path string)

	dataDir    string
	logger     *slog.Logger
	dockerRoot func(context.Context) (string, error)
	slots      chan struct{}

	// mu is never held across anything that touches a disk.
	mu    sync.Mutex
	roots []string // where Docker has said it keeps its data, oldest first
	// stuck holds where each call left behind stopped: see stuck.go.
	stuck map[*trail]string
	// savesEnded holds saves that ended once nobody was waiting for them.
	savesEnded chan *agentv1.SaveEnded
	// mounts reads what's mounted, as /proc/self/mountinfo has it.
	mounts func() string
	// systemPlaces, listedFiles and accounts are what a way in goes by: see wayin.go.
	systemPlaces func() []string
	listedFiles  []string
	accounts     func() (passwd, group string)
	// homeRoot is where people's homes are, as the rule for logins has it, which a
	// server can have lead elsewhere.
	homeRoot string
	// saving keeps saves of roots to one at a time.
	saving sync.Mutex
	// claimed holds the entries of Recently deleted a call has. It's mu that guards it.
	claimed map[string]bool
	// pruneNow asks for what's deleted for good to go now (KeepDeleted).
	pruneNow chan struct{}
	// now is the time, which a test can move on.
	now func() time.Time
}

// New returns a Server. dataDir is Dockwell's data directory, and dockerRoot says
// where Docker keeps its data, or fails while Docker isn't running.
func New(dataDir string, logger *slog.Logger, dockerRoot func(context.Context) (string, error)) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		dataDir: dataDir, logger: logger, dockerRoot: dockerRoot, slots: make(chan struct{}, slots),
		stuck: map[*trail]string{}, mounts: mounts.Read, savesEnded: make(chan *agentv1.SaveEnded, maxStuck),
		systemPlaces: resource.SystemPlaces, listedFiles: listedFiles, accounts: readAccounts, homeRoot: "/home",
		claimed: map[string]bool{}, pruneNow: make(chan struct{}, 1), now: time.Now,
	}
	s.roots = s.loadRoots()
	return s
}

// Take waits for one of the places these calls run in, and returns how to give it
// back. The caller keeps it until its result has gone, as what waits to send holds
// a listing in memory, and as many as a slow stream lets pile up otherwise.
func (s *Server) Take(ctx context.Context) (func(), error) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("the agent is busy reading other files: %w", ctx.Err())
	}
}

// CheckPath refuses a path that isn't absolute, or that could read otherwise
// than it looks: the kernel follows . and .. through symlinks, as ADR 0021 says.
// The control plane checks this first, and the agent again, as what a server
// does shouldn't rest on its caller.
func CheckPath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q isn't an absolute path. Start it with /, such as /var/www", p)
	case len(p) > maxPath:
		return fmt.Errorf("that path is over %d bytes, longer than a server takes", maxPath)
	case strings.IndexFunc(p, unicode.IsControl) >= 0:
		return errors.New("that path has a control character in it. Write it without one")
	}
	for _, part := range parts(p) {
		if part == "." || part == ".." {
			return fmt.Errorf("%q has %s in it, which a server reads through symlinks, so it may not lead where it looks. Write the path without it", p, part)
		}
	}
	return nil
}

// maxPrintable bounds what Printable keeps: a path, and a sentence around it.
const maxPrintable = 2 * maxPath

// Printable makes text that holds a path fit to store or show. A file's name can
// hold anything but a slash, so a link can lead through a newline, a terminal's
// escape codes, or what turns text around. Each of those becomes a mark that shows
// something was there, and what isn't UTF-8 does too.
func Printable(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return utf8.RuneError
		}
		return r
	}, strings.ToValidUTF8(text, string(utf8.RuneError)))
	if len(text) > maxPrintable {
		return strings.ToValidUTF8(text[:maxPrintable], "") + "…"
	}
	return text
}

// List returns what the directory at p holds.
func (s *Server) List(ctx context.Context, p string) (*agentv1.ListFilesResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.ListFilesResult, error) {
		return s.list(ctx, t, p)
	}, func(r *agentv1.FileRefusal) *agentv1.ListFilesResult { return &agentv1.ListFilesResult{Refusal: r} })
}

func (s *Server) list(ctx context.Context, t *trail, p string) (*agentv1.ListFilesResult, error) {
	g := s.guard(ctx)
	w, refusal := g.walk(t, p)
	if refusal != nil {
		return &agentv1.ListFilesResult{Refusal: refusal}, nil
	}
	defer w.close()
	where := w.path()
	if w.st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return &agentv1.ListFilesResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_DIRECTORY, Path: where,
			Message: where + " isn't a directory, so it has nothing to list. Read it as a file instead",
		}}, nil
	}
	if refusal := t.touch(where); refusal != nil {
		return &agentv1.ListFilesResult{Refusal: refusal}, nil
	}
	fd, err := unix.Openat(w.last().fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &agentv1.ListFilesResult{Refusal: denied(where, err)}, nil
	}
	dir := os.NewFile(uintptr(fd), where)
	defer dir.Close()
	names, err := dir.Readdirnames(MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return &agentv1.ListFilesResult{Refusal: denied(where, err)}, nil
	}
	truncated := len(names) > MaxEntries
	if truncated {
		names = names[:MaxEntries]
	}
	slices.Sort(names)
	owners := owners{}
	here := &dockwellv1.ListFilesResponse{Path: where, Directory: entry(path.Base(where), &w.st, owners), Truncated: truncated}
	room := maxListBytes
	for i, name := range names {
		if i%256 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e := &dockwellv1.FileEntry{Name: strings.ToValidUTF8(name, "\uFFFD"), UnreadableName: !utf8.ValidString(name)}
		// What's known not to answer is listed by name alone, so the folder around
		// a dead mount, or a link into one, still lists.
		if t.touch(joined(where, name)) == nil {
			var st unix.Stat_t
			if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
				continue // it went as we read
			}
			e = entry(name, &st, owners)
			if st.Mode&unix.S_IFMT == unix.S_IFLNK {
				followLink(e, fd, name)
			}
			_, why := g.enter(w.last().place, name, &st)
			e.Refused = why != "" || credentials(joined(where, name)) != ""
		}
		if room -= entryBytes + len(e.GetName()) + len(e.GetLinkTarget()); room < 0 {
			here.Truncated = true
			break
		}
		here.Entries = append(here.Entries, e)
	}
	return &agentv1.ListFilesResult{Listing: here}, nil
}

// Read returns the file at p, as text up to MaxFileBytes.
func (s *Server) Read(ctx context.Context, p string) (*agentv1.ReadFileResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.ReadFileResult, error) {
		return s.read(ctx, t, p), nil
	}, func(r *agentv1.FileRefusal) *agentv1.ReadFileResult { return &agentv1.ReadFileResult{Refusal: r} })
}

func (s *Server) read(ctx context.Context, t *trail, p string) *agentv1.ReadFileResult {
	w, refusal := s.guard(ctx).walk(t, p)
	if refusal != nil {
		return &agentv1.ReadFileResult{Refusal: refusal}
	}
	defer w.close()
	where := w.path()
	if w.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &agentv1.ReadFileResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_FILE, Path: where,
			Message: fmt.Sprintf("%s is %s, not a file, so there's nothing to read", where, kindOf(&w.st)),
		}}
	}
	if why := credentials(where); why != "" {
		return &agentv1.ReadFileResult{Refusal: refused(where, why)}
	}
	text, truncated, refusal := readText(t, w)
	if refusal != nil {
		return &agentv1.ReadFileResult{Refusal: refusal}
	}
	out := &dockwellv1.ReadFileResponse{Path: where, File: entry(w.last().name, &w.st, owners{}), Truncated: truncated, WayBackIn: s.wayIn(w)}
	if isBinary(text) {
		out.Binary, out.Truncated = true, false
		return &agentv1.ReadFileResult{File: out}
	}
	if why := whatItHolds(text); why != "" {
		return &agentv1.ReadFileResult{Refusal: refused(where, why)}
	}
	out.Content = string(text)
	if !truncated {
		out.Version = version(where, &w.st, text)
	}
	return &agentv1.ReadFileResult{File: out}
}

// version is what tells whether a file is as it was read: where a walk ended, which
// file that was, and what it held. A time and a size can't tell, as two saves can
// share both. Its text alone can't either: a link swapped in, to a file that holds
// the same text, would read as the file a person opened, and a save would follow it.
func version(where string, st *unix.Stat_t, text []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00", where, st.Dev, st.Ino)
	h.Write(text)
	return hex.EncodeToString(h.Sum(nil))
}

// readText opens the walk's last part for reading, through the descriptor of the
// directory that holds it, and reads its first MaxFileBytes. A file only
// opens as what the walk checked: a FIFO swapped in would hang a read, and
// O_NONBLOCK and the check below keep it from being read at all.
func readText(t *trail, w *walked) ([]byte, bool, *agentv1.FileRefusal) {
	where, parent, name := w.path(), w.steps[len(w.steps)-2].fd, w.last().name
	if refusal := t.touch(where); refusal != nil {
		return nil, false, refusal
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, failed(where, err)
	}
	f := os.NewFile(uintptr(fd), where)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, false, denied(where, err)
	}
	if identify(&st) != identify(&w.st) || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false, &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_FOUND, Path: where,
			Message: where + " changed as the agent opened it. Open it again",
		}
	}
	w.st = st
	text, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, false, denied(where, err)
	}
	if len(text) > MaxFileBytes {
		return trimRune(text[:MaxFileBytes]), true, nil
	}
	return text, false, nil
}

// trimRune cuts off a character the read limit split, so the rest is still text.
func trimRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if r := b[len(b)-i]; utf8.RuneStart(r) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			break
		}
	}
	return b
}

// isBinary reports what isn't text to show: a NUL, or anything that isn't UTF-8.
func isBinary(text []byte) bool {
	return bytes.IndexByte(text, 0) >= 0 || !utf8.Valid(text)
}

// whatItHolds says what a text is when it holds something that goes by what it
// holds, whatever the file is called, or nothing.
func whatItHolds(text []byte) string {
	switch {
	case holdsPrivateKey(text):
		return whyPrivateKey
	case hashLine.Match(text):
		return whyHashes
	case holdsDockerState(text):
		return whyDockerCopy
	}
	return ""
}

// beforeKey is what can come before a key on its line: an indent, as in YAML, and
// the byte order mark at the start of a file.
const beforeKey = " \t\ufeff"

// holdsPrivateKey finds a PEM private key on a line of its own, indented or not,
// which is how every tool writes one, and which a document that only mentions one
// isn't.
func holdsPrivateKey(text []byte) bool {
	for line := range bytes.Lines(text) {
		line = bytes.TrimRight(bytes.TrimLeft(line, beforeKey), " \t\r\n")
		if bytes.HasPrefix(line, []byte("-----BEGIN ")) && bytes.HasSuffix(line, []byte("PRIVATE KEY-----")) {
			return true
		}
	}
	return false
}

// hashLine is a line of a password file, as /etc/shadow and its copies have it,
// and .htpasswd: a name, then a hash in crypt's $id$ form, locked or not.
var hashLine = regexp.MustCompile(`(?m)^[^:\s#]+:[!*]*\$[0-9a-z-]{1,16}\$\S`)

// credentials says what a path holds when it is one of the server's own
// credentials by its name, or nothing, for what a listing can mark. A name proves
// little, so what's read goes by what it holds too (whatItHolds).
func credentials(p string) string {
	switch p {
	case "/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-":
		return whyHashes
	}
	switch path.Base(p) {
	case ".pgpass":
		return "a list of database passwords"
	case "config.v2.json", "hostconfig.json":
		return whyDocker // Docker's own names, and no one else's
	case "opts.json":
		if path.Base(path.Dir(path.Dir(p))) == "volumes" {
			return whyVolumeOpts
		}
	}
	return ""
}

// owners finds users' and groups' names once each for a call.
type owners map[string]string

func (o owners) name(key string, lookup func() (string, error), id uint32) string {
	if name, ok := o[key]; ok {
		return name
	}
	name, err := lookup()
	if err != nil {
		name = strconv.FormatUint(uint64(id), 10)
	}
	o[key] = name
	return name
}

func (o owners) user(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	return o.name("u"+id, func() (string, error) {
		u, err := user.LookupId(id)
		if err != nil {
			return "", err
		}
		return u.Username, nil
	}, uid)
}

func (o owners) group(gid uint32) string {
	id := strconv.FormatUint(uint64(gid), 10)
	return o.name("g"+id, func() (string, error) {
		g, err := user.LookupGroupId(id)
		if err != nil {
			return "", err
		}
		return g.Name, nil
	}, gid)
}

// entry describes what st says of something called name.
func entry(name string, st *unix.Stat_t, owners owners) *dockwellv1.FileEntry {
	return &dockwellv1.FileEntry{
		Name: strings.ToValidUTF8(name, "�"), UnreadableName: !utf8.ValidString(name),
		Type: typeOf(st.Mode), SizeBytes: uint64(max(st.Size, 0)), Mode: modeString(st.Mode),
		ModifyTime: timestamppb.New(time.Unix(st.Mtim.Unix())),
		Owner:      owners.user(st.Uid), Group: owners.group(st.Gid), Uid: st.Uid, Gid: st.Gid,
	}
}

// followLink says where the symlink called name in dir points, and what's there.
func followLink(e *dockwellv1.FileEntry, dir int, name string) {
	buf := make([]byte, maxPath)
	if n, err := unix.Readlinkat(dir, name, buf); err == nil {
		e.LinkTarget = strings.ToValidUTF8(string(buf[:n]), "�")
	}
	var st unix.Stat_t
	if unix.Fstatat(dir, name, &st, 0) == nil {
		e.LinkType = typeOf(st.Mode)
	}
}

func typeOf(mode uint32) dockwellv1.FileType {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return dockwellv1.FileType_FILE_TYPE_FILE
	case unix.S_IFDIR:
		return dockwellv1.FileType_FILE_TYPE_DIRECTORY
	case unix.S_IFLNK:
		return dockwellv1.FileType_FILE_TYPE_SYMLINK
	}
	return dockwellv1.FileType_FILE_TYPE_OTHER
}

// kindOf names what isn't a file, for a sentence.
func kindOf(st *unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFIFO:
		return "a FIFO"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFBLK, unix.S_IFCHR:
		return "a device"
	}
	return "something else"
}

// modeString writes a mode as ls does, set-ID and sticky bits included.
func modeString(mode uint32) string {
	var kind byte
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		kind = 'd'
	case unix.S_IFLNK:
		kind = 'l'
	case unix.S_IFIFO:
		kind = 'p'
	case unix.S_IFSOCK:
		kind = 's'
	case unix.S_IFBLK:
		kind = 'b'
	case unix.S_IFCHR:
		kind = 'c'
	default:
		kind = '-'
	}
	out := []byte{kind, '-', '-', '-', '-', '-', '-', '-', '-', '-'}
	for i := range 9 {
		if mode&(1<<(8-i)) != 0 {
			out[i+1] = "rwx"[i%3]
		}
	}
	special := func(bit uint32, at int, set, unset byte) {
		if mode&bit == 0 {
			return
		}
		if out[at] == 'x' {
			out[at] = set
		} else {
			out[at] = unset
		}
	}
	special(unix.S_ISUID, 3, 's', 'S')
	special(unix.S_ISGID, 6, 's', 'S')
	special(unix.S_ISVTX, 9, 't', 'T')
	return string(out)
}
