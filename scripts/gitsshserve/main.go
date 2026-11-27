// SPDX-License-Identifier: AGPL-3.0-only

// Command gitsshserve serves a bare Git repository over SSH (using
// internal/gitssh/gitsshtest) with a tiny HTTP control endpoint so Playwright
// end-to-end tests can add or remove authorized deploy keys, rotate host keys,
// and push new commits without shelling out to real SSH daemons.
//
// Usage:
//
//	gitsshserve <dir> [listen-addr]
//
// Prints one JSON object to stdout on startup and serves until SIGTERM/SIGINT.
// Never part of a release.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/gitssh/gitsshtest"
)

const appDockerfile = `FROM busybox:1.37
WORKDIR /app
COPY . .
RUN echo "built over ssh deploy key"
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/app"]
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("gitsshserve takes <dir> [listen-addr]")
	}
	root := filepath.Clean(os.Args[1])
	listenAddr := "127.0.0.1:0"
	if len(os.Args) >= 3 && os.Args[2] != "" {
		listenAddr = os.Args[2]
	}
	if err := os.MkdirAll(root, 0o750); err != nil { //nolint:gosec // test fixture directory
		return err
	}

	bare := filepath.Join(root, "acme-web.git")
	work := filepath.Join(root, "work")
	if err := initRepo(bare, work); err != nil {
		return fmt.Errorf("init repo: %w", err)
	}

	srv, err := gitsshtest.NewServer(listenAddr, "acme/web.git", bare)
	if err != nil {
		return fmt.Errorf("start ssh server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	var mu sync.Mutex
	commitN := 1

	var lc net.ListenConfig
	ctrlLn, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen control: %w", err)
	}
	defer func() { _ = ctrlLn.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /_keys", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if err := srv.SetAuthorizedKeys(lines...); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_rotate_host_key", func(w http.ResponseWriter, _ *http.Request) {
		algo, fp, err := srv.RotateHostKey()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"algorithm": algo, "fingerprint": fp})
	})
	mux.HandleFunc("POST /_commit", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		commitN++
		subject := fmt.Sprintf("Deploy commit %d over SSH", commitN)
		sha, err := addCommit(work, "main", subject, map[string]string{
			"Dockerfile": appDockerfile,
			"index.html": subject + "\n",
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"commit": sha, "subject": subject})
	})

	ctrlSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = ctrlSrv.Serve(ctrlLn)
	}()

	algo, fp := srv.HostKey()
	out := map[string]string{
		"sshAddr":            srv.Addr,
		"repoUrl":            fmt.Sprintf("ssh://git@%s/acme/web.git", srv.Addr),
		"hostKeyAlgorithm":   algo,
		"hostKeyFingerprint": fp,
		"controlUrl":         "http://" + ctrlLn.Addr().String(),
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = ctrlSrv.Shutdown(ctx)
	return nil
}

func initRepo(bare, work string) error {
	if err := os.RemoveAll(bare); err != nil { //nolint:gosec // test fixture directory
		return err
	}
	if err := os.RemoveAll(work); err != nil { //nolint:gosec // test fixture directory
		return err
	}
	if err := os.MkdirAll(work, 0o750); err != nil { //nolint:gosec // test fixture directory
		return err
	}
	if err := runGit(filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare); err != nil {
		return err
	}
	if err := runGit(bare, "config", "uploadpack.allowReachableSHA1InWant", "true"); err != nil {
		return err
	}
	if err := runGit(work, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := runGit(work, "remote", "add", "origin", bare); err != nil {
		return err
	}
	if _, err := addCommit(work, "release", "Initial release branch", map[string]string{
		"Dockerfile": appDockerfile,
		"index.html": "release\n",
	}); err != nil {
		return err
	}
	if _, err := addCommit(work, "main", "Add SSH deploy key app", map[string]string{
		"Dockerfile": appDockerfile,
		"index.html": "Add SSH deploy key app\n",
	}); err != nil {
		return err
	}
	return nil
}

func addCommit(work, branch, subject string, files map[string]string) (string, error) {
	if err := runGit(work, "checkout", "-q", "-B", branch); err != nil {
		return "", err
	}
	for rel, body := range files {
		full := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil { //nolint:gosec // test fixture directory
			return "", err
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil { //nolint:gosec // test fixture file
			return "", err
		}
	}
	if err := runGit(work, "add", "."); err != nil {
		return "", err
	}
	if err := runGit(work, "-c", "user.name=Leo Park", "-c", "user.email=leo@example.com", "commit", "-q", "-m", subject); err != nil {
		return "", err
	}
	if err := runGit(work, "push", "-q", "-u", "origin", branch); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "HEAD")
	cmd.Dir = work
	cmd.Env = gitEnv(work)
	raw, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func runGit(dir string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // test fixture git invocation
	cmd.Dir = dir
	cmd.Env = gitEnv(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitEnv(dir string) []string {
	return append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
}
