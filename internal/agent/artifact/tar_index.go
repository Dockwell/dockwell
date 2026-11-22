// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/maphash"
	"io"
	"os"
	"path/filepath"
)

const (
	tarBlock   = 512
	maxPadding = 10240
	indexSlots = 1 << 17
)

// tarIndex keeps fingerprints and header offsets on disk. An archive cannot
// make the agent keep its unrelated paths, and collisions fail closed.
type tarIndex struct {
	file *os.File
	seed maphash.Seed
}

func indexTar(ctx context.Context, src io.ReaderAt, size int64, dir string) (*tarIndex, error) {
	f, err := os.CreateTemp(dir, "tar-index-*")
	if err != nil {
		return nil, err
	}
	// This scratch index needs no name after open, including after a crash.
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, err
	}
	idx := &tarIndex{file: f, seed: maphash.MakeSeed()}
	if err := f.Truncate(indexSlots * 16); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := idx.scan(ctx, src, size); err != nil {
		_ = f.Close()
		return nil, err
	}
	return idx, nil
}

func (i *tarIndex) scan(ctx context.Context, src io.ReaderAt, size int64) error {
	var block [tarBlock]byte
	var name [256]byte
	for at, count := int64(0), 0; at+2*tarBlock <= size; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := src.ReadAt(block[:], at); err != nil {
			return ErrArchive
		}
		if allZero(block[:]) {
			return checkTarEnd(src, at+tarBlock, size)
		}
		path, sizeOf, isDir, ok := tarHeader(block[:], name[:0])
		if !ok || count == maxEntries || sizeOf > size-at-tarBlock {
			return ErrArchive
		}
		if _, err := i.find(path, at+tarBlock, sizeOf, isDir); err != nil {
			return err
		}
		at += tarBlock + ((sizeOf + tarBlock - 1) &^ (tarBlock - 1))
	}
	return ErrArchive
}

func checkTarEnd(src io.ReaderAt, at, size int64) error {
	if left := size - at; left < tarBlock || left > tarBlock+maxPadding {
		return ErrArchive
	}
	var padding [tarBlock + maxPadding]byte
	data := padding[:size-at]
	if _, err := src.ReadAt(data, at); err != nil || !allZero(data) {
		return ErrArchive
	}
	return nil
}

// find inserts if start is nonzero. Otherwise it looks up a named regular file.
func (i *tarIndex) find(name []byte, start, size int64, dir bool) (extent, error) {
	hash := maphash.Bytes(i.seed, name)
	var record [16]byte
	for probe := range 64 {
		slot := int64((hash+uint64(probe))&(indexSlots-1)) * int64(len(record))
		if _, err := i.file.ReadAt(record[:], slot); err != nil {
			return extent{}, err
		}
		meta := binary.LittleEndian.Uint64(record[8:])
		if meta == 0 {
			if start == 0 {
				return extent{}, ErrArchive
			}
			// Positions are block aligned. Store a presence bit and mark directories.
			meta = uint64(start/tarBlock)<<1 | 1 //nolint:gosec // positions are positive and bounded
			if dir {
				meta |= 1 << 63
			}
			binary.LittleEndian.PutUint64(record[:], hash)
			binary.LittleEndian.PutUint64(record[8:], meta)
			_, err := i.file.WriteAt(record[:], slot)
			return extent{start: start, size: size}, err
		}
		if hash == binary.LittleEndian.Uint64(record[:8]) {
			if start != 0 || meta>>63 != 0 {
				return extent{}, ErrArchive
			}
			return extent{start: int64(meta>>1) * tarBlock}, nil
		}
	}
	return extent{}, ErrArchive
}

func (i *tarIndex) fileExtent(src io.ReaderAt, name string) (extent, error) {
	e, err := i.find([]byte(name), 0, 0, false)
	if err != nil {
		return extent{}, err
	}
	var block [tarBlock]byte
	if _, err := src.ReadAt(block[:], e.start-tarBlock); err != nil {
		return extent{}, ErrArchive
	}
	var path [256]byte
	got, size, dir, ok := tarHeader(block[:], path[:0])
	if !ok || dir || !bytes.Equal(got, []byte(name)) {
		return extent{}, ErrArchive
	}
	e.size = size
	return e, nil
}

func tarIndexDir(src io.ReaderAt) string {
	if f, ok := src.(*os.File); ok {
		return filepath.Dir(f.Name())
	}
	return ""
}

// tarHeader reads a regular or directory header without allocating per member.
// Extension records are refused because the normalized archive uses fixed names.
func tarHeader(block, name []byte) ([]byte, int64, bool, bool) {
	if len(block) != tarBlock {
		return nil, 0, false, false
	}
	checksum, ok := tarNumber(block[148:156])
	var sum int64
	for j, b := range block {
		if j >= 148 && j < 156 {
			b = ' '
		}
		sum += int64(b)
	}
	if !ok || checksum != sum {
		return nil, 0, false, false
	}
	dir := block[156] == '5'
	if (!dir && block[156] != '0' && block[156] != 0) || !allZero(block[157:257]) {
		return nil, 0, false, false
	}
	size, ok := tarNumber(block[124:136])
	if !ok || size > MaxExpanded || dir && size != 0 {
		return nil, 0, false, false
	}
	base, _, _ := bytes.Cut(block[:100], []byte{0})
	switch {
	case bytes.Equal(block[257:263], []byte("ustar\x00")):
		prefix, _, _ := bytes.Cut(block[345:500], []byte{0})
		if len(prefix) > 0 {
			name = append(name, prefix...)
			name = append(name, '/')
		}
	case bytes.Equal(block[257:265], []byte("ustar  \x00")), allZero(block[257:265]):
	default:
		return nil, 0, false, false
	}
	name = append(name, base...)
	if dir {
		name = bytes.TrimSuffix(name, []byte{'/'})
	}
	return name, size, dir, validTarName(name)
}

func tarNumber(field []byte) (int64, bool) {
	var n int64
	if field[0]&0x80 != 0 {
		if field[0] != 0x80 {
			return 0, false
		}
		for _, b := range field[1:] {
			if n > MaxExpanded>>8 {
				return 0, false
			}
			n = n<<8 | int64(b)
		}
		return n, true
	}
	for _, b := range bytes.Trim(field, " \x00") {
		if b < '0' || b > '7' || n > MaxExpanded>>3 {
			return 0, false
		}
		n = n<<3 | int64(b-'0')
	}
	return n, true
}

func validTarName(name []byte) bool {
	if len(name) == 0 || len(name) > 240 || bytes.ContainsAny(name, "\\:\x00") {
		return false
	}
	for len(name) != 0 {
		part, tail, more := bytes.Cut(name, []byte{'/'})
		if len(part) == 0 || bytes.Equal(part, []byte{'.'}) || bytes.Equal(part, []byte("..")) || more && len(tail) == 0 {
			return false
		}
		name = tail
	}
	return true
}

func allZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}
