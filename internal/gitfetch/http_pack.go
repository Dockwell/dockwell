// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	// DefaultMaxDepth is the shallow depth bound when verifying an older commit
	// on a branch or tag over smart HTTP (matches gitssh.DefaultMaxDepth).
	DefaultMaxDepth   = 200
	maxHTTPRefs       = 10_000
	maxSameOriginHops = 2
	packFetchTimeout  = 2 * time.Minute
)

// PackFetchOptions configures a control-plane shallow Git pack fetch over
// smart HTTP (v2 preferred, v1 fallback) with SHA-1DC verification (ADR 0113).
type PackFetchOptions struct {
	// RepoURL is the repository base URL (HTTPS in production; may be a test
	// HTTP URL when Client is supplied).
	RepoURL string
	// Ref is the branch or tag name (short or full refs/heads/... / refs/tags/...)
	// or commit SHA. When non-empty and distinct from Commit, FetchVerifiedPack
	// verifies that Commit is an ancestor of Ref's tip.
	Ref string
	// Commit is the full 40-hex commit SHA to fetch and verify.
	Commit string
	// AuthHeader is an optional HTTP Authorization header value (such as
	// "Basic ..."). It is never logged, never included in errors, and never sent
	// to any host or scheme other than RepoURL's own.
	AuthHeader string
	// CredentialLabel names the credential in 401/403 error messages (for
	// example, "the GitLab connection corp's token"). Empty defaults to "the credentials".
	CredentialLabel string
	// Client overrides the outbound HTTP client in tests. When nil, netguard's
	// client is used with Private: AllowPrivate and no automatic redirects.
	Client *http.Client
	// AllowPrivate lets netguard dial RFC 1918 / ULA / CGNAT addresses (for
	// admin-configured self-managed GitLab hosts) while still blocking loopback,
	// link-local, metadata, multicast and reserved addresses.
	AllowPrivate bool
	// PackTempDir is the Dockwell-owned scratch directory where the temporary
	// pack file is staged. Empty falls back to os.TempDir().
	PackTempDir string
	// MaxPack bounds the received pack size in bytes (defaults to Default.Pack).
	MaxPack int64
	// MaxDepth bounds shallow history depth when verifying an older commit on
	// Ref (defaults to DefaultMaxDepth).
	MaxDepth int
}

