// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

type fixtureDaemon struct {
	t         *testing.T
	root      string
	config    string
	socket    string
	binary    string
	args      []string
	cmd       *exec.Cmd
	done      chan struct{}
	failStart bool
	actions   []v1.DockerDaemonAction
}

func privateDaemon(t *testing.T, initial string, extraArgs ...string) (*fixtureDaemon, *docker.Client) {
	t.Helper()
	if os.Getenv("DOCKWELL_TEST_DAEMON_DIND") != "1" {
		t.Skip("set DOCKWELL_TEST_DAEMON_DIND=1 inside the private privileged Docker fixture")
	}
	t.Setenv("TMPDIR", "/root")
	binary, err := exec.LookPath("dockerd")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/root", "dw-d-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	config := filepath.Join(root, "d.json")
	if err := os.WriteFile(config, []byte(initial), 0o640); err != nil {
		t.Fatal(err)
	}
	d := &fixtureDaemon{t: t, root: root, config: config, socket: filepath.Join(root, "d.sock"), binary: binary}
	d.args = []string{
		"--host=unix://" + d.socket, "--data-root=" + filepath.Join(root, "data"), "--exec-root=" + filepath.Join(root, "exec"),
		"--pidfile=" + filepath.Join(root, "pid"), "--storage-driver=vfs", "--bridge=none", "--iptables=false", "--ip6tables=false", "--ip-forward=false",
	}
	d.args = append(d.args, extraArgs...)
	if err := d.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stop)
	client := docker.New(d.socket)
	t.Cleanup(client.Close)
	return d, client
}

func (d *fixtureDaemon) start(ctx context.Context) error {
	log, err := os.OpenFile(filepath.Join(d.root, "docker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	args := append(slices.Clone(d.args), "--config-file", d.config)
	d.cmd = exec.Command(d.binary, args...)
	d.cmd.Stdout, d.cmd.Stderr = log, log
	if err := d.cmd.Start(); err != nil {
		return err
	}
	cmd := d.cmd
	done := make(chan struct{})
	d.done = done
	go func() { _ = cmd.Wait(); close(done) }()
	client := docker.New(d.socket)
	defer client.Close()
	wait, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if !waitHealthy(wait, client, nil) {
		d.stop()
		data, _ := os.ReadFile(filepath.Join(d.root, "docker.log"))
		d.t.Log(string(data))
		return errors.New("the private Docker daemon did not start")
	}
	return nil
}

func (d *fixtureDaemon) stop() {
	if d.cmd == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(20 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
	d.cmd = nil
}

func (d *fixtureDaemon) source(context.Context) (source, error) {
	if d.cmd == nil {
		return source{}, errSource
	}
	return source{path: d.config, binary: d.binary, pid: strconv.Itoa(d.cmd.Process.Pid), args: d.args}, nil
}

func (*fixtureDaemon) validate(ctx context.Context, src source, data []byte) error {
	return (systemd{}).validate(ctx, src, data)
}

func (d *fixtureDaemon) action(ctx context.Context, act v1.DockerDaemonAction) error {
	d.actions = append(d.actions, act)
	if act == reload {
		return d.cmd.Process.Signal(syscall.SIGHUP)
	}
	d.stop()
	if d.failStart {
		d.failStart = false
		return errors.New("fixture interrupted the candidate start")
	}
	return d.start(ctx)
}

func (*fixtureDaemon) launch(context.Context, string, string) error {
	return errors.New("the fixture runs its worker directly")
}

func executeFixture(t *testing.T, d *fixtureDaemon, client *docker.Client, p proposal, approve bool) string {
	t.Helper()
	dir, err := os.MkdirTemp(d.root, "tx-")
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		if err := os.WriteFile(filepath.Join(dir, "approved"), []byte(filepath.Base(dir)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	impact := p.plan.GetImpact()
	err = runTransaction(t.Context(), dir, &transaction{
		Config: d.config, Socket: d.socket, Binary: p.src.binary, PID: p.src.pid, Args: p.src.args,
		Previous: p.old, Candidate: p.next, Mode: p.mode, Action: impact.GetAction(), Recovery: impact.GetRecoveryAction(), Owned: p.desired, Probe: p.probe,
	}, d, client)
	if err != nil {
		t.Fatal(err)
	}
	var got outcome
	if err := readRecord(filepath.Join(dir, "result.json"), &got); err != nil {
		t.Fatal(err)
	}
	return got.State
}

func TestDockerDaemonMirrorsReloadOnRealDocker(t *testing.T) {
	daemon, client := privateDaemon(t, `{"registry-mirrors":[],"debug":false}`)
	driver := &Driver{Client: client, Dir: daemon.root, manager: daemon}
	before := daemon.cmd.Process.Pid
	for _, mirrors := range [][]string{{"https://mirror.example"}, nil} {
		p, err := driver.prepare(t.Context(), &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: mirrors}})
		if err != nil {
			t.Fatal(err)
		}
		if p.plan.GetImpact().GetAction() != reload {
			t.Fatal("mirror-only change should reload")
		}
		if got := executeFixture(t, daemon, client, p, true); got != "applied" {
			t.Fatal(got)
		}
		if daemon.cmd.Process.Pid != before {
			t.Fatal("the daemon restarted")
		}
		_, cfg, mode, err := readConfig(daemon.config)
		if err != nil || mode != 0o640 || string(cfg["debug"]) != "false" {
			t.Fatal(mode, err)
		}
	}
	// Begin with an absent key, take ownership, then Undo a nonempty
	// list. Moby needs a restart to clear a key omitted by Undo.
	daemon.stop()
	absent, absentClient := privateDaemon(t, `{"debug":false}`)
	absentDriver := &Driver{Client: absentClient, Dir: absent.root, manager: absent}
	mirror := &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}}}
	p, err := absentDriver.prepare(t.Context(), mirror)
	if err != nil || p.plan.GetImpact().GetRecoveryAction() != restart {
		t.Fatalf("take on absent key: plan = %v, err = %v", p.plan, err)
	}
	if got := executeFixture(t, absent, absentClient, p, true); got != "applied" {
		t.Fatal(got)
	}
	p, err = absentDriver.prepare(t.Context(), nil)
	if err != nil || p.plan.GetImpact().GetAction() != restart {
		t.Fatalf("release formerly absent key: plan = %v, err = %v", p.plan, err)
	}
	if got := executeFixture(t, absent, absentClient, p, true); got != "applied" {
		t.Fatal(got)
	}
	_, cfg, _, err := readConfig(absent.config)
	if _, present := cfg["registry-mirrors"]; err != nil || present {
		t.Fatalf("Undo did not restore absence: %v, %v", cfg, err)
	}
}

