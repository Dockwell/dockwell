// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

type transaction struct {
	Config     string
	Socket     string
	Binary     string
	PID        string
	Started    time.Time
	Args       []string
	Previous   []byte
	Candidate  []byte
	ReviewFile bool
	Rollback   []byte
	Mode       os.FileMode
	Label      []byte
	Timing     serviceTiming
	Running    map[string]string
	Action     v1.DockerDaemonAction
	Recovery   v1.DockerDaemonAction
	Probe      string
	Owned      record
	Evidence   string
}

type outcome struct {
	State string
	Note  string
}

var errStale = errors.New("the Docker configuration or restart impact changed. Review the settings again")

// Apply hands a reviewed change to the independent worker and waits for its outcome.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if d.Client == nil || d.Dir == "" || step.DockerDaemon == nil {
		return errSource
	}
	deploy := resource.DeployOf(ctx)
	if deploy.Change == "" {
		return errSource
	}
	token := sha256.Sum256([]byte(deploy.Change))
	name := hex.EncodeToString(token[:16])
	dir := filepath.Join(d.Dir, name)
	if err := d.recoveryIssue(name); err != nil {
		return err
	}
	if err := d.stage(ctx, step, desired, dir, name); err != nil {
		if errors.Is(err, errStale) || errors.Is(err, errPendingEdit) {
			resource.Report(ctx, resource.Outcome{Untouched: true})
		}
		return err
	}
	var tx transaction
	if err := readRecord(filepath.Join(dir, "transaction.json"), &tx); err != nil {
		return d.noteRecoveryIssue(name, err)
	}
	wait, cancel := context.WithTimeout(ctx, tx.waitBudget())
	defer cancel()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	acknowledged := false
	for {
		var result outcome
		if err := readRecord(filepath.Join(dir, "result.json"), &result); err == nil {
			switch result.State {
			case "applied":
				resource.Report(ctx, resource.Outcome{Note: result.Note})
				return d.checkApplied(ctx, &tx)
			case "restored":
				resource.Report(ctx, resource.Outcome{Untouched: true, Note: result.Note})
				message := "the Docker change could not finish. Previous settings restored"
				if result.Note != "" {
					message += ". " + result.Note
				}
				return errors.New(message)
			case "conflict":
				resource.Report(ctx, resource.Outcome{Untouched: true, Note: result.Note})
				if result.Note != "" {
					return fmt.Errorf("%w. %s", errStale, result.Note)
				}
				return errStale
			default:
				return d.noteRecoveryIssue(name, errRecoveryIncomplete)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return d.noteRecoveryIssue(name, err)
		}
		if err := d.recoveryIssue(name); err != nil {
			return err
		}
		if !acknowledged && ctx.Err() == nil {
			if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
				// A live agent call acknowledges a locally verified Engine. A
				// worker without this acknowledgement restores the old config.
				if err := atomicFile(filepath.Join(dir, "approved"), []byte(name), 0o600); err != nil {
					return d.noteRecoveryIssue(name, err)
				}
				acknowledged = true
			}
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			check, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			failed := cleanCommand(check, systemctl, "is-failed", "--quiet", "dockwell-docker-"+name+".service").Run() == nil
			done()
			if failed {
				return d.noteRecoveryIssue(name, errors.New("the recovery worker failed. Open its service logs"))
			}
			return d.noteRecoveryIssue(name, errRecoveryIncomplete)
		case <-poll.C:
		}
	}
}

func (d *Driver) checkApplied(ctx context.Context, tx *transaction) error {
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if !verifyEngine(check, d.Client, tx) {
		return errors.New("the Docker daemon changed after the settings were applied. Open Docker settings and review again")
	}
	return nil
}

