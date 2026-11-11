// SPDX-License-Identifier: AGPL-3.0-only

// Package agent is the agent role: the part of dockwell that runs on every
// server. It holds the server's identity and keeps it fresh, and keeps one
// stream open to the control plane, reporting what the server is and how
// busy it is. The control plane calls the agent's drivers over the same
// stream, to plan, apply and observe the resources on the server.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/terminal"
	"github.com/dockwell/dockwell/internal/agent/updates"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
)

// AccessConfirmer keeps a change to the firewall that waits for its
// confirmation, and says what the server holds of Dockwell's firewall now
// (ADR 0052).
type AccessConfirmer interface {
	Confirm(ctx context.Context, change string) resource.AccessVerdict
	Applied() access.Applied
}

// Config configures Run.
type Config struct {
	Logger *slog.Logger
	// Dir is the agent's data directory, holding its identity.
	Dir string
	// Identity is this server's identity, loaded from Dir.
	Identity *enroll.Identity
	// Build is reported to the control plane.
	Build buildinfo.Info
	// Collector samples the server. Nil means this machine.
	Collector *collect.Collector
	// Drivers are the kinds of resource this agent manages. It offers
	// those that can run now in its hello, and says why it can't run the
	// rest, such as the Docker kinds while Docker isn't running; it checks
	// again every KindsEvery and tells the control plane what changed. Nil
	// manages none.
	Drivers *resource.Registry
	// Desired hears of each step the agent applies. Nil keeps nothing.
	Desired Desired
	// KindsEvery is how often the agent checks which kinds it can run.
	// Zero means DefaultKindsEvery.
	KindsEvery time.Duration
	// Domains says where this server's routes' certificates stand, which the
	// agent tells the control plane as its stream opens and whenever they
	// change. Nil says nothing.
	Domains DomainReporter
	// Public hears the public address the control plane knows this server by.
	// Nil ignores it.
	Public PublicAddressListener
	// NoLookup keeps this server from asking a public STUN server how it reaches
	// the internet (ADR 0034).
	NoLookup bool
	// Logs reads what this server's containers print. Nil reads none.
	Logs LogReader
	// Tools runs the server's own tools, such as ufw and nft, as ADR 0043 has
	// it. Nil runs none, as in a test.
	Tools *hosttool.Runner
	// Access keeps a change to the firewall that waits for its confirmation,
	// when the control plane confirms it. Nil keeps none.
	Access AccessConfirmer
	// Docker lists this server's containers. Nil lists none.
	Docker *docker.Client
	// Backups takes and removes backups of this server's volumes. Nil takes none.
	Backups BackupTaker
	// Databases reads this server's databases, each in its own container (ADR
	// 0038). Nil reads none.
	Databases DatabaseReader
	// Health says how this server's services are, which the agent tells the
	// control plane as its stream opens, when a state changes, and every so
	// often while checks run. Nil says nothing.
	Health HealthReporter
	// Files reads this server's disk for a person. Nil reads nothing.
	Files *files.Server
	// Updates reads which package updates wait on this server. Nil reads none.
	Updates *updates.Reader
	// OnePanel reads what 1Panel keeps on this server, for an import (ADR 0056).
	// Nil reads nothing.
	OnePanel func(context.Context) (*agentv1.ReadOnePanelResult, error)
	// Images says how images are read from their registries, for a form (ADR
	// 0062). The zero value reads them as the server's Docker reaches registries.
	Images ImageReads
	// Tasks runs this server's scheduled tasks and keeps their runs (ADR 0053).
	// Nil runs none.
	Tasks TaskKeeper
	// Terminals opens shells on this server for a person, across streams. Run
	// runs it. Nil opens none.
	Terminals *terminal.Manager
	// CallGrace is how long a call gets to end once its time is up or its stream is
	// going, before it's left behind. Zero means DefaultCallGrace.
	CallGrace time.Duration
	// unfinished is what outlives its call, across this agent's streams.
	unfinished *unfinished
	// firewall reads what filters the server's inbound traffic, when asked.
	firewall *firewallReader
	// outlook keeps what the server found out from one stream to the next.
	outlook *outlook
	// disks adds up what takes the disk Docker's data is on, when asked.
	disks *diskUse
	// procs lists and ends processes, when asked.
	procs *processes
	// images reads images from their registries, when asked, and keeps what it
	// read for a while.
	images *images
	// ports says what holds the server's ports, for a form, keeping which
	// process holds each socket for a few seconds as the form asks again.
	ports *ingress.Holders
	// Hello changes what the agent says of itself as a stream opens, for a test
	// that stands in for an agent of another version. Nil changes nothing.
	Hello func(*agentv1.Hello)
	// Probe says whether what a website sends to answers. Nil is Probe, which
	// asks from this server.
	Probe func(context.Context, *agentv1.ProbeCall) *agentv1.ProbeResult
}

