// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubReleases is where Dockwell's releases are, unless a mirror says otherwise.
const GitHubReleases = "https://github.com/Dockwell/dockwell/releases"

// GitHubAPI lists Dockwell's newest releases, pre-releases with them, as each
// milestone's release is one, and latest/download skips them.
const GitHubAPI = "https://api.github.com/repos/Dockwell/dockwell/releases?per_page=10"

// ReleasesStale is how long the newest release stands before a look reads it
// again, and ReleaseFailedStale how long a read that failed does.
const (
	ReleasesStale      = 24 * time.Hour
	ReleaseFailedStale = time.Hour
)

// releaseTimeout bounds one read of the newest release, and maxChecksums and
// maxListing what of its checksums, or of the list of releases, are read.
const (
	releaseTimeout = 30 * time.Second
	maxChecksums   = 64 << 10
	maxListing     = 1 << 20
)

// archive is a line of a release's checksums.txt naming an archive of the full
// build, as the install script reads it.
var archive = regexp.MustCompile(`^[0-9a-f]{64}  dockwell_([0-9A-Za-z.+-]+)_linux_[0-9a-z]+\.tar\.gz$`)

// Releases reads which release of Dockwell is the newest, at most daily and only
// when someone looks: from GitHub's list of releases, pre-releases included, or,
// from a mirror, from the latest release's checksums, as the install script reads
// it. It downloads nothing else, and trusts what it reads only to show a version:
// updating is the install script's, which checks the signature (ADR 0043).
type Releases struct {
	// URL is the releases URL, such as GitHubReleases or a mirror, which a release
	// downloads from. API, when set, is where the list of releases is read instead.
	URL    string
	API    string
	Client *http.Client
	Logger *slog.Logger
	Now    func() time.Time
	// Agent is what a request says it is, as GitHub's API asks.
	Agent string

	mu      sync.Mutex
	newest  string
	at      time.Time
	failed  bool
	running chan struct{}
}

// NewReleases reads releases from url: GitHub's through its API, and a mirror's
// through its latest release's checksums.
func NewReleases(url, version string, logger *slog.Logger) *Releases {
	r := &Releases{
		URL: strings.TrimSuffix(url, "/"), Client: &http.Client{Timeout: releaseTimeout}, Logger: logger, Now: time.Now,
		Agent: "dockwell/" + cmp.Or(version, "dev"),
	}
	if r.URL == GitHubReleases {
		r.API = GitHubAPI
	}
	return r
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
		// One warning for each run of failures, rather than one each hour.
		switch {
		case err != nil && !r.failed:
			r.Logger.Warn("couldn't read Dockwell's newest release; it's tried again within the hour", slog.String("from", r.from()), slog.Any("error", err))
		case err == nil && r.failed:
			r.Logger.Info("read Dockwell's newest release again", slog.String("from", r.from()))
		}
		r.at, r.failed, r.running = r.Now(), err != nil, nil
		if err == nil {
			r.newest = v
		}
		close(done)
	}()
	return done
}

// from is where the newest release is read.
func (r *Releases) from() string { return cmp.Or(r.API, r.URL) }

// read is the newest release's version.
func (r *Releases) read(ctx context.Context) (string, error) {
	if r.API != "" {
		return r.readAPI(ctx)
	}
	return r.readChecksums(ctx)
}

// get reads url, up to limit bytes, or fails with what it answered.
func (r *Releases) get(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.Agent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}

// readAPI is the newest release on GitHub's list, by version, pre-releases
// included and drafts not.
func (r *Releases) readAPI(ctx context.Context) (string, error) {
	b, err := r.get(ctx, r.API, "application/vnd.github+json", maxListing)
	if err != nil {
		return "", err
	}
	var listed []struct {
		Tag   string `json:"tag_name"`
		Draft bool   `json:"draft"`
	}
	if err := json.Unmarshal(b, &listed); err != nil {
		return "", fmt.Errorf("%s answered what isn't a list of releases: %w", r.API, err)
	}
	newest := ""
	for _, l := range listed {
		if l.Draft || !isVersion(l.Tag) {
			continue
		}
		if newest == "" || CompareVersions(l.Tag, newest) > 0 {
			newest = l.Tag
		}
	}
	if newest == "" {
		return "", fmt.Errorf("%s lists no release", r.API)
	}
	return strings.TrimPrefix(newest, "v"), nil
}

// readChecksums is the latest release's version, from its checksums.
func (r *Releases) readChecksums(ctx context.Context) (string, error) {
	url := r.URL + "/latest/download/checksums.txt"
	b, err := r.get(ctx, url, "", maxChecksums)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if m := archive.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("%s names no archive of Dockwell", url)
}

// semver is a release's version: three numbers, then a pre-release's parts.
var semver = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

func isVersion(v string) bool { return semver.MatchString(v) }

// CompareVersions orders two release versions as semver does: by their three
// numbers, then a release after its pre-releases, and pre-releases part by part,
// numbers by value and before words.
func CompareVersions(a, b string) int {
	ma, mb := semver.FindStringSubmatch(a), semver.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return strings.Compare(a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.ParseUint(ma[i], 10, 64)
		y, _ := strconv.ParseUint(mb[i], 10, 64)
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	switch pa, pb := ma[4], mb[4]; {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	default:
		return comparePre(strings.Split(pa, "."), strings.Split(pb, "."))
	}
}

func comparePre(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, ex := strconv.ParseUint(a[i], 10, 64)
		y, ey := strconv.ParseUint(b[i], 10, 64)
		switch {
		case ex == nil && ey == nil:
			if c := cmp.Compare(x, y); c != 0 {
				return c
			}
		case ex == nil:
			return -1
		case ey == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(len(a), len(b))
}