func TestDockerDaemonInvalidConfigurationRefusedOnRealDocker(t *testing.T) {
	daemon, client := privateDaemon(t, `{"debug":false}`, "--registry-mirror=https://original.example")
	driver := &Driver{Client: client, Dir: daemon.root, manager: daemon}
	// The existing launch flag conflicts with the proposed key. The actual
	// daemon validator must refuse the draft before it can become a plan.
	original := []byte(`{"debug":false}`)
	pid := daemon.cmd.Process.Pid
	result := driver.Check(t.Context(), &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}}})
	if result.GetImpact() != nil || len(result.GetFindings()) != 1 || !strings.Contains(result.GetFindings()[0].GetDetail(), "--registry-mirror both at startup and in daemon.json") {
		t.Fatal("the invalid draft was not refused by the daemon validator", result)
	}
	held, _, mode, err := readConfig(daemon.config)
	if err != nil || mode != 0o640 || !slices.Equal(held, original) || daemon.cmd.Process.Pid != pid {
		t.Fatal("invalid candidate touched the daemon", err)
	}
}

func TestDockerDaemonRollbackOnRealDocker(t *testing.T) {
	daemon, client := privateDaemon(t, `{"debug":false}`)
	driver := &Driver{Client: client, Dir: daemon.root, manager: daemon}
	p, err := driver.prepare(t.Context(), &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	p.plan.Impact.Action, p.plan.Impact.RecoveryAction = restart, restart
	daemon.failStart = true
	if got := executeFixture(t, daemon, client, p, false); got != "restored" {
		t.Fatal(got)
	}
	if !slices.Equal(daemon.actions, []v1.DockerDaemonAction{restart, restart}) {
		t.Fatalf("expected one attempted candidate restart and one recovery restart, got %v", daemon.actions)
	}
	held, _, mode, err := readConfig(daemon.config)
	if err != nil || mode != 0o640 || !slices.Equal(held, p.old) || client.Ping(t.Context()) != nil {
		t.Fatal("Docker did not recover", err)
	}

	t.Run("crash-before-write", func(t *testing.T) {
		dir, err := os.MkdirTemp(daemon.root, "tx-")
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicFile(filepath.Join(dir, "started"), []byte("prepared"), 0o600); err != nil {
			t.Fatal(err)
		}
		daemon.actions = nil
		tx := &transaction{
			Config: daemon.config, Previous: p.old, Candidate: p.next, Mode: p.mode,
			Action: restart, Recovery: restart,
		}
		if err := runTransaction(t.Context(), dir, tx, daemon, client); err != nil {
			t.Fatal(err)
		}
		var result outcome
		if err := readRecord(filepath.Join(dir, "result.json"), &result); err != nil || result.State != "restored" || len(daemon.actions) > 0 {
			t.Fatalf("pre-write crash: result = %+v, actions = %v, err = %v", result, daemon.actions, err)
		}
	})

	t.Run("reviewed-base-drift", func(t *testing.T) {
		next, err := driver.prepare(t.Context(), &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}}})
		if err != nil {
			t.Fatal(err)
		}
		hand := []byte(`{"debug":true}`)
		if err := atomicFile(daemon.config, hand, next.mode); err != nil {
			t.Fatal(err)
		}
		daemon.actions = nil
		if got := executeFixture(t, daemon, client, next, false); got != "conflict" || len(daemon.actions) != 0 {
			t.Fatalf("hand edit reached activation: state = %s, actions = %v", got, daemon.actions)
		}
		data, _, _, err := readConfig(daemon.config)
		if err != nil || !slices.Equal(data, hand) {
			t.Fatalf("hand edit overwritten: %v", err)
		}
	})
}

