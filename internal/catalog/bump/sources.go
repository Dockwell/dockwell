// SPDX-License-Identifier: AGPL-3.0-only

package bump

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/registry"
)

// Sources are what a bump reads: registries, through ADR 0079's code, and the
// second channel that says a version is the upstream's own.
type Sources interface {
	// Tags are a repository's tags, for what's newer than tag: on Docker Hub with
	// their digests and when each was pushed.
	Tags(ctx context.Context, ref registry.Ref) ([]registry.Tag, error)
	// Digest is what a tag points at now.
	Digest(ctx context.Context, ref registry.Ref) (string, error)
	// Platforms are the platforms an image runs on.
	Platforms(ctx context.Context, ref registry.Ref) ([]registry.Platform, error)
	// Released says repository, a GitHub one, has published version as a release
	// that isn't a draft or a pre-release, or as a tag, with what it found.
	Released(ctx context.Context, repository, version string) (bool, string, error)
	// OfficialTag says a Docker Official Image's file in docker-library's
	// official-images, which Docker builds it from, names tag.
	OfficialTag(ctx context.Context, name, tag string) (bool, error)
}

// Live are the sources as a job reads them: registries anonymously, as an install
// does, and GitHub's API, with a token where one's given for its rate.
type Live struct {
	Registry *registry.Client
	// HTTP reaches GitHub; nil takes a client with a 30-second timeout.
	HTTP *http.Client
	// GitHubAPI and Raw are GitHub's API and where it serves a file of a
	// repository: https://api.github.com and https://raw.githubusercontent.com
	// unless a test names its own.
	GitHubAPI, Raw string
	// Token is a GitHub token for the API's rate, read-only; empty asks without.
	Token string
	// Tries is how many times Docker Hub is asked after it answers 429, and Wait
	// how long the first wait between is, doubling after.
	Tries int
	Wait  time.Duration
}

func (l *Live) client() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// retry asks again while a registry says it's asked too often (429), as Docker
// Hub does, within the run.
func retry[T any](ctx context.Context, l *Live, ask func() (T, error)) (T, error) {
	wait := l.Wait
	if wait == 0 {
		wait = 30 * time.Second
	}
	tries := max(l.Tries, 1)
	for i := 1; ; i++ {
		v, err := ask()
		if !errors.Is(err, registry.ErrLimited) || i >= tries {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// Tags reads a repository's tags as ADR 0079's check does: Docker Hub's 100 most
// recently pushed holding the track's word, with digests and push times.
func (l *Live) Tags(ctx context.Context, ref registry.Ref) ([]registry.Tag, error) {
	return retry(ctx, l, func() ([]registry.Tag, error) {
		return l.Registry.TagList(ctx, ref, registry.TrackWord(ref.Tag))
	})
}

// Digest asks with a HEAD, which no registry counts as a pull.
func (l *Live) Digest(ctx context.Context, ref registry.Ref) (string, error) {
	ref.Digest = ""
	return retry(ctx, l, func() (string, error) { return l.Registry.Head(ctx, ref) })
}

// Platforms reads an image's index.
func (l *Live) Platforms(ctx context.Context, ref registry.Ref) ([]registry.Platform, error) {
	return retry(ctx, l, func() ([]registry.Platform, error) {
		p, _, err := l.Registry.Platforms(ctx, ref)
		return p, err
	})
}

// GitHubRepository is a GitHub repository's owner/name, from an address such as
// https://github.com/WordPress/WordPress, or empty for one elsewhere.
func GitHubRepository(address string) string {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Host != "github.com" {
		return ""
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// Released looks for version as a release of the repository, tagged with or
// without a v, then as a tag.
func (l *Live) Released(ctx context.Context, repository, version string) (bool, string, error) {
	api := strings.TrimSuffix(cmpOr(l.GitHubAPI, "https://api.github.com"), "/")
	tags := []string{"v" + version, version}
	for _, tag := range tags {
		var release struct {
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			HTMLURL    string `json:"html_url"`
		}
		found, err := l.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", api, repository, url.PathEscape(tag)), &release)
		if err != nil {
			return false, "", err
		}
		switch {
		case !found:
			continue
		case release.Draft:
			return false, fmt.Sprintf("its release %s is a draft", tag), nil
		case release.Prerelease:
			return false, fmt.Sprintf("its release %s is a pre-release", tag), nil
		}
		return true, fmt.Sprintf("the release %s of github.com/%s", tag, repository), nil
	}
	for _, tag := range tags {
		var ref struct {
			Ref string `json:"ref"`
		}
		found, err := l.getJSON(ctx, fmt.Sprintf("%s/repos/%s/git/ref/tags/%s", api, repository, url.PathEscape(tag)), &ref)
		if err != nil {
			return false, "", err
		}
		if found && ref.Ref == "refs/tags/"+tag {
			return true, fmt.Sprintf("the tag %s of github.com/%s", tag, repository), nil
		}
	}
	return false, fmt.Sprintf("github.com/%s has no release or tag %s", repository, version), nil
}

func (l *Live) getJSON(ctx context.Context, target string, into any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if l.Token != "" {
		req.Header.Set("Authorization", "Bearer "+l.Token)
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return false, fmt.Errorf("asking GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("asking GitHub for %s: it answered %d", target, resp.StatusCode)
}

// OfficialTag reads the image's file in docker-library/official-images, whose
// Tags and SharedTags lines name every tag Docker builds from it.
func (l *Live) OfficialTag(ctx context.Context, name, tag string) (bool, error) {
	raw := strings.TrimSuffix(cmpOr(l.Raw, "https://raw.githubusercontent.com"), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/docker-library/official-images/master/library/%s", raw, url.PathEscape(name)), http.NoBody)
	if err != nil {
		return false, err
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return false, fmt.Errorf("reading docker-library's official-images: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("reading docker-library's file of %s: it answered %d", name, resp.StatusCode)
	}
	return OfficialTags(io.LimitReader(resp.Body, 4<<20), tag)
}

// OfficialTags says a library file of docker-library's names tag on a Tags or a
// SharedTags line.
func OfficialTags(r io.Reader, tag string) (bool, error) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 64<<10), 1<<20)
	for scan.Scan() {
		key, value, ok := strings.Cut(scan.Text(), ":")
		if !ok || key != "Tags" && key != "SharedTags" {
			continue
		}
		for t := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(t) == tag {
				return true, nil
			}
		}
	}
	return false, scan.Err()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// platformsDropped are the platforms before has that after hasn't.
func platformsDropped(before, after []registry.Platform) []string {
	var out []string
	for _, p := range before {
		if !slices.Contains(after, p) {
			out = append(out, p.String())
		}
	}
	return out
}
