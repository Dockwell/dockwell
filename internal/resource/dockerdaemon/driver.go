// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dockerlog"
	"github.com/dockwell/dockwell/internal/resource/network"
)

const (
	// Kind is the server-scoped Docker configuration resource.
	Kind resource.Kind = "docker.daemon"
	// Name is the singleton name on a server.
	Name    = "daemon"
	reload  = v1.DockerDaemonAction_DOCKER_DAEMON_ACTION_RELOAD
	restart = v1.DockerDaemonAction_DOCKER_DAEMON_ACTION_RESTART
)

// Driver plans and applies approved changes to a local Engine.
type Driver struct {
	Client         *docker.Client
	Dir            string
	manager        daemonManager
	mu             sync.Mutex
	evidenceOnce   sync.Once
	evidenceKey    [32]byte
	evidenceErr    error
	recoveryIssues sync.Map
}

// Kind identifies a daemon overlay.
func (*Driver) Kind() resource.Kind { return Kind }

// Level places it on the selected server.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New makes an empty overlay.
func (*Driver) New() proto.Message { return &spec.DockerDaemon{} }

// CheckName limits management to one daemon per server.
func (*Driver) CheckName(name string) error {
	if name != Name {
		return errors.New("each server has one Docker settings resource named daemon")
	}
	return nil
}

// Validate checks the typed allowlist.
func (*Driver) Validate(in proto.Message) error { return validate(in.(*spec.DockerDaemon)) }

// Plan requires the contextual Judge to read a live daemon.
func (*Driver) Plan(proto.Message, proto.Message, []resource.Change) (resource.Decision, error) {
	return resource.Decision{}, errSource
}

// Verify follows the independent worker’s completed read-back.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// Ready reports whether this agent has a settings reader.
// Read projects safe Engine facts and describes whether editing is available.
func (d *Driver) Ready(context.Context) error {
	if d.Client == nil || d.Dir == "" {
		return errSource
	}
	return nil
}

func (d *Driver) evidence() ([32]byte, error) {
	d.evidenceOnce.Do(func() { _, d.evidenceErr = rand.Read(d.evidenceKey[:]) })
	return d.evidenceKey, d.evidenceErr
}

func (d *Driver) localManager() daemonManager {
	if d.manager != nil {
		return d.manager
	}
	return systemd{d.Client.Socket()}
}

type record struct {
	Owned        *spec.DockerDaemon
	Baseline     config
	ConfigDigest string
	Active       []byte
	ActiveKnown  bool
	ActiveAt     time.Time
}

func (d *Driver) record() (record, error) {
	var out record
	if d.Dir == "" {
		return out, errSource
	}
	err := readRecord(filepath.Join(d.Dir, "owned.json"), &out)
	if errors.Is(err, os.ErrNotExist) {
		return record{}, nil
	}
	return out, err
}