// HealthReporter says how a server's services are (ADR 0027).
type HealthReporter interface {
	// Report is each container of a service, sorted, as of ScannedAt.
	Report() []*dockwellv1.InstanceHealth
	// ScannedAt is when it last read what the server runs: the zero time until it
	// has, when it has nothing to say.
	ScannedAt() time.Time
	// DockerUnreadableSince is since when it hasn't been able to read Docker,
	// including before it first has: the zero time while it can.
	DockerUnreadableSince() time.Time
	// Changed signals when Report has something new to say.
	Changed() <-chan struct{}
}

// PublicAddressListener uses what the control plane knows of a server's public
// address, which isn't on its interfaces (ADR 0034).
type PublicAddressListener interface {
	// SetPublicAddress takes what's known.
	SetPublicAddress(known *agentv1.PublicAddress)
}

// DomainReporter says where a server's routes' domains stand (ADR 0024).
type DomainReporter interface {
	// Domains is each domain, sorted.
	Domains() []*dockwellv1.DomainStatus
	// Ingress is where the server's ingress stands, or nil with no routes.
	Ingress() *dockwellv1.IngressStatus
	// DomainsChanged signals when Domains or Ingress has changed.
	DomainsChanged() <-chan struct{}
}

// LogReader reads a service's logs on this server. Its calls run apart from the
// engine's, each on a connection of its own, so log lines never stand between
// the control plane and the agent's stats, plans and applies.
type LogReader interface {
	// Sources lists the containers a service has logs from, leaving out what it
	// kept of revisions a deletion put out of reach, or the one container named.
	Sources(ctx context.Context, call *agentv1.LogSourcesCall) ([]*dockwellv1.LogSource, error)
	// Forgotten says which services' kept logs no reader may see any more.
	Forgotten(services []*agentv1.ForgottenService)
	// Stream reads what a call asks for, passing it to send, until it has read
	// it all or ctx ends. send may block: a follow drops what it can't send,
	// and says how much.
	Stream(ctx context.Context, call *agentv1.LogsCall, send func(*agentv1.SendLogsRequest) error) error
}

// TaskKeeper runs this server's scheduled tasks on its own clock, and keeps their
// runs until the control plane reads them (ADR 0053).
type TaskKeeper interface {
	// RunNow runs a task now, as its schedule would.
	RunNow(ctx context.Context, ref resource.Ref) (*dockwellv1.TaskRun, error)
	// Runs returns the runs past after, and lets go of the ones before it.
	Runs(after string) (*agentv1.TaskRunsResult, error)
	// RunsReady says a run started or ended.
	RunsReady() <-chan struct{}
	// Drain waits up to wait, once the agent is stopping, for the runs that stops
	// to end and be kept.
	Drain(wait time.Duration)
}

// BackupTaker takes and removes backups on this server (ADR 0033). What restores
// one is the volume's driver, as a restore is a step of a change.
type BackupTaker interface {
	// Preview says what backing up a volume would do.
	Preview(ctx context.Context, call *agentv1.BackupPreviewCall) (*agentv1.BackupPreviewResult, error)
	// Take takes a backup, and says what it took once it's whole on the disk.
	Take(ctx context.Context, call *agentv1.BackupCall) (*agentv1.BackupResult, error)
	// Send sends a backup that's on the disk to its destination, opened in memory for
	// this call alone.
	Send(ctx context.Context, call *agentv1.SendBackupCall, destination map[string]string) (*agentv1.SendBackupResult, error)
	// Fetch brings a backup back from where it was sent.
	Fetch(ctx context.Context, call *agentv1.FetchBackupCall, destination map[string]string) (*agentv1.BackupResult, error)
	// Delete removes a backup from the disk, and from where it was sent.
	Delete(ctx context.Context, call *agentv1.DeleteBackupCall, destination map[string]string) (*agentv1.DeleteBackupResult, error)
}

