// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Tag is one of a repository's tags.
type Tag struct {
	Name string
	// Digest is what it points at, where that's known.
	Digest string
	// Pushed is when it was last pushed, where the registry says.
	Pushed time.Time
	// Latest is true for a tag that points where "latest" does.
	Latest bool
}

// Tags lists a repository's tags, newest first, MaxTags at most: Docker Hub's
// by when each was pushed, from its own API, and any other registry's by
// version, with the digests of "latest" and the newest versions asked with a
// HEAD each, to tell which point where "latest" does.
func (c *Client) Tags(ctx context.Context, r Ref) ([]Tag, error) {
	if r.Registry == Hub {
		return c.hubTags(ctx, r)
	}
	names, err := c.tagNames(ctx, r)
	if err != nil {
		return nil, err
	}
	SortTags(names)
	if len(names) > MaxTags {
		names = names[:MaxTags]
	}
	out := make([]Tag, len(names))
	for i, n := range names {
		out[i] = Tag{Name: n}
	}
	// Which point where latest does is a nicety: out of time, or with no latest,
	// the list stands without it.
	latest := ""
	if ctx.Err() == nil {
		latest, _ = c.Head(ctx, Ref{Registry: r.Registry, Repository: r.Repository, Tag: "latest"})
	}
	if latest == "" {
		return out, nil
	}
	asked := 0
	for i := range out {
		if out[i].Name == "latest" {
			out[i].Digest, out[i].Latest = latest, true
			continue
		}
		if asked >= latestOf || !versionLike(out[i].Name) || ctx.Err() != nil {
			continue
		}
		asked++
		d, err := c.Head(ctx, Ref{Registry: r.Registry, Repository: r.Repository, Tag: out[i].Name})
		if err != nil {
			continue
		}
		out[i].Digest, out[i].Latest = d, d == latest
	}
	return out, nil
}

// tagNames reads a registry's list of a repository's tags, a page at a time:
// tagsPerPage names of a page at most, maxTagNames in all, and what a page
// holds past maxTagPage bytes goes unread, as does any page once time is up.
func (c *Client) tagNames(ctx context.Context, r Ref) ([]string, error) {
	var out []string
	path := fmt.Sprintf("/v2/%s/tags/list?n=%d", r.Repository, tagsPerPage)
	for page := 0; page < maxTagPages && path != "" && len(out) < maxTagNames; page++ {
		if page > 0 && ctx.Err() != nil {
			break
		}
		resp, err := c.get(ctx, http.MethodGet, r.Registry, r.Repository, path, "application/json", false)
		if err != nil {
			if page > 0 {
				break
			}
			return nil, err
		}
		names, whole := readTagPage(resp.Body, min(tagsPerPage, maxTagNames-len(out)))
		next := nextPage(resp.Header.Get("Link"))
		drain(resp)
		out = append(out, names...)
		if !whole {
			// What a page held before it ran out, or past what Dockwell reads, stands.
			if len(out) == 0 {
				return nil, fmt.Errorf("%w: its tags can't be read", ErrUnreachable)
			}
			break
		}
		path = next
	}
	return out, nil //nolint:nilerr // a later page that doesn't come leaves the list as far as it came
}

// readTagPage reads a page of tags, {"name":…,"tags":[…]}, as it comes,
// keeping up to limit names that could be tags, and reading no more than
// maxTagPage bytes of it. It says whether it read as far as it meant to.
func readTagPage(body io.Reader, limit int) (names []string, whole bool) {
	dec := json.NewDecoder(io.LimitReader(body, maxTagPage))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return names, false
		}
		if key != "tags" {
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return names, false
			}
			continue
		}
		open, err := dec.Token()
		if err != nil || open != json.Delim('[') {
			// A repository with no tags says null.
			return names, err == nil && open == nil
		}
		for dec.More() && len(names) < limit {
			var name string
			if dec.Decode(&name) != nil {
				return names, false
			}
			if tagRe().MatchString(name) {
				names = append(names, name)
			}
		}
		return names, true
	}
	return names, true
}

// nextPage is the path a Link header names as the next page, such as
// </v2/acme/web/tags/list?last=1.2&n=1000>; rel="next".
func nextPage(link string) string {
	for part := range strings.SplitSeq(link, ",") {
		target, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.Contains(params, `rel="next"`) {
			continue
		}
		target = strings.Trim(strings.TrimSpace(target), "<>")
		u, err := url.Parse(target)
		if err != nil || !strings.HasPrefix(u.Path, "/v2/") {
			return ""
		}
		return u.RequestURI()
	}
	return ""
}

