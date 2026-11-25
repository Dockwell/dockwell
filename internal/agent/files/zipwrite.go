// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"time"
	"unicode/utf8"
)

// A streaming zip archive writer whose memory stays flat whatever the entry
// count. archive/zip.Writer keeps a *zip.FileHeader for every entry in memory
// until Close writes the central directory (~250 bytes per entry, ~51 MiB at
// 200k entries). This writes each central directory record directly to an
// unlinked temporary file as each entry finishes, reuses one flate.Writer and
// one CRC-32 hasher across entries, and streams the central directory out on
// Close.
//
// Entries set the UTF-8 flag (bit 11) only when their name is valid non-ASCII
// UTF-8, matching Go's archive/zip so raw non-UTF-8 Linux names remain readable
// by Python and Java zip readers. Files use a data descriptor (bit 3) so sizes
// and CRC-32 are written after the payload without seeking on the output stream.
// Zip64 records are written whenever an entry size, local offset, directory size,
// directory offset or entry count reaches the 32-bit or 16-bit limit.

const (
	zipDataDescSig = 0x08074b50
	zipFlagDataDes = 0x0008
	zipFlagUTF8    = 0x0800
	zipVer20       = 20
	zipVer45       = 45
	zipCreatorUnix = 3 << 8
	zipCDIOBuf     = 32 << 10
	zipCopyBuf     = 32 << 10
)

// zipWriter streams a zip archive to out while spilling central directory
// records to an unlinked temp file.
type zipWriter struct {
	out     io.Writer
	written uint64

	cdFile  *os.File
	cdBuf   *bufio.Writer
	cdBytes uint64
	count   uint64

	flate   *flate.Writer
	crc     hash.Hash32
	cur     *zipEntryWriter
	comment string
	closed  bool
	aborted bool
}

// SetComment sets the archive-level zip comment written into the end-of-central-directory
// record on Close (clamped to the zip 16-bit comment limit on a UTF-8 rune boundary).
func (z *zipWriter) SetComment(comment string) {
	if len(comment) > zip16 {
		cut := zip16
		for cut > 0 && !utf8.RuneStart(comment[cut]) {
			cut--
		}
		comment = comment[:cut]
	}
	z.comment = comment
}

// zipEntryWriter tracks the one active entry being written to a zipWriter.
type zipEntryWriter struct {
	zw          *zipWriter
	name        string
	flags       uint16
	method      uint16
	dosTime     uint16
	dosDate     uint16
	extAttrs    uint32
	localOffset uint64
	compStart   uint64
	uncompSize  uint64
	isDir       bool
	closed      bool
	dest        io.Writer // either flate.Writer or direct countWriter
}

type zipCountWriter struct {
	zw *zipWriter
}

func (c zipCountWriter) Write(p []byte) (int, error) {
	n, err := c.zw.out.Write(p)
	c.zw.written += uint64(max(n, 0))
	return n, err
}