// FetchVerifiedPack fetches a shallow Git pack over smart HTTP (protocol v2
// with v1 fallback), verifies every object with SHA-1DC and bounds via
// VerifyPack (including commit-on-ref ancestry), and returns the open temp
// file rewound to offset 0. The caller must close and remove the file.
func FetchVerifiedPack(ctx context.Context, opts PackFetchOptions) (*os.File, int64, error) {
	commit := strings.ToLower(strings.TrimSpace(opts.Commit))
	if !Commit.MatchString(commit) {
		return nil, 0, fmt.Errorf("%q isn't a commit. Name one in full, as 40 hex digits", opts.Commit)
	}
	u, err := checkPackURL(opts.RepoURL, opts.Client != nil, opts.AllowPrivate)
	if err != nil {
		return nil, 0, err
	}
	maxPack := opts.MaxPack
	if maxPack <= 0 {
		maxPack = Default.Pack
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	ctx, cancel := context.WithTimeout(ctx, packFetchTimeout)
	defer cancel()

	via := packHTTPClient(opts.Client, opts.AllowPrivate, maxPack+1<<20)

	credLabel := strings.TrimSpace(opts.CredentialLabel)
	if credLabel == "" {
		credLabel = "the credentials"
	}

	// Step 1: discover capabilities and refs (v2 preferred, v1 fallback),
	// following only exact same-origin .git suffix redirects (such as /repo -> /repo.git)
	// so AuthHeader can never be sent to another host, port, or repository path.
	base, info, err := discoverSmartHTTP(ctx, via, u, opts.AuthHeader, credLabel)
	if err != nil {
		return nil, 0, err
	}

	var wantCommit, refTip string
	depth := 1
	ref := strings.TrimSpace(opts.Ref)
	if ref == "" || strings.EqualFold(ref, commit) {
		wantCommit = commit
	} else {
		var refs map[string]string
		var head string
		if info.v2 {
			refs, head, err = lsRefsV2HTTP(ctx, via, base, opts.AuthHeader)
			if err != nil {
				return nil, 0, err
			}
		} else {
			refs, head = info.v1Refs, info.v1Head
		}
		tip, err := resolveHTTPCommit(base, refs, head, ref)
		if err != nil {
			return nil, 0, err
		}
		refTip = tip
		wantCommit = tip
		if tip != commit {
			depth = maxDepth
		}
	}

	if opts.PackTempDir != "" {
		if err := os.MkdirAll(opts.PackTempDir, 0o700); err != nil {
			return nil, 0, fmt.Errorf("preparing pack scratch directory: %w", err)
		}
	}
	f, err := os.CreateTemp(opts.PackTempDir, "dockwell-git-pack-*")
	if err != nil {
		return nil, 0, fmt.Errorf("keeping what %s sends: %w", base.Host, err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}

	var size int64
	if info.v2 {
		size, err = fetchHTTPV2(ctx, via, base, opts.AuthHeader, credLabel, f, wantCommit, depth, maxPack, info.v2Shallow, info.v2Format)
	} else {
		size, err = fetchHTTPV1(ctx, via, base, opts.AuthHeader, credLabel, f, wantCommit, depth, maxPack, info.v1Caps)
	}
	if err != nil {
		cleanup()
		return nil, 0, err
	}
	if err := VerifyPack(ctx, f, size, 4*maxPack, commit, refTip); err != nil {
		cleanup()
		if errors.Is(err, ErrCommitNotOnRef) {
			return nil, 0, fmt.Errorf("%s isn't on %s of %s, so Dockwell won't build it: a build's commit has to be on the branch or tag it names", commit[:7], ref, shownPackRepo(base))
		}
		return nil, 0, fmt.Errorf("reading what %s sent: %w", base.Host, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, err
	}
	return f, size, nil
}

func checkPackURL(raw string, customClient, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q isn't a repository's URL", raw)
	}
	if u.User != nil {
		return nil, errors.New("the repository's URL can't hold credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q has a query or a fragment, which a repository's URL doesn't", raw)
	}
	if !customClient {
		if u.Scheme != "https" {
			return nil, fmt.Errorf("%q isn't over HTTPS, so Dockwell won't send credentials there", raw)
		}
		if why := netguard.Forbidden(u.Hostname(), netguard.Options{Private: allowPrivate}); why != "" {
			return nil, fmt.Errorf("%s is %s, so Dockwell won't fetch there", u.Hostname(), why)
		}
	} else if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("%q isn't an HTTP(S) repository URL", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func packHTTPClient(client *http.Client, allowPrivate bool, most int64) doer {
	if client == nil {
		return netguard.Client(netguard.Options{Private: allowPrivate, MaxBody: most, Timeout: packFetchTimeout})
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func shownPackRepo(u *url.URL) string {
	return u.Host + strings.TrimSuffix(u.Path, ".git")
}

type smartHTTPInfo struct {
	v2        bool
	v2Shallow bool
	v2Format  string
	v1Refs    map[string]string
	v1Head    string
	v1Caps    map[string]bool
}

func doSameOrigin(client doer, orig *url.URL, makeReq func(base *url.URL) (*http.Request, error), authHeader string) (*url.URL, *http.Response, error) {
	cur := *orig
	for hop := 0; hop <= maxSameOriginHops; hop++ {
		req, err := makeReq(&cur)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", "git/dockwell")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode != http.StatusMovedPermanently &&
			resp.StatusCode != http.StatusFound &&
			resp.StatusCode != http.StatusTemporaryRedirect &&
			resp.StatusCode != http.StatusPermanentRedirect {
			return &cur, resp, nil
		}
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		_ = resp.Body.Close()
		if loc == "" || hop == maxSameOriginHops {
			return nil, nil, fmt.Errorf("%s redirected too many times when asked for the repository", orig.Host)
		}
		next, err := req.URL.Parse(loc)
		if err != nil {
			return nil, nil, fmt.Errorf("%s sent an invalid redirect", orig.Host)
		}
		if next.User != nil {
			return nil, nil, fmt.Errorf("%s redirected to a URL with credentials, which Dockwell refuses", orig.Host)
		}
		if !strings.EqualFold(next.Scheme, orig.Scheme) || !strings.EqualFold(next.Host, orig.Host) {
			return nil, nil, fmt.Errorf("%s redirected to another host or port (%s), so Dockwell refused to send credentials there", orig.Host, next.Host)
		}
		// Only allow same-repository .git suffix normalization (such as /repo -> /repo.git).
		origRepo := strings.TrimSuffix(strings.TrimRight(orig.Path, "/"), ".git")
		p := next.Path
		p = strings.TrimSuffix(p, "/info/refs")
		p = strings.TrimSuffix(p, "/git-upload-pack")
		nextBase := strings.TrimRight(p, "/")
		nextRepo := strings.TrimSuffix(nextBase, ".git")
		if nextRepo != origRepo {
			return nil, nil, fmt.Errorf("%s redirected to another repository path (%s), so Dockwell refused to follow it", orig.Host, nextBase)
		}
		cur.Path = nextBase
	}
	return nil, nil, fmt.Errorf("%s redirected too many times", orig.Host)
}

func discoverSmartHTTP(ctx context.Context, client doer, u *url.URL, authHeader, credLabel string) (*url.URL, smartHTTPInfo, error) {
	base, resp, err := doSameOrigin(client, u, func(b *url.URL) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.String()+"/info/refs?service=git-upload-pack", http.NoBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Git-Protocol", "version=2")
		return req, nil
	}, authHeader)
	if err != nil {
		return nil, smartHTTPInfo{}, fmt.Errorf("asking %s for the repository: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, smartHTTPInfo{}, fmt.Errorf("%s refused %s (%s). Check that the credential is active and can read this repository", base.Host, credLabel, status(resp))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, smartHTTPInfo{}, fmt.Errorf("%s answered %s for %s", base.Host, status(resp), base.Path)
	}
	info := smartHTTPInfo{
		v2Format: "sha1",
		v1Refs:   map[string]string{},
		v1Caps:   map[string]bool{},
	}
	r := bufio.NewReader(io.LimitReader(resp.Body, 4<<20))
	first := true
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, smartHTTPInfo{}, fmt.Errorf("reading what %s speaks: %w", base.Host, err)
		}
		if kind != pktData {
			continue
		}
		text := strings.TrimSuffix(string(line), "\n")
		if strings.HasPrefix(text, "# service=") {
			continue
		}
		if first {
			first = false
			if text == "version 2" {
				info.v2 = true
				continue
			}
			if i := strings.IndexByte(text, 0); i >= 0 {
				for _, c := range strings.Fields(text[i+1:]) {
					info.v1Caps[c] = true
					if k, _, ok := strings.Cut(c, "="); ok {
						info.v1Caps[k] = true
					}
					if target, ok := strings.CutPrefix(c, "symref=HEAD:refs/heads/"); ok {
						info.v1Head = target
					}
				}
				text = text[:i]
			}
			addHTTPRef(info.v1Refs, text)
			continue
		}
		if info.v2 {
			switch {
			case strings.HasPrefix(text, "fetch"):
				features := " " + strings.TrimPrefix(text, "fetch=") + " "
				info.v2Shallow = strings.Contains(features, " shallow ")
			case strings.HasPrefix(text, "object-format="):
				info.v2Format = strings.TrimPrefix(text, "object-format=")
			}
		} else {
			addHTTPRef(info.v1Refs, text)
		}
	}
	return base, info, nil
}

func addHTTPRef(refs map[string]string, text string) {
	commit, name, ok := strings.Cut(text, " ")
	if !ok || !Commit.MatchString(commit) || name == "capabilities^{}" {
		return
	}
	if !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/") {
		return
	}
	if tag, peeled := strings.CutSuffix(name, "^{}"); peeled {
		if _, exists := refs[tag]; exists || len(refs) < maxHTTPRefs {
			refs[tag] = commit
		}
		return
	}
	if _, exists := refs[name]; !exists {
		if len(refs) < maxHTTPRefs {
			refs[name] = commit
		}
	} else if !strings.HasPrefix(name, "refs/tags/") {
		refs[name] = commit
	}
}

func lsRefsV2HTTP(ctx context.Context, client doer, base *url.URL, authHeader string) (map[string]string, string, error) {
	var body bytes.Buffer
	for _, line := range []string{"command=ls-refs\n", "agent=dockwell\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0001")
	for _, line := range []string{"peel\n", "symrefs\n", "ref-prefix HEAD\n", "ref-prefix refs/heads/\n", "ref-prefix refs/tags/\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0000")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/git-upload-pack", &body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "git/dockwell")
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("asking %s for its branches and tags: %w", base.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s answered %s when asked for its branches and tags", base.Host, status(resp))
	}
	r := bufio.NewReader(io.LimitReader(resp.Body, 4<<20))
	refs := map[string]string{}
	head := ""
	for {
		line, kind, err := readPkt(r)
		switch {
		case errors.Is(err, io.EOF) || kind == pktFlush && err == nil:
			return refs, head, nil
		case err != nil:
			return nil, "", fmt.Errorf("reading %s's branches and tags: %w", base.Host, err)
		case kind != pktData:
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) < 2 || !Commit.MatchString(fields[0]) {
			return nil, "", fmt.Errorf("reading %s's branches and tags: a line isn't one", base.Host)
		}
		at := fields[0]
		for _, attr := range fields[2:] {
			if peeled, ok := strings.CutPrefix(attr, "peeled:"); ok && Commit.MatchString(peeled) {
				at = peeled
			}
			if target, ok := strings.CutPrefix(attr, "symref-target:refs/heads/"); ok && fields[1] == "HEAD" {
				head = target
			}
		}
		if fields[1] == "HEAD" {
			continue
		}
		if len(refs) >= maxHTTPRefs {
			return nil, "", fmt.Errorf("%s has over %d branches and tags, more than Dockwell lists", base.Host, maxHTTPRefs)
		}
		refs[fields[1]] = at
	}
}

func resolveHTTPCommit(base *url.URL, refs map[string]string, head, ref string) (string, error) {
	names := []string{"refs/heads/" + ref, "refs/tags/" + ref}
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
		names = []string{ref}
	}
	for _, full := range names {
		if c, ok := refs[full]; ok {
			return c, nil
		}
	}
	var branches []string
	for full := range refs {
		if b, ok := strings.CutPrefix(full, "refs/heads/"); ok && b != head {
			branches = append(branches, b)
		}
	}
	slices.Sort(branches)
	if head != "" {
		branches = append([]string{head}, branches...)
	}
	where := shownPackRepo(base)
	if len(branches) == 0 {
		return "", fmt.Errorf("%s has no branch or tag %s", where, ref)
	}
	shown := branches
	more := ""
	if len(shown) > 5 {
		more = fmt.Sprintf(", and %d more", len(shown)-5)
		shown = shown[:5]
	}
	return "", fmt.Errorf("%s has no branch or tag %s. Its branches are %s%s", where, ref, strings.Join(shown, ", "), more)
}

func fetchHTTPV2(ctx context.Context, client doer, base *url.URL, authHeader, credLabel string, f *os.File, commit string, depth int, most int64, shallow bool, format string) (int64, error) {
	if format != "sha1" {
		return 0, fmt.Errorf("the repository at %s names its objects by %s, where Dockwell reads SHA-1 alone", base.Host, strings.ToUpper(format))
	}
	var body bytes.Buffer
	for _, line := range []string{"command=fetch\n", "agent=dockwell\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0001")
	writePkt(&body, "no-progress\n")
	writePkt(&body, "ofs-delta\n")
	if shallow && depth > 0 {
		writePkt(&body, fmt.Sprintf("deepen %d\n", depth))
	}
	writePkt(&body, "want "+commit+"\n")
	writePkt(&body, "done\n")
	body.WriteString("0000")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/git-upload-pack", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "git/dockwell")
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetching %s from %s: %w", commit, base.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 0, fmt.Errorf("%s refused %s (%s) when fetching %s", base.Host, credLabel, status(resp), commit[:7])
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s when asked for %s", base.Host, status(resp), commit)
	}
	overLimit := fmt.Sprintf("the commit %s's pack is over %d MiB, more than Dockwell reads", commit, most>>20)
	return receivePackLimit(resp.Body, f, base, commit, most, overLimit)
}

func fetchHTTPV1(ctx context.Context, client doer, base *url.URL, authHeader, credLabel string, f *os.File, commit string, depth int, most int64, caps map[string]bool) (int64, error) {
	var wantCaps []string
	if caps["side-band-64k"] {
		wantCaps = append(wantCaps, "side-band-64k")
	} else if caps["side-band"] {
		wantCaps = append(wantCaps, "side-band")
	}
	if caps["ofs-delta"] {
		wantCaps = append(wantCaps, "ofs-delta")
	}
	if caps["no-progress"] {
		wantCaps = append(wantCaps, "no-progress")
	}
	var body bytes.Buffer
	wantLine := "want " + commit
	if len(wantCaps) > 0 {
		wantLine += " " + strings.Join(wantCaps, " ")
	}
	writePkt(&body, wantLine+"\n")
	if caps["shallow"] && depth > 0 {
		writePkt(&body, fmt.Sprintf("deepen %d\n", depth))
	}
	body.WriteString("0000")
	writePkt(&body, "done\n")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/git-upload-pack", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "git/dockwell")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetching %s from %s: %w", commit, base.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 0, fmt.Errorf("%s refused %s (%s) when fetching %s", base.Host, credLabel, status(resp), commit[:7])
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s when asked for %s", base.Host, status(resp), commit)
	}
	useSideband := caps["side-band-64k"] || caps["side-band"]
	if !useSideband {
		return receiveRawV1Pack(resp.Body, f, base, commit, most)
	}
	return receiveV1SidebandPack(resp.Body, f, base, commit, most)
}