// hubTags reads Docker Hub's tags of a repository, newest pushed first, with
// their digests, from its own API, which isn't a pull.
func (c *Client) hubTags(ctx context.Context, r Ref) ([]Tag, error) {
	namespace, name, _ := strings.Cut(r.Repository, "/")
	target := fmt.Sprintf("%s/v2/namespaces/%s/repositories/%s/tags?page_size=100&ordering=last_updated",
		c.hubAPI(), url.PathEscape(namespace), url.PathEscape(name))
	resp, err := c.do(ctx, http.MethodGet, target, "application/json", "", false)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrNotFound
	case http.StatusTooManyRequests:
		return nil, ErrLimited
	default:
		return nil, fmt.Errorf("%w: it answered %d", ErrUnreachable, resp.StatusCode)
	}
	var page struct {
		Results []struct {
			Name          string `json:"name"`
			Digest        string `json:"digest"`
			LastUpdated   string `json:"last_updated"`
			TagLastPushed string `json:"tag_last_pushed"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("%w: its tags can't be read", ErrUnreachable)
	}
	out := make([]Tag, 0, len(page.Results))
	latest := ""
	for _, t := range page.Results {
		pushed := t.TagLastPushed
		if pushed == "" {
			pushed = t.LastUpdated
		}
		at, _ := time.Parse(time.RFC3339Nano, pushed)
		out = append(out, Tag{Name: t.Name, Digest: t.Digest, Pushed: at})
		if t.Name == "latest" {
			latest = t.Digest
		}
	}
	slices.SortStableFunc(out, func(a, b Tag) int { return b.Pushed.Compare(a.Pushed) })
	if latest == "" {
		// Its newest hundred don't hold latest, so ask where it points: a HEAD.
		latest, _ = c.Head(ctx, Ref{Registry: r.Registry, Repository: r.Repository, Tag: "latest"})
	}
	if latest != "" {
		for i := range out {
			out[i].Latest = out[i].Digest == latest
		}
	}
	return out, nil
}

var versionRe = lazyRe(`^v?(\d+(?:\.\d+)*)(?:([-+._])(.*))?$`)

// preRelease are the words that make a tag's suffix a version before its
// release, rather than a variant of it, such as 1.27-rc1 against 1.27-alpine.
var preRelease = []string{"alpha", "beta", "rc", "pre", "dev", "snapshot", "nightly"}

func versionLike(tag string) bool { return versionRe().MatchString(tag) }

type version struct {
	nums   []int
	suffix string
	pre    bool
	ok     bool
}

func parseVersion(tag string) version {
	m := versionRe().FindStringSubmatch(tag)
	if m == nil {
		return version{}
	}
	var v version
	v.ok = true
	for part := range strings.SplitSeq(m[1], ".") {
		n, _ := strconv.Atoi(part)
		v.nums = append(v.nums, n)
	}
	v.suffix = m[3]
	low := strings.ToLower(v.suffix)
	for _, w := range preRelease {
		if strings.HasPrefix(low, w) {
			v.pre = true
		}
	}
	return v
}

// SortTags orders tags newest version first: numbers compared as numbers, a
// release before its pre-releases, a plain version before its variants (1.27
// before 1.27-alpine), and what isn't a version, such as "latest" or "edge",
// after every version, by name. Each tag's version is read once.
func SortTags(tags []string) {
	keyed := make([]versioned, len(tags))
	for i, t := range tags {
		keyed[i] = versioned{t, parseVersion(t)}
	}
	slices.SortStableFunc(keyed, compareVersions)
	for i, k := range keyed {
		tags[i] = k.name
	}
}

type versioned struct {
	name string
	v    version
}

func compareVersions(x, y versioned) int {
	a, b, va, vb := x.name, y.name, x.v, y.v
	switch {
	case va.ok != vb.ok:
		if va.ok {
			return -1
		}
		return 1
	case !va.ok:
		return strings.Compare(a, b)
	}
	for i := range max(len(va.nums), len(vb.nums)) {
		// A number left out is 0, so 1.28 is 1.28.0, and 1.28.0-rc1 comes after it.
		x, y := 0, 0
		if i < len(va.nums) {
			x = va.nums[i]
		}
		if i < len(vb.nums) {
			y = vb.nums[i]
		}
		if x != y {
			return cmp.Compare(y, x)
		}
	}
	if va.pre != vb.pre {
		if va.pre {
			return 1
		}
		return -1
	}
	if (va.suffix == "") != (vb.suffix == "") {
		if va.suffix == "" {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
