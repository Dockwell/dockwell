// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GitHubReleases is where Dockwell's releases are, unless a mirror says otherwise.
const GitHubReleases = "https://github.com/Dockwell/dockwell/releases"

// ReleasesStale is how long the newest release stands before a look reads it
// again, and ReleaseFailedStale how long a read that failed does.
const (
	ReleasesStale      = 24 * time.Hour
	ReleaseFailedStale = time.Hour
)

// releaseTimeout bounds one read of the newest release, and maxChecksums what
// of its checksums are read.
const (
	releaseTimeout = 30 * time.Second
	maxChecksums   = 64 << 10
)

// archive is a line of a release's checksums.txt naming an archive of the full
// build, as the install script reads it.
var archive = regexp.MustCompile(`^[0-9a-f]{64}  dockwell_([0-9A-Za-z.+-]+)_linux_[0-9a-z]+\.tar\.gz$`)

// Releases reads which release of Dockwell is the newest, from a releases URL in
// GitHub's layout, at most daily and only when someone looks. It downloads nothing
// but the release's checksums, and trusts what they say only to show a version:
// updating is the install script's, which checks the signature (ADR 0043).
type Releases struct {
	// URL is the releases URL, such as GitHubReleases or a mirror.
	URL    string
	Client *http.Client
	Logger *slog.Logger
	Now    func() time.Time

	mu      sync.Mutex
	newest  string
	at      time.Time
	failed  bool
	running chan struct{}
}

// NewReleases reads releases from url.
func NewReleases(url string, logger *slog.Logger) *Releases {
	return &Releases{URL: strings.TrimSuffix(url, "/"), Client: &http.Client{Timeout: releaseTimeout}, Logger: logger, Now: time.Now}
}

// Newest is the newest release, and when it was read, or "" when it isn't known.
func (r *Releases) Newest() (string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.newest, r.at
}

// Look reads the newest release again when what's known is stale, and returns a
// channel closed once it's read, or nil when there's nothing to wait for.
func (r *Releases) Look() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != nil {
		return r.running
	}
	stale := ReleasesStale
	if r.failed {
		stale = ReleaseFailedStale
	}
	if !r.at.IsZero() && r.Now().Sub(r.at) < stale {
		return nil
	}
	done := make(chan struct{})
	r.running = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		v, err := r.read(ctx)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.at, r.failed, r.running = r.Now(), err != nil, nil
		if err != nil {
			r.Logger.Warn("couldn't read Dockwell's newest release", slog.String("from", r.URL), slog.Any("error", err))
		} else {
			r.newest = v
		}
		close(done)
	}()
	return done
}

// read is the newest release's version, from its checksums.
func (r *Releases) read(ctx context.Context) (string, error) {
	url := r.URL + "/latest/download/checksums.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	res, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, res.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(res.Body, maxChecksums))
	for sc.Scan() {
		if m := archive.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			return m[1], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s names no archive of Dockwell", url)
}
