// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const (
	configLimit   = 1 << 20
	service       = "docker.service"
	systemctl     = "/usr/bin/systemctl"
	systemdRun    = "/usr/bin/systemd-run"
	defaultConfig = "/etc/docker/daemon.json"
	commandPath   = "/usr/sbin:/usr/bin:/sbin:/bin"
	// WorkerCommand invokes the detached host recovery helper.
	WorkerCommand = "docker-settings-worker"
)

var errSource = errors.New("the Docker daemon is managed outside the supported local service")

type source struct {
	path    string
	binary  string
	pid     string
	args    []string
	started time.Time
}

type daemonManager interface {
	source(context.Context) (source, error)
	validate(context.Context, source, []byte) error
	action(context.Context, v1.DockerDaemonAction) error
	launch(context.Context, string, string) error
}

type systemd struct{ socket string }

func cleanCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // calls use fixed trusted tools and validated argv without a shell
	cmd.Env = []string{"PATH=" + commandPath, "LANG=C"}
	return cmd
}

func boundedOutput(cmd *exec.Cmd, limit int64) ([]byte, error) {
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if int64(len(data)) > limit || err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errSource
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return data, nil
}

func (s systemd) source(ctx context.Context) (source, error) {
	if os.Geteuid() != 0 || (s.socket != docker.DefaultSocket && s.socket != "/run/docker.sock") {
		return source{}, errSource
	}
	if err := trustedFile(systemdRun, true); err != nil {
		return source{}, errSource
	}
	data, err := boundedOutput(cleanCommand(ctx, systemctl, "show", service, "--property=MainPID", "--property=ControlGroup", "--property=ExecStart", "--property=NeedDaemonReload"), 4096)
	if err != nil {
		return source{}, errSource
	}
	var pid, cgroup, execStart, needReload string
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "MainPID":
			pid = value
		case "ControlGroup":
			cgroup = value
		case "ExecStart":
			execStart = value
		case "NeedDaemonReload":
			needReload = value
		}
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n < 2 || cgroup != "/system.slice/docker.service" {
		return source{}, errSource
	}
	proc := "/proc/" + pid
	path, err := os.Readlink(filepath.Join(proc, "exe"))
	if err != nil || (path != "/usr/bin/dockerd" && path != "/usr/sbin/dockerd") || trustedFile(path, true) != nil {
		return source{}, errSource
	}
	cmdline, err := readBounded(filepath.Join(proc, "cmdline"), 8192)
	if err != nil {
		return source{}, errSource
	}
	fields := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	_, recorded, ok := strings.Cut(execStart, "argv[]=")
	recorded, rest, separated := strings.Cut(recorded, " ; ")
	if len(fields) < 1 || filepath.Base(fields[0]) != "dockerd" || !ok || !separated ||
		strings.Contains(rest, "argv[]=") || needReload != "no" || recorded != strings.Join(fields, " ") {
		return source{}, errSource
	}
	configPath := defaultConfig
	var args []string
	hasListener := false
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-H" || f == "--host":
			if i+1 == len(fields) || fields[i+1] != "fd://" || hasListener {
				return source{}, errSource
			}
			hasListener = true
			args = append(args, f, fields[i+1])
			i++
		case f == "--host=fd://" && !hasListener:
			hasListener = true
			args = append(args, f)
		case f == "--containerd=/run/containerd/containerd.sock" || f == "--containerd=/var/run/containerd/containerd.sock":
			args = append(args, f)
		case f == "--config-file=/etc/docker/daemon.json":
		default:
			flag, _, _ := strings.Cut(f, "=")
			switch flag {
			case "--registry-mirror", "--log-driver", "--log-opt", "--default-address-pool":
				return source{}, fmt.Errorf("the Docker service sets %s at startup. It must be configured in daemon.json to manage it here", flag)
			}
			return source{}, errors.New("the Docker service uses startup flags this editor cannot verify")
		}
	}
	if !hasListener {
		return source{}, errSource
	}
	// A systemd socket gives this daemon its listener. Compare the configured socket
	// and verify the daemon still owns it before accepting the service as our target.
	listens, err := boundedOutput(cleanCommand(ctx, systemctl, "show", "docker.socket", "--property=Listen", "--value"), 4096)
	if err != nil || (strings.TrimSpace(string(listens)) != "/run/docker.sock (Stream)" && strings.TrimSpace(string(listens)) != "/var/run/docker.sock (Stream)") ||
		!ownsSocket(proc) {
		return source{}, errSource
	}
	if err := trustedDirectory(filepath.Dir(configPath)); err != nil {
		return source{}, errSource
	}
	started, err := processStarted(proc)
	if err != nil {
		return source{}, errSource
	}
	return source{path: configPath, binary: path, pid: pid, args: args, started: started}, nil
}