func receiveRawV1Pack(body io.Reader, f *os.File, u *url.URL, commit string, most int64) (int64, error) {
	r := bufio.NewReader(body)
	for {
		peek, err := r.Peek(4)
		if err != nil {
			return 0, fmt.Errorf("reading what %s sent: %w", u.Host, err)
		}
		if string(peek) == "PACK" {
			break
		}
		line, kind, err := readPkt(r)
		if err != nil {
			return 0, fmt.Errorf("reading what %s sent: %w", u.Host, err)
		}
		if kind == pktData {
			text := strings.TrimSuffix(string(line), "\n")
			if strings.HasPrefix(text, "ERR ") {
				return 0, fmt.Errorf("%s refused to send %s. Check the commit is in the repository", u.Host, commit)
			}
			if text == "NAK" {
				break
			}
		}
	}
	n, err := io.Copy(f, io.LimitReader(r, most+1))
	if err != nil {
		return 0, fmt.Errorf("keeping what %s sends: %w", u.Host, err)
	}
	if n > most {
		return 0, fmt.Errorf("the commit %s's pack is over %d MiB, more than Dockwell reads", commit, most>>20)
	}
	if n == 0 {
		return 0, fmt.Errorf("%s sent no objects for %s. Check the commit is in the repository", u.Host, commit)
	}
	return n, nil
}

