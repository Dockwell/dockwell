// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
)

// LabelSet lists what a container's spec sets that its image could set
// too: its command, entrypoint, user, working directory, health check, and
// each environment variable and label by key. Docker fills in from the
// image what a container leaves out, so a value the image has could be
// either. Reading it back, the label says which, so neither a spec that
// repeats its image nor what the image adds shows as drift.
const LabelSet = docker.OwnPrefix + "set"

// Docker is a Runtime on the Docker Engine.
type Docker struct {
	Client *docker.Client
	// PullLogins remembers which registry login pulled each image, so a copy one
	// app pulled with a login isn't run by another without asking its registry
	// (ADR 0086). Nil remembers in memory alone.
	PullLogins *PullLogins
	// Joins names the networks of other environments a service's container joins,
	// with the name it answers to on each, as a database shared with an app joins
	// the app's link network (ADR 0084): the agent's record of each network's
	// members. A container made anew, by a rollback, an undo or the agent while
	// the control plane is away, joins them as it's made. Nil joins none.
	Joins func(t Target) []Join
	// Settle is how long a container without a health check must keep
	// running after it starts to count as healthy. Zero means 2 s.
	Settle time.Duration
	// ListenWait is how long a new instance with no health check gets to take
	// connections on the ports its routes send to. Zero means 2 minutes.
	ListenWait time.Duration
	// HealthWait bounds waiting for a health check to pass. Zero means 5
	// minutes.
	HealthWait time.Duration
	// Poll is how often Healthy looks. Zero means 250 ms.
	Poll time.Duration
	// Deleted reports a volume deleted through a plan, which the server
	// keeps for its data: the agent's volume.Kept. A service can't mount
	// one, as it isn't in its environment. Nil means none are kept.
	Deleted Deleted
	// CleanupWait bounds removing a container that a change made but
	// couldn't swap in. Zero means 30 s.
	CleanupWait time.Duration
	// Logger hears what the runtime can't do on its own time, such as
	// cleaning up after a change that failed. Nil means slog's default.
	Logger *slog.Logger
	// Folders makes a server path a mount names when it isn't there, with the owner
	// the mount gives. Nil makes none, and leaves it to Docker, which makes it
	// root's.
	Folders FolderMaker
	// Paths finds what no container may mount on this server, and follows
	// symlinks there. Nil checks only what Validate does.
	Paths *Paths
	// Ingress is this server's ingress, which a service with routes joins,
	// and which holds the server's public ports while it has routes. Nil
	// means none.
	Ingress Ingress
	// Logs keeps the logs of the containers a change removes, which Docker
	// removes with them. Nil keeps none.
	Logs LogKeeper
	// Probe makes a check the agent makes of a container. Nil means health.Run,
	// on the container's own address.
	Probe func(ctx context.Context, c *health.Check, address string) (health.Result, error)
	// Soak is how long a new container must stay healthy before anything moves to
	// it: DefaultSoak when zero, and none when negative.
	Soak time.Duration
	// BeforeReplace runs before a container is replaced by one of spec, with the
	// one there, as a database's move to another release asks (ADR 0038): an
	// error stops the change there, with the old one as it was. Nil runs nothing.
	BeforeReplace func(ctx context.Context, old *docker.Container, spec *resourcev1.Container) error
	// Kept keeps the image a pull of a moving tag leaves, for the rollback that
	// would run it again (ADR 0080). Nil keeps none.
	Kept *KeptImages
	// Room says how much is free, and how large it is in all, in bytes, on the disk
	// where Docker keeps images, for the room a pull needs (room.go). Nil checks
	// nothing, and leaves a full disk to Docker's own error.
	Room func(ctx context.Context) (free, size uint64, err error)
	// ResolvePath says where a server path leads. Nil follows its links on this
	// server.
	ResolvePath func(path string) (string, error)
	// soaked holds the containers a rollout has held to Soak, for Healthy, which
	// then has no need to again.
	soaked sync.Map
	// joining is held from checking the names a container takes on networks others
	// share until it is on them, so two changes applying at once can't both find a
	// name free and both take it.
	joining sync.Mutex
	// registries is what the agent keeps of asking registries for images as it
	// plans (registry.go).
	registries registries
	// Now tells the time, for a test. Nil is time.Now.
	Now func() time.Time
	// Points hears each step of a rollout, for a test to stop it there. Nil
	// hears nothing.
	Points fault.Points
}

// Ingress is what the container driver needs of a server's ingress
// (internal/ingress).
type Ingress interface {
	ingress.Upstreams
	// PublicPort reports a port the ingress holds while it has routes.
	PublicPort(ctx context.Context, port uint32, udp bool) (bool, error)
	// RoutedAfter reports a route that still sends to an instance's service once
	// the plan under way applies.
	RoutedAfter(ctx context.Context, instance ingress.Instance, desired []resource.Desired) (bool, error)
}

// Labels of a container that say which deploy made it, which logs are read
// by. They are Dockwell's own, so they aren't drift, and a container without
// them, as one docker compose made, isn't recreated for them.
const (
	LabelRevision = docker.LabelRevision
	LabelChange   = docker.OwnPrefix + "change"
	// LabelReconcile identifies the durable local action that created it.
	LabelReconcile = docker.OwnPrefix + "reconcile"
	// LabelLeads says where the server paths a container mounts led (docker.LabelLeads).
	LabelLeads = docker.LabelLeads
)

// LogKeeper keeps the log of a container about to be removed. A failure to
// keep one never stops the change: it says so in its own log.
type LogKeeper interface {
	// Begin copies what the container's log holds so far, while it still
	// runs, so the step's downtime doesn't wait for that. failed says the
	// container failed, so its log outlives those of containers replaced in
	// the ordinary way.
	Begin(ctx context.Context, c *docker.Container, failed bool) LogKeep
}

// LogKeep is one container's log being kept.
type LogKeep interface {
	// Finish copies what the container wrote since Begin, once it has
	// stopped and before it is removed, for up to LogCatchUp.
	Finish(ctx context.Context, c *docker.Container)
}

// LogCatchUp bounds what a replace waits for a stopped container's last
// lines, which counts as downtime.
const LogCatchUp = time.Second

// Deleted is the list of volumes the server keeps for 7 days once a plan
// takes them out of use: the agent's volume.Kept. It has an environment's
// volumes, which a service can't mount, as they aren't in its environment
// any more; and the volumes Docker made for a container's image, which
// deleting the container, or mounting something else at their paths,
// leaves behind, and which a container of the same name mounts again.
type Deleted interface {
	// Has reports a volume on the list, by its name in Docker.
	Has(name string) (bool, error)
	// KeepImageVolumes lists the volumes a container's image kept data in,
	// by the path each was mounted at, from now.
	KeepImageVolumes(container string, byPath map[string]string) error
	// ImageVolumes returns the volumes listed for a container's image, by
	// path, and when each was listed.
	ImageVolumes(container string) (map[string]string, map[string]time.Time, error)
	// TakeImageVolumes takes the volumes at paths off the list, as the
	// container mounts them again; undo puts them back.
	TakeImageVolumes(container string, paths []string) (undo func(), err error)
}

var _ Runtime = (*Docker)(nil)

// Labels are what a container at t carries for spec: the spec's own, the
// ones Compose gives a service's container, and Dockwell's own.
func Labels(t Target, spec *resourcev1.Container) map[string]string {
	l := maps.Clone(spec.GetLabels())
	if l == nil {
		l = map[string]string{}
	}
	l[docker.LabelProject] = t.Project
	l[docker.LabelService] = t.Service
	l[docker.LabelNumber] = numberLabel(t)
	l[docker.LabelOneoff] = "False"
	l[docker.LabelConfigHash] = ConfigHash(spec)
	maps.Copy(l, OwnLabels(spec))
	return l
}