func processStarted(proc string) (time.Time, error) {
	stat, err := readBounded(filepath.Join(proc, "stat"), 8192)
	if err != nil {
		return time.Time{}, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return time.Time{}, errSource
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return time.Time{}, errSource
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 53)
	if err != nil {
		return time.Time{}, err
	}
	var boot unix.Timespec
	now := time.Now()
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot); err != nil {
		return time.Time{}, err
	}
	// Linux exposes proc start time in USER_HZ (100 ticks/sec on the
	// supported architectures). Round up to the end of the sampled tick.
	return now.Add(time.Duration(ticks+1)*10*time.Millisecond - time.Duration(boot.Nano())), nil
}

func ownsSocket(proc string) bool {
	contents, err := readBounded("/proc/net/unix", configLimit)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 8 || (fields[7] != "/run/docker.sock" && fields[7] != "/var/run/docker.sock") {
			continue
		}
		f, err := os.Open(filepath.Join(proc, "fd")) //nolint:gosec // proc is an inspected numeric PID below /proc
		if err != nil {
			return false
		}
		names, err := f.Readdirnames(2048)
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return false
		}
		for _, name := range names {
			target, err := os.Readlink(filepath.Join(proc, "fd", name))
			if err == nil && target == "socket:["+fields[6]+"]" {
				return true
			}
		}
	}
	return false
}

func (systemd) validate(ctx context.Context, src source, data []byte) error {
	f, err := os.CreateTemp("", "dockwell-docker-check-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	args := append(slices.Clone(src.args), "--validate", "--config-file", f.Name())
	cmd := cleanCommand(ctx, src.binary, args...)
	// Docker's static distribution installs docker-proxy beside dockerd. The
	// validator resolves it even though it does not start the daemon.
	cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(src.binary)+":"+commandPath)
	cmd.WaitDelay = time.Second
	var stderr validationOutput
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("dockerd did not validate the candidate configuration before the deadline")
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return errors.New("could not run dockerd to validate the candidate configuration")
		}
		return fmt.Errorf("dockerd rejected the candidate configuration: %s", validationReason(string(stderr)))
	}
	return nil
}

type validationOutput []byte

func (o *validationOutput) Write(p []byte) (int, error) {
	*o = append(*o, p[:min(len(p), 4096-len(*o))]...)
	return len(p), nil
}

