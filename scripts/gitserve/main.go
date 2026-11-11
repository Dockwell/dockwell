// SPDX-License-Identifier: AGPL-3.0-only

// Command gitserve serves a Git repository over Git's smart HTTP protocol, with
// the real git http-backend, for the panel's end-to-end test of builds from Git on
// a machine without Docker, beside fakedocker (ADR 0046). It makes the repository
// web.git, whose branches build in fakedocker's stand-in for BuildKit:
//
//   - main builds, and its commit says "Add order export endpoint", by Leo Park;
//   - broken fails at its fourth step, saying "npm ERR! missing script: build";
//   - slow prints "installing packages" and takes a minute, for a build seen under way;
//   - noisy builds a larger image and prints 20,000 lines, for an agent's memory;
//   - confined-agent and confined-control each run a step that says which AppArmor
//     profile it runs under, for a build on each of two servers;
//   - secret reads the build secret NPM_TOKEN in two steps, and prints it in one;
//   - secret-fails prints NPM_TOKEN in a step that then fails;
//   - node is a Node app with no Dockerfile, which Railpack builds;
//   - docs has nothing Railpack knows how to build;
//   - library is a Node app with nothing that starts it, which Railpack refuses;
//   - oldnode asks for a Node that isn't, 99, which Railpack can't find;
//   - stable stays where main began, for main to go back to.
//
// Each serves its commit's subject on port 8080 on real Docker.
//
// POST /_move?branch=main&to=broken points main at what broken names, as a push
// would, so a deploy of main builds that next.
//
// It prints where it serves, such as http://127.0.0.1:41234, and serves until it's
// stopped. A second argument says where it listens, such as 172.17.0.1:0, Docker's
// bridge, which a builder's container reaches; 127.0.0.1:0 unless given. It is
// never part of a release.
//
//	go run ./scripts/gitserve /tmp/git [address]
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // a test's own Git server, on a Go long past the CVE
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// app serves the commit's page on port 8080 with busybox's httpd, so on real Docker
// a request says which commit runs.
const app = `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN echo "installed 214 packages in 9s"
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`

// confined is app with a step that prints the AppArmor profile it runs under.
const confined = `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN echo "this step runs under: $(cat /proc/self/attr/current 2>/dev/null || echo no AppArmor)"
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`

// branch is a branch of web.git: its name, its commit's subject, and the files it
// changes from main's.
type branch struct {
	name, subject string
	files         map[string]string
}

var branches = []branch{
	{"main", "Add order export endpoint", map[string]string{"Dockerfile": app, "index.html": "Add order export endpoint\n"}},
	{"broken", "Move sessions to valkey", map[string]string{"Dockerfile": `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN echo "npm ERR! missing script: build" && exit 1
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`, "index.html": "Move sessions to valkey\n"}},
	{"slow", "Batch inventory sync with smaller pages", map[string]string{"Dockerfile": `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN echo "installing packages" && sleep 60
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`, "index.html": "Batch inventory sync with smaller pages\n"}},
	// A build of a larger image that prints a great deal, for what an agent holds as
	// it passes them on. Pace it below BuildKit's 200 KiB/s limit and the agent's
	// 256 KiB/s limit, so both receive every line rather than clip a short burst.
	{"noisy", "Print every line of the inventory", map[string]string{"Dockerfile": `FROM node:22-alpine
WORKDIR /app
COPY . .
RUN for i in $(seq 1 20000); do echo "line $i of what a build prints, and then some more words"; if [ "$((i % 100))" -eq 0 ]; then sleep 0.1; fi; done
EXPOSE 8080
CMD ["node", "-e", "require('http').createServer((q, s) => s.end('noisy')).listen(8080)"]
`, "index.html": "noisy\n"}},
	// A step that says what confines it: each step of a build runs under Docker's
	// AppArmor profile where the server has it (ADR 0046). Two branches whose files
	// differ, so the second server's build runs the step, and doesn't take it from
	// the cache the first one's left.
	{"confined-agent", "Tidy the checkout page", map[string]string{"Dockerfile": confined, "index.html": "Tidy the checkout page\n"}},
	{"confined-control", "Tidy the cart page", map[string]string{"Dockerfile": confined, "index.html": "Tidy the cart page\n"}},
	// A build that uses a secret, which has to reach its step and nowhere else.
	{"secret", "Install private packages", map[string]string{"Dockerfile": `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN --mount=type=secret,id=NPM_TOKEN,required=true test -n "$(cat /run/secrets/NPM_TOKEN)" && echo "the secret arrived"
RUN --mount=type=secret,id=NPM_TOKEN,required=true cat /run/secrets/NPM_TOKEN
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`, "index.html": "Install private packages\n"}},
	// A step that prints the secret, then fails, which has to keep it out of the
	// failure as well as the log.
	{"secret-fails", "Print the token, then fail", map[string]string{"Dockerfile": `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN --mount=type=secret,id=NPM_TOKEN,required=true cat /run/secrets/NPM_TOKEN && exit 1
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`, "index.html": "Print the token, then fail\n"}},
	{"node", "Serve the storefront with Railpack", node},
	{"docs", "Write the storefront's docs", map[string]string{"Dockerfile": "", "index.html": "", "README.md": "# storefront\n"}},
	{"library", "Ship the pricing library", map[string]string{
		"Dockerfile": "", "index.html": "", "server.js": "",
		"package.json": `{"name":"pricing","version":"1.0.0","scripts":{"build":"echo built the library"}}` + "\n",
	}},
	{"oldnode", "Pin a Node that isn't", map[string]string{
		"Dockerfile": "", "index.html": "",
		"package.json": `{"name":"storefront","scripts":{"start":"node server.js"},"engines":{"node":"99"}}` + "\n",
		"server.js":    "require('http').createServer((q, s) => s.end('ok')).listen(8080)\n",
	}},
}