// newZipWriter starts a streaming zip archive to out, spilling its central
// directory to an unlinked temporary file in tmpDir (or os.TempDir if empty).
func newZipWriter(out io.Writer, tmpDir string) (*zipWriter, error) {
	if tmpDir != "" {
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.CreateTemp(tmpDir, "dockwell-zipcd-*")
	if err != nil {
		return nil, err
	}
	// Unlink immediately so even a crash leaves no temporary file behind.
	_ = os.Remove(f.Name())
	return &zipWriter{
		out:    out,
		cdFile: f,
		cdBuf:  bufio.NewWriterSize(f, zipCDIOBuf),
		crc:    crc32.NewIEEE(),
	}, nil
}

// Create begins a new archive entry. Directories should pass method zipStored
// and a name ending in "/". The returned io.Writer is valid until the next
// Create or Close call.
func (z *zipWriter) Create(name string, modTime time.Time, mode fs.FileMode, method uint16) (io.Writer, error) {
	if z.closed || z.aborted {
		return nil, errors.New("zip writer is closed")
	}
	if z.cur != nil {
		if err := z.cur.finish(); err != nil {
			return nil, err
		}
		z.cur = nil
	}
	if len(name) > zip16 {
		return nil, errors.New("archive entry name is too long")
	}
	isDir := mode.IsDir()
	if isDir {
		method = zipStored
	}
	dosTime, dosDate := timeToDOS(modTime)
	flags := zipEntryFlags(name, isDir)
	localOffset := z.written

	var local [zipLocalLen]byte
	binary.LittleEndian.PutUint32(local[0:], zipLocalSig)
	binary.LittleEndian.PutUint16(local[4:], zipVer20)
	binary.LittleEndian.PutUint16(local[6:], flags)
	binary.LittleEndian.PutUint16(local[8:], method)
	binary.LittleEndian.PutUint16(local[10:], dosTime)
	binary.LittleEndian.PutUint16(local[12:], dosDate)
	// CRC, compressed and uncompressed sizes are 0 in the local header:
	// directories have 0 bytes, and files carry them in the data descriptor.
	binary.LittleEndian.PutUint16(local[26:], uint16(len(name))) //nolint:gosec // checked <= zip16 above
	binary.LittleEndian.PutUint16(local[28:], 0)

	cw := zipCountWriter{zw: z}
	if _, err := cw.Write(local[:]); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(cw, name); err != nil {
		return nil, err
	}

	z.crc.Reset()
	ew := &zipEntryWriter{
		zw:          z,
		name:        name,
		flags:       flags,
		method:      method,
		dosTime:     dosTime,
		dosDate:     dosDate,
		extAttrs:    unixExternalAttrs(mode),
		localOffset: localOffset,
		compStart:   z.written,
		isDir:       isDir,
	}
	if !isDir {
		if method == zipDeflated {
			if z.flate == nil {
				fw, err := flate.NewWriter(cw, flate.DefaultCompression)
				if err != nil {
					return nil, err
				}
				z.flate = fw
			} else {
				z.flate.Reset(cw)
			}
			ew.dest = z.flate
		} else {
			ew.dest = cw
		}
	}
	z.cur = ew
	return ew, nil
}

func (e *zipEntryWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("zip entry is closed")
	}
	if e.isDir {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, errors.New("cannot write bytes to a directory entry")
	}
	n, err := e.dest.Write(p)
	if n > 0 {
		_, _ = e.zw.crc.Write(p[:n])
		e.uncompSize += uint64(n)
	}
	return n, err
}