// Moby's errors can quote both daemon.json values and command-line values.
// Translate known diagnostics without returning strings from those values.
func validationReason(stderr string) string {
	if _, reported, ok := strings.Cut(stderr, "specified both as a flag and in the configuration file:"); ok {
		var flags []string
		for _, field := range []struct{ key, flag string }{
			{"registry-mirrors", "--registry-mirror"},
			{"log-driver", "--log-driver"},
			{"log-opts", "--log-opt"},
			{"default-address-pools", "--default-address-pool"},
			{"hosts", "--host"},
			{"containerd", "--containerd"},
			{"data-root", "--data-root"},
		} {
			if strings.Contains(reported, " "+field.key+": (from flag:") {
				flags = append(flags, field.flag)
			}
		}
		if len(flags) != 0 {
			return "the service sets " + strings.Join(flags, ", ") + " both at startup and in daemon.json"
		}
		return "a setting is specified both in the service startup flags and in daemon.json"
	}
	switch {
	case strings.Contains(stderr, "userland-proxy is enabled, but userland-proxy-path is not set"):
		return "userland-proxy is enabled, but dockerd could not find docker-proxy"
	case strings.Contains(stderr, "invalid userland-proxy-path"):
		return "the configured userland-proxy-path is invalid"
	case strings.Contains(stderr, "don't match any configuration option"):
		return "daemon.json contains an option this Docker version does not support"
	case strings.Contains(stderr, "failed to decode configuration JSON"), strings.Contains(stderr, "cannot unmarshal"), strings.Contains(stderr, "invalid character"):
		return "dockerd could not read a setting in daemon.json as JSON"
	case strings.Contains(stderr, "unknown flag:"):
		return "dockerd does not recognize a Docker service startup flag"
	default:
		return "check daemon.json and the Docker service startup flags"
	}
}

func (systemd) action(ctx context.Context, act v1.DockerDaemonAction) error {
	switch act {
	case v1.DockerDaemonAction_DOCKER_DAEMON_ACTION_RELOAD:
		return cleanCommand(ctx, systemctl, "kill", "--kill-who=main", "--signal=HUP", service).Run()
	case v1.DockerDaemonAction_DOCKER_DAEMON_ACTION_RESTART:
		if err := cleanCommand(ctx, systemctl, "reset-failed", service).Run(); err != nil {
			return err
		}
		return cleanCommand(ctx, systemctl, "restart", service).Run()
	default:
		return errSource
	}
}

func (systemd) launch(ctx context.Context, dir, name string) error {
	bin, err := os.Executable()
	if err != nil {
		return errors.New("the Docker recovery helper is unavailable")
	}
	return launchWorker(ctx, dir, name, bin)
}

func launchWorker(ctx context.Context, dir, name, bin string) error {
	if trustedFile(bin, true) != nil {
		return errors.New("the Docker recovery helper is unavailable")
	}
	unit := "dockwell-docker-" + name + ".service"
	err := cleanCommand(ctx, systemdRun, "--quiet", "--collect", "--unit="+unit,
		"--property=Type=exec", "--property=Restart=on-failure", "--property=RestartSec=5s", "--", bin, WorkerCommand, dir).Run()
	if err != nil && cleanCommand(ctx, systemctl, "is-active", "--quiet", unit).Run() == nil {
		return nil
	}
	return err
}

type serviceTiming struct {
	Restart time.Duration
	Retry   time.Duration
}

func (systemd) timing(ctx context.Context) (serviceTiming, error) {
	out, err := boundedOutput(cleanCommand(ctx, systemctl, "show", service,
		"--property=StartLimitIntervalUSec,StartLimitBurst,TimeoutStopUSec,TimeoutStartUSec,RestartUSec"), 4096)
	if err != nil {
		return serviceTiming{}, err
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	stop := unitDuration(values["TimeoutStopUSec"])
	start := unitDuration(values["TimeoutStartUSec"])
	interval := unitDuration(values["StartLimitIntervalUSec"])
	retry := unitDuration(values["RestartUSec"])
	burst, err := strconv.Atoi(values["StartLimitBurst"])
	if err != nil || stop < 0 || start < 0 || interval < 0 || retry < 0 {
		return serviceTiming{}, errSource
	}
	// An unlimited start timeout still gets a bounded health deadline. The
	// unit can retry a failed start on its own, so include its start-limit
	// window and retry spacing without waiting beyond the stop/start budget.
	if start == 0 {
		start = 90 * time.Second
	}
	budget := stop + start + 10*time.Second
	if burst > 0 && interval > 0 {
		budget = max(budget, interval+min(time.Duration(burst), 10)*retry)
	}
	return serviceTiming{Restart: min(max(budget, 20*time.Second), 4*time.Minute), Retry: min(max(retry, time.Second), 10*time.Second)}, nil
}

func unitDuration(s string) time.Duration {
	if s == "infinity" {
		return 0
	}
	var total time.Duration
	for part := range strings.FieldsSeq(s) {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "min", "m"), "us", "µs")
		d, err := time.ParseDuration(part)
		if err != nil || d < 0 || d > 24*time.Hour {
			return -1
		}
		total += d
	}
	if s == "" {
		return -1
	}
	return total
}

