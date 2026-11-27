// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"go.yaml.in/yaml/v4"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

type ciServices struct {
	dockwellv1connect.UnimplementedServiceServiceHandler
	check       func(*connect.Request[v1.DeployServiceRequest])
	state       v1.ChangeState
	waits       string
	unavailable atomic.Bool
}

func (s *ciServices) DeployService(_ context.Context, req *connect.Request[v1.DeployServiceRequest]) (*connect.Response[v1.DeployServiceResponse], error) {
	if s.check != nil {
		s.check(req)
	}
	if s.unavailable.Swap(false) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("control server restarting"))
	}
	return connect.NewResponse(&v1.DeployServiceResponse{Change: &v1.Change{
		Id: stubChange, State: s.state, Waits: s.waits, Plan: &v1.Plan{Fingerprint: "reviewed"},
	}}), nil
}

type ciChanges struct {
	*stubChanges
	applied atomic.Int32
}

func (s *ciChanges) ApplyChange(_ context.Context, req *connect.Request[v1.ApplyChangeRequest]) (*connect.Response[v1.ApplyChangeResponse], error) {
	if req.Msg.GetId() != stubChange || req.Msg.GetFingerprint() != "reviewed" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected change or fingerprint"))
	}
	s.applied.Add(1)
	// Another CI run can apply the same key between deploy and apply.
	err := connect.NewError(connect.CodeFailedPrecondition, errors.New("already applying"))
	err.Meta().Set(authapi.ReasonKey, "not_planned")
	return nil, err
}

func serveCIDeploy(t *testing.T, s *ciServices, c *ciChanges) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewServiceServiceHandler(s))
	mux.Handle(dockwellv1connect.NewChangeServiceHandler(c))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCIDeployWaitExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name               string
		planned, end       v1.ChangeState
		rollback           v1.RollbackState
		want, applications int
	}{
		{"healthy", v1.ChangeState_CHANGE_STATE_PLANNED, v1.ChangeState_CHANGE_STATE_APPLIED, 0, exitOK, 1},
		{"in-progress retry", v1.ChangeState_CHANGE_STATE_APPLYING, v1.ChangeState_CHANGE_STATE_APPLIED, 0, exitOK, 0},
		{"failed", v1.ChangeState_CHANGE_STATE_PLANNED, v1.ChangeState_CHANGE_STATE_FAILED, 0, exitError, 1},
		{"rolled back", v1.ChangeState_CHANGE_STATE_FAILED, v1.ChangeState_CHANGE_STATE_FAILED, v1.RollbackState_ROLLBACK_STATE_APPLIED, 6, 0},
		{"discarded", v1.ChangeState_CHANGE_STATE_DISCARDED, v1.ChangeState_CHANGE_STATE_DISCARDED, 0, 7, 0},
		{"returned failed", v1.ChangeState_CHANGE_STATE_FAILED, v1.ChangeState_CHANGE_STATE_FAILED, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ciChanges{stubChanges: &stubChanges{watch: func(_ int, send func(*v1.Change) error) error {
				out := &v1.Change{Id: stubChange, State: tc.end, Error: "the health check failed"}
				if tc.rollback != 0 {
					out.RolledBackBy = "chg_rollback"
					out.RollbackStatus = &v1.RollbackStatus{Id: out.RolledBackBy, State: tc.rollback}
				}
				return send(out)
			}}}
			url := serveCIDeploy(t, &ciServices{state: tc.planned, check: func(req *connect.Request[v1.DeployServiceRequest]) {
				if req.Msg.GetId() != "svc_web" || req.Msg.GetImage() != "example.com/acme/web:1" {
					t.Errorf("unexpected CI deploy request: %v", req.Msg)
				}
			}}, c)
			r := run(t, map[string]string{apicli.EnvURL: url, apicli.EnvToken: wellFormedToken()}, "services", "deploy",
				"--wait", "--image=example.com/acme/web:1", "--", "svc_web")
			if r.code != tc.want || int(c.applied.Load()) != tc.applications || !strings.Contains(r.err, "/changes/"+stubChange) {
				t.Fatalf("exit = %d, apply attempts = %d; stdout %q, stderr %q", r.code, c.applied.Load(), r.out, r.err)
			}
		})
	}
}

