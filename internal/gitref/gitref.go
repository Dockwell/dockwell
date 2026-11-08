// SPDX-License-Identifier: AGPL-3.0-only

// Package gitref reads what a Git repository's branches and tags name, over Git's
// smart HTTP protocol, as git ls-remote does, so a change can pin the commit it
// builds as it is planned (ADR 0046). It asks for the refs and nothing else, and
// says no more of an answer than which refs it has. The control plane runs it;
// agents don't.
package gitref

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Bounds on asking a repository.
const (
	// Timeout bounds one question, as a plan waits on it.
	Timeout = 10 * time.Second
	// maxAnswer bounds what a repository's refs may come to.
	maxAnswer = 32 << 20
	// maxRedirects bounds how far a repository may send the question on.
	maxRedirects = 5
)

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Resolver reads repositories' refs.
type Resolver struct {
	// Client asks; one with Timeout, the bounds above and Guard unless set.
	Client *http.Client
	// Local lets it ask this machine and link-local addresses, as a test's Git
	// server is; a control plane's never does (Guard).
	Local bool
}

// Guard refuses what a lookup of a repository may not dial: this machine, a
// link-local address such as a cloud's metadata service, or nowhere, unless local
// says it's a test's; and over plain HTTP, anything but a private network, as
// anyone between could choose the commit a plan pins. So no token can make the
// control plane ask its own ports, or map a network by what answers.
func Guard(local, plain bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("%s isn't an address", host)
		}
		switch {
		case ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
			return fmt.Errorf("%s isn't an address a repository is at", ip)
		case !local && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()):
			return fmt.Errorf("%s is this server, or link-local, which a repository's URL can't lead to", ip)
		case plain && !ip.IsPrivate() && (!local || !ip.IsLoopback()):
			return fmt.Errorf("%s isn't on a private network, and Dockwell fetches over plain HTTP only from one", ip)
		}
		return nil
	}
}

// Refs is what a repository's branches and tags name.
type Refs struct {
	// Head is the branch HEAD names, such as "main", when the repository says.
	Head string
	// Commits is the commit each ref names, by its full name, such as
	// "refs/heads/main", an annotated tag's being the commit it points at.
	Commits map[string]string
}

// Resolve gives the commit ref names in the repository at url: a branch's, a
// tag's, or with ref empty the default branch's. A commit's ID is itself, unasked.
func (r *Resolver) Resolve(ctx context.Context, url, ref string) (commit, branch string, err error) {
	if commitID.MatchString(strings.ToLower(ref)) {
		return strings.ToLower(ref), "", nil
	}
	refs, err := r.List(ctx, url)
	if err != nil {
		return "", "", err
	}
	where := shown(url)
	if ref == "" {
		if refs.Head == "" {
			return "", "", fmt.Errorf("%s doesn't say what its default branch is, as an empty repository doesn't. Name a branch, or push one", where)
		}
		return refs.Commits["refs/heads/"+refs.Head], "refs/heads/" + refs.Head, nil
	}
	// A branch or a tag, and nothing else a repository advertises, such as a pull
	// request's head, which a fork pushes. A branch comes first where a tag has its
	// name too, and the build fetches what this says by that full name.
	names := []string{"refs/heads/" + ref, "refs/tags/" + ref}
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
		names = []string{ref}
	}
	for _, full := range names {
		if c, ok := refs.Commits[full]; ok {
			return c, full, nil
		}
	}
	branches := refs.branches()
	switch {
	case len(branches) == 0:
		return "", "", fmt.Errorf("%s has no branch or tag %s, and no branches at all yet. Push one, then try again", where, ref)
	case len(branches) > 5:
		return "", "", fmt.Errorf("%s has no branch or tag %s. Its branches include %s", where, ref, list(branches[:5]))
	}
	return "", "", fmt.Errorf("%s has no branch or tag %s. Its branches are %s", where, ref, list(branches))
}

// branches are the repository's branches, the default first.
func (r Refs) branches() []string {
	var out []string
	for full := range r.Commits {
		if b, ok := strings.CutPrefix(full, "refs/heads/"); ok && b != r.Head {
			out = append(out, b)
		}
	}
	slices.Sort(out)
	if r.Head != "" {
		out = append([]string{r.Head}, out...)
	}
	return out
}