func (e *zipEntryWriter) finish() error {
	if e.closed {
		return nil
	}
	e.closed = true
	z := e.zw
	cw := zipCountWriter{zw: z}
	if !e.isDir && e.method == zipDeflated && z.flate != nil {
		if err := z.flate.Close(); err != nil {
			return err
		}
	}
	compSize := z.written - e.compStart
	crc := z.crc.Sum32()
	if e.isDir {
		crc, compSize = 0, 0
	}
	needZip64Sizes := compSize >= zip32 || e.uncompSize >= zip32
	needZip64Offset := e.localOffset >= zip32

	if !e.isDir {
		if needZip64Sizes {
			var dd [24]byte
			binary.LittleEndian.PutUint32(dd[0:], zipDataDescSig)
			binary.LittleEndian.PutUint32(dd[4:], crc)
			binary.LittleEndian.PutUint64(dd[8:], compSize)
			binary.LittleEndian.PutUint64(dd[16:], e.uncompSize)
			if _, err := cw.Write(dd[:]); err != nil {
				return err
			}
		} else {
			var dd [16]byte
			binary.LittleEndian.PutUint32(dd[0:], zipDataDescSig)
			binary.LittleEndian.PutUint32(dd[4:], crc)
			binary.LittleEndian.PutUint32(dd[8:], uint32(compSize))
			binary.LittleEndian.PutUint32(dd[12:], uint32(e.uncompSize)) //nolint:gosec // checked < zip32
			if _, err := cw.Write(dd[:]); err != nil {
				return err
			}
		}
	}

	// Build optional zip64 extra field for the central directory record.
	var extraBuf [28]byte
	extraLen := 0
	if needZip64Sizes || needZip64Offset {
		pos := 4
		if e.uncompSize >= zip32 {
			binary.LittleEndian.PutUint64(extraBuf[pos:], e.uncompSize)
			pos += 8
		}
		if compSize >= zip32 {
			binary.LittleEndian.PutUint64(extraBuf[pos:], compSize)
			pos += 8
		}
		if needZip64Offset {
			binary.LittleEndian.PutUint64(extraBuf[pos:], e.localOffset)
			pos += 8
		}
		binary.LittleEndian.PutUint16(extraBuf[0:], zip64ExtraID)
		binary.LittleEndian.PutUint16(extraBuf[2:], uint16(pos-4))
		extraLen = pos
	}

	ver := uint16(zipVer20)
	if extraLen > 0 {
		ver = zipVer45
	}
	flags := e.flags

	cdComp := uint32(min(compSize, zip32))
	cdUncomp := uint32(min(e.uncompSize, zip32))
	cdOffset := uint32(min(e.localOffset, zip32))

	var rec [zipDirLen]byte
	binary.LittleEndian.PutUint32(rec[0:], zipDirSig)
	binary.LittleEndian.PutUint16(rec[4:], zipCreatorUnix|ver)
	binary.LittleEndian.PutUint16(rec[6:], ver)
	binary.LittleEndian.PutUint16(rec[8:], flags)
	binary.LittleEndian.PutUint16(rec[10:], e.method)
	binary.LittleEndian.PutUint16(rec[12:], e.dosTime)
	binary.LittleEndian.PutUint16(rec[14:], e.dosDate)
	binary.LittleEndian.PutUint32(rec[16:], crc)
	binary.LittleEndian.PutUint32(rec[20:], cdComp)
	binary.LittleEndian.PutUint32(rec[24:], cdUncomp)
	binary.LittleEndian.PutUint16(rec[28:], uint16(len(e.name))) //nolint:gosec // checked <= zip16
	binary.LittleEndian.PutUint16(rec[30:], uint16(extraLen))
	binary.LittleEndian.PutUint16(rec[32:], 0) // comment length
	binary.LittleEndian.PutUint16(rec[34:], 0) // disk number start
	binary.LittleEndian.PutUint16(rec[36:], 0) // internal attrs
	binary.LittleEndian.PutUint32(rec[38:], e.extAttrs)
	binary.LittleEndian.PutUint32(rec[42:], cdOffset)

	if _, err := z.cdBuf.Write(rec[:]); err != nil {
		return err
	}
	if _, err := z.cdBuf.WriteString(e.name); err != nil {
		return err
	}
	if extraLen > 0 {
		if _, err := z.cdBuf.Write(extraBuf[:extraLen]); err != nil {
			return err
		}
	}
	z.cdBytes += uint64(zipDirLen + len(e.name) + extraLen)
	z.count++
	return nil
}