// DatabaseReader asks this server's databases how they are, each with its own tools
// in its own container (ADR 0038). It changes nothing.
type DatabaseReader interface {
	// Read says what each database asked about says of itself, in order.
	Read(ctx context.Context, call *agentv1.DatabaseCall) (*agentv1.DatabaseResult, error)
}

// Desired keeps what the control plane told this agent to apply, so the
// server can reconcile toward it while the control plane is away (PRD
// principle 5). The agent tells it after each step it applies and
// verifies. The cache itself, and reconciling from it, come with #32; this
// is where it plugs in.
//
// What it gets is the step's whole spec. That spec names the variables and
// secrets a service uses and pins each to a revision, and their values reach
// the agent only sealed to one stream, for one step, and are dropped once the
// step ends (ADR 0022), so it hears of none of those. A cache that wants to
// start a service again on its own needs them from the control plane again.
//
// A spec also holds what is written in it: a container's own env, and a
// volume's options, which can give a CIFS password. So a cache must seal what
// it keeps on disk.
type Desired interface {
	// Applied records a resource's desired spec; nil means it was deleted.
	Applied(ctx context.Context, ref resource.Ref, spec proto.Message) error
}

// outlook is what this server last found out of how the internet sees it. It belongs
// to the server, not a stream: a link that flaps opens a stream every 30 s, and each
// says what's known, where only the schedule asks a third party again.
type outlook struct {
	mu   sync.Mutex
	last *agentv1.Network
	at   time.Time
}

// current is what's known, looking again if that's due, and how long until the next look.
func (o *outlook) current(ctx context.Context, cfg Config) (*agentv1.Network, time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.last == nil || time.Since(o.at) >= networkEvery {
		network := cfg.Collector.Network(ctx, !cfg.NoLookup)
		if ctx.Err() != nil {
			return network, 0
		}
		o.last, o.at = network, time.Now()
	}
	return o.last, networkEvery - time.Since(o.at)
}

// networkEvery is how often a server looks again at how the internet sees it, as a
// home router's address changes.
const networkEvery = 6 * time.Hour

// Timing.
const (
	// retryAfter bounds how long the agent waits after a failed renewal.
	retryAfter = 5 * time.Minute
	// Reconnects back off from minBackoff to maxBackoff, and reset after a
	// session that lasted stableSession.
	minBackoff    = time.Second
	maxBackoff    = 30 * time.Second
	stableSession = time.Minute
	// defaultStatsInterval applies until the control plane says otherwise.
	defaultStatsInterval = 5 * time.Second
	// minStatsInterval is the soonest the agent samples, whatever it is told.
	minStatsInterval = time.Second
	// endWait bounds how long a stream that ends on purpose waits for the
	// control plane to end it.
	endWait = 10 * time.Second
	// DefaultKindsEvery is how often the agent checks which kinds it can
	// run, such as whether Docker answers.
	DefaultKindsEvery = 30 * time.Second
	// readyWait bounds one check of every driver.
	readyWait = 5 * time.Second
)

// kinds is what an agent says it can run, in its hello or after.
type kinds struct {
	ready       []string
	unavailable map[string]string
	// notHere are the unavailable kinds whose server doesn't have what they
	// need, such as nft or fail2ban: said for the record, not as a warning.
	notHere map[string]bool
}

// readyKinds checks which kinds the agent's drivers can run now.
func readyKinds(ctx context.Context, drivers *resource.Registry) kinds {
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	ready, unavailable := drivers.ReadyWhy(ctx)
	k := kinds{ready: make([]string, 0, len(ready)), notHere: map[string]bool{}}
	for _, r := range ready {
		k.ready = append(k.ready, string(r))
	}
	for kind, why := range unavailable {
		if k.unavailable == nil {
			k.unavailable = map[string]string{}
		}
		k.unavailable[string(kind)] = why.Error()
		if resource.IsNotHere(why) {
			k.notHere[string(kind)] = true
		}
	}
	return k
}