// List reads the repository's refs.
func (r *Resolver) List(ctx context.Context, url string) (Refs, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	where := shown(url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return Refs{}, fmt.Errorf("%s isn't a URL Dockwell can ask: %w", where, err)
	}
	req.Header.Set("User-Agent", "git/dockwell")
	res, err := r.client(req.URL.Scheme == "http").Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Refs{}, fmt.Errorf("%s didn't answer within %s. Check the URL, and that the control server reaches it", where, Timeout)
		}
		return Refs{}, fmt.Errorf("can't reach %s: %w. Check the URL, and that the control server reaches it", where, unwrapURL(err))
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
		return Refs{}, fmt.Errorf("%s wants credentials, so it's private, or isn't there. Check the URL; Dockwell builds public repositories for now", where)
	case res.StatusCode == http.StatusNotFound:
		return Refs{}, fmt.Errorf("there's no repository at %s. Check the URL", where)
	case res.StatusCode != http.StatusOK:
		return Refs{}, fmt.Errorf("%s answered %s when asked for its branches. Check the URL", where, res.Status)
	}
	body := io.LimitReader(res.Body, maxAnswer+1)
	var refs Refs
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/x-git-upload-pack-advertisement") {
		refs, err = readAdvertisement(body)
	} else {
		refs, err = readDumb(body)
	}
	if err != nil {
		return Refs{}, fmt.Errorf("%s answered with something that isn't a Git repository's branches: %w. Check the URL", where, err)
	}
	return refs, nil
}

func (r *Resolver) client(plain bool) *http.Client {
	if r.Client != nil {
		return r.Client
	}
	dialer := &net.Dialer{Timeout: Timeout, Control: Guard(r.Local, plain)}
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own
	transport.Proxy, transport.DialContext = nil, dialer.DialContext
	return &http.Client{
		Timeout:   Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("it sent the question on over plain HTTP")
			}
			return nil
		},
	}
}

// readAdvertisement reads what git-upload-pack says of its refs, in pkt-lines:
// "# service=git-upload-pack", a flush, then a ref to a line, the first with the
// server's capabilities after a zero byte, then a flush.
func readAdvertisement(r io.Reader) (Refs, error) {
	br := bufio.NewReader(r)
	refs := Refs{Commits: map[string]string{}}
	read, flushes, first := 0, 0, true
	for {
		var head [4]byte
		if _, err := io.ReadFull(br, head[:]); err != nil {
			if errors.Is(err, io.EOF) && flushes > 0 {
				return refs, nil
			}
			return refs, errors.New("its answer ended partway")
		}
		n, err := strconv.ParseUint(string(head[:]), 16, 16)
		if err != nil {
			return refs, errors.New("a line of its answer has no length")
		}
		if n == 0 {
			flushes++
			if flushes == 2 || flushes == 1 && read > 0 && !first {
				return refs, nil
			}
			continue
		}
		if n < 4 {
			return refs, errors.New("a line of its answer is too short")
		}
		read += int(n)
		if read > maxAnswer {
			return refs, errors.New("its branches and tags come to more than 32 MiB")
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return refs, errors.New("its answer ended partway through a line")
		}
		text := strings.TrimSuffix(string(line), "\n")
		if strings.HasPrefix(text, "# service=") {
			continue
		}
		if i := strings.IndexByte(text, 0); i >= 0 {
			for _, c := range strings.Fields(text[i+1:]) {
				if target, ok := strings.CutPrefix(c, "symref=HEAD:refs/heads/"); ok {
					refs.Head = target
				}
			}
			text = text[:i]
		}
		first = false
		add(&refs, text)
	}
}

// readDumb reads a repository served as files, whose info/refs is a ref to a line,
// with a tab between the commit and the name.
func readDumb(r io.Reader) (Refs, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return Refs{}, err
	}
	if len(b) > maxAnswer {
		return Refs{}, errors.New("its branches and tags come to more than 32 MiB")
	}
	refs := Refs{Commits: map[string]string{}}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		commit, name, ok := strings.Cut(string(line), "\t")
		if !ok || !commitID.MatchString(commit) {
			return Refs{}, errors.New("a line of it isn't a commit and a ref")
		}
		add(&refs, commit+" "+name)
	}
	return refs, nil
}

// add takes one "<commit> <name>" line, where "<name>^{}" names the commit an
// annotated tag points at, which is what the tag means to a build.
func add(refs *Refs, text string) {
	commit, name, ok := strings.Cut(text, " ")
	if !ok || !commitID.MatchString(commit) || name == "capabilities^{}" {
		// An empty repository names no ref, only what its server can do.
		return
	}
	if tag, peeled := strings.CutSuffix(name, "^{}"); peeled {
		refs.Commits[tag] = commit
		return
	}
	if _, ok := refs.Commits[name]; !ok || !strings.HasPrefix(name, "refs/tags/") {
		refs.Commits[name] = commit
	}
}

// shown is a repository's URL as a person reads it, without https://.
func shown(url string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), ".git")
}

func list(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// unwrapURL is a request's error without the URL Go puts in front, which the words
// around it say already.
func unwrapURL(err error) error {
	var u interface{ Unwrap() error }
	if errors.As(err, &u) {
		if inner := u.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}