// Close finishes any open entry, streams the central directory from the temp
// file, writes the end-of-central-directory records, and closes the temp file.
func (z *zipWriter) Close() error {
	if z.closed {
		return nil
	}
	z.closed = true
	defer z.closeCD()

	if z.cur != nil {
		if err := z.cur.finish(); err != nil {
			return err
		}
		z.cur = nil
	}
	if err := z.cdBuf.Flush(); err != nil {
		return err
	}
	if _, err := z.cdFile.Seek(0, io.SeekStart); err != nil {
		return err
	}

	cdStart := z.written
	cw := zipCountWriter{zw: z}
	buf := make([]byte, zipCopyBuf)
	if _, err := io.CopyBuffer(cw, z.cdFile, buf); err != nil {
		return err
	}

	needZip64 := z.count >= zip16 || z.cdBytes >= zip32 || cdStart >= zip32
	if needZip64 {
		z64EndAt := z.written
		var z64End [zip64EndLen]byte
		binary.LittleEndian.PutUint32(z64End[0:], zip64EndSig)
		binary.LittleEndian.PutUint64(z64End[4:], zip64EndLen-12)
		binary.LittleEndian.PutUint16(z64End[12:], zipCreatorUnix|zipVer45)
		binary.LittleEndian.PutUint16(z64End[14:], zipVer45)
		binary.LittleEndian.PutUint32(z64End[16:], 0)
		binary.LittleEndian.PutUint32(z64End[20:], 0)
		binary.LittleEndian.PutUint64(z64End[24:], z.count)
		binary.LittleEndian.PutUint64(z64End[32:], z.count)
		binary.LittleEndian.PutUint64(z64End[40:], z.cdBytes)
		binary.LittleEndian.PutUint64(z64End[48:], cdStart)
		if _, err := cw.Write(z64End[:]); err != nil {
			return err
		}
		var loc [zip64LocLen]byte
		binary.LittleEndian.PutUint32(loc[0:], zip64LocSig)
		binary.LittleEndian.PutUint32(loc[4:], 0)
		binary.LittleEndian.PutUint64(loc[8:], z64EndAt)
		binary.LittleEndian.PutUint32(loc[16:], 1)
		if _, err := cw.Write(loc[:]); err != nil {
			return err
		}
	}

	var eocd [zipEndLen]byte
	binary.LittleEndian.PutUint32(eocd[0:], zipEndSig)
	binary.LittleEndian.PutUint16(eocd[4:], 0)
	binary.LittleEndian.PutUint16(eocd[6:], 0)
	eocdCount := uint16(min(z.count, zip16))
	binary.LittleEndian.PutUint16(eocd[8:], eocdCount)
	binary.LittleEndian.PutUint16(eocd[10:], eocdCount)
	binary.LittleEndian.PutUint32(eocd[12:], uint32(min(z.cdBytes, zip32)))
	binary.LittleEndian.PutUint32(eocd[16:], uint32(min(cdStart, zip32)))
	binary.LittleEndian.PutUint16(eocd[20:], uint16(len(z.comment))) //nolint:gosec // clamped <= zip16 in SetComment
	if _, err := cw.Write(eocd[:]); err != nil {
		return err
	}
	if z.comment != "" {
		_, err := io.WriteString(cw, z.comment)
		return err
	}
	return nil
}

// Abort releases the temporary central-directory file when an archive stops early.
func (z *zipWriter) Abort() {
	z.aborted = true
	z.closeCD()
}

func (z *zipWriter) closeCD() {
	if z.cdFile != nil {
		_ = z.cdFile.Close()
		z.cdFile = nil
	}
}

// detectUTF8 reports whether s is valid UTF-8 and requires the UTF-8 flag
// (bit 11), matching Go's archive/zip.Writer.
func detectUTF8(s string) (valid, require bool) {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r < 0x20 || r > 0x7d || r == 0x5c {
			if !utf8.ValidRune(r) || (r == utf8.RuneError && size == 1) {
				return false, false
			}
			require = true
		}
	}
	return true, require
}

func zipEntryFlags(name string, isDir bool) uint16 {
	var flags uint16
	if valid, require := detectUTF8(name); valid && require {
		flags |= zipFlagUTF8
	}
	if !isDir {
		flags |= zipFlagDataDes
	}
	return flags
}

// unixExternalAttrs encodes mode as archive/zip.FileHeader.SetMode does.
func unixExternalAttrs(mode fs.FileMode) uint32 {
	var unixMode uint32
	var dosAttr uint32
	if mode.IsDir() {
		unixMode = 0o040000 // S_IFDIR
		dosAttr = 0x10      // MS-DOS directory flag
	} else {
		unixMode = 0o100000 // S_IFREG
	}
	unixMode |= uint32(mode.Perm())
	return (unixMode << 16) | dosAttr
}

// timeToDOS converts t into MS-DOS 16-bit time and date fields, matching
// archive/zip's clamping for years outside 1980-2107.
func timeToDOS(t time.Time) (dosTime, dosDate uint16) {
	t = t.UTC()
	year := t.Year()
	if year < 1980 {
		return 0, 1<<5 | 1
	}
	if year > 2107 {
		year = 2107
	}
	dosTime = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)  //nolint:gosec // bounded DOS time
	dosDate = uint16(t.Day() + int(t.Month())<<5 + (year-1980)<<9) //nolint:gosec // bounded DOS date
	return dosTime, dosDate
}