// ConfigHash is the config hash a container carries for spec, without
// which docker compose doesn't take it as its project's: compose ps leaves
// it out, and compose up fails on its name. It is a SHA-256 of the spec
// with its environment's values left out, as they may be secrets, so it
// changes with the spec. It never equals the hash compose computes from
// its own types, so docker compose up from an export recreates each
// container once, and compose's own hash is on it from then on.
func ConfigHash(spec *resourcev1.Container) string {
	c := proto.CloneOf(spec)
	for k := range c.GetEnv() {
		c.Env[k] = ""
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// OwnLabels are Dockwell's own labels for spec, which an exported Compose
// file carries too, so a container docker compose makes from it reads
// back the same. A spec that sets nothing its image could needs none.
func OwnLabels(spec *resourcev1.Container) map[string]string {
	out := map[string]string{}
	if c := AgentCheck(spec.GetHealthcheck()); c != nil {
		out[health.Label] = c.Encode()
	}
	if r := rolloutLabel(spec.GetRollout()); r != "" {
		out[LabelRollout] = r
	}
	if r := referencesLabel(spec); r != "" {
		out[LabelReferences] = r
	}
	if o := makeOwnersLabel(spec); o != "" {
		out[LabelMakeOwners] = o
	}
	if h := heldPortsLabel(spec); h != "" {
		out[LabelHeldPorts] = h
	}
	var set []string
	add := func(name string, ok bool) {
		if ok {
			set = append(set, name)
		}
	}
	add("command", len(spec.GetCommand()) > 0)
	add("entrypoint", len(spec.GetEntrypoint()) > 0)
	add("no_entrypoint", spec.GetNoEntrypoint())
	add("user", spec.GetUser() != "")
	add("working_dir", spec.GetWorkingDir() != "")
	add("shm_size", spec.GetShmSize() != "")
	add("hostname", spec.GetHostname() != "")
	add("stop_signal", spec.GetStopSignal() != "")
	for _, p := range spec.GetExpose() {
		set = append(set, "expose:"+p)
	}
	if hc := spec.GetHealthcheck(); hc != nil {
		set = append(set, "healthcheck")
		add("healthcheck.interval", hc.GetInterval() != "")
		add("healthcheck.timeout", hc.GetTimeout() != "")
		add("healthcheck.start_period", hc.GetStartPeriod() != "")
		add("healthcheck.retries", hc.GetRetries() != 0)
	}
	for k := range spec.GetEnv() {
		set = append(set, "env:"+k)
	}
	for k := range spec.GetLabels() {
		set = append(set, "label:"+k)
	}
	if len(set) > 0 {
		slices.Sort(set)
		b, _ := json.Marshal(set)
		out[LabelSet] = string(b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LabelReferences holds a container's fields that name values, as its spec has
// them, with ${NAME} in them and never a value. Docker can tell only what those
// fields came to, so reading a container back goes by this, and what they came to
// is never passed on (ADR 0022).
const LabelReferences = docker.OwnPrefix + "references"

// referenceWords are a container's fields that name values, as LabelReferences
// holds them. Its order is fixed, so one spec makes one label, which the config
// hash takes in.
type referenceWords struct {
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Test       []string `json:"healthcheck.test,omitempty"`
	// Scripts says which word of each field is a shell's script, which keeps its
	// names for the shell (binding.RenderField). A label from before has none, and
	// its container had every value written in.
	Scripts map[string]int `json:"scripts,omitempty"`
}

// referencesLabel is LabelReferences for a spec, or empty when it lists nothing.
func referencesLabel(spec *resourcev1.Container) string {
	var words referenceWords
	for _, field := range spec.GetReferences() {
		switch field {
		case binding.FieldCommand:
			words.Command = spec.GetCommand()
		case binding.FieldEntrypoint:
			words.Entrypoint = spec.GetEntrypoint()
		case binding.FieldTest:
			words.Test = spec.GetHealthcheck().GetTest()
		}
	}
	if words.Command == nil && words.Entrypoint == nil && words.Test == nil {
		return ""
	}
	for _, field := range spec.GetReferences() {
		if s := binding.ScriptIn(spec, field); s >= 0 {
			if words.Scripts == nil {
				words.Scripts = map[string]int{}
			}
			words.Scripts[field] = s
		}
	}
	b, _ := json.Marshal(words)
	return string(b)
}

// rendered writes a spec's references into what Docker is asked for, once they have
// their values, which spec's environment holds in a step's call. Nothing else ever
// holds this text: Labels, the config hash and what a plan reads all go by the spec.
func rendered(req *docker.CreateRequest, spec *resourcev1.Container) error {
	for _, field := range spec.GetReferences() {
		words, err := binding.RenderField(binding.WordsOf(spec, field), spec.GetEnv(), binding.ScriptIn(spec, field))
		if err != nil {
			return fmt.Errorf("can't write the %s: %w. Plan the change again", field, err)
		}
		switch field {
		case binding.FieldCommand:
			req.Cmd = words
		case binding.FieldEntrypoint:
			req.Entrypoint = noEntrypoint(words)
		case binding.FieldTest:
			if req.Healthcheck == nil {
				return fmt.Errorf("can't write the %s: the container has no health check. Plan the change again", field)
			}
			test := *req.Healthcheck
			test.Test = words
			req.Healthcheck = &test
		}
	}
	return nil
}

// readWords puts a container's fields that name values back as its spec had them,
// by LabelReferences, whose words are worked out again from the container's own
// environment. A field that doesn't come to what Docker has, as when docker compose
// ran an export without the value in its shell, reads as binding.Unread, and so does
// each field of a container with a label Dockwell can't read: either may hold a
// value, so none of it goes further, and either reads as drift.
func readWords(c *docker.Container, out *resourcev1.Container) {
	label, ok := c.Config.Labels[LabelReferences]
	if !ok {
		return
	}
	var words referenceWords
	unreadable := json.Unmarshal([]byte(label), &words) != nil
	env := envOf(c.Config.Env)
	read := func(field string, want, have []string) []string {
		if !unreadable && want == nil {
			return nil
		}
		out.References = append(out.References, field)
		script, ok := words.Scripts[field]
		if !ok {
			script = -1
		}
		comesTo := func(script int) bool {
			got, err := binding.RenderField(want, env, script)
			if field == binding.FieldEntrypoint {
				got = noEntrypoint(got)
			}
			return err == nil && slices.Equal(got, have)
		}
		// docker compose, running an export, writes each value into a script, as
		// Compose does any value it's given, where Dockwell keeps the name for the
		// shell: that container is the spec too.
		if !unreadable && (comesTo(script) || script >= 0 && comesTo(-1)) {
			return want
		}
		return []string{binding.Unread}
	}
	if w := read(binding.FieldCommand, words.Command, c.Config.Cmd); w != nil {
		out.Command = w
	}
	if w := read(binding.FieldEntrypoint, words.Entrypoint, c.Config.Entrypoint); w != nil {
		out.Entrypoint, out.NoEntrypoint = w, false
	}
	var test []string
	if c.Config.Healthcheck != nil {
		test = c.Config.Healthcheck.Test
	}
	if w := read(binding.FieldTest, words.Test, test); w != nil {
		if out.Healthcheck == nil {
			out.Healthcheck = &resourcev1.Healthcheck{}
		}
		out.Healthcheck.Test = w
	}
}

// noEntrypoint is an entrypoint that comes to "" as Docker is asked for none, empty and
// not left out, as Validate reads a spec's entrypoint of "" and Compose's entrypoint: ""
// is: what runs is the command alone, and not even the image's entrypoint.
func noEntrypoint(words []string) []string {
	if len(words) == 1 && words[0] == "" {
		return []string{}
	}
	return words
}

// hideIn masks values in what Docker said, where a person may read it. Docker quotes
// what it couldn't run, as in exec: "…": executable file not found, so a value in a
// command's first word is in its answer. Call it where Docker answers, before
// anything wraps the error in words of its own.
func hideIn(err error, values []string) error {
	var d *docker.Error
	if len(values) > 0 && errors.As(err, &d) {
		d.Message = binding.Hide(d.Message, values)
	}
	return err
}

// valuesInSpec are the values a step's spec writes in its fields.
func valuesInSpec(spec *resourcev1.Container) []string {
	return binding.ValuesOf(binding.ReferencedNames(spec), spec.GetEnv())
}

// valuesInContainer are the values a container's fields hold, by what its label says
// they name and its own environment. A label Dockwell can't read names nothing.
func valuesInContainer(c *docker.Container) []string {
	label, ok := c.Config.Labels[LabelReferences]
	if !ok {
		return nil
	}
	var words referenceWords
	if json.Unmarshal([]byte(label), &words) != nil {
		return nil
	}
	var names []string
	for _, w := range [][]string{words.Command, words.Entrypoint, words.Test} {
		found, _ := binding.NamesIn(w)
		names = append(names, found...)
	}
	return binding.ValuesOf(names, envOf(c.Config.Env))
}

// envOf reads an environment as Docker reports it, by name. A name with no value is
// one Docker takes out, and it keeps that in the list: docker compose up sends it for
// a variable its shell hasn't got, so what runs hasn't got it either.
func envOf(list []string) map[string]string {
	env := make(map[string]string, len(list))
	for _, kv := range list {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// rolloutSetting is a spec's rollout settings, as LabelRollout holds them.
type rolloutSetting struct {
	Order string `json:"order,omitempty"`
	Drain string `json:"drain,omitempty"`
}

var rolloutOrders = map[resourcev1.RolloutOrder]string{
	resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST: "start-first",
	resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST:  "stop-first",
}

func rolloutLabel(r *resourcev1.Rollout) string {
	if r == nil {
		return ""
	}
	b, _ := json.Marshal(rolloutSetting{Order: rolloutOrders[r.GetOrder()], Drain: r.GetDrain()})
	return string(b)
}

// projectRollout reads a container's rollout settings back from its label.
func projectRollout(label string) *resourcev1.Rollout {
	if label == "" {
		return nil
	}
	var s rolloutSetting
	if err := json.Unmarshal([]byte(label), &s); err != nil {
		return nil
	}
	out := &resourcev1.Rollout{Drain: s.Drain}
	for order, name := range rolloutOrders {
		if name == s.Order {
			out.Order = order
		}
	}
	return out
}

// AgentCheck is a spec's check as the agent makes it, or nil for a check Docker
// makes, or none.
func AgentCheck(hc *resourcev1.Healthcheck) *health.Check {
	if hc.GetHttp() == nil && hc.GetTcp() == nil {
		return nil
	}
	d := func(s string) time.Duration {
		v, _ := time.ParseDuration(s)
		return v
	}
	c := &health.Check{
		Interval: d(hc.GetInterval()), Timeout: d(hc.GetTimeout()), StartPeriod: d(hc.GetStartPeriod()), Retries: int(hc.GetRetries()),
	}
	if h := hc.GetHttp(); h != nil {
		c.HTTP = &health.HTTP{Port: int(h.GetPort()), Path: h.GetPath(), Host: h.GetHost()}
	} else {
		c.TCP = &health.TCP{Port: int(hc.GetTcp().GetPort())}
	}
	return c
}

// projectAgentCheck reads a check the agent makes back from a container's label.
func projectAgentCheck(label string) *resourcev1.Healthcheck {
	c, err := health.Parse(label)
	if err != nil {
		return nil
	}
	dur := func(d time.Duration) string {
		if d <= 0 {
			return ""
		}
		return d.String()
	}
	out := &resourcev1.Healthcheck{
		Interval: dur(c.Interval), Timeout: dur(c.Timeout), StartPeriod: dur(c.StartPeriod), Retries: uint32(max(c.Retries, 0)), //nolint:gosec // retries are few
	}
	if c.HTTP != nil {
		out.Http = &resourcev1.HttpCheck{Port: uint32(c.HTTP.Port), Path: c.HTTP.Path, Host: c.HTTP.Host} //nolint:gosec // a port
	} else {
		out.Tcp = &resourcev1.TcpCheck{Port: uint32(c.TCP.Port)} //nolint:gosec // a port
	}
	return out
}

func setOf(labels map[string]string) map[string]bool {
	var list []string
	_ = json.Unmarshal([]byte(labels[LabelSet]), &list)
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

var restartNames = map[resourcev1.RestartPolicy]string{
	resourcev1.RestartPolicy_RESTART_POLICY_NO:             "no",
	resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE:     "on-failure",
	resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED: "unless-stopped",
	resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS:         "always",
}

func restartPolicy(name string) resourcev1.RestartPolicy {
	if name == "" {
		return resourcev1.RestartPolicy_RESTART_POLICY_NO
	}
	for p, n := range restartNames {
		if n == name {
			return p
		}
	}
	return resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED
}

// restartOf is a spec's restart policy as Docker takes it.
func restartOf(spec *resourcev1.Container) docker.RestartPolicy {
	return docker.RestartPolicy{Name: restartNames[spec.GetRestart()], MaximumRetryCount: int(spec.GetRestartRetries())}
}

func protocolName(p resourcev1.Protocol) string {
	if p == resourcev1.Protocol_PROTOCOL_UDP {
		return "udp"
	}
	return "tcp"
}

// healthcheck is a spec's health check as Docker takes it.
func healthcheck(hc *resourcev1.Healthcheck) *docker.Healthcheck {
	if hc == nil {
		return nil
	}
	if AgentCheck(hc) != nil {
		// The agent makes it, and it stands in for the image's, as a spec's
		// command does. Docker has nothing to run.
		return &docker.Healthcheck{Test: []string{"NONE"}}
	}
	d := func(s string) time.Duration {
		v, _ := time.ParseDuration(s)
		return v
	}
	return &docker.Healthcheck{
		Test: hc.GetTest(), Interval: d(hc.GetInterval()), Timeout: d(hc.GetTimeout()),
		StartPeriod: d(hc.GetStartPeriod()), Retries: int(hc.GetRetries()),
	}
}

// anonymous reports a volume Docker made for a path an image declares,
// which is named by 64 hex digits.
func anonymous(name string) bool { return docker.AnonymousVolume(name) }

// createRequest is what Docker is asked to create for spec at t. Volumes
// Docker made for paths the image declares move from old, the container
// it replaces, if any, so their data stays, as Compose keeps them. Where
// old has none, one kept for the container's image, as a deletion left it,
// is mounted again: those are returned, by path.
func (d *Docker) createRequest(ctx context.Context, t Target, spec *resourcev1.Container, old *docker.Container, kept map[string]string) (*docker.CreateRequest, []string, map[string]string) {
	labels := Labels(t, spec)
	if deploy := resource.DeployOf(ctx); deploy.Revision > 0 {
		labels[LabelRevision] = strconv.Itoa(deploy.Revision)
		labels[LabelChange] = deploy.Change
	}
	if id := resource.DeployOf(ctx).Reconcile; id != "" {
		labels[LabelReconcile] = id
	}
	req := &docker.CreateRequest{Config: docker.Config{
		Image:       spec.GetImage(),
		Cmd:         spec.GetCommand(),
		Entrypoint:  spec.GetEntrypoint(),
		Labels:      labels,
		User:        spec.GetUser(),
		WorkingDir:  spec.GetWorkingDir(),
		Healthcheck: healthcheck(spec.GetHealthcheck()),
	}}
	if spec.GetNoEntrypoint() {
		// Empty, not left out, as Compose sends entrypoint: [].
		req.Entrypoint = []string{}
	}
	req.Env = environment(spec)
	if v, err := time.ParseDuration(spec.GetStopGracePeriod()); err == nil && v > 0 {
		seconds := int(v / time.Second)
		req.StopTimeout = &seconds
	}
	req.HostConfig.RestartPolicy = restartOf(spec)
	req.HostConfig.Privileged, req.HostConfig.CapAdd, req.HostConfig.CapDrop, req.HostConfig.GroupAdd =
		spec.GetPrivileged(), spec.GetCapAdd(), spec.GetCapDrop(), spec.GetGroupAdd()
	req.HostConfig.ExtraHosts = spec.GetExtraHosts()
	req.Hostname, req.Tty, req.OpenStdin, req.StopSignal = spec.GetHostname(), spec.GetTty(), spec.GetStdinOpen(), spec.GetStopSignal()
	req.HostConfig.Init, req.HostConfig.Ulimits, req.HostConfig.DNS = dockerInit(spec), dockerUlimits(spec), spec.GetDns()
	req.HostConfig.ReadonlyRootfs, req.HostConfig.PidsLimit = spec.GetReadOnly(), spec.GetPidsLimit()
	if spec.GetNoNewPrivileges() {
		req.HostConfig.SecurityOpt = []string{noNewPrivileges}
	}
	req.HostConfig.NanoCPUs = cpuNanos(spec.GetResources().GetCpus())
	req.HostConfig.Memory = memoryBytes(spec.GetResources().GetMemory())
	req.HostConfig.MemoryReservation = memoryBytes(spec.GetResources().GetMemoryReservation())
	req.HostConfig.ShmSize = memoryBytes(spec.GetShmSize())
	req.HostConfig.LogConfig = d.logConfig(ctx)
	for _, p := range spec.GetPorts() {
		key := fmt.Sprintf("%d/%s", p.GetContainerPort(), protocolName(p.GetProtocol()))
		if req.HostConfig.PortBindings == nil {
			req.HostConfig.PortBindings = map[string][]docker.PortBinding{}
			req.ExposedPorts = map[string]struct{}{}
		}
		req.ExposedPorts[key] = struct{}{}
		req.HostConfig.PortBindings[key] = append(req.HostConfig.PortBindings[key],
			docker.PortBinding{HostIP: boundIP(p), HostPort: strconv.Itoa(int(p.GetHostPort()))})
	}
	for _, m := range spec.GetTmpfs() {
		if req.HostConfig.Tmpfs == nil {
			req.HostConfig.Tmpfs = map[string]string{}
		}
		req.HostConfig.Tmpfs[m.GetTarget()] = tmpfsOptions(m)
	}
	for _, p := range spec.GetExpose() {
		if req.ExposedPorts == nil {
			req.ExposedPorts = map[string]struct{}{}
		}
		req.ExposedPorts[p] = struct{}{}
	}
	targets := mountTargets(spec)
	for _, m := range spec.GetMounts() {
		switch src := m.GetSource().(type) {
		case *resourcev1.Mount_Volume:
			req.HostConfig.Mounts = append(req.HostConfig.Mounts, docker.Mount{
				Type: "volume", Source: t.VolumeName(m), Target: m.GetTarget(), ReadOnly: m.GetReadOnly(),
			})
		case *resourcev1.Mount_HostPath:
			req.HostConfig.Binds = append(req.HostConfig.Binds, bind(src.HostPath, m))
		}
	}
	from := imageVolumes(old)
	// Made again while the control plane is away, it mounts the data its image kept
	// before, as a replace would (ADR 0054).
	for p, name := range resource.ImageVolumesFrom(ctx) {
		if _, ok := from[p]; !ok {
			from[p] = name
		}
	}
	restored := map[string]string{}
	for _, p := range slices.Sorted(maps.Keys(kept)) {
		if _, ok := from[p]; !ok && !targets[p] {
			from[p], restored[p] = kept[p], kept[p]
		}
	}
	for _, p := range slices.Sorted(maps.Keys(from)) {
		if !targets[p] {
			req.HostConfig.Mounts = append(req.HostConfig.Mounts, docker.Mount{Type: "volume", Source: from[p], Target: p})
		}
	}
	if spec.GetHostNetwork() {
		req.HostConfig.NetworkMode = "host"
		return req, nil, restored
	}
	aliases := endpointAliases(t, spec)
	var later []string
	for i, name := range dockerNetworks(t, spec) {
		if i == 0 {
			req.HostConfig.NetworkMode = name
		} else if !d.Client.AtLeast("1.44") {
			// Older engines take one network at create; the rest join after.
			later = append(later, name)
			continue
		}
		if req.NetworkingConfig.EndpointsConfig == nil {
			req.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{}
		}
		req.NetworkingConfig.EndpointsConfig[name] = &docker.EndpointSettings{Aliases: aliases}
	}
	return req, later, restored
}

// logConfig is how Docker keeps a new container's log. Docker's default,
// json-file, keeps it without a limit, which can fill a server's disk, so it
// gets rotation. Any other default is the operator's choice, which stands:
// local rotates by itself, and journald and the like keep logs elsewhere. A
// container's log config isn't part of its spec, so it can't read as drift,
// and it comes in with the next replace, never by itself.
func (d *Docker) logConfig(ctx context.Context) *docker.LogConfig {
	info, err := d.Client.Info(ctx)
	if err != nil || info.LoggingDriver == "" || info.LoggingDriver == docker.LogDriverJSONFile {
		return docker.Rotation()
	}
	return nil
}

// failedBefore reports a container that wasn't well when a step came to
// replace it, so its log is worth keeping longer than an ordinary one's.
func failedBefore(c *docker.Container) bool {
	s := c.State
	switch {
	case s.Restarting, s.Health != nil && s.Health.Status == "unhealthy":
		return true
	default:
		return !s.Running && s.ExitCode != 0
	}
}

// beginKeepingLogs starts keeping the log of a container a step is about to
// remove. Dockwell's rollback removes what failed, whatever its state says. It can
// remove what ran too, as one pinned to a digest does, and that didn't fail.
func (d *Docker) beginKeepingLogs(ctx context.Context, c *docker.Container) LogKeep {
	if d.Logs == nil {
		return nil
	}
	return d.Logs.Begin(ctx, c, madeByWhatFailed(ctx, c) || failedBefore(c))
}

// madeByWhatFailed reports a container the deploy a rollback rolls back made: a
// rollback's revision is the one after that deploy's.
func madeByWhatFailed(ctx context.Context, c *docker.Container) bool {
	deploy := resource.DeployOf(ctx)
	revision, _ := strconv.Atoi(c.Config.Labels[LabelRevision])
	return deploy.Rollback && revision == deploy.Revision-1
}

// finishKeepingLogs takes in a stopped container's last lines, and how it
// ended, before it is removed.
func (d *Docker) finishKeepingLogs(ctx context.Context, keep LogKeep, c *docker.Container) {
	if keep == nil {
		return
	}
	ctx, cancel := resource.Finishing(ctx, LogCatchUp)
	defer cancel()
	if stopped, err := d.Client.InspectContainer(ctx, c.ID); err == nil {
		c = stopped
	}
	keep.Finish(ctx, c)
}

// mountTargets are the paths spec mounts something at, in memory or not.
func mountTargets(spec *resourcev1.Container) map[string]bool {
	out := map[string]bool{}
	for _, m := range spec.GetMounts() {
		out[m.GetTarget()] = true
	}
	for _, m := range spec.GetTmpfs() {
		out[m.GetTarget()] = true
	}
	return out
}

// imageVolumes are the volumes Docker made for c's image, by the path
// each is mounted at. They hold data, such as a database's, that no spec
// names.
func imageVolumes(c *docker.Container) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	for _, m := range c.Mounts {
		if m.Type == "volume" && anonymous(m.Name) {
			out[m.Destination] = m.Name
		}
	}
	return out
}

// covered are the volumes of c's image at paths spec mounts something
// else at, which a replace leaves behind.
func covered(c *docker.Container, spec *resourcev1.Container) map[string]string {
	out := map[string]string{}
	targets := mountTargets(spec)
	for p, name := range imageVolumes(c) {
		if targets[p] {
			out[p] = name
		}
	}
	return out
}

// keptVolumes are the volumes kept for the image of the container at t,
// by path, that are still on the server.
func (d *Docker) keptVolumes(ctx context.Context, t Target) (map[string]string, error) {
	if d.Deleted == nil {
		return nil, nil
	}
	byPath, _, err := d.Deleted.ImageVolumes(t.keptKey())
	if err != nil {
		return nil, err
	}
	for p, name := range byPath {
		_, err := d.Client.InspectVolume(ctx, name)
		switch {
		case docker.IsNotFound(err):
			delete(byPath, p)
		case err != nil:
			return nil, err
		}
	}
	return byPath, nil
}

// keep lists the volumes of the container at t's image as kept, which a
// change took out of use. They stay on the server whatever happens: one
// the list can't take is logged, and stays until a docker volume prune.
func (d *Docker) keep(t Target, byPath map[string]string) {
	if d.Deleted == nil || len(byPath) == 0 {
		return
	}
	if err := d.Deleted.KeepImageVolumes(t.keptKey(), byPath); err != nil {
		d.logger().Warn("can't list the volumes a container's image kept data in, so they stay on the server until docker volume prune removes them",
			slog.String("container", t.Name), slog.Any("volumes", byPath), slog.String("error", err.Error()))
	}
}

// relabels are Docker's options for relabelling a server path.
var relabels = map[resourcev1.Relabel]string{resourcev1.Relabel_RELABEL_SHARED: "z", resourcev1.Relabel_RELABEL_PRIVATE: "Z"}

// bind is a server path's mount as Docker's binds have it, such as
// /srv/data:/data:ro,z.
func bind(source string, m *resourcev1.Mount) string {
	var options []string
	if m.GetReadOnly() {
		options = append(options, "ro")
	}
	if o, ok := relabels[m.GetRelabel()]; ok {
		options = append(options, o)
	}
	b := source + ":" + m.GetTarget()
	if len(options) > 0 {
		b += ":" + strings.Join(options, ",")
	}
	return b
}

// environment is a spec's environment as Docker takes it. For a service
// it holds the values of what the service uses, which the service driver
// resolved in memory for this one call (binding.Environment), so nothing
// here may log it or keep it. The config hash leaves its values out too, and a
// field that names values goes into it as its names (rendered).
func environment(spec *resourcev1.Container) []string {
	keys := slices.Sorted(maps.Keys(spec.GetEnv()))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+spec.GetEnv()[k])
	}
	return out
}

// exposedPorts reads a port as Docker keeps it into what a spec holds: one port with its
// protocol. Docker keeps what it was asked for, and docker compose sends expose: as it
// was written, so that can be "3000", "3000/tcp", or a range of either. One that can't
// be read stays as it is, for Validate to say so.
func exposedPorts(key string) []string {
	ports, protocol, found := strings.Cut(key, "/")
	if !found {
		protocol = "tcp"
	}
	low, high, isRange := strings.Cut(ports, "-")
	if !isRange {
		return []string{ports + "/" + protocol}
	}
	first, errFirst := strconv.Atoi(low)
	last, errLast := strconv.Atoi(high)
	if errFirst != nil || errLast != nil || last < first || last-first >= MaxPorts {
		return []string{key}
	}
	out := make([]string, 0, last-first+1)
	for p := first; p <= last; p++ {
		out = append(out, strconv.Itoa(p)+"/"+protocol)
	}
	return out
}

// Project reads a container as Docker reports it back into a spec, at t,
// leaving out what Docker took from its image: an image's environment,
// labels, command and the rest, unless the spec set them too, as LabelSet
// says; Dockwell's and Compose's own labels; and anything Docker adds,
// such as IDs, start times and the volumes it made for the image.
func Project(c *docker.Container, img *docker.Config, t Target) *resourcev1.Container {
	imageKnown := img != nil
	if !imageKnown {
		img = &docker.Config{}
	}
	set := setOf(c.Config.Labels)
	out := &resourcev1.Container{
		Image:   c.Config.Image,
		Restart: restartPolicy(c.HostConfig.RestartPolicy.Name),
		//nolint:gosec // a count of restarts is small, and Docker keeps it as an int
		RestartRetries: uint32(max(c.HostConfig.RestartPolicy.MaximumRetryCount, 0)),
		Privileged:     c.HostConfig.Privileged,
		CapAdd:         c.HostConfig.CapAdd,
		CapDrop:        c.HostConfig.CapDrop,
		GroupAdd:       c.HostConfig.GroupAdd,
		ExtraHosts:     extraHostsOf(c.HostConfig.ExtraHosts),
		Resources:      projectResources(&c.HostConfig),
		Tmpfs:          projectTmpfs(&c.HostConfig),
		Init:           c.HostConfig.Init != nil && *c.HostConfig.Init,
		Tty:            c.Config.Tty,
		StdinOpen:      c.Config.OpenStdin,
		Ulimits:        ulimitsOf(c.HostConfig.Ulimits),
		ReadOnly:       c.HostConfig.ReadonlyRootfs,
		PidsLimit:      max(c.HostConfig.PidsLimit, 0),
		Dns:            c.HostConfig.DNS,
		NoNewPrivileges: slices.ContainsFunc(c.HostConfig.SecurityOpt, func(o string) bool {
			on, ok := NoNewPrivileges(o)
			return ok && on
		}),
	}
	// Docker names each container for the start of its ID unless asked for another,
	// and gives one on the server's network the server's name.
	if h := c.Config.Hostname; set["hostname"] || h != "" && !strings.HasPrefix(c.ID, h) && c.HostConfig.NetworkMode != "host" {
		out.Hostname = h
	}
	if s := c.Config.StopSignal; set["stop_signal"] || s != "" && s != img.StopSignal {
		out.StopSignal = s
	}
	if s := c.HostConfig.ShmSize; set["shm_size"] || s != dockerShmSize {
		out.ShmSize = FormatMemory(s)
	}
	if set["command"] || !slices.Equal(c.Config.Cmd, img.Cmd) {
		out.Command = c.Config.Cmd
	}
	// Docker gives a container the image's entrypoint unless it was asked
	// for none: one with none, of an image that has one, runs none. The
	// label tells for an image without one.
	switch {
	case len(c.Config.Entrypoint) == 0 && (set["no_entrypoint"] || len(img.Entrypoint) > 0):
		out.NoEntrypoint = true
	case set["entrypoint"] || !slices.Equal(c.Config.Entrypoint, img.Entrypoint):
		out.Entrypoint = c.Config.Entrypoint
	}
	if set["user"] || c.Config.User != img.User {
		out.User = c.Config.User
	}
	if set["working_dir"] || c.Config.WorkingDir != img.WorkingDir {
		out.WorkingDir = c.Config.WorkingDir
	}
	imageEnv := envOf(img.Env)
	env := envOf(c.Config.Env)
	// A name with no value takes the image's value out of what runs. A spec that sets
	// it reads that as gone, and one that doesn't says the image's stands, so that reads
	// as nothing in it, which differs from what the image gives.
	for _, name := range c.Config.Env {
		if _, ok := imageEnv[name]; ok && !set["env:"+name] {
			env[name] = ""
		}
	}
	for k, v := range env {
		if iv, ok := imageEnv[k]; set["env:"+k] || !ok || iv != v {
			if out.Env == nil {
				out.Env = map[string]string{}
			}
			out.Env[k] = v
		}
	}
	for k, v := range c.Config.Labels {
		if docker.IsOwnLabel(k) {
			continue
		}
		if iv, ok := img.Labels[k]; set["label:"+k] || !ok || iv != v {
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			out.Labels[k] = v
		}
	}
	if label := c.Config.Labels[health.Label]; label != "" {
		out.Healthcheck = projectAgentCheck(label)
	} else {
		out.Healthcheck = projectHealthcheck(c.Config.Healthcheck, img.Healthcheck, set)
	}
	out.Rollout = projectRollout(c.Config.Labels[LabelRollout])
	if s := c.Config.StopTimeout; s != nil {
		out.StopGracePeriod = (time.Duration(*s) * time.Second).String()
	}
	// Docker lists here what its image exposes and what it publishes as well, and a
	// spec says neither. With no image to tell by, it says what the spec does alone.
	for key := range c.Config.ExposedPorts {
		for _, port := range exposedPorts(key) {
			_, fromImage := img.ExposedPorts[port]
			_, published := c.HostConfig.PortBindings[port]
			if set["expose:"+port] || imageKnown && !fromImage && !published {
				out.Expose = append(out.Expose, port)
			}
		}
	}
	slices.Sort(out.Expose)
	out.Expose = slices.Compact(out.Expose)
	held := heldPorts(c.Config.Labels[LabelHeldPorts])
	for key, bindings := range c.HostConfig.PortBindings {
		port, transport, _ := strings.Cut(key, "/")
		cp, _ := strconv.ParseUint(port, 10, 16)
		protocol := resourcev1.Protocol_PROTOCOL_TCP
		if transport == "udp" {
			protocol = resourcev1.Protocol_PROTOCOL_UDP
		}
		for _, b := range bindings {
			hp, _ := strconv.ParseUint(b.HostPort, 10, 16)
			p := &resourcev1.PortMapping{HostPort: uint32(hp), ContainerPort: uint32(cp), Protocol: protocol, HostIp: b.HostIP}
			if ip, ok := held[heldKey(p)]; ok && b.HostIP == heldIP {
				p.HostIp, p.Held = ip, true
			}
			out.Ports = append(out.Ports, p)
		}
	}
	unscope := func(name string) string {
		if prefix := t.Project + "_"; t.Scoped && strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
		return name
	}
	// What a service mounts is its environment's, by a name of that form, and
	// anything else is a volume Dockwell didn't make.
	outside := func(name string) bool { return t.Scoped && !strings.HasPrefix(name, t.Project+"_") }
	for _, m := range c.HostConfig.Mounts {
		switch {
		case m.Type == "volume" && m.Source != "" && !anonymous(m.Source):
			out.Mounts = append(out.Mounts, &resourcev1.Mount{
				Source: &resourcev1.Mount_Volume{Volume: unscope(m.Source)}, Target: m.Target, ReadOnly: m.ReadOnly, External: outside(m.Source),
			})
		case m.Type == "bind":
			out.Mounts = append(out.Mounts, &resourcev1.Mount{
				Source: &resourcev1.Mount_HostPath{HostPath: m.Source}, Target: m.Target, ReadOnly: m.ReadOnly,
			})
		}
	}
	for _, b := range c.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		m := &resourcev1.Mount{Target: parts[1]}
		if len(parts) > 2 {
			for _, o := range strings.Split(parts[2], ",") {
				switch o {
				case "ro":
					m.ReadOnly = true
				case "z", "Z":
					// Dockwell relabels a server path alone.
					if strings.HasPrefix(parts[0], "/") {
						m.Relabel = resourcev1.Relabel_RELABEL_SHARED
						if o == "Z" {
							m.Relabel = resourcev1.Relabel_RELABEL_PRIVATE
						}
					}
				}
			}
		}
		switch {
		case strings.HasPrefix(parts[0], "/"):
			m.Source = &resourcev1.Mount_HostPath{HostPath: parts[0]}
		case anonymous(parts[0]):
			continue
		default:
			m.Source, m.External = &resourcev1.Mount_Volume{Volume: unscope(parts[0])}, outside(parts[0])
		}
		out.Mounts = append(out.Mounts, m)
	}
	projectNetworks(c, t, out)
	readWords(c, out)
	readMakeOwners(c, out)
	return out
}

// LabelMakeOwners holds whom each server path a container mounts was to be made
// for, where it wasn't there, by the mount's target, as Docker keeps nothing of it.
const LabelMakeOwners = docker.OwnPrefix + "make-owners"

// makeOwnersLabel is LabelMakeOwners for a spec, such as "/var/www/html=82:82", or
// empty when no mount says one. Mounts are sorted, so one spec makes one label.
func makeOwnersLabel(spec *resourcev1.Container) string {
	var out []string
	for _, m := range spec.GetMounts() {
		if m.GetHostPath() != "" && m.GetMakeOwner() != "" {
			out = append(out, m.GetTarget()+"="+m.GetMakeOwner())
		}
	}
	return strings.Join(out, ",")
}

// readMakeOwners puts back what LabelMakeOwners says on the mounts read from a
// container.
func readMakeOwners(c *docker.Container, out *resourcev1.Container) {
	label := c.Config.Labels[LabelMakeOwners]
	if label == "" {
		return
	}
	owners := map[string]string{}
	for _, pair := range strings.Split(label, ",") {
		if target, owner, ok := strings.Cut(pair, "="); ok {
			owners[target] = owner
		}
	}
	for _, m := range out.GetMounts() {
		if m.GetHostPath() != "" {
			m.MakeOwner = owners[m.GetTarget()]
		}
	}
}

// tmpfsOptions is a tmpfs as Docker takes it. Docker gives it noexec, nosuid and
// nodev, as Compose reads it too.
func tmpfsOptions(t *resourcev1.Tmpfs) string {
	if n := memoryBytes(t.GetSize()); n > 0 {
		return "size=" + strconv.FormatInt(n, 10)
	}
	return ""
}

// projectTmpfs reads what a container holds in memory, in either of the two forms
// docker compose sends: tmpfs as a list, and a volume of type tmpfs. Dockwell makes
// the first alone, and a file's two forms are one spec.
func projectTmpfs(h *docker.HostConfig) []*resourcev1.Tmpfs {
	var out []*resourcev1.Tmpfs
	for target, options := range h.Tmpfs {
		t := &resourcev1.Tmpfs{Target: target}
		for o := range strings.SplitSeq(options, ",") {
			if size, ok := strings.CutPrefix(o, "size="); ok {
				n, _ := parseMemory(size)
				t.Size = FormatMemory(n)
			}
		}
		out = append(out, t)
	}
	for _, m := range h.Mounts {
		if m.Type == "tmpfs" {
			t := &resourcev1.Tmpfs{Target: m.Target}
			if m.TmpfsOptions != nil {
				t.Size = FormatMemory(m.TmpfsOptions.SizeBytes)
			}
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *resourcev1.Tmpfs) int { return strings.Compare(a.GetTarget(), b.GetTarget()) })
	return out
}

// projectHealthcheck reads a container's health check, leaving out what
// Docker took from the image's. A container whose spec sets no check has
// the image's whole; one whose spec sets one has the image's values only
// in the fields the spec left out, as LabelSet says.
func projectHealthcheck(c, img *docker.Healthcheck, set map[string]bool) *resourcev1.Healthcheck {
	if c == nil {
		return nil
	}
	if img == nil {
		img = &docker.Healthcheck{}
	}
	if !set["healthcheck"] && slices.Equal(c.Test, img.Test) &&
		c.Interval == img.Interval && c.Timeout == img.Timeout && c.StartPeriod == img.StartPeriod && c.Retries == img.Retries {
		return nil
	}
	// fromImage reports a value Docker filled in from the image.
	fromImage := func(field string, same bool) bool { return set["healthcheck"] && !set["healthcheck."+field] && same }
	dur := func(field string, v, iv time.Duration) string {
		if v == 0 || fromImage(field, v == iv) {
			return ""
		}
		return v.String()
	}
	out := &resourcev1.Healthcheck{
		Test:        c.Test,
		Interval:    dur("interval", c.Interval, img.Interval),
		Timeout:     dur("timeout", c.Timeout, img.Timeout),
		StartPeriod: dur("start_period", c.StartPeriod, img.StartPeriod),
	}
	if c.Retries > 0 && !fromImage("retries", c.Retries == img.Retries) {
		out.Retries = uint32(c.Retries) //nolint:gosec // retries are few
	}
	return out
}

// find returns the container named t.Name exactly, or nil. Docker takes a
// name as a reference that can match another container's ID, so every
// lookup goes through here, and what follows acts on the ID it returns.
func (d *Docker) find(ctx context.Context, name string) (*docker.Container, error) {
	c, err := d.Client.ContainerNamed(ctx, name)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	return c, err
}

// Inspect reads the container at t, and its image's defaults: its newest, which
// a rollout under way, or one that stopped partway, is moving toward.
func (d *Docker) Inspect(ctx context.Context, t Target) (*resourcev1.Container, error) {
	c, err := d.current(ctx, t)
	if err != nil || c == nil {
		return nil, err
	}
	return d.projected(ctx, t, c)
}

// ImageAt is the image the container at t runs: its ID, and what its registry
// calls it, as name@sha256:…, where one does. A container that's gone runs none.
func (d *Docker) ImageAt(ctx context.Context, t Target) (resource.Image, error) {
	c, err := d.current(ctx, t)
	if err != nil || c == nil {
		return resource.Image{}, err
	}
	out := resource.Image{ID: c.Image}
	img, err := d.Client.InspectImage(ctx, c.Image)
	switch {
	case docker.IsNotFound(err):
		return out, nil
	case err != nil:
		return resource.Image{}, err
	}
	if digest := img.Digest(c.Config.Image); digest != "" {
		out.Digest = docker.Repository(c.Config.Image) + "@" + digest
	}
	return out, nil
}

// RunStateAt is how the container at t stands: whether it runs, why Docker
// couldn't start it, if it couldn't, and whether its policy has it run.
func (d *Docker) RunStateAt(ctx context.Context, t Target) (resource.RunState, error) {
	c, sole, err := d.stands(ctx, t)
	if err != nil || c == nil {
		return resource.RunState{}, err
	}
	policy := c.HostConfig.RestartPolicy.Name
	return resource.RunState{
		Exists: true, Running: c.State.Running || c.State.Restarting, Failed: c.State.Error,
		Sole: sole, Revision: c.Stand(0).Revision, Adopted: c.Stand(0).Adopted, Image: c.Image,
		ID: c.ID, Reconcile: c.Config.Labels[LabelReconcile], Created: c.State.Status == "created" && c.State.StartedAt.IsZero(),
		Leaving: c.State.Status == "removing" || c.State.Status == "dead",
		Wants:   policy == "unless-stopped" || policy == "always" || policy == "on-failure",
		Volumes: imageVolumes(c),
	}, nil
}

// StartAt starts the container at t, as it is.
func (d *Docker) StartAt(ctx context.Context, t Target) error {
	c, _, err := d.stands(ctx, t)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("there's no container for %s to start", t.Name)
	}
	return d.Client.StartContainer(ctx, c.ID)
}

// stands is the container that says how t stands, for the agent's own look while
// the control plane is away: chosen as the health monitor's list is read, so the
// two looks can't disagree (docker.Pick), with whether it's the only one that
// isn't leaving. One that's leaving is returned only when nothing else answers
// to t, and then says so (RunState.Leaving). A step reads current instead, whose
// order finishes a rollout that was cut short.
func (d *Docker) stands(ctx context.Context, t Target) (c *docker.Container, sole bool, err error) {
	all, err := d.instances(ctx, t)
	if err != nil {
		return nil, false, err
	}
	var pick docker.Pick
	var best *instance
	for _, i := range all {
		if pick.Add(i.Stand(i.number)) {
			best = i
		}
	}
	if best != nil {
		return best.Container, pick.Sole(), nil
	}
	if left, ok := pick.Leaving(); ok {
		for _, i := range all {
			if i.ID == left.ID {
				return i.Container, false, nil
			}
		}
	}
	c, err = d.find(ctx, t.Name)
	return c, c != nil, err
}

// HoldAt says why the container at t can't be made again from spec now, with
// volumes, the volumes Docker made for its image's paths as last seen, or nothing
// when it can (resource.Reviver).
func (d *Docker) HoldAt(ctx context.Context, t Target, spec *resourcev1.Container, volumes map[string]string) (string, error) {
	targets := mountTargets(spec)
	mine := map[string]string{} // by volume, what it's to people
	for _, m := range spec.GetMounts() {
		if m.GetVolume() == "" {
			continue
		}
		name := t.VolumeName(m)
		if _, err := d.Client.InspectVolume(ctx, name); docker.IsNotFound(err) {
			return fmt.Sprintf("The volume %s it mounts is gone, and Docker would make it again empty, which would hide that its data is gone. "+
				"Put the volume back, or apply it again from the control plane to start it empty", m.GetVolume()), nil
		} else if err != nil {
			return "", err
		}
		mine[name] = "the volume " + m.GetVolume()
	}
	for _, p := range slices.Sorted(maps.Keys(volumes)) {
		if targets[p] {
			continue
		}
		if _, err := d.Client.InspectVolume(ctx, volumes[p]); docker.IsNotFound(err) {
			return fmt.Sprintf("The volume Docker made for %s, where its image keeps its data, is gone, and making it again would start it empty. "+
				"Apply it again from the control plane to start it empty", p), nil
		} else if err != nil {
			return "", err
		}
		mine[volumes[p]] = "the volume at " + p
	}
	// Where the image keeps data is only known from the image, so one that isn't
	// here, or that Docker can't read, holds the container back: made again
	// unchecked, it could start empty beside its old data. The caller has the
	// image its record names in place before it asks (ADR 0054).
	img, err := d.Client.InspectImage(ctx, spec.GetImage())
	switch {
	case docker.IsNotFound(err):
		return fmt.Sprintf("Its image, %s, isn't on this server, so the agent can't tell where it keeps data, and making it again could start it empty. "+
			"Apply it again from the control plane once it's back", spec.GetImage()), nil
	case err != nil:
		return "", fmt.Errorf("can't read its image, %s, to see where it keeps data: %w", spec.GetImage(), err)
	}
	if img.Config != nil {
		for _, p := range slices.Sorted(maps.Keys(img.Config.Volumes)) {
			if _, seen := volumes[p]; !seen && !targets[p] {
				return fmt.Sprintf("Its image keeps data at %s, and the agent never saw the volume it was in, so making it again would start it empty. "+
					"Apply it again from the control plane once it's back", p), nil
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(mine)) {
		list, err := d.Client.ListContainers(ctx, map[string][]string{"volume": {name}})
		if err != nil {
			return "", err
		}
		for i := range list {
			other := &list[i]
			if other.Labels[docker.LabelProject] != t.Project || other.Labels[docker.LabelService] != t.Service {
				who := other.ID
				if len(other.Names) > 0 {
					who = strings.TrimPrefix(other.Names[0], "/")
				}
				return fmt.Sprintf("Another container, %s, mounts %s now, as one that took its place would, and two on one volume can damage its data. "+
					"Remove %s, or apply this again from the control plane once it's back", who, mine[name], who), nil
			}
		}
	}
	return "", nil
}

// projected reads a container back into a spec, with its image's defaults.
func (d *Docker) projected(ctx context.Context, t Target, c *docker.Container) (*resourcev1.Container, error) {
	var defaults *docker.Config
	img, err := d.Client.InspectImage(ctx, c.Image)
	switch {
	case err == nil:
		defaults = img.Config
	case !docker.IsNotFound(err):
		return nil, err
	}
	return Project(c, defaults, t), nil
}

// ensureImage pulls an image that isn't on the server yet. One that is
// stays as it is, as docker compose up leaves it. It asks for the image
// with its tag: Docker takes a name without one, made of hex digits such
// as cafe, as a prefix of any image's ID too.
func (d *Docker) ensureImage(ctx context.Context, ref string) error {
	img, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	if err == nil {
		return d.mayReuse(ctx, ref, img.ID, "")
	}
	if !docker.IsNotFound(err) {
		return err
	}
	if docker.IsBuilt(ref) {
		return resource.PlainlyBecause(resource.ReasonBuild, "This server hasn't the image "+ref+", which Dockwell builds, and nothing pulls one. Deploy again to build it",
			fmt.Errorf("the image %s isn't on this server, and only its service's build makes it", ref))
	}
	resource.Stage(ctx, resource.PhasePulling, "Pulling "+ref)
	err = d.Client.PullImageBytes(ctx, ref, func(done, total int64) {
		resource.Tell(ctx, resource.Activity{Message: "Pulling " + ref + ": " + docker.SizeOf(done, total), Phase: resource.PhasePulling, Done: done, Total: total})
	})
	d.pulled(ref)
	if err != nil && !docker.IsUnavailable(err) && ctx.Err() == nil {
		return pullFailure(ctx, ref, err)
	}
	if err == nil {
		d.notePull(ctx, ref)
	}
	return err
}

// PullAgain asks for a container's image again, as a deploy of the tag it already
// names does, and says whether what it got is what runs, and is running. What it
// finds goes on record, before anything is replaced. With pulled, a run stopped
// partway has done the pull, so it needs no registry. See "What pull means" in ADR
// 0021.
func (d *Docker) PullAgain(ctx context.Context, t Target, spec *resourcev1.Container, pulled bool) (same bool, err error) {
	ref := spec.GetImage()
	old, err := d.current(ctx, t)
	if err != nil {
		return false, err
	}
	// Only a container of this tag can say what it pointed at. Another image is
	// there after a failure, and what replaces that isn't a tag that moved.
	moving := old != nil && resource.BareImage(old.Config.Image) == resource.BareImage(ref)
	previous := ""
	if moving {
		if img, err := d.Client.InspectImage(ctx, old.Image); err == nil {
			previous = img.Digest(ref)
		}
	}
	if !pulled {
		if err := d.pullAgain(ctx, t, old, ref); err != nil {
			return false, err
		}
	}
	img, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	if err != nil {
		return false, fmt.Errorf("pulled %s, but can't read it back: %w", ref, err)
	}
	digest := img.Digest(ref)
	all, err := d.instances(ctx, t)
	if err != nil {
		return false, err
	}
	// A rollout stopped partway reads as its new container, with the old one still
	// there or a name still to give: that isn't done, however it reads.
	finished := len(all) == 1 && !all[0].temp && !all[0].stopping
	if finished && old.Image == img.ID && notRunning(old) == "" {
		resource.Report(ctx, resource.Outcome{Digest: digest})
		return true, nil
	}
	replacing := moving && old.Image != img.ID
	if replacing {
		// Docker leaves what the tag named with no name, which a prune removes.
		if err := d.Kept.Keep(ctx, ref, old.Image); err != nil {
			d.logger().Warn("can't keep the image a moved tag left, so a rollback pulls it again if its registry still has it", "error", err)
		}
	}
	resource.Report(ctx, resource.Outcome{Digest: digest, Previous: previous, Replacing: replacing})
	return false, nil
}

// pullAgain pulls ref. One that fails leaves what runs running, and fails the
// step. Where nothing is running there's nothing to keep safe, so the image the
// server has will do, which is how someone gets a service back while its registry
// is down. An image that came from no registry isn't waiting on one: a registry
// that says it hasn't got it has said all there is, so what the server has under
// that tag stands. Unless that's a new build over what's there, as nothing but the
// tag it just lost names what ran, so nothing could go back to it, and it's refused.
func (d *Docker) pullAgain(ctx context.Context, t Target, old *docker.Container, ref string) error {
	resource.Stage(ctx, resource.PhasePulling, "Pulling "+ref+" again")
	err := d.Client.PullImageBytes(ctx, ref, func(done, total int64) {
		resource.Tell(ctx, resource.Activity{Message: "Pulling " + ref + " again, " + docker.SizeOf(done, total), Phase: resource.PhasePulling, Done: done, Total: total})
	})
	d.pulled(ref)
	if err == nil {
		d.notePull(ctx, ref)
	}
	if err == nil || docker.IsUnavailable(err) || ctx.Err() != nil {
		return err
	}
	if docker.NotPullable(err) {
		if img, have := d.Client.InspectImage(ctx, docker.WithTag(ref)); have == nil && img.Digest(ref) == "" {
			if old != nil && resource.BareImage(old.Config.Image) == resource.BareImage(ref) && old.Image != img.ID {
				resource.Report(ctx, resource.Outcome{Untouched: true})
				return builtOverFailure(t, old, ref, err)
			}
			resource.Report(ctx, resource.Outcome{Note: fmt.Sprintf("%s was built or loaded on this server, and no registry gives it to a pull, so %s runs the image this server has under that tag", ref, t.Service)})
			return nil
		}
	}
	if old != nil && notRunning(old) == "" {
		resource.Report(ctx, resource.Outcome{Untouched: true})
		return pullAgainFailure(ctx, ref, err)
	}
	switch _, have := d.Client.InspectImage(ctx, docker.WithTag(ref)); {
	case have == nil:
		resource.Report(ctx, resource.Outcome{Note: fmt.Sprintf("Can't pull %s again: %s, so %s runs the image this server has. Deploy again once its registry answers, to get anything newer", ref, err, t.Service)})
		return nil
	case docker.IsNotFound(have):
		return fmt.Errorf("can't pull %s: %w. %s isn't running, and this server doesn't have that image, so it can't start. Check that its registry answers, and the image's name and tag, then deploy again. Dockwell can't pull private images yet", ref, err, t.Service)
	default:
		return have
	}
}

// exitRefused is what a container exits with when the kernel won't run its binary, as
// real Docker reports an image for another architecture.
const exitRefused = 255

// binaryRefused reports a container that failed because the kernel wouldn't run its
// binary, which is what an image for another architecture comes to. An image built for
// another architecture isn't enough to say that: 386 on amd64, arm on arm64 and anything
// under emulation really run, and then a failure is the app's own. So it takes what
// Docker gave of a real refusal: exit 255 from such an image, or the runtime's own words
// in what the container logged.
func (d *Docker) binaryRefused(ctx context.Context, c *docker.Container) bool {
	return c.State.ExitCode == exitRefused && d.builtForAnother(ctx, c) || d.loggedRefusal(ctx, c)
}

// builtForAnother reports a container whose image isn't built for this server, as
// its image says and the engine does. Either side that can't be read says no.
func (d *Docker) builtForAnother(ctx context.Context, c *docker.Container) bool {
	img, err := d.Client.InspectImage(ctx, c.Image)
	if err != nil || img.Architecture == "" {
		return false
	}
	here, err := d.Client.Architecture(ctx)
	return err == nil && here != "" && here != img.Architecture
}

// loggedRefusal reports a container whose last lines say the kernel refused its binary.
// A log that can't be read says no.
func (d *Docker) loggedRefusal(ctx context.Context, c *docker.Container) bool {
	reading, cancel := context.WithTimeout(ctx, logRefusalWait)
	defer cancel()
	r, err := d.Client.ContainerLogs(reading, c.ID, c.Config.Tty, docker.LogOptions{Tail: logRefusalLines})
	if err != nil {
		return false
	}
	defer r.Close()
	for {
		line, err := r.Next()
		if err != nil {
			return false
		}
		if wrongArchitecture(string(line.Text)) {
			return true
		}
	}
}

// fatalSaid is why a container's last lines say it stopped, where they say it in a
// form whose words carry nothing of the app's: a MongoDB server's fatal or error line,
// whose message is MongoDB's own, such as "MongoDB cannot start: Linux kernel
// versions 6.19 and newer has a known incompatibility...". The line's attributes,
// which can hold what the app stores, are left out. Anything else stays in the log,
// which only someone who may read logs reads (ADR 0025). "" where nothing says, or
// the log can't be read.
func (d *Docker) fatalSaid(ctx context.Context, c *docker.Container) string {
	reading, cancel := context.WithTimeout(ctx, logRefusalWait)
	defer cancel()
	r, err := d.Client.ContainerLogs(reading, c.ID, c.Config.Tty, docker.LogOptions{Tail: fatalLines})
	if err != nil {
		return ""
	}
	defer r.Close()
	var fatal, failed string
	for {
		line, err := r.Next()
		if err != nil {
			break
		}
		switch severity, msg := structuredLine(line.Text); severity {
		case "F":
			fatal = msg
		case "E":
			failed = msg
		}
	}
	return sentenceOf(cmp.Or(fatal, failed))
}

// NotRunning says how the container at t isn't running, in a word.
func (d *Docker) NotRunning(ctx context.Context, t Target) (string, error) {
	c, err := d.current(ctx, t)
	if err != nil || c == nil {
		return "", err
	}
	return notRunning(c), nil
}

// notRunning says how a container isn't running, in a word, or nothing if it is.
// Docker counts one that's restarting or paused as running, and neither serves.
func notRunning(c *docker.Container) string {
	if c.State.Running && !c.State.Restarting && !c.State.Paused {
		return ""
	}
	return cmp.Or(c.State.Status, "stopped")
}

// reportRan puts the image a container now runs on record, by digest. It reads
// the container's own image, as its tag may have moved since it was made, by a
// pull or a build on the server. Not knowing it costs a moving tag its rollback,
// not the deploy, so a failure to read it doesn't fail anything.
func (d *Docker) reportRan(ctx context.Context, c *docker.Container) {
	if img, err := d.Client.InspectImage(ctx, c.Image); err == nil {
		resource.Report(ctx, resource.Outcome{Digest: img.Digest(c.Config.Image)})
	}
}

// checkImage refuses a plan for an image the server hasn't got and its registry
// won't give it, which would otherwise fail only once the change was applied. A
// file with build beside image often names one only the server it was built on
// has. One the server has runs as it is, wherever it came from.
func (d *Docker) checkImage(ctx context.Context, t Target, ref string) error {
	if docker.IsBuilt(ref) {
		// No registry has it: the service's step builds it (ADR 0046).
		return nil
	}
	img, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	switch {
	case err == nil:
		return d.mayReuse(ctx, ref, img.ID, t.Service)
	case !docker.IsNotFound(err):
		return err
	}
	if d.askRegistry(ctx, ref) == cantPull {
		if login := loginOf(ctx); login != "" {
			return fmt.Errorf("the image %s isn't on this server, and its registry won't give it to a pull with the registry login %s, so %s can't run: it has no such image, or the login can't read it. "+
				"Check the image's name and the login, then plan again", ref, login, t.Service)
		}
		return fmt.Errorf("the image %s isn't on this server, and its registry won't give it to a pull, so %s can't run: it has no such image, or keeps it private. "+
			"Check the image's name, or save a registry login for %s, then plan again", ref, t.Service, registryOf(ref))
	}
	return nil
}

// checkRefs refuses a service whose volumes aren't on the server, before
// Docker would make an unmanaged volume of the name, and a container on its own
// that mounts a volume a plan deleted. CheckHost refuses a network that isn't
// there, as Docker would take its name as the start of another's ID.
func (d *Docker) checkRefs(ctx context.Context, t Target, spec *resourcev1.Container) error {
	if !t.Scoped {
		for _, m := range spec.GetMounts() {
			v := m.GetVolume()
			if v == "" || d.Deleted == nil {
				continue
			}
			if deleted, err := d.Deleted.Has(v); err != nil {
				return err
			} else if deleted {
				return fmt.Errorf("the volume %s was deleted, and the server keeps its data for 7 days, so %s can't mount it. Mount a volume of another name", v, t.Name)
			}
		}
		return nil
	}
	for _, m := range spec.GetMounts() {
		v := m.GetVolume()
		if v == "" {
			continue
		}
		if _, err := d.Client.InspectVolume(ctx, t.VolumeName(m)); docker.IsNotFound(err) {
			if m.GetExternal() {
				return fmt.Errorf("the volume %s isn't on this server, so %s can't mount it. Dockwell didn't make it, so it won't make it again: restore it, or mount another", v, t.Service)
			}
			return fmt.Errorf("the volume %s isn't on this server, so %s can't mount it. Add it to the environment, in this change or an earlier one", v, t.Service)
		} else if err != nil {
			return err
		}
		if d.Deleted == nil {
			continue
		}
		if deleted, err := d.Deleted.Has(t.VolumeName(m)); err != nil {
			return err
		} else if deleted {
			return fmt.Errorf("the volume %s was deleted, so %s can't mount it, though the server keeps its data for 7 days. "+
				"Add it to the environment again, in this change or an earlier one, which brings its data back", v, t.Service)
		}
	}
	for _, n := range spec.GetNetworks() {
		if _, err := d.Client.NetworkNamed(ctx, t.DockerName(n)); docker.IsNotFound(err) {
			return fmt.Errorf("the network %s isn't on this server, so %s can't join it. Add it to the environment, in this change or an earlier one", n, t.Service)
		} else if err != nil {
			return err
		}
	}
	return nil
}

// Run makes what runs at t match spec, from whatever it finds. A service goes
// through a rollout (rollout.go), and a container on its own is replaced under
// its name: the new one is made first, under a name of its own, before the old
// one stops, so only stopping one and starting the other is downtime. The
// volumes Docker made for the old one's image move to the new one, and those a
// mount now covers are kept for 7 days, as a deleted container's are; a
// container made again mounts those kept for it. What it fails with is in plain
// words.
func (d *Docker) Run(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision) error {
	return explained(ctx, t, d.run(ctx, t, spec, dec))
}

func (d *Docker) run(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision) error {
	if err := d.Prepare(ctx, t, spec); err != nil {
		return err
	}
	if c, err := d.find(ctx, t.Name); err != nil {
		return err
	} else if c != nil {
		if err := owned(t, c); err != nil {
			return err
		}
		if d.BeforeReplace != nil {
			if err := d.BeforeReplace(ctx, c, spec); err != nil {
				return err
			}
		}
	}
	return d.roll(ctx, t, spec, dec)
}

// Prepare does what a run does before anything stops: it pulls spec's image where
// the server hasn't it, with room for it, and checks what spec mounts and joins is
// there. A move does it before it stops anything (ADR 0080).
func (d *Docker) Prepare(ctx context.Context, t Target, spec *resourcev1.Container) error {
	if err := d.roomFor(ctx, t, spec.GetImage()); err != nil {
		return err
	}
	if err := d.ensureImage(ctx, spec.GetImage()); err != nil {
		return err
	}
	if err := d.CheckHost(ctx, t, spec); err != nil {
		return err
	}
	return d.checkRefs(ctx, t, spec)
}

// joinIngress puts a service's new container on the ingress network, at its
// instance's address, before it starts, so a replaced container serves its
// routes as the old one did. A service with no routes doesn't join, and nor does
// one on the server's own network, which Docker can't put on another. A plan
// refuses that with a route left on it (checkRouted), so here its route is moving
// to a port on the server in the same plan, in a later step.
func (d *Docker) joinIngress(ctx context.Context, t Target, spec *resourcev1.Container, id string) error {
	if d.Ingress == nil || !t.Scoped || spec.GetHostNetwork() {
		return nil
	}
	network, ep, ok, err := d.Ingress.Endpoint(ctx, t.instance(max(t.Number, 1)))
	if err != nil || !ok {
		return err
	}
	if err := d.Client.ConnectEndpoint(ctx, network, id, ep); err != nil {
		return fmt.Errorf("can't put %s on the network %s: %w", t.Name, network, err)
	}
	return nil
}

// owned refuses a container at t's name that Dockwell didn't make: one
// without the Compose project and service Dockwell labels t's with, such
// as one someone started by hand. Dockwell never stops, replaces or
// removes it.
func owned(t Target, c *docker.Container) error {
	l := c.Config.Labels
	if l[docker.LabelProject] == t.Project && l[docker.LabelService] == t.Service {
		return nil
	}
	return fmt.Errorf("a container named %s is on this server, but Dockwell didn't make it: it doesn't have the labels %s=%s and %s=%s that Dockwell's has, "+
		"so Dockwell leaves it alone. Rename it with docker rename %s <another name>, or remove it with docker rm -f %s, then plan again",
		t.Name, docker.LabelProject, t.Project, docker.LabelService, t.Service, t.Name, t.Name)
}

// Judge says what a step does on this server beyond what the specs say: it
// refuses a container at the name that Dockwell didn't make; a step that takes
// data the image keeps out of use destroys it, as the server keeps it for 7
// days; a step that mounts such data again restores it; a step on a service says
// how it replaces what runs, and why; and a recreated container's health check,
// its image's included, says how long it may take to be healthy.
func (d *Docker) Judge(ctx context.Context, t Target, desired *resourcev1.Container, dec *resource.Decision) error {
	if dec.Action == resource.ActionDelete {
		letsGo, err := d.lettingGo(ctx, t)
		if err != nil {
			return err
		}
		if letsGo {
			dec.Releases = true
			return nil
		}
	}
	if dec.Action == resource.ActionDelete && d.Logs != nil && t.Scoped {
		dec.HidesLogs = true
	}
	stranger, err := d.find(ctx, t.Name)
	if err != nil {
		return err
	}
	if stranger != nil {
		if err := owned(t, stranger); err != nil {
			return err
		}
	}
	all, err := d.instances(ctx, t)
	if err != nil {
		return err
	}
	// A change in place leaves what it finds as it is, and verifies nothing that
	// doesn't run, so what a rollout left would stay and read as applied. A rollout
	// is what finishes it (ADR 0027).
	if dec.Action == resource.ActionUpdate && unfinished(all) {
		dec.Action, dec.Effect, dec.Downtime = resource.ActionReplace, resource.EffectRecreate, stopGrace(all[0].Container)+LogCatchUp+recreateStart
	}
	var c *docker.Container
	if len(all) > 0 {
		c = all[0].Container
		if dec.Action == resource.ActionReplace && c.Config.Labels[docker.LabelAdopted] != "" {
			dec.Elsewhere = elsewhere(c)
			leaning, err := d.leaningOn(ctx, c)
			if err != nil {
				return err
			}
			if len(leaning) > 0 {
				return fmt.Errorf("%s can't be replaced while others depend on it: %s, so a new container would cut that off. It stays as it is. Make that again on its own, then try this again", t.Service, human.List(leaning))
			}
		}
		if dec.Action == resource.ActionReplace && !all[0].temp && !all[0].stopping {
			// A replace makes the new one under the name of its number, after
			// the one it replaces, and a try that failed before leaves one
			// there, but it may be someone's. That name is the readable one,
			// whatever the one it replaces is named (ADR 0035).
			swap := c.ID[:12] + "_" + instanceName(t, all[0].number)
			left, err := d.find(ctx, swap)
			if err == nil && left != nil {
				err = owned(Target{Name: swap, Project: t.Project, Service: t.Service}, left)
			}
			if err != nil {
				return err
			}
		}
	} else if stranger != nil {
		c = stranger
	}
	var out map[string]string
	switch dec.Action {
	case resource.ActionDelete:
		out = map[string]string{}
		for _, i := range all {
			maps.Copy(out, imageVolumes(i.Container))
		}
	case resource.ActionCreate, resource.ActionReplace:
		// A pinned image that is gone says so below, as a rollback has somewhere
		// else to go.
		if !strings.Contains(desired.GetImage(), "@") {
			if err := d.checkImage(ctx, t, desired.GetImage()); err != nil {
				return err
			}
		}
		out = covered(c, desired)
		kept, err := d.keptVolumes(ctx, t)
		if err != nil {
			return err
		}
		have := imageVolumes(c)
		for p := range kept {
			_, moves := have[p]
			dec.Restores = dec.Restores || !moves && !slices.ContainsFunc(desired.GetMounts(), func(m *resourcev1.Mount) bool { return m.GetTarget() == p })
		}
	case resource.ActionNone, resource.ActionUpdate:
	}
	if len(out) > 0 {
		dec.Destroys, dec.Kept = true, slices.Sorted(maps.Keys(out))
	}
	// A pinned image that is gone can't run: where a rollback pins one, it says so
	// and goes back to its tag.
	if ref := desired.GetImage(); strings.Contains(ref, "@") && (dec.Action == resource.ActionCreate || dec.Action == resource.ActionReplace) {
		if _, err := d.Client.InspectImage(ctx, ref); docker.IsNotFound(err) {
			answer := d.askRegistry(ctx, ref)
			dec.ImageGone = answer == cantPull
			if answer == notAsked && resource.DeployOf(ctx).Run {
				return fmt.Errorf("the image %s isn't on this server, and this plan used up the time it gives registries before it could ask for it, so it can't say a pull would get it. Plan it again", ref)
			}
		}
	}
	if t.Scoped && dec.Action == resource.ActionReplace {
		keep, others, err := d.pick(ctx, t, desired, dec.Restart, all)
		if err != nil {
			return err
		}
		// What its tag names now is for the step to find out, so this is what it takes
		// if that's newer: a new container beside what runs, which reads as the spec
		// only by its tag.
		if keep != nil && (resource.Pulling(ctx) || stuck(keep)) {
			others, keep = withNewestFirst(others, keep), nil
		}
		if err := d.judgeRollout(ctx, t, desired, keep, others, dec); err != nil {
			return err
		}
		if dec.Rollout.Rolling {
			return nil
		}
	}
	if hc := desired.GetHealthcheck(); dec.Action == resource.ActionReplace && !complete(hc) {
		img, err := d.Client.InspectImage(ctx, docker.WithTag(desired.GetImage()))
		switch {
		case err == nil:
			if b := HealthBound(hc, img.Config.Healthcheck); b > 0 {
				dec.UntilHealthy = dec.Downtime + b
			}
		case docker.IsNotFound(err):
			// It is pulled as the step applies: its own check isn't known.
			dec.HealthUnknown = true
		default:
			return err
		}
	} else if b := HealthBound(hc, nil); dec.Action == resource.ActionReplace && b > 0 {
		dec.UntilHealthy = dec.Downtime + b
	}
	return nil
}

// complete reports a health check that says all HealthBound reads, so
// the image's can't change it.
func complete(hc *resourcev1.Healthcheck) bool {
	if AgentCheck(hc) != nil {
		return true
	}
	t := hc.GetTest()
	return len(t) > 0 && (t[0] == "NONE" || hc.GetInterval() != "" && hc.GetTimeout() != "" && hc.GetStartPeriod() != "" && hc.GetRetries() > 0)
}

// current is what reads as the container at t: the newest of its service's, or
// one of its name that Dockwell didn't make, for Judge to refuse.
func (d *Docker) current(ctx context.Context, t Target) (*docker.Container, error) {
	all, err := d.instances(ctx, t)
	if err != nil {
		return nil, err
	}
	if len(all) > 0 {
		return all[0].Container, nil
	}
	return d.find(ctx, t.Name)
}

// finishWait bounds finishing a replace once the old container is gone.
const finishWait = 30 * time.Second

// discard removes a container Run made but couldn't swap in, even if the
// caller stopped waiting, for up to CleanupWait. One that is left, as
// when Docker doesn't answer, is logged with how to remove it; the next
// replace removes it too, as it has the name a replace gives the new one.
func (d *Docker) discard(ctx context.Context, id, name string) {
	ctx, cancel := resource.Finishing(ctx, or(d.CleanupWait, cleanupWait))
	defer cancel()
	if ctx.Err() != nil {
		return // left behind: the next step to this service decides what becomes of it
	}
	if err := d.Client.RemoveContainer(ctx, id, true); err != nil && !docker.IsNotFound(err) {
		d.logger().Warn("can't remove a container made for a change that failed, so it is left on the server. Remove it with docker rm -f",
			slog.String("container", name), slog.String("id", id[:min(12, len(id))]), slog.String("error", err.Error()))
	}
}

// discardFailed removes a container that ran and failed with nothing to follow it,
// as a restart's does, keeping what it said, which is all that says why.
func (d *Docker) discardFailed(ctx context.Context, i *instance) {
	if d.Logs != nil {
		wait, cancel := resource.Finishing(ctx, or(d.CleanupWait, cleanupWait))
		defer cancel()
		keep := d.Logs.Begin(wait, i.Container, true)
		if err := d.Client.StopContainer(wait, i.ID, stopGrace(i.Container)); err != nil && !docker.IsNotFound(err) {
			d.logger().Warn("can't stop a container a change that failed made, so its last lines may not be kept",
				slog.String("container", i.name()), slog.String("error", err.Error()))
		}
		d.finishKeepingLogs(wait, keep, i.Container)
	}
	d.discard(ctx, i.ID, i.name())
}

// cleanupWait bounds removing a container a failed change made.
const cleanupWait = 30 * time.Second

func (d *Docker) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Update changes the restart policy and the limits in place, on each container
// of t.
func (d *Docker) Update(ctx context.Context, t Target, spec *resourcev1.Container) error {
	all, err := d.instances(ctx, t)
	switch {
	case err != nil:
		return err
	case len(all) == 0:
		c, err := d.find(ctx, t.Name)
		if err != nil {
			return err
		}
		if c != nil {
			return owned(t, c)
		}
		return fmt.Errorf("there's no container %s on this server to change. Plan the change again, which makes it", t.Name)
	}
	if err := d.checkCPUs(ctx, t, spec); err != nil {
		return err
	}
	for _, c := range all {
		err := d.Client.UpdateContainer(ctx, c.ID, docker.ContainerUpdate{
			RestartPolicy:     restartOf(spec),
			NanoCPUs:          cpuNanos(spec.GetResources().GetCpus()),
			Memory:            memoryBytes(spec.GetResources().GetMemory()),
			MemoryReservation: memoryBytes(spec.GetResources().GetMemoryReservation()),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Remove stops each container of t, giving it the time it was made with, and
// removes it, if it is Dockwell's. Its volumes stay: an environment's are
// resources of their own, and those Docker made for its image are kept for 7
// days, so a container of the name made again in that time, as undoing the
// deletion does, mounts them again.
func (d *Docker) Remove(ctx context.Context, t Target) error {
	id, err := d.takenOn(ctx, t)
	if err != nil {
		return err
	}
	if id != "" {
		return d.Client.Adopted.Forget(id)
	}
	c, err := d.find(ctx, t.Name)
	if err != nil {
		return err
	}
	if c != nil {
		if err := owned(t, c); err != nil {
			return err
		}
	}
	all, err := d.instances(ctx, t)
	if err != nil {
		return err
	}
	for _, i := range all {
		keep := d.beginKeepingLogs(ctx, i.Container)
		if err := d.Client.StopContainer(ctx, i.ID, stopGrace(i.Container)); err != nil && !docker.IsNotFound(err) {
			return err
		}
		d.finishKeepingLogs(ctx, keep, i.Container)
		if err := d.Client.RemoveContainer(ctx, i.ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
		d.keep(t, imageVolumes(i.Container))
	}
	return nil
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Healthy waits for the newest container of t to pass its health check, or,
// without one, to keep running for Settle. One that exits, keeps restarting or
// fails its check isn't healthy. The plain words name what it runs as, and what to
// look at, and the detail names the container, and what its check printed, cut short
// and masked (health.Clean).
func (d *Docker) Healthy(ctx context.Context, t Target) error {
	all, err := d.instances(ctx, t)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("%s isn't on this server, though it was just started. See if someone removed it with docker ps -a", t.Name)
	}
	soak := d.soak()
	if _, done := d.soaked.LoadAndDelete(all[0].ID); done || !all[0].State.StartedAt.IsZero() && time.Since(all[0].State.StartedAt) >= settled {
		soak = 0 // a step held it to that already, or it isn't new
	}
	if err := d.waitHealthy(ctx, t, all[0], soak); err != nil {
		return err
	}
	// What it ran goes on record once it passes, as a step does, however far it got
	// before it was cut short.
	d.reportRan(ctx, all[0].Container)
	return nil
}

// healthWait is how long a container gets to pass its check, as it was made:
// HealthWait, or what its check takes to give up, if that is longer.
func (d *Docker) healthWait(c *docker.Container) time.Duration {
	wait := or(d.HealthWait, DefaultHealthWait)
	if d.HealthWait > 0 {
		return wait
	}
	if label := c.Config.Labels[health.Label]; label != "" {
		if check, err := health.Parse(label); err == nil {
			return max(wait, min(check.Bound(), MaxHealthWait))
		}
	}
	if hc := c.Config.Healthcheck; hc != nil {
		return max(wait, HealthBound(nil, hc))
	}
	return wait
}

// waitHealthy waits for one container to pass its check, for what its routes send
// to to take a connection, and for that to last for soak. A check runs inside the
// container, or on a port of its own, so its passing says nothing of a route's
// port. Messages name the container and say how to see more. What a check printed
// goes in them cut short and with what the container's environment holds masked
// (health.Clean), as it is what a person needs first, and what it prints is rarely
// what a log is.
func (d *Docker) waitHealthy(ctx context.Context, t Target, i *instance, soak time.Duration) error {
	wait := d.healthWait(i.Container) + soak
	ctx, cancel := context.WithTimeoutCause(ctx, wait, errWaited)
	defer cancel()
	name := i.name()
	settle := or(d.Settle, DefaultSettle)
	poll := time.NewTicker(or(d.Poll, 250*time.Millisecond))
	defer poll.Stop()
	start := time.Now()
	restarts := -1
	var check *health.Check
	if label := i.Config.Labels[health.Label]; label != "" {
		var err error
		if check, err = health.Parse(label); err != nil {
			return fmt.Errorf("%s has a health check Dockwell can't read: %w. Plan the change again, which makes it anew", name, err)
		}
	}
	needPasses := 1
	pace := probeEvery
	if soak > 0 {
		needPasses = soakPasses
		pace = min(pace, soak/soakPasses)
	}
	var last string
	var nextProbe, lastCounted, nextReach time.Time
	var passedAt, holdingSince time.Time // zero while it isn't passing, and while its routes' ports aren't answering
	failures, passes := 0, 0
	for {
		c, err := d.Client.InspectContainer(ctx, i.ID)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if docker.IsNotFound(err) {
				return resource.Plainly(t.Service+" was removed while Dockwell waited for it to start. See who removed it with docker events, then try again.",
					fmt.Errorf("%s was removed while Dockwell waited for it to start. See who removed it with docker events", name))
			}
			return err
		}
		s := c.State
		if restarts < 0 {
			restarts = c.RestartCount
			if s.Health != nil {
				resource.Stage(ctx, resource.PhaseChecking, "Waiting for "+t.Service+" to pass its health check")
			} else {
				resource.Stage(ctx, resource.PhaseChecking, "Checking that "+t.Service+" stays up")
			}
		}
		switch {
		case s.Restarting || c.RestartCount > restarts:
			restarting := fmt.Errorf("%s keeps restarting: it exited with code %d", name, s.ExitCode)
			if d.binaryRefused(ctx, c) {
				return wrongArchitectureFailure(t, restarting)
			}
			// No word of docker logs in the plain words: a rollback takes this container away.
			if said := d.fatalSaid(ctx, c); said != "" {
				return resource.PlainlyBecause(resource.ReasonExits, fmt.Sprintf("%s keeps restarting: it exited with code %d, saying: %s", t.Service, s.ExitCode, said),
					fmt.Errorf("%w, saying: %s", restarting, said))
			}
			return resource.PlainlyBecause(resource.ReasonExits, fmt.Sprintf("%s keeps restarting: it exited with code %d. Check its command and variables, then try again.", t.Service, s.ExitCode), restarting)
		case !s.Running:
			stopped := fmt.Errorf("%s stopped right after it started, with exit code %d", name, s.ExitCode)
			if s.ExitCode != 0 && d.binaryRefused(ctx, c) {
				return wrongArchitectureFailure(t, stopped)
			}
			if said := d.fatalSaid(ctx, c); s.ExitCode != 0 && said != "" {
				return resource.PlainlyBecause(resource.ReasonExits, fmt.Sprintf("%s stopped right after it started, with exit code %d, saying: %s", t.Service, s.ExitCode, said),
					fmt.Errorf("%w, saying: %s", stopped, said))
			}
			return resource.PlainlyBecause(resource.ReasonExits, fmt.Sprintf("%s stopped right after it started, with exit code %d. Check its command and variables, then try again.", t.Service, s.ExitCode), stopped)
		case s.Paused:
			// Docker stops its checks, so what the last one said is as it left it.
			return resource.Plainly(t.Service+" is paused, so it serves nothing and its health check doesn't run. Unpause it on this server, then try again.",
				fmt.Errorf("%s is paused, so it serves nothing and its health check doesn't run. Unpause it with docker unpause %s", name, name))
		case check != nil:
			if now := time.Now(); !now.Before(nextProbe) {
				r, err := d.probe(ctx, check, health.Address(c.NetworkSettings.Networks))
				if err != nil {
					if ctx.Err() != nil {
						break
					}
					return err
				}
				if r.Passed {
					passes, failures = passes+1, 0
					if passedAt.IsZero() {
						passedAt = now
					}
				} else {
					last = health.Clean(r.Output, health.SpecEnv(c.Config.Env, c.Config.Labels))
					passes, passedAt, holdingSince = 0, time.Time{}, time.Time{}
					// Failures count only once the start period is over, and one for
					// each interval, though the agent looks again sooner, so a
					// container that comes up early takes traffic early.
					if now.Sub(startedAt(c, start)) >= check.StartPeriod && now.Sub(lastCounted) >= check.IntervalOr() {
						failures++
						lastCounted = now
					}
					if failures >= check.RetriesOr() {
						return resource.PlainlyBecause(resource.ReasonUnhealthy, t.Service+" runs, but fails its health check: "+health.WithFix(last),
							fmt.Errorf("%s runs, but fails its health check: %s", name, last))
					}
				}
				nextProbe = time.Now().Add(min(pace, check.IntervalOr()))
			}
		case s.Health != nil && s.Health.Status == "healthy":
			if passedAt.IsZero() {
				passedAt = time.Now()
			}
			passes = needPasses // Docker's own check runs at its own pace, and says if that changes
		case s.Health != nil && s.Health.Status == "unhealthy":
			if r, ok := health.FromDocker(s.Health, health.SpecEnv(c.Config.Env, c.Config.Labels)); ok {
				return resource.PlainlyBecause(resource.ReasonUnhealthy, t.Service+" runs, but fails its health check: "+health.WithFix(r.Output),
					fmt.Errorf("%s runs, but fails its health check: %s", name, r.Output))
			}
			return resource.PlainlyBecause(resource.ReasonUnhealthy, t.Service+" runs, but fails its health check. Check the command its health check runs, then try again.",
				fmt.Errorf("%s runs, but fails its health check", name))
		case s.Health != nil:
			passes, passedAt, holdingSince = 0, time.Time{}, time.Time{}
			if r, ok := health.FromDocker(s.Health, health.SpecEnv(c.Config.Env, c.Config.Labels)); ok {
				last = r.Output
			}
		case time.Since(start) >= settle:
			if passedAt.IsZero() {
				passedAt = time.Now()
			}
			passes = needPasses
		}
		if !passedAt.IsZero() && time.Now().After(nextReach) {
			reach := error(nil)
			if d.Ingress != nil && t.Scoped {
				reach = d.Ingress.Reachable(ctx, t.instance(i.number))
				nextReach = time.Now().Add(min(probeEvery, or(d.ListenWait, ListenWait)/4))
			}
			switch {
			case reach != nil:
				last, holdingSince = reach.Error(), time.Time{}
				if time.Since(passedAt) >= or(d.ListenWait, ListenWait) {
					return notTakingTraffic(t, name, check != nil || s.Health != nil, or(d.ListenWait, ListenWait), reach)
				}
			case holdingSince.IsZero():
				holdingSince = time.Now()
			}
		}
		if !holdingSince.IsZero() && passes >= needPasses && time.Since(holdingSince) >= soak {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-poll.C:
			continue
		}
		break
	}
	// The caller's deadline may come first, such as the step's own time
	// running out after a long pull: that isn't the health check's doing.
	said := ""
	if last != "" {
		said = ": " + last
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, errWaited) {
		plain := fmt.Sprintf("%s didn't pass its health check within %s, so Dockwell stopped waiting. Check the command its health check runs and how long the app takes to start, then try again.", t.Service, human.Duration(wait))
		if last != "" {
			plain = fmt.Sprintf("%s didn't pass its health check within %s, so Dockwell stopped waiting: %s If it only takes longer than that to start, give its check a start period.", t.Service, human.Duration(wait), health.WithFix(last))
		}
		return resource.PlainlyBecause(resource.ReasonUnhealthy, plain,
			fmt.Errorf("%s didn't pass its health check within %s%s", name, human.Duration(wait), said))
	}
	lastSaid := ""
	if last != "" {
		lastSaid = " Its check last said: " + last + "."
	}
	inspect := "See how it is with docker inspect --format '{{json .State.Health}}' " + name
	switch resource.StopOf(ctx, cause) {
	case resource.StepTime:
		return resource.Plainly(fmt.Sprintf("The step's time ran out before %s passed its health check, so Dockwell stopped waiting.%s A long pull may have used it up, so try again.", t.Service, lastSaid),
			fmt.Errorf("the step's time ran out before %s passed its health check, so Dockwell stopped waiting: %w%s. %s", name, cause, said, inspect))
	case resource.InnerWait:
		return resource.Plainly(fmt.Sprintf("A wait on this server ran out before %s passed its health check, with the step's own time left.%s Check what %s waits on, such as Docker, on this server, then try again.", t.Service, lastSaid, t.Service),
			fmt.Errorf("a wait ran out before %s passed its health check, with the step's own time left: %w%s. %s", name, cause, said, inspect))
	case resource.CalledOff:
		return resource.Plainly(fmt.Sprintf("Dockwell stopped waiting for %s's health check, as the step was called off: its change was cancelled, the control plane stopped waiting, or the agent stopped. Run the change again if you still want it.", t.Service),
			fmt.Errorf("stopped waiting for %s's health check: %w", name, cause))
	default:
		return cause
	}
}

// notTakingTraffic says a container's routes' port doesn't answer, what that means,
// and what to do about it, which is different for one with no check, as it may
// only be slow to start.
func notTakingTraffic(t Target, name string, checked bool, wait time.Duration, reach error) error {
	if checked {
		const fix = "A check says nothing of the port a route sends to, as it runs inside the container or on a port of its own"
		return resource.PlainlyBecause(resource.ReasonUnhealthy, fmt.Sprintf("%s passes its health check, but isn't ready for its traffic after %s: %s. "+fix+".", t.Service, human.Duration(wait), reach),
			fmt.Errorf("%s passes its health check, but isn't ready for its traffic after %s: %w. "+fix, name, human.Duration(wait), reach))
	}
	return resource.PlainlyBecause(resource.ReasonUnhealthy, fmt.Sprintf("%s runs, but isn't ready for its traffic after %s: %s. If it only takes longer than that to start, give it a health check with a start period, which can wait for up to %s.", t.Service, human.Duration(wait), reach, human.Duration(MaxHealthWait)),
		fmt.Errorf("%s runs, but isn't ready for its traffic after %s: %w. If it only takes longer than that to start, give it a health check with a start period, which can wait for up to %s", name, human.Duration(wait), reach, human.Duration(MaxHealthWait)))
}

// probeEvery is how often the agent looks at a container it waits on, whatever
// the check's own interval, which goes on counting the failures.
const probeEvery = time.Second

// MaxHealthWait is the most a container gets to pass its check, however long a
// start period its spec asks for.
const MaxHealthWait = 30 * time.Minute

// startedAt is when a container started, or failing that when waiting began.
func startedAt(c *docker.Container, fallback time.Time) time.Time {
	if c.State.StartedAt.IsZero() {
		return fallback
	}
	return c.State.StartedAt
}

// probe makes a check the agent makes.
func (d *Docker) probe(ctx context.Context, c *health.Check, address string) (health.Result, error) {
	if address == "" {
		return health.Result{Output: "the container has no address on any network, so the agent can't reach it. Use a command check, which Docker runs inside the container"}, nil
	}
	if d.Probe != nil {
		return d.Probe(ctx, c, address)
	}
	return health.Run(ctx, c, address)
}

// errWaited ends waiting for a health check that took HealthWait.
var errWaited = errors.New("waited for the health check for long enough")

// Ready checks Docker answers, and agrees an API version with it.
func (d *Docker) Ready(ctx context.Context) error { return d.Client.Ping(ctx) }

// ReadyKey is the client: whether its Docker answers.
func (d *Docker) ReadyKey() any { return d.Client }

// pullLogins is where the runtime remembers which login pulled each image.
func (d *Docker) pullLogins() *PullLogins {
	d.registries.mu.Lock()
	defer d.registries.mu.Unlock()
	if d.PullLogins == nil {
		d.PullLogins = &PullLogins{}
	}
	return d.PullLogins
}

// notePull remembers which login, if any, pulled the image ref names now.
func (d *Docker) notePull(ctx context.Context, ref string) {
	img, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	if err != nil {
		return
	}
	if err := d.pullLogins().Record(img.ID, pullIdentity(registry.AuthOf(ctx))); err != nil {
		d.logger().Warn("can't note which registry login pulled an image; an app of another project then asks its registry before running it", "image", ref, "error", err.Error())
	}
}

// mayReuse lets a step run the copy of ref the server has, id, unless a registry
// login pulled it that this step doesn't sign in with: then the step's own pull
// has to be able to get it, as its registry says, or an app of a project that may
// not use that login would run the image with no pull and no login (ADR 0086).
// A registry that doesn't answer leaves the copy to run, as one an older agent
// pulled does.
func (d *Docker) mayReuse(ctx context.Context, ref, id, service string) error {
	who, known := d.pullLogins().Who(id)
	if !known || who == "" || who == pullIdentity(registry.AuthOf(ctx)) {
		return nil
	}
	if d.askRegistry(ctx, ref) != cantPull {
		return nil
	}
	if service == "" {
		service = "this app"
	}
	return resource.PlainlyBecause(resource.ReasonImage,
		fmt.Sprintf("The image %s on this server came with a registry login %s doesn't pull with, and its registry won't give it to %s's own pull. Name a registry login this app may use, then try again", ref, service, service),
		fmt.Errorf("the image %s on this server came with a registry login %s doesn't pull with, and its registry won't give it to %s's own pull. Name a registry login this app may use, then try again", ref, service, service))
}

// Join is a network of another environment a container joins, and the name it
// answers to there (Docker.Joins).
type Join struct {
	Network, Alias string
}

// LabelHeldPorts lists the ports a container publishes on 127.0.0.1 alone while its
// app is held (PortMapping.held), by host port and protocol, each with the address
// it listens on once opened where that isn't every address, such as
// "8080/tcp,9000/udp@192.0.2.10", so each reads back held, as its spec has it
// (ADR 0084).
const LabelHeldPorts = docker.OwnPrefix + "held-ports"

// heldIP is where a held port listens.
const heldIP = "127.0.0.1"

// boundIP is the address a port listens on: 127.0.0.1 while it's held.
func boundIP(p *resourcev1.PortMapping) string {
	if p.GetHeld() {
		return heldIP
	}
	return p.GetHostIp()
}

// heldKey names a held port in LabelHeldPorts, such as "8080/tcp".
func heldKey(p *resourcev1.PortMapping) string {
	return strconv.Itoa(int(p.GetHostPort())) + "/" + protocolName(p.GetProtocol())
}

// heldPortsLabel is LabelHeldPorts for a spec, or "" while it holds none.
func heldPortsLabel(spec *resourcev1.Container) string {
	var keys []string
	for _, p := range spec.GetPorts() {
		if !p.GetHeld() {
			continue
		}
		k := heldKey(p)
		if ip := p.GetHostIp(); ip != "" {
			k += "@" + ip
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(slices.Compact(keys), ",")
}

// heldPorts reads LabelHeldPorts: each held port's address once opened, by its key.
func heldPorts(label string) map[string]string {
	out := map[string]string{}
	for _, k := range strings.Split(label, ",") {
		if k == "" {
			continue
		}
		key, ip, _ := strings.Cut(k, "@")
		out[key] = ip
	}
	return out
}