func trustedDirectory(path string) error {
	for at := filepath.Clean(path); ; at = filepath.Dir(at) {
		fi, err := os.Lstat(at)
		if err != nil {
			return err
		}
		if !fi.IsDir() || !trusted(fi) {
			return errSource
		}
		if at == "/" {
			return nil
		}
	}
}

func trusted(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && fi.Mode().Perm()&0o022 == 0
}

func trustedFile(path string, executable bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !trusted(fi) || !fi.Mode().IsRegular() || (executable && fi.Mode().Perm()&0o100 == 0) {
		return errSource
	}
	return trustedDirectory(filepath.Dir(path))
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // callers pass verified host files or fixed proc paths
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("the Docker configuration is too large")
	}
	return data, err
}

func readConfig(path string) ([]byte, config, os.FileMode, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, config{}, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	if err := trustedFile(path, false); err != nil {
		return nil, nil, 0, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Gid != 0 || st.Nlink != 1 || fi.Mode()&^os.ModePerm != 0 {
		return nil, nil, 0, errSource
	}
	if _, err := configLabel(path); err != nil {
		return nil, nil, 0, err
	}
	data, err := readBounded(path, configLimit)
	if err != nil {
		return nil, nil, 0, err
	}
	decoded, err := decodeConfig(data)
	return data, decoded, fi.Mode().Perm(), err
}

func decodeConfig(data []byte) (config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	out := config{}
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("the Docker configuration must be one JSON object")
	}
	for dec.More() {
		key, err := dec.Token()
		s, ok := key.(string)
		if err != nil || !ok || out[s] != nil {
			return nil, errors.New("the Docker configuration contains invalid or duplicate settings")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errors.New("the Docker configuration contains an invalid value")
		}
		out[s] = raw
	}
	_, err = dec.Token()
	var extra json.RawMessage
	if err != nil || !errors.Is(dec.Decode(&extra), io.EOF) {
		return nil, errors.New("the Docker configuration must be one JSON object")
	}
	return out, nil
}

// configLabel accepts the standard SELinux label and refuses attributes the
// atomic replacement cannot preserve.
func configLabel(path string) ([]byte, error) {
	var buf [4096]byte
	n, err := syscall.Listxattr(path, buf[:])
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTSUP) || (err == nil && n == 0) {
		return nil, nil
	}
	if err != nil || string(buf[:n]) != "security.selinux\x00" {
		return nil, errors.New("the Docker config has attributes this editor cannot preserve")
	}
	n, err = syscall.Getxattr(path, "security.selinux", buf[:])
	if err != nil {
		return nil, err
	}
	return slices.Clone(buf[:n]), nil
}

func atomicFile(path string, data []byte, mode os.FileMode) error {
	return atomicLabeledFile(path, data, mode, nil)
}

func atomicLabeledFile(path string, data []byte, mode os.FileMode, label []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".dockwell-docker-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if label != nil {
		if err := unix.Fsetxattr(int(f.Fd()), "security.selinux", label, 0); err != nil {
			return err
		}
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // callers fsync the already verified config or state directory
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func encodeConfig(c config) ([]byte, error) {
	out, err := json.MarshalIndent(c, "", "  ")
	if len(out) > configLimit {
		return nil, fmt.Errorf("the proposed Docker configuration exceeds %d bytes", configLimit)
	}
	return append(out, '\n'), err
}