func TestCIDeployReconnectsToTheRecordedChange(t *testing.T) {
	var proposals, reads atomic.Int32
	s := &ciServices{state: v1.ChangeState_CHANGE_STATE_APPLYING, check: func(req *connect.Request[v1.DeployServiceRequest]) {
		proposals.Add(1)
		if req.Msg.GetDeployOrigin().GetCommit() != "same-commit" {
			t.Errorf("retry changed the proposal: %v", req.Msg)
		}
	}}
	s.unavailable.Store(true)
	c := &ciChanges{stubChanges: &stubChanges{
		watch: func(n int, send func(*v1.Change) error) error {
			if n == 1 {
				if err := send(&v1.Change{Id: stubChange, State: v1.ChangeState_CHANGE_STATE_APPLYING}); err != nil {
					return err
				}
				return connect.NewError(connect.CodeUnavailable, errors.New("stream dropped"))
			}
			return send(&v1.Change{Id: stubChange, State: v1.ChangeState_CHANGE_STATE_APPLIED})
		},
		get: func(id string) *v1.Change {
			reads.Add(1)
			return &v1.Change{Id: id, State: v1.ChangeState_CHANGE_STATE_APPLYING}
		},
	}}
	url := serveCIDeploy(t, s, c)
	r := run(t, map[string]string{apicli.EnvURL: url, apicli.EnvToken: wellFormedToken()}, "services", "deploy",
		"--wait", "--timeout=5s", "--image=example.com/acme/web:1", `--deploy-origin={"commit":"same-commit"}`, "svc_web")
	c.mu.Lock()
	watches := c.watches
	c.mu.Unlock()
	if r.code != exitOK || proposals.Load() != 2 || reads.Load() != 1 || watches != 2 {
		t.Fatalf("exit %d, proposals %d, gets %d, watches %d: %s", r.code, proposals.Load(), reads.Load(), watches, r.err)
	}
}

func TestCIDeployWaitDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, waits string
		state       v1.ChangeState
		code        int
	}{
		{"approval", "Waiting for an admin", v1.ChangeState_CHANGE_STATE_PLANNED, 4},
		{"rollout", "", v1.ChangeState_CHANGE_STATE_APPLYING, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ciChanges{stubChanges: &stubChanges{watch: func(_ int, send func(*v1.Change) error) error {
				// A proxy can end a quiet stream cleanly, with no terminal state.
				return send(&v1.Change{Id: stubChange, State: tc.state, Waits: tc.waits})
			}, get: func(id string) *v1.Change {
				return &v1.Change{Id: id, State: tc.state, Waits: tc.waits}
			}}}
			url := serveCIDeploy(t, &ciServices{state: tc.state, waits: tc.waits}, c)
			r := run(t, map[string]string{apicli.EnvURL: url, apicli.EnvToken: wellFormedToken()}, "services", "deploy",
				"--wait", "--timeout=500ms", "--image=example.com/acme/web:1", "svc_web")
			if r.code != tc.code || c.applied.Load() != 0 || !strings.Contains(r.err, "/changes/"+stubChange) {
				t.Fatalf("exit %d, apply attempts %d: %s", r.code, c.applied.Load(), r.err)
			}
		})
	}
}