// kindsCheck checks which kinds the agent can run in the background, one
// check at a time. A check can take readyWait, as when Docker takes the
// connection but doesn't answer, and the stream's loop goes on meanwhile:
// sending stats, and hearing the stream end.
type kindsCheck struct {
	drivers *resource.Registry
	// done delivers a check's answer; take it, then call got.
	done    chan kinds
	running bool
}

func newKindsCheck(drivers *resource.Registry) *kindsCheck {
	return &kindsCheck{drivers: drivers, done: make(chan kinds, 1)}
}

// start begins a check, unless one is under way.
func (k *kindsCheck) start(ctx context.Context) {
	if k.running {
		return
	}
	k.running = true
	go func() { k.done <- readyKinds(ctx, k.drivers) }()
}

// got notes the answer was taken, so the next start checks again.
func (k *kindsCheck) got() { k.running = false }

func (k kinds) equal(o kinds) bool {
	return slices.Equal(k.ready, o.ready) && maps.Equal(k.unavailable, o.unavailable)
}

func (k kinds) proto() ([]string, []*agentv1.UnavailableKind) {
	var out []*agentv1.UnavailableKind
	for _, kind := range slices.Sorted(maps.Keys(k.unavailable)) {
		out = append(out, &agentv1.UnavailableKind{Kind: kind, Reason: k.unavailable[kind]})
	}
	return k.ready, out
}

// log says which kinds the agent can't run, and why, once per reason: as a
// warning where something is wrong, such as Docker not answering, and for the
// record alone where the server doesn't have what a kind needs, such as nft or
// fail2ban, which nobody may ever have asked of it.
func (k kinds) log(ctx context.Context, logger *slog.Logger) {
	byReason := map[string][]string{}
	for kind, why := range k.unavailable {
		byReason[why] = append(byReason[why], kind)
	}
	for _, why := range slices.Sorted(maps.Keys(byReason)) {
		names := byReason[why]
		slices.Sort(names)
		if k.notHere[names[0]] {
			logger.InfoContext(ctx, "the agent offers some kinds only once the server has what they need",
				slog.Any("kinds", names), slog.String("reason", why))
			continue
		}
		logger.WarnContext(ctx, "the agent can't run some kinds of resource; it offers them once it can",
			slog.Any("kinds", names), slog.String("reason", why))
	}
}

// Run keeps the server's certificate fresh and its stream to the control
// plane open, until ctx is done.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Collector == nil {
		cfg.Collector = collect.New()
	}
	cfg.outlook = &outlook{}
	cfg.firewall = newFirewallReader(cfg)
	cfg.disks = &diskUse{docker: cfg.Docker, collector: cfg.Collector, now: time.Now}
	cfg.procs = newProcesses(cfg.Collector, cfg.Docker)
	cfg.images = newImages(cfg.Docker, cfg.Images, nil)
	cfg.ports = &ingress.Holders{Client: cfg.Docker, OwnersFor: socketOwnersFor}
	if cfg.Drivers == nil {
		cfg.Drivers = resource.NewRegistry()
	}
	cfg.unfinished = newUnfinished()
	defer resource.OnPathBack(cfg.unfinished.wake)()
	id := cfg.Identity
	cfg.Logger = cfg.Logger.With(slog.String("server", id.ServerID))
	cfg.Logger.Info("agent running", slog.String("name", id.ServerName), slog.String("control_plane", id.ControlURL),
		slog.Time("certificate_expires", id.Cert().NotAfter))

	var wg sync.WaitGroup
	if cfg.Files != nil {
		wg.Go(func() { cfg.Files.KeepDeleted(ctx) })
	}
	wg.Go(func() { keepRenewed(ctx, cfg) })
	wg.Go(func() { keepConnected(ctx, cfg) })
	if cfg.Terminals != nil {
		wg.Go(func() { cfg.Terminals.Run(ctx) })
	}
	wg.Wait()
	return nil
}

