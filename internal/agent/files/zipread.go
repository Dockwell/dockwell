// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
)

// A zip archive's entries, read from its central directory one record at a time.
// archive/zip reads the whole directory into memory before giving any of it, a few
// hundred bytes for each entry, so an archive of small entries took the agent past
// its budget before the entry limit could say no. This reads the end of the
// archive first, refuses one that says it holds too many before anything else, and
// then holds one record, a buffer and one decompressor at a time, whatever the
// archive holds.

const (
	zipEndSig      = 0x06054b50
	zip64LocSig    = 0x07064b50
	zip64EndSig    = 0x06064b50
	zipDirSig      = 0x02014b50
	zipDirSignSig  = 0x05054b50
	zipLocalSig    = 0x04034b50
	zipEndLen      = 22
	zip64LocLen    = 20
	zip64EndLen    = 56
	zipDirLen      = 46
	zipDirSignLen  = 6
	zipLocalLen    = 30
	zipMaxComment  = 0xffff
	zipDirBuffer   = 64 << 10
	zip64ExtraID   = 0x0001
	zipEncrypted   = 0x1
	zipStored      = 0
	zipDeflated    = 8
	zip16, zip32   = 0xffff, 0xffffffff
	zipNotReadable = "it isn't a zip archive Dockwell can read"
)

// zipDir is a zip archive's central directory, as its end says.
type zipDir struct {
	f    io.ReaderAt
	size int64
	// base is where the archive starts in f: past whatever is before it, as a
	// self-extracting archive has.
	base int64
	// count, start and length are how many entries it says it has, and where it is.
	count         uint64
	start, length int64
	inflate       io.ReadCloser
}

// zipEntry is one record of the directory.
type zipEntry struct {
	name   string
	mode   fs.FileMode
	flags  uint16
	method uint16
	crc    uint32
	packed uint64
	size   uint64
	local  uint64
}

func notZip(why string) error { return fmt.Errorf("%s: %s", zipNotReadable, why) }

// readZipDir finds the directory of the archive f, size bytes long, from its end.
func readZipDir(f io.ReaderAt, size int64) (*zipDir, error) {
	if size < zipEndLen {
		return nil, notZip("it's too short to be one")
	}
	tail := make([]byte, min(size, zipEndLen+zipMaxComment))
	at := size - int64(len(tail))
	if _, err := f.ReadAt(tail, at); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	end := -1
	for i := len(tail) - zipEndLen; i >= 0; i-- {
		// The end record's comment runs to the archive's end.
		if binary.LittleEndian.Uint32(tail[i:]) == zipEndSig && i+zipEndLen+int(binary.LittleEndian.Uint16(tail[i+20:])) <= len(tail) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, notZip("it has no end record")
	}
	rec := tail[end : end+zipEndLen]
	endAt := at + int64(end)
	if binary.LittleEndian.Uint16(rec[4:]) != 0 || binary.LittleEndian.Uint16(rec[6:]) != 0 {
		return nil, notZip("it spans several files")
	}
	count := uint64(binary.LittleEndian.Uint16(rec[10:]))
	length := uint64(binary.LittleEndian.Uint32(rec[12:]))
	offset := uint64(binary.LittleEndian.Uint32(rec[16:]))
	if count == zip16 || length == zip32 || offset == zip32 {
		var err error
		if endAt, count, length, offset, err = readZip64End(f, endAt); err != nil {
			return nil, err
		}
	}
	if length > uint64(endAt) || offset > uint64(endAt)-length { //nolint:gosec // an offset in a file is never negative
		return nil, notZip("its directory isn't where it says")
	}
	if count > length/zipDirLen {
		return nil, notZip("its directory is too short for what it says it holds")
	}
	d := &zipDir{f: f, size: size, count: count, start: int64(offset), length: int64(length)} //nolint:gosec // both below endAt
	// Offsets count from where the archive starts, which is past anything before it.
	if base := endAt - d.length - d.start; base > 0 {
		var sig [4]byte
		if _, err := f.ReadAt(sig[:], d.start); err != nil || binary.LittleEndian.Uint32(sig[:]) != zipDirSig {
			d.base = base
		}
	}
	return d, nil
}