// node is a Node app with no Dockerfile and no dependencies, which Railpack builds
// with npm, and which serves its commit's subject on port 8080. An empty file is
// one the branch removes.
var node = map[string]string{
	"Dockerfile": "", "index.html": "",
	"package.json": `{
  "name": "storefront",
  "version": "1.0.0",
  "scripts": { "build": "echo built the storefront", "start": "node server.js" },
  "engines": { "node": "22" }
}
`,
	"server.js": `require("http").createServer((q, s) => s.end("Serve the storefront with Railpack\n")).listen(Number(process.env.PORT) || 8080)
`,
}

var name = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		log.Fatal("gitserve takes the directory to keep its repository in, such as /tmp/git, and where to listen, 127.0.0.1:0 unless given")
	}
	listen := "127.0.0.1:0"
	if len(os.Args) == 3 {
		listen = os.Args[2]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := serve(ctx, os.Args[1], listen)
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

// serve makes the repository in dir and serves it on listen until ctx is done.
func serve(ctx context.Context, dir, listen string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // the directory the person running it names
		return err
	}
	bare := filepath.Join(dir, "web.git")
	if err := makeRepo(ctx, dir, bare); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "git", "--exec-path").Output()
	if err != nil {
		return fmt.Errorf("git doesn't say where its programs are: %w", err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_move", func(w http.ResponseWriter, r *http.Request) {
		branch, to := r.URL.Query().Get("branch"), r.URL.Query().Get("to")
		if !name.MatchString(branch) || !name.MatchString(to) {
			http.Error(w, "name a branch and where it goes", http.StatusBadRequest)
			return
		}
		if err := git(r.Context(), bare, "update-ref", "refs/heads/"+branch, "refs/heads/"+to); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", backend)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	fmt.Println("http://" + ln.Addr().String())
	srv := &http.Server{Handler: mux} //nolint:gosec // a test's own server
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// makeRepo makes web.git with a branch of each, unless it's there already.
func makeRepo(ctx context.Context, dir, bare string) error {
	if _, err := os.Stat(bare); err == nil { //nolint:gosec // in the directory the person running it names
		return nil
	}
	work := filepath.Join(dir, "work")
	if err := git(ctx, dir, "init", "-q", "--bare", "-b", "main", bare); err != nil {
		return err
	}
	if err := git(ctx, bare, "config", "uploadpack.allowReachableSHA1InWant", "true"); err != nil {
		return err
	}
	if err := git(ctx, dir, "init", "-q", "-b", "main", work); err != nil {
		return err
	}
	for i, b := range branches {
		if i > 0 {
			if err := git(ctx, work, "checkout", "-q", "-b", b.name, "main"); err != nil {
				return err
			}
		}
		for p, body := range b.files {
			if body == "" {
				if err := os.Remove(filepath.Join(work, p)); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // this program's own files, in its own directory
					return err
				}
				continue
			}
			if err := os.WriteFile(filepath.Join(work, p), []byte(body), 0o600); err != nil { //nolint:gosec // this program's own files, in its own directory
				return err
			}
		}
		if err := git(ctx, work, "add", "-A"); err != nil {
			return err
		}
		if err := git(ctx, work, "-c", "user.name=Leo Park", "-c", "user.email=leo@example.com", "commit", "-q", "-m", b.subject); err != nil {
			return err
		}
		if err := git(ctx, work, "push", "-q", bare, b.name); err != nil {
			return err
		}
	}
	return git(ctx, work, "push", "-q", bare, "main:refs/heads/stable")
}

func git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // this program's own repository
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}