func TestCIWrappersAgainstTheDeployAPI(t *testing.T) {
	for _, command := range []string{"bash", "jq", "tar", "sha256sum"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("this test needs %s", command)
		}
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("the release wrappers run on linux amd64 and arm64")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []struct{ Run string }
		}
	}
	parseCIYAML(t, filepath.Join(repo, "ci", "github", "action.yml"), &action)
	var jobs map[string]struct{ Script []string }
	parseCIYAML(t, filepath.Join(repo, "ci", "gitlab", "dockwell-deploy.yml"), &jobs)
	github := action.Runs.Steps[0].Run
	gitlab := strings.Join(jobs["$[[ inputs.job ]]"].Script, "\n")
	if github == "" || gitlab == "" {
		t.Fatal("missing wrapper scripts")
	}

	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "cli-test")); err != nil {
		t.Fatal(err)
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("dockwell", "#!/usr/bin/env bash\nexec \"$CI_FIXTURES/cli-test\" -test.run '^TestCIDeployCLIProcess$' -- \"$@\"\n")
	archive := "dockwell_9.9.9_linux_" + runtime.GOARCH + ".tar.gz"
	if out, err := exec.CommandContext(t.Context(), "tar", "-czf", filepath.Join(dir, archive), "-C", dir, "dockwell").CombinedOutput(); err != nil {
		t.Fatalf("packing the test CLI: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, archive))
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := os.ReadFile(filepath.Join(repo, "ci", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	write("deploy.sh", string(deploy))
	write("checksums.txt", fmt.Sprintf("%x  %s\n%x  deploy.sh\n", sha256.Sum256(raw), archive, sha256.Sum256(deploy)))
	write("checksums.txt.sigstore.json", "{}")
	write("cosign", `#!/usr/bin/env bash
set -eu
[[ $1 == verify-blob && $2 == --certificate-identity && $3 == https://github.com/Dockwell/dockwell/.github/workflows/release.yml@refs/tags/v9.9.9 ]]
[[ $4 == --certificate-oidc-issuer && $5 == https://token.actions.githubusercontent.com && $6 == --bundle && -s $7 && -s $8 ]]
`)
	sha256bin, _ := exec.LookPath("sha256sum")
	write("sha256sum", `#!/usr/bin/env bash
set -euo pipefail
read -r line
case "$line" in
  4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71*" /"*"/cosign" | c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a*" /"*"/cosign") exit 0 ;;
esac
printf '%s\n' "$line" | "$CI_REAL_SHA256SUM" "$@"
`)
	write("apt-get", "#!/usr/bin/env bash\nexit 0\n")
	write("curl", `#!/usr/bin/env bash
set -eu
out=
while (( $# > 1 )); do
  if [[ $1 == -o ]]; then out=$2; shift; fi
  shift
done
url=$1
[[ -n $out ]]
case "$url" in
  https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-*) src="$CI_FIXTURES/cosign" ;;
  https://github.com/Dockwell/dockwell/releases/download/v9.9.9/*) src="$CI_FIXTURES/${url##*/}" ;;
  *) printf 'Unexpected URL %s\n' "$url" >&2; exit 1 ;;
esac
cp -- "$src" "$out"
if [[ ${CI_TAMPER:-} == "${src##*/}" ]]; then
  printf 'tampered' >> "$out"
fi
`)
	for _, tc := range []struct {
		name, script, project, runURL string
		provider                      v1.DeployProvider
	}{
		{"github-actions", github, "https://github.com/acme/web", "https://github.com/acme/web/actions/runs/42", v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS},
		{"gitlab-ci", gitlab, "https://gitlab.example.com/acme/web", "https://gitlab.example.com/acme/web/-/pipelines/42", v1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commit := strings.Repeat("d", 40)
			wantToken := wellFormedToken()
			var calls atomic.Int32
			url := serveCIDeploy(t, &ciServices{state: v1.ChangeState_CHANGE_STATE_APPLYING, check: func(r *connect.Request[v1.DeployServiceRequest]) {
				calls.Add(1)
				o := r.Msg.GetDeployOrigin()
				if r.Header().Get("Authorization") != "Bearer "+wantToken || r.Msg.GetId() != "svc_web" ||
					r.Msg.GetImage() != "example.com/acme/web:1" || o.GetRepository() != tc.project || o.GetCommit() != commit ||
					o.GetRef() != "main" || o.GetRunUrl() != tc.runURL || o.GetProvider() != tc.provider {
					t.Errorf("unexpected deploy request: %v", r.Msg)
				}
			}}, &ciChanges{stubChanges: &stubChanges{watch: func(_ int, send func(*v1.Change) error) error {
				return send(&v1.Change{Id: stubChange, State: v1.ChangeState_CHANGE_STATE_APPLIED})
			}}})
			env := append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CI_FIXTURES="+dir,
				"CI_REAL_SHA256SUM="+sha256bin,
				"GITHUB_ACTION_PATH="+filepath.Join(repo, "ci", "github"),
				"DOCKWELL_URL="+url, "DOCKWELL_TOKEN="+wantToken, "DOCKWELL_IMAGE=example.com/acme/web:1",
				"DOCKWELL_CI_IMAGE=example.com/acme/web:1", "DOCKWELL_SERVICE=svc_web", "DOCKWELL_VERSION=v9.9.9", "DOCKWELL_CI_PROVIDER="+tc.name,
				"XDG_CONFIG_HOME="+t.TempDir(),
				"GITHUB_SERVER_URL=https://github.com", "GITHUB_REPOSITORY=acme/web", "GITHUB_SHA="+commit,
				"GITHUB_REF=main", "GITHUB_RUN_ID=42", "CI_PROJECT_URL="+tc.project,
				"CI_COMMIT_SHA="+commit, "CI_COMMIT_REF_NAME=main", "CI_PIPELINE_URL="+tc.runURL)
			if tc.name == "github-actions" {
				env = append(env, "DOCKWELL_VERSION=", "DOCKWELL_ACTION_REF=v9.9.9")
			}
			runScript := func(tamper string) ([]byte, error) {
				cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", tc.script)
				cmd.Env = env
				cmd.Env = append(cmd.Env, "CI_TAMPER="+tamper)
				return cmd.CombinedOutput()
			}
			out, err := runScript("")
			if err != nil || calls.Load() != 1 || !bytes.Contains(out, []byte("/changes/"+stubChange)) {
				t.Fatalf("CI script = %v, %d API calls\n%s", err, calls.Load(), out)
			}
			for _, asset := range []string{archive, "deploy.sh"} {
				if asset == "deploy.sh" && tc.name != "gitlab-ci" {
					continue
				}
				out, err := runScript(asset)
				if err == nil || calls.Load() != 1 || !bytes.Contains(out, []byte("checksum")) {
					t.Fatalf("tampered %s = %v, %d API calls\n%s", asset, err, calls.Load(), out)
				}
			}
		})
	}
}

func parseCIYAML(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if strings.Contains(path, "gitlab") {
		var inputs any
		if err := dec.Decode(&inputs); err != nil {
			t.Fatal(err)
		}
	}
	if err := dec.Decode(out); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of %s, got %v", path, err)
	}
}

func TestCIDeployCLIProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" && os.Getenv("CI_FIXTURES") != "" {
			os.Exit(Execute(t.Context(), os.Args[i+1:], IO{Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv}))
		}
	}
}