// readZip64End reads the zip64 end record the locator before endAt points to.
func readZip64End(f io.ReaderAt, endAt int64) (at int64, count, length, offset uint64, err error) {
	if endAt < zip64LocLen {
		return 0, 0, 0, 0, notZip("its zip64 end is missing")
	}
	var loc [zip64LocLen]byte
	if _, err := f.ReadAt(loc[:], endAt-zip64LocLen); err != nil {
		return 0, 0, 0, 0, notZip("its zip64 end is missing")
	}
	if binary.LittleEndian.Uint32(loc[:]) != zip64LocSig {
		return 0, 0, 0, 0, notZip("its zip64 end is missing")
	}
	if binary.LittleEndian.Uint32(loc[4:]) != 0 || binary.LittleEndian.Uint32(loc[16:]) > 1 {
		return 0, 0, 0, 0, notZip("it spans several files")
	}
	where := binary.LittleEndian.Uint64(loc[8:])
	if where > uint64(endAt-zip64LocLen) {
		return 0, 0, 0, 0, notZip("its zip64 end isn't where it says")
	}
	var rec [zip64EndLen]byte
	if _, err := f.ReadAt(rec[:], int64(where)); err != nil || binary.LittleEndian.Uint32(rec[:]) != zip64EndSig { //nolint:gosec // below endAt
		return 0, 0, 0, 0, notZip("its zip64 end isn't where it says")
	}
	if binary.LittleEndian.Uint32(rec[16:]) != 0 || binary.LittleEndian.Uint32(rec[20:]) != 0 {
		return 0, 0, 0, 0, notZip("it spans several files")
	}
	return int64(where), binary.LittleEndian.Uint64(rec[32:]), binary.LittleEndian.Uint64(rec[40:]), binary.LittleEndian.Uint64(rec[48:]), nil //nolint:gosec // below endAt
}

// each gives each of the directory's records to fn, in order, one at a time.
func (d *zipDir) each(fn func(*zipEntry) error) error {
	r := bufio.NewReaderSize(io.NewSectionReader(d.f, d.base+d.start, d.length), zipDirBuffer)
	var fixed [zipDirLen]byte
	var extra []byte
	for range d.count {
		if _, err := io.ReadFull(r, fixed[:]); err != nil || binary.LittleEndian.Uint32(fixed[:]) != zipDirSig {
			return notZip("its directory ends before what it says it holds")
		}
		e := &zipEntry{
			flags:  binary.LittleEndian.Uint16(fixed[8:]),
			method: binary.LittleEndian.Uint16(fixed[10:]),
			crc:    binary.LittleEndian.Uint32(fixed[16:]),
			packed: uint64(binary.LittleEndian.Uint32(fixed[20:])),
			size:   uint64(binary.LittleEndian.Uint32(fixed[24:])),
			local:  uint64(binary.LittleEndian.Uint32(fixed[42:])),
		}
		name := make([]byte, binary.LittleEndian.Uint16(fixed[28:]))
		if n := int(binary.LittleEndian.Uint16(fixed[30:])); cap(extra) < n {
			extra = make([]byte, n)
		} else {
			extra = extra[:n]
		}
		if _, err := io.ReadFull(r, name); err != nil {
			return notZip("its directory ends before what it says it holds")
		}
		if _, err := io.ReadFull(r, extra); err != nil {
			return notZip("its directory ends before what it says it holds")
		}
		if _, err := r.Discard(int(binary.LittleEndian.Uint16(fixed[32:]))); err != nil {
			return notZip("its directory ends before what it says it holds")
		}
		if err := e.zip64(extra); err != nil {
			return err
		}
		e.name = string(name)
		// archive/zip knows how each system that makes zips writes a mode.
		h := zip.FileHeader{Name: e.name, CreatorVersion: binary.LittleEndian.Uint16(fixed[4:]), ExternalAttrs: binary.LittleEndian.Uint32(fixed[38:])}
		e.mode = h.Mode()
		if err := fn(e); err != nil {
			return err
		}
	}
	return endZipDirectory(r)
}