// keepRenewed renews at two thirds of the certificate's life, and on
// failure retries with jitter while the current certificate is still valid.
func keepRenewed(ctx context.Context, cfg Config) {
	id := cfg.Identity
	for {
		if !sleep(ctx, time.Until(pki.RenewAt(id.Cert()))) {
			return
		}
		err := renew(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			cfg.Logger.Warn("can't renew the server certificate; retrying", slog.String("error", err.Error()),
				slog.String("valid_for", human.Duration(time.Until(id.Cert().NotAfter).Round(time.Minute))))
			if !sleep(ctx, jitter(retryAfter)) {
				return
			}
		}
	}
}

func renew(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := cfg.Identity.Renew(ctx); err != nil {
		return err
	}
	if err := cfg.Identity.Save(cfg.Dir); err != nil {
		return fmt.Errorf("renewed, but can't save the new certificate: %w", err)
	}
	cfg.Logger.Info("renewed the server certificate", slog.Time("expires", cfg.Identity.Cert().NotAfter))
	return nil
}

// keepConnected holds the stream open, reconnecting with jittered
// exponential backoff whenever it drops.
func keepConnected(ctx context.Context, cfg Config) {
	backoff := minBackoff
	for {
		start := time.Now()
		err := session(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errRenewed) {
			cfg.Logger.Info("moving the stream to the renewed certificate")
			continue
		}
		if time.Since(start) >= stableSession {
			backoff = minBackoff
		}
		wait := jitter(backoff)
		level, hint := slog.LevelWarn, "reconnecting"
		if connect.CodeOf(err) == connect.CodePermissionDenied {
			// Removed servers stay quiet, but keep trying in case they come back.
			level, hint, backoff = slog.LevelError, "this server was removed from Dockwell; join it again with a new ticket", maxBackoff
		}
		cfg.Logger.Log(ctx, level, "lost the control plane", slog.String("error", errText(err)), slog.String("next", hint),
			slog.String("retry_in", human.Duration(wait.Round(100*time.Millisecond))))
		if !sleep(ctx, wait) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// errRenewed ends a session whose certificate was renewed, so the next one
// connects with the new certificate.
var errRenewed = errors.New("the certificate was renewed")

// session runs one stream: hello, welcome, then stats until it breaks or
// the certificate is renewed.
func session(ctx context.Context, cfg Config) (err error) {
	renewed := cfg.Identity.Renewed()
	client, release, err := cfg.Identity.Client()
	if err != nil {
		return err
	}
	defer release() // after the stream closes, so its connection goes too
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Connect(ctx)
	defer func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() }()

	// A delivery key for this stream alone: the values of variables and
	// secrets come sealed to it, and it is never written anywhere, so
	// nothing outlives the stream that could open them.
	key, err := delivery.NewKey()
	if err != nil {
		return err
	}
	offered := readyKinds(ctx, cfg.Drivers)
	offered.log(ctx, cfg.Logger)
	hello := &agentv1.Hello{Build: cfg.Build.Proto(), Inventory: cfg.Collector.Inventory(ctx), DeliveryKey: key.Public(), WritesReferences: true, GuardsWaysIn: true, MakesFolders: true, Updates: cfg.Updates != nil, FollowsPace: true, ReadsOnepanel: cfg.OnePanel != nil, ManagesFiles: true, TransfersFiles: true, PagesListings: true, FirstPasswords: true}
	hello.Kinds, hello.Unavailable = offered.proto()
	if cfg.Terminals != nil {
		hello.Terminals, hello.OpenTerminals, hello.ContainerTerminals = true, cfg.Terminals.Known(), true
	}
	if cfg.Hello != nil {
		cfg.Hello(hello)
	}
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: hello}}); err != nil {
		if errors.Is(err, io.EOF) {
			// The control plane ended the stream; Receive says why.
			_, err = stream.Receive()
		}
		return err
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	welcome := first.GetWelcome()
	if welcome == nil {
		return errors.New("the control plane didn't answer the hello with a welcome")
	}
	interval := defaultStatsInterval
	if d := welcome.GetStatsInterval().AsDuration(); d >= minStatsInterval {
		interval = d
	}
	cfg.Logger.Info("connected to the control plane", slog.String("as", welcome.GetServerName()),
		slog.String("stats_every", human.Duration(interval)))

	// The stream carries stats up and calls down, with their results back
	// up. Sends take turns, and the request side closes between two.
	out := newSender(stream)
	send := out.send
	// The calls outlive the stream's context, which stops the receiver, so
	// the stream ends in order: refuse new calls, stop receiving, then
	// cancel what still runs and wait for it.
	calls := newCalls(parent, cfg, send, key)
	recvErr := make(chan error, 1)
	received := make(chan struct{})
	// The pace the control plane asks for, latest first: it changes as people start
	// and stop watching (ADR 0040).
	paced := make(chan time.Duration, 1)
	go func() {
		defer close(received)
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			switch {
			case msg.GetCall() != nil:
				calls.start(msg.GetCall())
			case msg.GetCancel() != nil:
				calls.cancel(msg.GetCancel().GetId())
			case msg.GetDrained() != nil:
				calls.acknowledged()
			case msg.GetPublicAddress() != nil && cfg.Public != nil:
				cfg.Public.SetPublicAddress(msg.GetPublicAddress())
			case msg.GetPace() != nil:
				d := msg.GetPace().GetStatsInterval().AsDuration()
				if d < minStatsInterval {
					continue
				}
				// Only the latest counts.
				select {
				case <-paced:
				default:
				}
				paced <- d
			}
		}
	}()
	defer func() {
		calls.close()
		// Half-close first, so what was sent goes out. Moving to a renewed
		// certificate, the stream ends on purpose: the control plane ends
		// it once it has read everything, the last results included, which
		// closing the response at once could drop. Closing the response
		// then ends a Receive under way, and the stream's context, which
		// drops what hasn't gone out, is cancelled last. A call may still
		// be sending its result, so the half-close waits for that send.
		out.closeRequest(endWait)
		if errors.Is(err, errRenewed) {
			wait := time.NewTimer(endWait)
			select {
			case <-received:
			case <-wait.C:
			}
			wait.Stop()
		}
		_ = stream.CloseResponse()
		<-received
		calls.stop()
	}()

	// When the control plane ends the stream, Send only says EOF; the
	// reason comes from Receive.
	sendStats := func() error {
		stats := cfg.Collector.Stats(time.Now())
		stats.Interval = durationpb.New(interval)
		err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Stats{Stats: stats}})
		if !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case err := <-recvErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	every := cfg.KindsEvery
	if every <= 0 {
		every = DefaultKindsEvery
	}
	recheck := time.NewTicker(every)
	defer recheck.Stop()
	checks := newKindsCheck(cfg.Drivers)
	if err := sendStats(); err != nil {
		return err
	}
	// Apart from the rest, as a server that isn't on a cloud has nothing to answer
	// and takes its whole timeout to say so, and nothing waits for this.
	go func() {
		for {
			network, next := cfg.outlook.current(ctx, cfg)
			if ctx.Err() != nil || send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Network{Network: network}}) != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(next):
			}
		}
	}()
	var domainsChanged <-chan struct{}
	sendDomains := func() error { return nil }
	if cfg.Domains != nil {
		domainsChanged = cfg.Domains.DomainsChanged()
		sendDomains = func() error {
			return send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Domains{Domains: &agentv1.Domains{Domains: cfg.Domains.Domains(), Ingress: cfg.Domains.Ingress()}}})
		}
		if err := sendDomains(); err != nil {
			return err
		}
	}
	var healthChanged <-chan struct{}
	sendHealth := func() error { return nil }
	if cfg.Health != nil {
		healthChanged = cfg.Health.Changed()
		sendHealth = func() error {
			instances := cfg.Health.Report()
			scanned := cfg.Health.ScannedAt()
			blind := cfg.Health.DockerUnreadableSince()
			h := &agentv1.Health{ReportedAt: timestamppb.Now()}
			if !blind.IsZero() {
				h.DockerUnreadableSince = timestamppb.New(blind)
			}
			switch {
			case !scanned.IsZero():
				h.Instances, h.ScannedAt = instances, timestamppb.New(scanned)
			case blind.IsZero():
				return nil // what it hasn't read isn't that nothing runs
			}
			// Before Docker has answered once, all there is to say is that it hasn't,
			// so its server reads as such from the start, never as healthy.
			return send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Health{Health: h}})
		}
		if err := sendHealth(); err != nil {
			return err
		}
	}
	var savesEnded <-chan *agentv1.SaveEnded
	if cfg.Files != nil {
		savesEnded = cfg.Files.SavesEnded()
	}
	var runsReady <-chan struct{}
	if cfg.Tasks != nil {
		runsReady = cfg.Tasks.RunsReady()
	}
	var terminalsEnded <-chan struct{}
	if cfg.Terminals != nil {
		terminalsEnded = cfg.Terminals.Ended()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-terminalsEnded:
			// What can't go now goes on the next stream, which says it's still to come.
			ended := cfg.Terminals.TakeEnded()
			for i, e := range ended {
				if err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_TerminalEnded{TerminalEnded: e}}); err != nil {
					cfg.Terminals.Untold(ended[i:])
					return err
				}
			}
		case ended := <-savesEnded:
			if err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_SaveEnded{SaveEnded: ended}}); err != nil {
				return err
			}
		case <-runsReady:
			if err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_TaskRunsReady{TaskRunsReady: &agentv1.TaskRunsReady{}}}); err != nil {
				return err
			}
		case <-domainsChanged:
			if err := sendDomains(); err != nil {
				return err
			}
		case <-healthChanged:
			if err := sendHealth(); err != nil {
				return err
			}
		case <-recheck.C:
			checks.start(ctx)
		case now := <-checks.done:
			checks.got()
			if now.equal(offered) {
				continue
			}
			if len(now.unavailable) == 0 {
				cfg.Logger.InfoContext(ctx, "the agent can run every kind of resource it has a driver for", slog.Any("kinds", now.ready))
			} else {
				now.log(ctx, cfg.Logger)
			}
			msg := &agentv1.Kinds{}
			msg.Kinds, msg.Unavailable = now.proto()
			if err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Kinds{Kinds: msg}}); err != nil {
				return err
			}
			offered = now
		case <-renewed:
			// Finish the calls under way first, so moving the stream
			// doesn't cut a step off halfway.
			if err := calls.drain(ctx, recvErr, ticker.C, sendStats); err != nil {
				return err
			}
			return errRenewed
		case <-cfg.unfinished.back:
			if err := send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Unstuck{Unstuck: &agentv1.Unstuck{}}}); err != nil {
				return err
			}
		case err := <-recvErr:
			return err
		case <-ticker.C:
			if err := sendStats(); err != nil {
				return err
			}
		case d := <-paced:
			if d == interval {
				continue
			}
			sooner := d < interval
			cfg.Logger.DebugContext(ctx, "the control plane changed how often to send stats", slog.String("stats_every", human.Duration(d)))
			interval = d
			ticker.Reset(d)
			// Someone just started watching: a new sample now, not at the old pace.
			if sooner {
				if err := sendStats(); err != nil {
					return err
				}
			}
		}
	}
}