func readRecord(path string, into any) error {
	if err := trustedFile(path, false); err != nil {
		return err
	}
	b, err := readBounded(path, 6*configLimit)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func writeRecord(path string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return atomicFile(path, b, 0o600)
}

// Observe reads the owned settings from the current configuration.
func (d *Driver) Observe(ctx context.Context, _ resource.Ref) (proto.Message, error) {
	state, err := d.record()
	if err != nil || state.Owned == nil {
		return nil, err
	}
	src, err := d.localManager().source(ctx)
	if err != nil {
		return nil, err
	}
	_, doc, _, err := readConfig(src.path)
	if err != nil {
		return nil, err
	}
	observed := &spec.DockerDaemon{}
	if state.Owned.Mirrors != nil && doc["registry-mirrors"] != nil {
		observed.Mirrors = &spec.DockerRegistryMirrors{}
		_ = json.Unmarshal(doc["registry-mirrors"], &observed.Mirrors.Urls)
	}
	if state.Owned.Logging != nil && doc["log-driver"] != nil {
		var driver string
		_ = json.Unmarshal(doc["log-driver"], &driver)
		observed.Logging = logSettings(driver, doc["log-opts"])
	}
	if state.Owned.AddressPools != nil && doc["default-address-pools"] != nil {
		observed.AddressPools = &spec.DockerDefaultAddressPools{Pools: poolSettings(doc["default-address-pools"])}
	}
	return observed, nil
}

type proposal struct {
	src      source
	old      []byte
	next     []byte
	mode     os.FileMode
	desired  record
	plan     *v1.DockerDaemonPlan
	probe    string
	running  map[string]string
	rollback []byte
}

func (d *Driver) prepare(ctx context.Context, want *spec.DockerDaemon) (proposal, error) {
	return d.prepareSettings(ctx, want, true)
}

func (d *Driver) prepareSettings(ctx context.Context, want *spec.DockerDaemon, inspect bool) (proposal, error) {
	var p proposal
	if d.Client == nil || d.Dir == "" {
		return p, errSource
	}
	if attention := d.RecoveryAttention(); attention != "" {
		return p, errors.New(attention)
	}
	manager := d.localManager()
	src, err := manager.source(ctx)
	if err != nil {
		return p, err
	}
	old, doc, mode, err := readConfig(src.path)
	if err != nil {
		return p, err
	}
	held, err := d.record()
	if err != nil {
		return p, err
	}
	age := checkConfigAge(src, old, held)
	review := want.GetReviewCurrentFile()
	if age != nil && (!review || !errors.Is(age, errPendingEdit)) {
		return p, age
	}
	if want != nil {
		if err := validate(want); err != nil {
			return p, err
		}
	}
	next, active := old, old
	desired := held
	var candidate config
	var changed map[string]bool
	var fileChanges []*v1.FieldChange
	if review {
		if age == nil {
			return p, errors.New("there are no pending Docker file changes to review")
		}
		if !held.ActiveKnown || (!src.started.IsZero() && held.ActiveAt.Before(src.started)) {
			return p, errors.New("the running Docker configuration is unavailable for comparison and recovery. Open Docker service and logs")
		}
		active = held.Active
		base := config{}
		if active != nil {
			base, err = decodeConfig(active)
			if err != nil {
				return p, err
			}
		}
		var draft *spec.DockerDaemon
		fileChanges, draft, err = activeFileChanges(base, doc)
		if err != nil {
			return p, err
		}
		if len(fileChanges) == 0 {
			return p, errors.New("there are no pending Docker settings to review")
		}
		if err := d.checkPools(ctx, draft.GetAddressPools(), base["default-address-pools"]); err != nil {
			return p, err
		}
		candidate, changed = doc, make(map[string]bool)
		for _, change := range fileChanges {
			key, _, _ := strings.Cut(change.Path, ".")
			changed[key] = true
		}
	} else {
		var baseline config
		candidate, baseline, changed, err = ownedOverlay(doc, held, want)
		if err != nil {
			return p, err
		}
		if err := d.checkPools(ctx, want.GetAddressPools(), doc["default-address-pools"]); err != nil {
			return p, err
		}
		if changed["registry-mirrors"] || changed["log-driver"] || changed["log-opts"] || changed["default-address-pools"] {
			next, err = encodeConfig(candidate)
			if err != nil {
				return p, err
			}
		}
		if slices.Equal(old, next) {
			if proto.Equal(held.Owned, want) {
				return p, nil
			}
			clear(changed)
		}
		desired = record{Owned: want, Baseline: baseline}
	}
	desired.setActive(next)
	info, err := d.Client.Info(ctx)
	if err != nil {
		return p, err
	}
	_, version := d.Client.Version()
	if major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0]); major < 23 {
		return p, errors.New("update Docker to 23 or later to edit settings here")
	}
	act, recoverAct := settingsActions(changed, active, candidate)
	if act != 0 {
		validateCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := manager.validate(validateCtx, src, next)
		cancel()
		if err != nil {
			return p, err
		}
	}
	impact := &v1.DockerDaemonImpact{Action: act, RecoveryAction: recoverAct, LiveRestore: &info.LiveRestoreEnabled, CoverageComplete: true}
	running := make(map[string]string)
	if impact.Action == restart || impact.RecoveryAction == restart {
		if info.Swarm.Nodes > 0 || (info.Swarm.LocalNodeState != "" && info.Swarm.LocalNodeState != "inactive") {
			return p, errors.New("this server is in Docker Swarm. Dockwell cannot verify restart impact here")
		}
		list, err := d.workloads(ctx)
		if err != nil {
			return p, errors.New("could not enumerate the Docker workloads affected by a restart")
		}
		var infrastructure, workloads []string
		for i := range list {
			c := &list[i]
			if !runningWorkload(c.State) {
				continue
			}
			impact.RunningCount++
			name := c.ID[:min(len(c.ID), 12)]
			if len(c.Names) > 0 {
				name = shortSafe(strings.TrimPrefix(c.Names[0], "/"), 100)
			}
			running[c.ID] = name
			if inspect {
				if c.Labels[docker.LabelIngress] != "" && len(infrastructure) < 20 {
					infrastructure = append(infrastructure, name+" (Dockwell infrastructure)")
				} else if len(workloads) < 20 {
					workloads = append(workloads, name)
				}
			}
			if inspect && !info.LiveRestoreEnabled {
				item, err := d.Client.InspectContainer(ctx, c.ID)
				if err != nil {
					return p, errors.New("the running workloads changed. Review Docker settings again")
				}
				policy := item.HostConfig.RestartPolicy.Name
				if policy != "always" && policy != "unless-stopped" {
					impact.MayStayStoppedCount++
				}
			}
		}
		slices.Sort(infrastructure)
		slices.Sort(workloads)
		infrastructure = append(infrastructure, workloads[:min(len(workloads), 20-len(infrastructure))]...)
		impact.AffectedWorkloads = infrastructure
	}
	probe := ""
	if inspect && (changed["log-opts"] || changed["log-driver"]) {
		probe, err = d.logProbeImage(ctx)
		if err != nil {
			return p, err
		}
	}
	key, err := d.evidence()
	if err != nil {
		return p, err
	}
	sum := hmac.New(sha256.New, key[:])
	// Bind the manager, exact source, candidate and stable impact to the reviewed step.
	fmt.Fprintf(sum, "%s %s %s %q\x00", src.path, src.binary, src.pid, src.args)
	sum.Write(old)
	sum.Write([]byte{0})
	sum.Write(next)
	if review {
		sum.Write([]byte{0})
		sum.Write(active)
	}
	// Counts and names are snapshots for review. Only a change of impact
	// class or scale invalidates approval.
	fmt.Fprintf(sum, "|%d|%d|%t|%d", act, recoverAct, info.LiveRestoreEnabled, workloadBucket(impact.RunningCount))
	return proposal{
		src: src, old: old, next: next, mode: mode, desired: desired, rollback: active,
		plan: &v1.DockerDaemonPlan{Impact: impact, Evidence: hex.EncodeToString(sum.Sum(nil)), FileChanges: fileChanges}, probe: probe, running: running,
	}, nil
}