func receiveV1SidebandPack(body io.Reader, f *os.File, u *url.URL, commit string, most int64) (int64, error) {
	r := bufio.NewReader(body)
	w := bufio.NewWriter(f)
	var size int64
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, fmt.Errorf("reading what %s sent: %w", u.Host, err)
		}
		if kind == pktFlush {
			if size > 0 {
				break
			}
			continue
		}
		if kind != pktData {
			continue
		}
		if size == 0 {
			text := strings.TrimSuffix(string(line), "\n")
			if text == "NAK" || strings.HasPrefix(text, "shallow ") || strings.HasPrefix(text, "unshallow ") || strings.HasPrefix(text, "ACK ") {
				continue
			}
			if strings.HasPrefix(text, "ERR ") {
				return 0, fmt.Errorf("%s refused to send %s. Check the commit is in the repository", u.Host, commit)
			}
		}
		switch line[0] {
		case 1:
			if size += int64(len(line) - 1); size > most {
				return 0, fmt.Errorf("the commit %s's pack is over %d MiB, more than Dockwell reads", commit, most>>20)
			}
			if _, err := w.Write(line[1:]); err != nil {
				return 0, fmt.Errorf("keeping what %s sends: %w", u.Host, err)
			}
		case 3:
			return 0, fmt.Errorf("%s stopped sending %s. Try again, or check the repository", u.Host, commit)
		}
	}
	if size == 0 {
		return 0, fmt.Errorf("%s sent no objects for %s. Check the commit is in the repository", u.Host, commit)
	}
	if err := w.Flush(); err != nil {
		return 0, fmt.Errorf("keeping what %s sends: %w", u.Host, err)
	}
	return size, nil
}