// requestSide is the agent's side of its stream to the control plane.
type requestSide interface {
	Send(*agentv1.ConnectRequest) error
	CloseRequest() error
}

// sender sends on a stream one message at a time, and half-closes it only
// between two messages. A send writes a message in parts, and connect-go
// doesn't allow closing the request side during one: it would cut the
// message off, and the control plane would read a broken stream rather
// than the last results.
type sender struct {
	stream requestSide
	turn   chan struct{} // held by the send or the close under way
}

func newSender(stream requestSide) *sender {
	return &sender{stream: stream, turn: make(chan struct{}, 1)}
}

func (s *sender) send(m *agentv1.ConnectRequest) error {
	s.turn <- struct{}{}
	defer func() { <-s.turn }()
	return s.stream.Send(m)
}

// closeRequest half-closes the stream once the send under way, if any, is
// done. A send still under way after wait, as on a stream the control
// plane no longer reads, is left to fail as the stream ends: waiting on
// for it would keep the stream, and the agent, from ever moving on.
func (s *sender) closeRequest(wait time.Duration) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.turn <- struct{}{}:
	case <-t.C:
		return
	}
	defer func() { <-s.turn }()
	_ = s.stream.CloseRequest()
}

// sleep waits for d or ctx, and reports whether to carry on.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter spreads waits between 50% and 100% of d, so servers that lost the
// control plane together don't all return at once.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) //nolint:gosec // timing jitter, not security
}

func errText(err error) string {
	if err == nil {
		return "the stream ended"
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}