func ownedOverlay(doc config, held record, want *spec.DockerDaemon) (config, config, map[string]bool, error) {
	doc = maps.Clone(doc)
	baseline := make(config, 4)
	if held.Baseline != nil {
		for k, val := range held.Baseline {
			baseline[k] = val
		}
	}
	// An owned key is restored to its pre-adoption value when Undo releases it.
	groups := []struct {
		was, will bool
		keys      []string
	}{
		{held.Owned.GetMirrors() != nil, want.GetMirrors() != nil, []string{"registry-mirrors"}},
		{held.Owned.GetLogging() != nil, want.GetLogging() != nil, []string{"log-driver", "log-opts"}},
		{held.Owned.GetAddressPools() != nil, want.GetAddressPools() != nil, []string{"default-address-pools"}},
	}
	changed := make(map[string]bool, 4)
	for _, group := range groups {
		for _, key := range group.keys {
			switch {
			case group.was && !group.will:
				was := doc[key]
				if val, present := baseline[key]; present {
					doc[key] = val
				} else {
					delete(doc, key)
				}
				delete(baseline, key)
				changed[key] = !slices.Equal(was, doc[key])
			case !group.was && group.will && doc[key] != nil:
				baseline[key] = doc[key]
			}
		}
	}
	candidate, err := overlay(doc, want)
	if err != nil {
		return nil, nil, nil, err
	}
	for k, v := range candidate {
		if !slices.Equal(v, doc[k]) {
			changed[k] = true
		}
	}
	return candidate, baseline, changed, nil
}