// The count must account for the whole directory. Its optional digital signature
// can follow the file records, but no additional entries or unexplained bytes can.
func endZipDirectory(r *bufio.Reader) error {
	if _, err := r.Peek(1); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return notZip("its directory is cut short")
	}
	var signature [zipDirSignLen]byte
	if _, err := io.ReadFull(r, signature[:]); err != nil || binary.LittleEndian.Uint32(signature[:]) != zipDirSignSig {
		return notZip("its directory holds records its count leaves out")
	}
	if _, err := r.Discard(int(binary.LittleEndian.Uint16(signature[4:]))); err != nil {
		return notZip("its directory's signature is cut short")
	}
	if _, err := r.Peek(1); !errors.Is(err, io.EOF) {
		return notZip("its directory holds records its count leaves out")
	}
	return nil
}

// zip64 reads the sizes and the offset that didn't fit in the record from its
// zip64 extra field.
func (e *zipEntry) zip64(extra []byte) error {
	for len(extra) >= 4 {
		id, n := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if n > len(extra) {
			return notZip("an entry's extra field is cut short")
		}
		field := extra[:n]
		extra = extra[n:]
		if id != zip64ExtraID {
			continue
		}
		for _, v := range []*uint64{&e.size, &e.packed, &e.local} {
			if *v != zip32 {
				continue
			}
			if len(field) < 8 {
				return notZip("an entry's zip64 field is cut short")
			}
			*v, field = binary.LittleEndian.Uint64(field), field[8:]
		}
	}
	return nil
}

// open reads what the entry e holds, which is checked against its size and CRC-32
// as it ends. It reuses one decompressor, so the last one's reader is done with.
func (d *zipDir) open(e *zipEntry) (io.Reader, error) {
	switch {
	case e.flags&zipEncrypted != 0:
		return nil, fmt.Errorf("it holds %s, which is encrypted", Printable(e.name))
	case e.method != zipStored && e.method != zipDeflated:
		return nil, fmt.Errorf("it holds %s, packed in a way Dockwell can't unpack (method %d)", Printable(e.name), e.method)
	}
	var local [zipLocalLen]byte
	if e.local > uint64(d.size) { //nolint:gosec // a size is never negative
		return nil, notZip("an entry isn't where it says")
	}
	at := d.base + int64(e.local) //nolint:gosec // below its size
	if _, err := d.f.ReadAt(local[:], at); err != nil || binary.LittleEndian.Uint32(local[:]) != zipLocalSig {
		return nil, notZip("an entry isn't where it says")
	}
	start := at + zipLocalLen + int64(binary.LittleEndian.Uint16(local[26:])) + int64(binary.LittleEndian.Uint16(local[28:]))
	if start > d.size || e.packed > uint64(d.size-start) { //nolint:gosec // start is below the size
		return nil, notZip("an entry runs past its end")
	}
	var r io.Reader = io.NewSectionReader(d.f, start, int64(e.packed)) //nolint:gosec // below its size
	if e.method == zipDeflated {
		if d.inflate == nil {
			d.inflate = flate.NewReader(r)
		} else if err := d.inflate.(flate.Resetter).Reset(r, nil); err != nil { //nolint:forcetypeassert // flate's own reader
			return nil, err
		}
		r = d.inflate
	}
	return &zipChecked{r: r, e: e, crc: crc32.NewIEEE()}, nil
}

// zipChecked reads an entry, and fails at its end if it isn't the size or the
// CRC-32 its record says.
type zipChecked struct {
	r    io.Reader
	e    *zipEntry
	crc  hash.Hash32
	read uint64
}

func (z *zipChecked) Read(p []byte) (int, error) {
	n, err := z.r.Read(p)
	z.read += uint64(max(n, 0))
	_, _ = z.crc.Write(p[:n])
	if z.read > z.e.size {
		return n, fmt.Errorf("it holds %s, which is larger than it says", Printable(z.e.name))
	}
	if errors.Is(err, io.EOF) && (z.read != z.e.size || z.crc.Sum32() != z.e.crc) {
		return n, fmt.Errorf("it holds %s, which doesn't read as its checksum says", Printable(z.e.name))
	}
	return n, err
}