func (d *Driver) stage(ctx context.Context, step resource.Step, desired proto.Message, dir, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	if err := trustedDirectory(d.Dir); err != nil {
		return err
	}
	path := filepath.Join(dir, "transaction.json")
	var existing transaction
	if err := readRecord(path, &existing); err == nil {
		want, _ := desired.(*spec.DockerDaemon)
		if existing.Evidence != step.DockerDaemon.GetEvidence() || existing.ReviewFile != want.GetReviewCurrentFile() ||
			(!existing.ReviewFile && !proto.Equal(existing.Owned.Owned, want)) {
			return errStale
		}
		var result outcome
		if err := readRecord(filepath.Join(dir, "result.json"), &result); err == nil {
			return nil
		}
		// A stream may have fallen between staging and requesting the worker.
		// A running unit already owns the same durable transaction.
		if err := d.localManager().launch(ctx, dir, name); err != nil {
			if resultErr := readRecord(filepath.Join(dir, "result.json"), &result); resultErr != nil {
				return errors.New("the Docker recovery worker could not restart. Open Docker logs")
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return d.noteRecoveryIssue(name, err)
	}
	want, _ := desired.(*spec.DockerDaemon)
	p, err := d.prepareSettings(ctx, want, false)
	if err != nil {
		return err
	}
	if p.plan == nil || p.plan.Evidence != step.DockerDaemon.GetEvidence() {
		return errStale
	}
	probe := ""
	cfg, _ := decodeConfig(p.rollback)
	next, _ := decodeConfig(p.next)
	if !slices.Equal(cfg["log-driver"], next["log-driver"]) || !slices.Equal(cfg["log-opts"], next["log-opts"]) {
		probe, err = d.logProbeImage(ctx)
		if err != nil {
			return err
		}
	}
	label, err := configLabel(p.src.path)
	if err != nil {
		return err
	}
	var timing serviceTiming
	if manager, ok := d.localManager().(interface {
		timing(context.Context) (serviceTiming, error)
	}); ok && (p.plan.Impact.Action == restart || p.plan.Impact.RecoveryAction == restart) {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		timing, err = manager.timing(check)
		cancel()
		if err != nil {
			return errors.New("the Docker service start and stop limits could not be read. Review its unit settings")
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := trustedDirectory(dir); err != nil {
		return err
	}
	tx := transaction{
		Config: p.src.path, Socket: d.Client.Socket(), Binary: p.src.binary, PID: p.src.pid, Started: p.src.started, Args: p.src.args,
		Previous: p.old, Candidate: p.next, ReviewFile: want.GetReviewCurrentFile(), Mode: p.mode, Action: p.plan.Impact.Action, Recovery: p.plan.Impact.RecoveryAction,
		Label: label, Timing: timing, Running: p.running, Probe: probe, Owned: p.desired, Evidence: p.plan.Evidence,
	}
	if tx.ReviewFile {
		tx.Rollback = p.rollback
	}
	// The verified active bytes are already in Candidate, even for a replay.
	tx.Owned.Active = nil
	if err := writeRecord(path, &tx); err != nil {
		return err
	}
	if err := syncDir(d.Dir); err != nil {
		return err
	}
	if err := d.localManager().launch(ctx, dir, name); err != nil {
		return d.noteRecoveryIssue(name, err)
	}
	return nil
}

// Work runs the local transaction independently of the agent and its connection.
func Work(ctx context.Context, dir string) error {
	return work(ctx, dir, false)
}

var (
	errWorkerActive       = errors.New("a Docker settings worker holds the transaction lock")
	errRecoveryIncomplete = errors.New("the Docker recovery did not finish. Open Docker service and logs to check why")
)

func work(ctx context.Context, dir string, startup bool) error {
	if !filepath.IsAbs(dir) || trustedDirectory(dir) != nil {
		return errSource
	}
	lock, err := openTransactionLock(filepath.Dir(dir))
	if err != nil {
		return err
	}
	defer lock.Close()
	if startup {
		err = tryTransactionLock(lock)
	} else {
		err = lockTransaction(ctx, lock)
	}
	if err != nil || marker(dir, "result.json") {
		return err
	}
	var tx transaction
	if err := readRecord(filepath.Join(dir, "transaction.json"), &tx); err != nil {
		return err
	}
	if tx.Config != defaultConfig || (tx.Socket != docker.DefaultSocket && tx.Socket != "/run/docker.sock") {
		return errSource
	}
	client := docker.New(tx.Socket)
	defer client.Close()
	return runLockedTransaction(ctx, dir, &tx, systemd{tx.Socket}, client)
}

// Recover settles abandoned transactions after the control stream opens. A
// live worker keeps its lock and the resumed Apply can acknowledge it.
func (d *Driver) Recover(ctx context.Context, logger *slog.Logger) {
	if d.Dir == "" {
		return
	}
	report := func(dir string, err error) {
		logger.WarnContext(ctx, "Docker settings recovery needs attention", "transaction", dir, "error", err)
		_ = d.noteRecoveryIssue(filepath.Base(dir), err)
	}
	pending, err := scanRecovery(ctx, d.Dir, report)
	if err != nil && ctx.Err() == nil {
		logger.WarnContext(ctx, "Docker settings recovery needs attention", "error", err)
		_ = d.noteRecoveryIssue("", err)
		return
	}
	for _, dir := range pending {
		if err := followRecovery(ctx, dir); err != nil && ctx.Err() == nil {
			report(dir, err)
		}
	}
	if ctx.Err() == nil && d.RecoveryAttention() == "" {
		d.rememberActive(ctx)
	}
}

func scanRecovery(ctx context.Context, root string, report func(string, error)) ([]string, error) {
	if err := trustedDirectory(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	dirs, err := os.Open(root) //nolint:gosec // root is the verified agent settings directory
	if err != nil {
		return nil, err
	}
	defer dirs.Close()
	var pending []string
	for {
		entries, err := dirs.ReadDir(64)
		for _, entry := range entries {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !entry.IsDir() || len(entry.Name()) != 32 {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if !marker(dir, "started") {
				continue
			}
			err := recoveryResult(ctx, dir)
			switch {
			case errors.Is(err, errWorkerActive):
				pending = append(pending, dir)
			case err != nil && ctx.Err() == nil:
				report(dir, err)
			}
		}
		if errors.Is(err, io.EOF) {
			return pending, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func recoveryResult(ctx context.Context, dir string) error {
	var result outcome
	path := filepath.Join(dir, "result.json")
	err := readRecord(path, &result)
	if errors.Is(err, os.ErrNotExist) {
		if err := work(ctx, dir, true); err != nil {
			return err
		}
		err = readRecord(path, &result)
	}
	if err != nil {
		return err
	}
	if result.State == "attention" {
		return errRecoveryIncomplete
	}
	return nil
}

func followRecovery(ctx context.Context, dir string) error {
	started, err := os.Stat(filepath.Join(dir, "started"))
	if err != nil {
		return err
	}
	var tx transaction
	if err := readRecord(filepath.Join(dir, "transaction.json"), &tx); err != nil {
		return err
	}
	deadline := started.ModTime().Add(tx.waitBudget())
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for {
		if err := recoveryResult(ctx, dir); !errors.Is(err, errWorkerActive) {
			return err
		}
		if !time.Now().Before(deadline) {
			return errRecoveryIncomplete
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}

func (d *Driver) noteRecoveryIssue(name string, err error) error {
	var detail string
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		detail = "The server has no space to finish Docker settings recovery. Free disk space and restart the agent to retry."
	case errors.Is(err, errRecoveryIncomplete):
		detail = errRecoveryIncomplete.Error()
	default:
		detail = "The agent could not finish Docker settings recovery. Check the agent service logs, then restart the agent to retry."
	}
	issue := errors.New("Docker settings need attention. " + detail)
	d.recoveryIssues.Store(name, issue)
	return issue
}

func (d *Driver) recoveryIssue(name string) error {
	if value, ok := d.recoveryIssues.Load(""); ok {
		return value.(error)
	}
	if value, ok := d.recoveryIssues.Load(name); ok {
		return value.(error)
	}
	return nil
}

// RecoveryAttention lets the panel read recovery status even if Docker is down.
func (d *Driver) RecoveryAttention() string {
	var detail string
	d.recoveryIssues.Range(func(_, value any) bool {
		detail = value.(error).Error()
		return false
	})
	return strings.TrimPrefix(detail, "Docker settings need attention. ")
}

const ackBudget = 40 * time.Second

func (tx *transaction) waitBudget() time.Duration {
	return 45*time.Second + tx.budget(tx.Action) + ackBudget + tx.budget(tx.Recovery) + tx.retry()
}

func (tx *transaction) budget(act v1.DockerDaemonAction) time.Duration {
	if act == restart {
		if tx.Timing.Restart > 0 {
			return tx.Timing.Restart
		}
		return 60 * time.Second
	}
	return 15 * time.Second
}

func (tx *transaction) retry() time.Duration {
	if tx.Recovery == restart {
		return 30*time.Second + tx.Timing.Retry
	}
	return 0
}

func marker(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func lockTransaction(ctx context.Context, file *os.File) error {
	wait, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		err := tryTransactionLock(file)
		if !errors.Is(err, errWorkerActive) {
			return err
		}
		select {
		case <-wait.Done():
			return wait.Err()
		case <-tick.C:
		}
	}
}

func tryTransactionLock(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWorkerActive
	}
	return err
}

func openTransactionLock(root string) (*os.File, error) {
	return os.OpenFile(filepath.Join(root, "active.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600) //nolint:gosec // callers use the verified local Docker settings state directory
}

func runTransaction(ctx context.Context, dir string, tx *transaction, manager daemonManager, client *docker.Client) error {
	lock, err := openTransactionLock(filepath.Dir(dir))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockTransaction(ctx, lock); err != nil {
		return err
	}
	return runLockedTransaction(ctx, dir, tx, manager, client)
}

func runLockedTransaction(ctx context.Context, dir string, tx *transaction, manager daemonManager, client *docker.Client) error {
	if marker(dir, "result.json") {
		return nil
	}
	note := ""
	finish := func(state string) error {
		return writeRecord(filepath.Join(dir, "result.json"), outcome{State: state, Note: note})
	}
	current, _, mode, err := readConfig(tx.Config)
	if err != nil {
		return finish("attention")
	}
	if marker(dir, "committing") {
		if mode != tx.candidateMode() || !slices.Equal(current, tx.Candidate) {
			return finish("conflict")
		}
		tx.Owned.setActive(tx.Candidate)
		if err := writeRecord(filepath.Join(filepath.Dir(dir), "owned.json"), tx.Owned); err != nil {
			return err
		}
		note = readWorkloadResult(dir)
		return finish("applied")
	}
	putBack := func() error {
		return restore(ctx, dir, tx, manager, client, func(state string) error {
			if state == "restored" && tx.Recovery == restart && marker(dir, "activated") {
				note = workloadResult(context.WithoutCancel(ctx), client, tx.Running)
			}
			return finish(state)
		})
	}
	if marker(dir, "started") {
		return putBack()
	}
	if !slices.Equal(current, tx.Previous) || tx.Mode != mode {
		return finish("conflict")
	}
	check, cancel := context.WithTimeout(ctx, 8*time.Second)
	src, err := manager.source(check)
	if err != nil || src.path != tx.Config || src.binary != tx.Binary || src.pid != tx.PID || !slices.Equal(src.args, tx.Args) ||
		(!tx.Started.IsZero() && src.started.Sub(tx.Started).Abs() > time.Second) {
		cancel()
		return finish("conflict")
	}
	if tx.Action != 0 {
		err = manager.validate(check, src, tx.Candidate)
	}
	cancel()
	if err != nil {
		note = safeError(err)
		return finish("conflict")
	}
	// Validation can take time. Re-read the reviewed bytes and metadata
	// immediately before the durable marker and atomic replacement.
	current, _, mode, err = readConfig(tx.Config)
	label, labelErr := configLabel(tx.Config)
	if err != nil || labelErr != nil || !slices.Equal(label, tx.Label) || !slices.Equal(current, tx.Previous) || mode != tx.Mode {
		return finish("conflict")
	}
	if err := atomicFile(filepath.Join(dir, "started"), []byte("prepared"), 0o600); err != nil {
		return err
	}
	if !slices.Equal(tx.Previous, tx.Candidate) {
		if tx.Previous == nil {
			mode = 0o600
		}
		if err := atomicLabeledFile(tx.Config, tx.Candidate, mode, tx.Label); err != nil {
			return putBack()
		}
	}
	applyCtx, cancel := context.WithTimeout(ctx, tx.budget(tx.Action))
	defer cancel()
	if tx.Action != 0 {
		if err := atomicFile(filepath.Join(dir, "activated"), []byte("activation requested"), 0o600); err != nil {
			return putBack()
		}
		if manager.action(applyCtx, tx.Action) != nil || !waitHealthy(applyCtx, client, tx) {
			return putBack()
		}
	}
	if tx.Probe != "" {
		cfg, err := decodeConfig(tx.Candidate)
		driver := "json-file"
		if raw := cfg["log-driver"]; raw != nil && json.Unmarshal(raw, &driver) != nil {
			return putBack()
		}
		if err != nil || !verifyLogging(applyCtx, client, tx.Probe, logSettings(driver, cfg["log-opts"])) {
			return putBack()
		}
	}
	if tx.Action == restart {
		note = workloadResult(ctx, client, tx.Running)
		if err := atomicFile(filepath.Join(dir, "workloads"), []byte(note), 0o600); err != nil {
			return putBack()
		}
	}
	if err := atomicFile(filepath.Join(dir, "ready"), []byte("verified"), 0o600); err != nil {
		return putBack()
	}
	ack, done := context.WithTimeout(ctx, ackBudget)
	defer done()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		if approved, err := readBounded(filepath.Join(dir, "approved"), 64); err == nil && string(approved) == filepath.Base(dir) {
			if !verifyEngine(ack, client, tx) {
				return putBack()
			}
			if err := atomicFile(filepath.Join(dir, "committing"), []byte("approved"), 0o600); err != nil {
				return err
			}
			tx.Owned.setActive(tx.Candidate)
			if err := writeRecord(filepath.Join(filepath.Dir(dir), "owned.json"), tx.Owned); err != nil {
				return err
			}
			return finish("applied")
		}
		select {
		case <-ack.Done():
			return putBack()
		case <-poll.C:
		}
	}
}

func restore(ctx context.Context, dir string, tx *transaction, manager daemonManager, client *docker.Client, finish func(string) error) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), tx.budget(tx.Recovery)+tx.retry())
	defer cancel()
	current, _, mode, err := readConfig(tx.Config)
	if err != nil {
		return finish("attention")
	}
	label, err := configLabel(tx.Config)
	if err != nil || (current != nil && tx.Previous != nil && !slices.Equal(label, tx.Label)) {
		return finish("attention")
	}
	old, oldMode := tx.rollbackFile()
	written := mode == tx.candidateMode() && slices.Equal(current, tx.Candidate)
	base := mode == tx.Mode && slices.Equal(current, tx.Previous)
	previous := mode == oldMode && slices.Equal(current, old)
	if !written && !previous && !base {
		return finish("conflict")
	}
	activate := marker(dir, "activated")
	if previous && !activate {
		return finish("restored")
	}
	// After a reboot a candidate already on disk may have been loaded, even
	// if the worker never reached its own activation marker.
	if !activate && tx.Action != 0 {
		check, done := context.WithTimeout(recovery, 5*time.Second)
		source, err := manager.source(check)
		done()
		activate = err != nil || source.pid != tx.PID || (!tx.Started.IsZero() && source.started.Sub(tx.Started).Abs() > time.Second)
		if !activate && tx.ReviewFile && base {
			return finish("restored")
		}
	}
	if !previous {
		if old == nil {
			err = os.Remove(tx.Config)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err == nil {
				err = syncDir(filepath.Dir(tx.Config))
			}
		} else {
			err = atomicLabeledFile(tx.Config, old, oldMode, tx.Label)
		}
	}
	if err != nil {
		return finish("attention")
	}
	if activate && tx.Recovery != 0 && !marker(dir, "recovered") {
		if !recoverActivation(recovery, dir, tx, manager, client) {
			return finish("attention")
		}
	}
	if err := atomicFile(filepath.Join(dir, "recovered"), []byte("restored"), 0o600); err != nil {
		return err
	}
	current, _, mode, err = readConfig(tx.Config)
	if err != nil || mode != oldMode || !slices.Equal(current, old) {
		return finish("conflict")
	}
	statePath := filepath.Join(filepath.Dir(dir), "owned.json")
	var state record
	if err := readRecord(statePath, &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return finish("attention")
	}
	state.setActive(old)
	if err := writeRecord(statePath, state); err != nil {
		return finish("attention")
	}
	return finish("restored")
}

func recoverActivation(ctx context.Context, dir string, tx *transaction, manager daemonManager, client *docker.Client) bool {
	path := filepath.Join(dir, "recovering")
	if initial, err := readBounded(path, 64); err == nil {
		// A previous worker may have completed recovery before running out
		// of space to record it. Verify that activation instead of repeating it.
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		source, err := manager.source(check)
		return err == nil && (tx.Recovery == reload || source.pid != string(initial)) && waitHealthy(check, client, restoredConfig(tx))
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	check, done := context.WithTimeout(ctx, 5*time.Second)
	source, _ := manager.source(check)
	done()
	if err := atomicFile(path, []byte(source.pid), 0o600); err != nil {
		return false
	}
	run := func(budget time.Duration) bool {
		attempt, done := context.WithTimeout(ctx, budget)
		defer done()
		return manager.action(attempt, tx.Recovery) == nil && waitHealthy(attempt, client, restoredConfig(tx))
	}
	if run(tx.budget(tx.Recovery)) {
		return true
	}
	if tx.Recovery != restart {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(max(time.Second, tx.Timing.Retry)):
		return run(30 * time.Second)
	}
}

func (tx *transaction) rollbackFile() ([]byte, os.FileMode) {
	if tx.ReviewFile {
		if tx.Rollback == nil {
			return nil, 0
		}
		return tx.Rollback, tx.candidateMode()
	}
	return tx.Previous, tx.Mode
}

func restoredConfig(tx *transaction) *transaction {
	file, mode := tx.rollbackFile()
	return &transaction{Config: tx.Config, Candidate: file, Previous: file, Mode: mode}
}

func readWorkloadResult(dir string) string {
	b, _ := readBounded(filepath.Join(dir, "workloads"), 4096)
	return string(b)
}

func workloadResult(ctx context.Context, client *docker.Client, before map[string]string) string {
	if len(before) == 0 {
		return ""
	}
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	missing := maps.Clone(before)
	err := client.EachCleanupContainer(check, func(c *docker.ContainerSummary) error {
		if c.State == "running" || c.State == "paused" {
			delete(missing, c.ID)
		}
		return nil
	})
	if err != nil {
		return "Could not verify which containers came back"
	}
	if len(missing) == 0 {
		return ""
	}
	names := slices.Sorted(maps.Values(missing))
	extra := ""
	if len(names) > 5 {
		extra = fmt.Sprintf(" and %d more", len(names)-5)
		names = names[:5]
	}
	return fmt.Sprintf("Containers that did not return: %s%s", strings.Join(names, ", "), extra)
}

func waitHealthy(ctx context.Context, client *docker.Client, tx *transaction) bool {
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		single, cancel := context.WithTimeout(ctx, 3*time.Second)
		healthy := client.Ping(single) == nil
		if healthy && tx != nil {
			healthy = verifyEngine(single, client, tx)
		}
		cancel()
		if healthy {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-poll.C:
		}
	}
}

func (tx *transaction) candidateMode() os.FileMode {
	if tx.Previous == nil && tx.Candidate != nil {
		return 0o600
	}
	return tx.Mode
}

func verifyEngine(ctx context.Context, client *docker.Client, tx *transaction) bool {
	info, err := client.Info(ctx)
	if err != nil {
		return false
	}
	current, cfg, mode, err := readConfig(tx.Config)
	if err != nil || !slices.Equal(current, tx.Candidate) || mode != tx.candidateMode() {
		return false
	}
	var mirrors, wanted []string
	if info.RegistryConfig != nil {
		mirrors = info.RegistryConfig.Mirrors
	}
	if raw := cfg["registry-mirrors"]; raw != nil && json.Unmarshal(raw, &wanted) != nil {
		return false
	}
	if !slices.EqualFunc(wanted, mirrors, func(a, b string) bool {
		return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
	}) {
		return false
	}
	if tx.Owned.Owned.GetLogging() != nil || tx.Probe != "" {
		driver := "json-file"
		if raw := cfg["log-driver"]; raw != nil && json.Unmarshal(raw, &driver) != nil {
			return false
		}
		if info.LoggingDriver != driver {
			return false
		}
	}
	if (tx.ReviewFile || tx.Owned.Owned.GetAddressPools() != nil) && cfg["default-address-pools"] != nil {
		pools := poolSettings(cfg["default-address-pools"])
		if len(pools) != len(info.DefaultAddressPools) {
			return false
		}
		for i, got := range info.DefaultAddressPools {
			if got.Base != pools[i].Base || got.Size != pools[i].PrefixLength {
				return false
			}
		}
	}
	return true
}

func verifyLogging(ctx context.Context, client *docker.Client, image string, limits *spec.DockerDaemonLogging) bool {
	img, err := client.InspectImage(ctx, image)
	if err != nil || img.ID != image || img.Config == nil || len(img.Config.Volumes) > 0 {
		return false
	}
	req := &docker.CreateRequest{Config: docker.Config{Image: image, Cmd: []string{"/dockwell-log-check"}}, HostConfig: docker.HostConfig{NetworkMode: "none"}}
	id, err := client.CreateContainer(ctx, "", req)
	if err != nil || !docker.IsFullID(id) {
		return false
	}
	c, readErr := client.InspectContainer(ctx, id)
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	err = client.RemoveContainer(removeCtx, id, false)
	if err != nil || readErr != nil || c.HostConfig.LogConfig == nil {
		return false
	}
	return sameLogging(c.HostConfig.LogConfig, limits)
}