// Docker's configuration reload table includes registry mirrors, but not
// logging or address pools. Moby's reloadRegistryConfig retains the live
// mirrors when the key is omitted, so removing a nonempty list needs a restart.
// https://docs.docker.com/reference/cli/dockerd/#configuration-reload-behavior
func settingsActions(changed map[string]bool, old []byte, next config) (action, recovery v1.DockerDaemonAction) {
	if changed["log-driver"] || changed["log-opts"] || changed["default-address-pools"] {
		return restart, restart
	}
	if !changed["registry-mirrors"] {
		return 0, 0
	}
	before, _ := decodeConfig(old)
	var prior, after []string
	priorErr := json.Unmarshal(before["registry-mirrors"], &prior)
	afterErr := json.Unmarshal(next["registry-mirrors"], &after)
	if (priorErr == nil || before["registry-mirrors"] == nil) &&
		(afterErr == nil || next["registry-mirrors"] == nil) && slices.Equal(prior, after) {
		return 0, 0
	}
	action, recovery = reload, reload
	if next["registry-mirrors"] == nil {
		action = restart
	}
	if before["registry-mirrors"] == nil {
		recovery = restart
	}
	return action, recovery
}

func configDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var errPendingEdit = errors.New("daemon.json has pending local edits. Review the current file before changing Docker settings")

func checkConfigAge(src source, data []byte, held record) error {
	if src.started.IsZero() || ((held.ActiveAt.IsZero() || !held.ActiveAt.Before(src.started)) && held.ConfigDigest == configDigest(data)) {
		return nil
	}
	path := src.path
	if data == nil {
		// Removing the file can also leave an unactivated change. The
		// containing directory's timestamps are the only remaining evidence.
		path = filepath.Dir(path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fi.ModTime().After(src.started) || time.Unix(st.Ctim.Sec, st.Ctim.Nsec).After(src.started) {
		return errPendingEdit
	}
	return nil
}

func runningWorkload(state string) bool {
	return state == "running" || state == "restarting" || state == "paused"
}

func workloadBucket(n uint32) int {
	switch {
	case n == 0:
		return 0
	case n <= 10:
		return 1
	case n <= 50:
		return 2
	case n <= 200:
		return 3
	default:
		return 4
	}
}

func (d *Driver) checkPools(ctx context.Context, pools *spec.DockerDefaultAddressPools, current json.RawMessage) error {
	if pools == nil {
		return nil
	}
	before := poolSettings(current)
	var pending []netip.Prefix
	for _, p := range pools.Pools {
		if slices.ContainsFunc(before, func(old *spec.DockerAddressPool) bool { return proto.Equal(old, p) }) {
			continue
		}
		pending = append(pending, netip.MustParsePrefix(p.Base))
	}
	if len(pending) == 0 {
		return nil
	}
	live, err := d.Client.ListCleanupNetworks(ctx, 1200)
	if err != nil {
		return err
	}
	ipam := &spec.NetworkIPAM{}
	for _, p := range pending {
		ipam.Pools = append(ipam.Pools, &spec.NetworkIPAMPool{Subnet: p.String()})
	}
	finding, err := network.HostPoolConflict(ctx, &spec.Network{Ipam: ipam})
	if err != nil {
		return errors.New("the server's routes could not be checked")
	}
	if finding != nil {
		return fmt.Errorf("the pool overlaps %s on %s", finding.GetConflictingSubnet(), finding.GetConflictingNetwork())
	}
	for i := range live {
		for _, entry := range live[i].IPAM.Config {
			p, err := netip.ParsePrefix(entry.Subnet)
			if err == nil && slices.ContainsFunc(pending, p.Overlaps) {
				return fmt.Errorf("the pool overlaps %s on Docker network %s", p, shortSafe(live[i].Name, 100))
			}
		}
	}
	return nil
}

// Judge validates the candidate and binds activation to the reviewed source.
func (d *Driver) Judge(ctx context.Context, _ resource.Ref, actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	want, _ := desired.(*spec.DockerDaemon)
	p, err := d.prepare(ctx, want)
	if err != nil {
		return resource.Decision{}, err
	}
	if p.plan == nil {
		return resource.Decision{Action: resource.ActionNone}, nil
	}
	action, effect := resource.ActionUpdate, resource.EffectNone
	switch {
	case desired == nil:
		action = resource.ActionDelete
	case actual == nil:
		action = resource.ActionCreate
	}
	if p.plan.GetImpact().GetAction() == restart {
		effect = resource.EffectRestart
	}
	return resource.Decision{Action: action, Effect: effect, DockerDaemon: p.plan}, nil
}

// Check validates a draft without changing the active configuration.
func (d *Driver) Check(ctx context.Context, want *spec.DockerDaemon) *v1.CheckDockerSettingsResponse {
	out := &v1.CheckDockerSettingsResponse{}
	p, err := d.prepareSettings(ctx, want, false)
	if err == nil {
		out.Impact = p.plan.GetImpact()
		out.MirrorReachability = checkMirrors(ctx, want.GetMirrors().GetUrls())
		return out
	}
	reason := v1.DockerSettingsFindingReason_DOCKER_SETTINGS_FINDING_REASON_INVALID
	if errors.Is(err, errPendingEdit) {
		reason = v1.DockerSettingsFindingReason_DOCKER_SETTINGS_FINDING_REASON_CONFLICT
	}
	out.Findings = []*v1.DockerSettingsFinding{{Reason: reason, Detail: safeError(err), ReviewCurrentFile: errors.Is(err, errPendingEdit)}}
	return out
}

func safeError(err error) string {
	// Docker API errors and OS errors can include settings or host paths.
	var engine *docker.Error
	var path *os.PathError
	if errors.As(err, &engine) || errors.As(err, &path) {
		return "Docker could not verify this configuration. Open Docker logs for details."
	}
	return shortSafe(err.Error(), 320)
}

func shortSafe(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 127 {
			return ' '
		}
		return r
	}, s)
	if len(s) > limit {
		return strings.ToValidUTF8(s[:limit], "")
	}
	return s
}