func TestDockerDaemonLoggingAndPoolsOnRealDocker(t *testing.T) {
	daemon, client := privateDaemon(t, `{"debug":false}`)
	ctx := t.Context()
	const image = "dockwell.invalid/settings-fixture:1"
	probe := filepath.Join(daemon.root, "probe")
	if err := os.WriteFile(probe, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.ImportFiles(ctx, image, []docker.ImageFile{{Name: "probe", Path: probe, Mode: 0o600}}); err != nil {
		t.Fatal(err)
	}
	create := func() string {
		t.Helper()
		id, err := client.CreateContainer(ctx, "", &docker.CreateRequest{
			Config: docker.Config{Image: image, Cmd: []string{"/probe"}}, HostConfig: docker.HostConfig{NetworkMode: "none"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	readLogs := func(id string) *docker.LogConfig {
		t.Helper()
		c, err := client.InspectContainer(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.HostConfig.LogConfig
	}
	old := create()
	if logs := readLogs(old); logs == nil || logs.Type != "json-file" || logs.Config["max-size"] != "" {
		t.Fatalf("the original container should inherit unbounded json-file logs: %+v", logs)
	}
	driver := &Driver{Client: client, Dir: daemon.root, manager: daemon}
	want := &spec.DockerDaemon{
		Logging:      &spec.DockerDaemonLogging{Driver: "local", MaxSizeBytes: 10 << 20, MaxFiles: 5},
		AddressPools: &spec.DockerDefaultAddressPools{Pools: []*spec.DockerAddressPool{{Base: "192.0.2.0/24", PrefixLength: 28}}},
	}
	p, err := driver.prepare(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if p.plan.GetImpact().GetAction() != restart || p.probe == "" {
		t.Fatal("changing logging and pools must use a restart and verify the new limits")
	}
	if got := executeFixture(t, daemon, client, p, true); got != "applied" {
		t.Fatal(got)
	}
	if logs := readLogs(old); logs == nil || logs.Type != "json-file" || logs.Config["max-size"] != "" {
		t.Fatalf("the old container's logging changed: %+v", logs)
	}
	if logs := readLogs(create()); logs == nil || logs.Type != "local" || logs.Config["max-size"] != "10485760" || logs.Config["max-file"] != "5" {
		t.Fatalf("the new container did not inherit the reviewed logging policy: %+v", logs)
	}
	id, err := client.CreateNetwork(ctx, &docker.NetworkCreate{Name: "allocate-from-settings", Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := client.InspectNetwork(ctx, id)
	if err != nil || len(n.IPAM.Config) != 1 {
		t.Fatal(n, err)
	}
	pool, err := netip.ParsePrefix(n.IPAM.Config[0].Subnet)
	if err != nil || pool.Bits() != 28 || !netip.MustParsePrefix("192.0.2.0/24").Contains(pool.Addr()) {
		t.Fatal("Docker did not allocate from the reviewed pool", n.IPAM.Config, err)
	}
}
