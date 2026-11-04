// SPDX-License-Identifier: AGPL-3.0-only

// Package footprint keeps what dockwell holds in memory at idle to what it
// uses (docs/adr/0017-release-the-pages-startup-maps.md).
//
// Starting dockwell maps far more of its own binary than it touches again.
// Go runs the initialization of every package linked in, the control
// plane's included, and each page it touches brings in its neighbors too:
// on a fault the kernel maps up to 64 KB around the address from the page
// cache. Those pages are clean copies of the file, but they count in the
// process's RSS for as long as it runs. On an agent they are most of it.
package footprint

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnsupported is ReleaseStartupPages' answer where it can't release
// pages: anywhere but Linux.
var ErrUnsupported = errors.New("releasing pages is supported only on Linux")

// span is one mapping, from lo up to hi.
type span struct{ lo, hi uintptr }

// readOnlySpans picks the read-only mappings of the file with device
// (major, minor) and inode ino out of /proc/self/maps: an executable's code
// and constant data. Writable mappings are left out, since a private one
// may hold changes that dropping its pages would lose.
func readOnlySpans(maps string, major, minor uint32, ino uint64) ([]span, error) {
	var spans []span
	for line := range strings.Lines(maps) {
		// address perms offset dev inode [path]
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		perms, dev, inode := f[1], f[3], f[4]
		if len(perms) < 2 || perms[0] != 'r' || perms[1] == 'w' {
			continue
		}
		if i, err := strconv.ParseUint(inode, 10, 64); err != nil || i != ino {
			continue
		}
		maj, mnr, ok := strings.Cut(dev, ":")
		if !ok {
			continue
		}
		if m, err := strconv.ParseUint(maj, 16, 32); err != nil || m != uint64(major) {
			continue
		}
		if m, err := strconv.ParseUint(mnr, 16, 32); err != nil || m != uint64(minor) {
			continue
		}
		lo, hi, ok := strings.Cut(f[0], "-")
		if !ok {
			return nil, fmt.Errorf("can't read the address range %q in /proc/self/maps", f[0])
		}
		l, err := strconv.ParseUint(lo, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("can't read the address range %q in /proc/self/maps: %w", f[0], err)
		}
		h, err := strconv.ParseUint(hi, 16, 64)
		if err != nil || h < l {
			return nil, fmt.Errorf("can't read the address range %q in /proc/self/maps", f[0])
		}
		spans = append(spans, span{uintptr(l), uintptr(h)})
	}
	return spans, nil
}