func poolSettings(raw []byte) []*spec.DockerAddressPool {
	var entries []poolEntry
	_ = json.Unmarshal(raw, &entries)
	var out []*spec.DockerAddressPool
	for _, entry := range entries[:min(len(entries), 16)] {
		if p, err := netip.ParsePrefix(entry.Base); err == nil {
			out = append(out, &spec.DockerAddressPool{Base: p.String(), PrefixLength: entry.Size})
		}
	}
	return out
}

func logSettings(driver string, raw []byte) *spec.DockerDaemonLogging {
	var opts map[string]string
	_ = json.Unmarshal(raw, &opts)
	return &spec.DockerDaemonLogging{Driver: shortSafe(driver, 64), MaxSizeBytes: dockerlog.Size(opts["max-size"]), MaxFiles: dockerlog.Files(opts["max-file"])}
}

func (d *Driver) Read(ctx context.Context) (*v1.DockerSettings, error) {
	attention := d.RecoveryAttention()
	if d.Client == nil && attention == "" {
		return nil, errSource
	}
	if d.Client == nil {
		return &v1.DockerSettings{RecoveryAttention: attention, EditabilityReason: attention}, nil
	}
	if attention != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	info, err := d.Client.Info(ctx)
	if err != nil {
		if attention != "" {
			return &v1.DockerSettings{RecoveryAttention: attention, EditabilityReason: attention}, nil
		}
		return nil, err
	}
	api, version := d.Client.Version()
	out := &v1.DockerSettings{
		ObservedAt: timestamppb.Now(), RecoveryAttention: attention, EditabilityReason: attention, EngineVersion: version, ApiVersion: api,
		Endpoint: d.Client.Socket(), Mode: "Rootful", DataRoot: shortSafe(info.DockerRootDir, 512), LiveRestore: &info.LiveRestoreEnabled,
		ImageStore: "Classic", Logging: logSettings(info.LoggingDriver, nil),
	}
	if info.ContainerdImageStore() {
		out.ImageStore = "Containerd"
	}
	if slices.ContainsFunc(info.SecurityOptions, func(v string) bool { return strings.Contains(v, "name=rootless") }) {
		out.Mode = "Rootless"
	}
	if cfg := info.RegistryConfig; cfg != nil {
		for _, entry := range cfg.Mirrors[:min(len(cfg.Mirrors), 16)] {
			if validMirror(entry) {
				out.Mirrors = append(out.Mirrors, entry)
			}
		}
		for _, entry := range cfg.InsecureRegistryCIDRs[:min(len(cfg.InsecureRegistryCIDRs), 32)] {
			if p, err := netip.ParsePrefix(entry); err == nil {
				out.InsecureEntries = append(out.InsecureEntries, p.String())
			}
		}
		for name, entry := range cfg.IndexConfigs {
			if !entry.Secure && len(out.InsecureEntries) < 32 && !strings.ContainsAny(name, "/@?#") {
				out.InsecureEntries = append(out.InsecureEntries, shortSafe(name, 255))
			}
		}
		slices.Sort(out.InsecureEntries)
	}
	for _, p := range info.DefaultAddressPools[:min(len(info.DefaultAddressPools), 16)] {
		if base, err := netip.ParsePrefix(p.Base); err == nil {
			out.Pools = append(out.Pools, &spec.DockerAddressPool{Base: base.String(), PrefixLength: p.Size})
		}
	}
	src, err := d.localManager().source(ctx)
	if err != nil || d.Dir == "" {
		if attention == "" {
			out.EditabilityReason = errSource.Error()
		}
		return out, nil
	}
	out.Manager, out.Source = "systemd", src.path
	data, doc, _, err := readConfig(src.path)
	if err != nil {
		out.EditabilityReason = safeError(err)
		return out, nil
	}
	var ipv6 bool
	if json.Unmarshal(doc["ipv6"], &ipv6) == nil {
		out.Ipv6 = &ipv6
	}
	out.Logging = logSettings(info.LoggingDriver, doc["log-opts"])
	if doc["log-opts"] != nil {
		out.Provenance = append(out.Provenance, &v1.DockerSettingSource{Field: "logging", SourceKind: "daemon.json", Availability: "Configured"})
	}
	if state, err := d.record(); err == nil {
		out.PendingFileReview = errors.Is(checkConfigAge(src, data, state), errPendingEdit)
		if out.PendingFileReview && state.ActiveKnown && !state.ActiveAt.Before(src.started) {
			// Docker is still running the saved options, not the pending file.
			active, _ := decodeConfig(state.Active)
			out.Logging = logSettings(info.LoggingDriver, active["log-opts"])
		}
		for _, owned := range []struct {
			field string
			yes   bool
		}{
			{"mirrors", state.Owned.GetMirrors() != nil},
			{"logging", state.Owned.GetLogging() != nil},
			{"addressPools", state.Owned.GetAddressPools() != nil},
		} {
			if owned.yes {
				out.Provenance = append(out.Provenance, &v1.DockerSettingSource{Field: owned.field, SourceKind: "Dockwell"})
			}
		}
	}
	var flags map[string]bool
	if json.Unmarshal(doc["features"], &flags) == nil {
		out.KnownFeatures = make(map[string]bool)
		for _, key := range []string{"containerd-snapshotter", "buildkit", "cdi"} {
			if v, ok := flags[key]; ok {
				out.KnownFeatures[key] = v
			}
		}
	}
	return out, nil
}

func (d *Driver) workloads(ctx context.Context) ([]docker.ContainerSummary, error) {
	var rows []docker.ContainerSummary
	err := d.Client.EachCleanupContainer(ctx, func(c *docker.ContainerSummary) error {
		if len(rows) >= 1200 {
			return errors.New("the Docker workload list is too large to review safely")
		}
		rows = append(rows, *c)
		return nil
	})
	return rows, err
}

func (d *Driver) logProbeImage(ctx context.Context) (string, error) {
	list, err := d.workloads(ctx)
	if err != nil {
		return "", err
	}
	seen := make(map[string]bool)
	for i := range list {
		id := list[i].ImageID
		if seen[id] || !strings.HasPrefix(id, "sha256:") || !docker.IsFullID(strings.TrimPrefix(id, "sha256:")) {
			continue
		}
		seen[id] = true
		image, err := d.Client.InspectImage(ctx, id)
		if err == nil && image.ID == id && image.Config != nil && len(image.Config.Volumes) == 0 {
			return id, nil
		}
		if len(seen) >= 16 {
			break
		}
	}
	return "", errors.New("no suitable local Docker image is available to check the new log limits")
}
