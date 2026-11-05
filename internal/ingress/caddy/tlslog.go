// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dockwell/dockwell/internal/ingress"
)

// tlsLogName is where Caddy writes what its certificates are doing, in its
// data directory. Caddy has no API that says, and rolls this at 1 MB.
const tlsLogName = "dockwell-tls.log"

// logReader keeps what the log last said of each domain. It reads only what
// is new, so it costs next to nothing when nothing happens.
type logReader struct {
	offset int64
	last   map[string]ingress.CertState
}

// Bounds on what the log costs to read, as it is in Caddy's directory, and a
// compromised Caddy can write anything there: what one read takes, and how many
// names it remembers. Caddy rolls the log at 1 MB, and a server has at most
// ingress.MaxRoutes of them.
const (
	logReadLimit = 4 << 20
	logNames     = 4096
)

// read takes what is new in the log at path. It reads a regular file only,
// never through a link, and at most logReadLimit each time, leaving the rest
// for next time.
func (l *logReader) read(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("can't read what Caddy says of its certificates: %w", err)
	}
	defer root.Close()
	f, err := openRegular(root, filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("can't read what Caddy says of its certificates: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("can't read what Caddy says of its certificates: %w", err)
	}
	if info.Size() < l.offset {
		l.offset = 0 // Caddy rolled it
	}
	if _, err := f.Seek(l.offset, io.SeekStart); err != nil {
		return fmt.Errorf("can't read what Caddy says of its certificates: %w", err)
	}
	r := bufio.NewReader(io.LimitReader(f, logReadLimit))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// A line without its end is one Caddy is writing: it is read whole
			// next time. One longer than a whole read is no line of Caddy's,
			// and would hold up the log for ever.
			if len(line) >= logReadLimit {
				l.offset += int64(len(line))
			}
			return nil
		}
		l.offset += int64(len(line))
		l.take(line)
	}
}

// take applies one line of the log.
func (l *logReader) take(line []byte) {
	var e struct {
		Msg        string `json:"msg"`
		Identifier string `json:"identifier"`
		Error      string `json:"error"`
	}
	if json.Unmarshal(line, &e) != nil || e.Identifier == "" {
		return
	}
	if l.last == nil || len(l.last) >= logNames {
		l.last = map[string]ingress.CertState{}
	}
	switch {
	case strings.Contains(e.Msg, "certificate obtained successfully") || strings.Contains(e.Msg, "certificate renewed successfully"):
		l.last[e.Identifier] = ingress.CertState{Phase: ingress.CertIssued}
	case strings.Contains(e.Msg, "could not get certificate from issuer"):
		if problem, detail, failed := ingress.ClassifyFailure(e.Error); failed {
			l.last[e.Identifier] = ingress.CertState{Phase: ingress.CertFailed, Problem: problem, Detail: detail}
		}
	}
}

// state is what the log last said of a domain: pending until it says more.
func (l *logReader) state(domain string) ingress.CertState { return l.last[domain] }
