// SPDX-License-Identifier: AGPL-3.0-only

// Package volumetool reads and writes a volume's bytes. The agent runs it in a
// throwaway container that has the volume and nothing else, so what an app left
// in its volume, such as a symlink to /etc, leads nowhere (ADR 0033). An archive
// is a gzipped tar with numeric owners, so it opens by hand with
// tar --numeric-owner -xzpf.
package volumetool

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Command is the hidden command the binary runs this as.
const Command = "__volume-tool"

// What a restore keeps in the volume's own top directory while it works, so
// that moving things in and out is a rename. Neither is ever in a backup.
const (
	stagingDir = ".dockwell-restore"
	oldDir     = ".dockwell-old"
)

// Stats says how a run went. It is the last line of what the tool prints to its
// standard error, as JSON, and earlier lines say how it is going.
type Stats struct {
	// Files counts what it went through, directories and links included.
	Files int64 `json:"files"`
	// Bytes counts the bytes of regular files.
	Bytes int64 `json:"bytes"`
	// Changed counts files that changed while they were read, or went. It
	// stays at zero for a volume nothing writes to.
	Changed int64 `json:"changed,omitempty"`
	// Skipped counts sockets, which hold nothing, and which a process makes
	// again as it starts.
	Skipped int64 `json:"skipped,omitempty"`
	// Swapping says an unpack began moving what's in the volume out. A failure
	// after that leaves the volume neither what it was nor what it was to be.
	Swapping bool `json:"swapping,omitempty"`
	// Free is what's free on the disk the volume is on, and Filesystem tells that
	// disk from another. Only a measure says them.
	Free       int64  `json:"free,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	// Size is how large that disk is.
	Size int64 `json:"size,omitempty"`
	// Done marks the last line.
	Done bool `json:"done,omitempty"`
}

// Progress hears how a run is going, at most every ProgressEvery.
type Progress func(Stats)

// ProgressEvery is how often a run says how it is going.
const ProgressEvery = 2 * time.Second

// ticker says how a run is going, now and then.
type ticker struct {
	fn   Progress
	next time.Time
}

func (t *ticker) tell(s Stats) {
	if t.fn == nil {
		return
	}
	if now := time.Now(); now.After(t.next) {
		t.next = now.Add(ProgressEvery)
		t.fn(s)
	}
}

// inode names a file wherever it is linked.
type inode struct {
	dev, ino uint64
}

type packer struct {
	root  string
	tw    *tar.Writer
	seen  map[inode]string // the first name of each file with more than one
	stats Stats
	tick  ticker
}

// Pack writes the volume at dir to w as an archive. A file that can't be read
// fails it, as a backup with a hole in it is worse than none. One that goes or
// changes while it is read is counted, as that's what a copy of a running app
// comes to.
func Pack(w io.Writer, dir string, progress Progress) (Stats, error) {
	if _, err := os.Lstat(filepath.Join(dir, oldDir)); err == nil {
		return Stats{}, errors.New("an earlier restore of this volume stopped partway, so its files are in two places. " +
			"Finish that restore from Activity, or restore a backup, before backing it up")
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed) // an app may be stopped for it
	if err != nil {
		return Stats{}, err
	}
	p := &packer{root: dir, tw: tar.NewWriter(gz), seen: map[inode]string{}, tick: ticker{fn: progress}}
	if err := p.add("."); err != nil {
		return p.stats, err
	}
	if err := p.walk(""); err != nil {
		return p.stats, err
	}
	if err := p.tw.Close(); err != nil {
		return p.stats, err
	}
	if err := gz.Close(); err != nil {
		return p.stats, err
	}
	p.stats.Done = true
	return p.stats, nil
}

func (p *packer) walk(rel string) error {
	entries, err := os.ReadDir(filepath.Join(p.root, rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.stats.Changed++
		return nil
	case err != nil:
		return fmt.Errorf("can't read the directory %s in the volume: %w", shown(rel), err)
	}
	for _, e := range entries {
		if rel == "" && (e.Name() == stagingDir || e.Name() == oldDir) {
			continue
		}
		name := path.Join(rel, e.Name())
		if err := p.add(name); err != nil {
			return err
		}
		if e.IsDir() {
			if err := p.walk(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// shown is a path in the volume, for a message.
func shown(rel string) string {
	if rel == "" || rel == "." {
		return "/"
	}
	return "/" + rel
}

// add writes one entry, and a regular file's bytes.
func (p *packer) add(name string) error {
	full := filepath.Join(p.root, name)
	fi, err := os.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.stats.Changed++
		return nil
	case err != nil:
		return fmt.Errorf("can't read %s in the volume: %w", shown(name), err)
	}
	if fi.Mode()&os.ModeSocket != 0 {
		p.stats.Skipped++
		return nil
	}
	var file *os.File
	if fi.Mode().IsRegular() {
		// Not following a link, and not waiting on a pipe, as a file may have
		// been swapped for one since.
		file, err = os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // what's in the volume, which is what this is for, in a container with nothing else
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ELOOP):
			p.stats.Changed++
			return nil
		case err != nil:
			return fmt.Errorf("can't read %s in the volume: %w", shown(name), err)
		}
		defer file.Close()
		if fi, err = file.Stat(); err != nil {
			return fmt.Errorf("can't read %s in the volume: %w", shown(name), err)
		}
		if !fi.Mode().IsRegular() {
			p.stats.Changed++
			return nil
		}
	}
	link := ""
	if fi.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(full); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				p.stats.Changed++
				return nil
			}
			return fmt.Errorf("can't read the link %s in the volume: %w", shown(name), err)
		}
	}
	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return fmt.Errorf("can't describe %s in the volume: %w", shown(name), err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("can't read the owner of %s in the volume", shown(name))
	}
	hdr.Format = tar.FormatPAX
	hdr.Name = name
	if name == "." {
		hdr.Name = "./"
	} else if fi.IsDir() {
		hdr.Name = name + "/"
	}
	hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
	hdr.Uname, hdr.Gname = "", "" // numbers, as names mean nothing outside the app
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
	if xattrs, err := readXattrs(full); err != nil {
		return fmt.Errorf("can't read the extended attributes of %s in the volume: %w", shown(name), err)
	} else if len(xattrs) > 0 {
		hdr.PAXRecords = make(map[string]string, len(xattrs))
		for k, v := range xattrs {
			hdr.PAXRecords[paxXattr+k] = v
		}
	}
	p.stats.Files++
	if fi.Mode().IsRegular() && st.Nlink > 1 {
		key := inode{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev's type differs by architecture
		if first, ok := p.seen[key]; ok {
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
			file = nil
		} else {
			p.seen[key] = name
		}
	}
	if err := p.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("can't write %s to the archive: %w", shown(name), err)
	}
	if file != nil {
		if err := p.copyFile(name, file, fi); err != nil {
			return err
		}
	}
	p.tick.tell(p.stats)
	return nil
}

// paxXattr starts the record of an extended attribute, as GNU tar and others
// write it.
const paxXattr = "SCHILY.xattr."

// copyFile writes a file's bytes, as many as its header said. One that got
// shorter since is padded, as a tar entry has the size it has, and counted.
func (p *packer) copyFile(name string, f *os.File, fi fs.FileInfo) error {
	n, err := io.CopyN(p.tw, f, fi.Size())
	p.stats.Bytes += n
	switch {
	case errors.Is(err, io.EOF):
		p.stats.Changed++
		if _, err := io.CopyN(p.tw, zeros{}, fi.Size()-n); err != nil {
			return fmt.Errorf("can't write %s to the archive: %w", shown(name), err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("can't read %s in the volume: %w", shown(name), err)
	}
	if after, err := f.Stat(); err == nil && (after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime())) {
		p.stats.Changed++
	}
	return nil
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// Hooks stand in for what only root can do, for a test that isn't.
type hooks struct {
	lchown func(path string, uid, gid int) error
}

// Unpack replaces what's in the volume at dir with the archive r. It writes
// everything into a directory inside the volume first, and only once all of it
// is there, and whole, moves what's there out and that in. So a bad archive,
// or a full disk, leaves the volume as it was.
func Unpack(r io.Reader, dir string, progress Progress) (Stats, error) {
	return unpack(r, dir, progress, hooks{lchown: os.Lchown})
}

func unpack(r io.Reader, dir string, progress Progress, h hooks) (Stats, error) {
	staging := filepath.Join(dir, stagingDir)
	if err := os.RemoveAll(staging); err != nil {
		return Stats{}, fmt.Errorf("can't clear what an earlier restore left in the volume: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Stats{}, fmt.Errorf("can't write to the volume: %w", err)
	}
	stats, top, err := extract(r, staging, progress, h)
	if err != nil {
		_ = os.RemoveAll(staging)
		return stats, err
	}
	syscall.Sync() // what it wrote is on disk before what it replaces goes
	stats.Swapping = true
	if progress != nil {
		progress(stats)
	}
	if err := swap(dir, top, h); err != nil {
		return stats, err
	}
	syscall.Sync()
	stats.Done = true
	return stats, nil
}

// directory is a directory unpacked, for what's set once all it holds is.
type directory struct {
	path string
	hdr  *tar.Header
}

// extract unpacks an archive into dir, and returns the header of its top
// directory, which is the volume's own.
func extract(r io.Reader, dir string, progress Progress, h hooks) (Stats, *tar.Header, error) {
	var stats Stats
	gz, err := gzip.NewReader(r)
	if err != nil {
		return stats, nil, fmt.Errorf("the archive doesn't open: %w", err)
	}
	defer gz.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return stats, nil, err
	}
	defer root.Close()
	tick := ticker{fn: progress}
	tr := tar.NewReader(gz)
	var top *tar.Header
	var dirs []directory
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, nil, fmt.Errorf("the archive stops short or is damaged after %d files: %w", stats.Files, err)
		}
		name := path.Clean(hdr.Name)
		if name == "." {
			top = hdr
			continue
		}
		if !filepath.IsLocal(name) {
			return stats, nil, fmt.Errorf("the archive names %q, which is outside the volume, so it isn't one Dockwell made", hdr.Name)
		}
		if err := place(root, dir, name, hdr, tr, h); err != nil {
			return stats, nil, err
		}
		stats.Files++
		stats.Bytes += hdr.Size
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, directory{name, hdr})
		}
		tick.tell(stats)
	}
	// The archive's end is only the tar's: the gzip's own check comes after it.
	if _, err := io.Copy(io.Discard, gz); err != nil { //nolint:gosec // to its end, which is the point
		return stats, nil, fmt.Errorf("the archive is damaged: %w", err)
	}
	if top == nil {
		return stats, nil, errors.New("the archive has no top directory, so it isn't one Dockwell made")
	}
	// Writing into a directory moves its time, so these go last, deepest first.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := root.Chtimes(dirs[i].path, time.Time{}, dirs[i].hdr.ModTime); err != nil {
			return stats, nil, fmt.Errorf("can't set the time of %s: %w", shown(dirs[i].path), err)
		}
	}
	return stats, top, nil
}

// place makes one entry and gives it what its header says, in the order that
// keeps it all: an owner clears setuid and capabilities, so it comes first.
func place(root *os.Root, dir, name string, hdr *tar.Header, tr io.Reader, h hooks) error {
	full := filepath.Join(dir, name)
	var err error
	switch hdr.Typeflag {
	case tar.TypeDir:
		err = root.MkdirAll(name, 0o700)
	case tar.TypeReg:
		var f *os.File
		if f, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	case tar.TypeSymlink:
		err = root.Symlink(hdr.Linkname, name)
	case tar.TypeLink:
		if !filepath.IsLocal(hdr.Linkname) {
			return fmt.Errorf("the archive links to %q, which is outside the volume, so it isn't one Dockwell made", hdr.Linkname)
		}
		// A link shares what its first name has been given.
		return root.Link(hdr.Linkname, name)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		err = makeNode(full, hdr)
	default:
		return fmt.Errorf("the archive holds %s of a type Dockwell doesn't make (%q)", shown(name), hdr.Typeflag)
	}
	if err != nil {
		return fmt.Errorf("can't write %s to the volume: %w", shown(name), err)
	}
	if err := h.lchown(full, hdr.Uid, hdr.Gid); err != nil {
		return fmt.Errorf("can't set the owner of %s to %d:%d: %w", shown(name), hdr.Uid, hdr.Gid, err)
	}
	if hdr.Typeflag != tar.TypeSymlink {
		if err := root.Chmod(name, hdr.FileInfo().Mode()); err != nil {
			return fmt.Errorf("can't set the mode of %s: %w", shown(name), err)
		}
	}
	if err := writeXattrs(full, xattrsOf(hdr)); err != nil {
		return fmt.Errorf("can't set the extended attributes of %s: %w", shown(name), err)
	}
	if hdr.Typeflag == tar.TypeDir {
		return nil
	}
	if err := setTime(full, hdr.ModTime); err != nil {
		return fmt.Errorf("can't set the time of %s: %w", shown(name), err)
	}
	return nil
}

// xattrsOf reads a header's extended attributes.
func xattrsOf(hdr *tar.Header) map[string]string {
	var out map[string]string
	for k, v := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(k, paxXattr); ok {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = v
		}
	}
	return out
}

// swap moves what's in the volume aside, and what was unpacked in, then gives
// the volume's own directory what the archive says of it. A restore that dies
// here runs again from the start, and finds what it moved aside still there,
// which a copy of the volume taken first has already saved (ADR 0033).
func swap(dir string, top *tar.Header, h hooks) error {
	staging, old := filepath.Join(dir, stagingDir), filepath.Join(dir, oldDir)
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("can't clear what an earlier restore left in the volume: %w", err)
	}
	if err := os.Mkdir(old, 0o700); err != nil {
		return fmt.Errorf("can't write to the volume: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("can't read the volume: %w", err)
	}
	for _, e := range entries {
		if e.Name() == stagingDir || e.Name() == oldDir {
			continue
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(old, e.Name())); err != nil {
			return fmt.Errorf("can't move %s aside: %w", e.Name(), err)
		}
	}
	unpacked, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("can't read what was unpacked: %w", err)
	}
	for _, e := range unpacked {
		if err := os.Rename(filepath.Join(staging, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("can't move %s into the volume: %w", e.Name(), err)
		}
	}
	if err := h.lchown(dir, top.Uid, top.Gid); err != nil {
		return fmt.Errorf("can't set the owner of the volume to %d:%d: %w", top.Uid, top.Gid, err)
	}
	if err := os.Chmod(dir, top.FileInfo().Mode()); err != nil {
		return fmt.Errorf("can't set the mode of the volume: %w", err)
	}
	if err := writeXattrs(dir, xattrsOf(top)); err != nil {
		return fmt.Errorf("can't set the extended attributes of the volume: %w", err)
	}
	if err := os.Remove(staging); err != nil {
		return fmt.Errorf("can't remove what the restore left: %w", err)
	}
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("can't remove what the restore replaced: %w", err)
	}
	return os.Chtimes(dir, time.Time{}, top.ModTime)
}

// Empty removes everything in the volume at dir, and leaves the volume.
func Empty(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("can't read the volume: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("can't remove %s from the volume: %w", e.Name(), err)
		}
	}
	syscall.Sync()
	return nil
}

// Measure says how much is in the volume at dir, as a backup would read it, and how
// much is free beside it. It counts a hard link's file once.
func Measure(dir string) (Stats, error) {
	var stats Stats
	seen := map[inode]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case !d.Type().IsRegular():
			stats.Files++
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		stats.Files++
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			key := inode{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev's type differs by architecture
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		stats.Bytes += fi.Size()
		return nil
	})
	if err != nil {
		return stats, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return stats, fmt.Errorf("can't read how much is free beside the volume: %w", err)
	}
	stats.Free = int64(st.Bavail) * int64(st.Bsize) //nolint:gosec,unconvert // a disk's size fits, and Bsize's type differs by architecture
	stats.Size = int64(st.Blocks) * int64(st.Bsize) //nolint:gosec,unconvert // as above
	stats.Filesystem = fmt.Sprint(st.Fsid)
	stats.Done = true
	return stats, nil
}

// Verify reads an archive to its end, and says whether it's whole.
func Verify(r io.Reader) (Stats, error) {
	var stats Stats
	gz, err := gzip.NewReader(r)
	if err != nil {
		return stats, fmt.Errorf("the archive doesn't open: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("the archive stops short or is damaged after %d files: %w", stats.Files, err)
		}
		n, err := io.Copy(io.Discard, tr) //nolint:gosec // to its end, which is the point
		if err != nil {
			return stats, fmt.Errorf("the archive stops short or is damaged after %d files: %w", stats.Files, err)
		}
		if path.Clean(hdr.Name) != "." {
			stats.Files++
		}
		stats.Bytes += n
	}
	if _, err := io.Copy(io.Discard, gz); err != nil { //nolint:gosec // to its end, which is the point
		return stats, fmt.Errorf("the archive is damaged: %w", err)
	}
	stats.Done = true
	return stats, nil
}

// Main runs the tool, as the binary's hidden command does in its container. It
// writes an archive to stdout and reads one from stdin, and says how it's going
// on stderr, as JSON lines, the last with Done set. A failure is a plain line
// and an exit code of 1.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: "+Command+" pack|unpack|empty|measure <directory>")
		return 2
	}
	report := json.NewEncoder(stderr)
	tell := func(s Stats) { _ = report.Encode(s) }
	var stats Stats
	var err error
	switch verb, dir := args[0], args[1]; verb {
	case "pack":
		stats, err = Pack(stdout, dir, tell)
	case "unpack":
		stats, err = Unpack(stdin, dir, tell)
	case "empty":
		err = Empty(dir)
		stats.Done = true
	case "measure":
		stats, err = Measure(dir)
	default:
		fmt.Fprintln(stderr, "usage: "+Command+" pack|unpack|empty|measure <directory>")
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	tell(stats)
	return 0
}
