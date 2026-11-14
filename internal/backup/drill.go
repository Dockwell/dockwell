// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// labelDrill marks a drill's container and volume, with the drill's ID, so the agent
// finds what one left, and nothing reads them as a person's.
const labelDrill = docker.LabelDrill

const (
	// drillLeast and drillMost bound how long a drill gets.
	drillLeast = 10 * time.Minute
	drillMost  = 6 * time.Hour
	// drillMemory is what a drill may use where the service has no limit.
	drillMemory = 1 << 30
	// memoryBeside is what a drill leaves the server's memory beyond its own limit.
	memoryBeside = 256 << 20
	// logRoom is the most a database's log takes beside its data while it loads: the
	// WAL, with max_wal_size, or MySQL's redo log, with the binary log off.
	logRoom = 256 << 20
	// drillWatch is how often a drill reads its disk while it runs, and
	// drillWatchHelper how often where each read starts a helper container.
	drillWatch       = 5 * time.Second
	drillWatchHelper = time.Minute
	// drillShares weighs a drill's CPU against the live app's 1024.
	drillShares = 256
	// drillScore has the kernel kill a drill first when the server runs out of memory.
	drillScore = 1000
	// drillTail is how many of the last lines a database printed a drill keeps.
	drillTail = 8
	// drillGone bounds removing what a drill made, once it ended.
	drillGone = 2 * time.Minute
	// drillStopWait is how long MySQL gets to shut down cleanly after its load.
	drillStopWait = 5 * time.Minute
	// fullAt is the disk-full rule's own threshold, 90%, which a drill never takes
	// a disk past.
	fullAt = 90
)

// DrillOutcome is how a drill went.
type DrillOutcome int

// Drill outcomes.
const (
	DrillPassed DrillOutcome = iota + 1
	DrillFailed
	// DrillNotChecked says nothing of the backup: the drill couldn't run, or was
	// cut short.
	DrillNotChecked
)

// Drill is a check that a backup restores (ADR 0068).
type Drill struct {
	// ID is the drill's, which its container and volume are labelled with.
	ID     string
	Backup string
	Ref    resource.Ref
}

// DrillReport says how a drill went.
type DrillReport struct {
	Outcome DrillOutcome
	// Why is what failed, or why it wasn't checked, in a line. Detail is what the
	// database printed last, without what it quoted.
	Why, Detail string
	Restored    int64
	Tables      int
	// UnpackedOnly is a copy it could only unpack.
	UnpackedOnly bool
}

// drillError is a drill's end, as a failure of the backup or not.
type drillError struct {
	outcome DrillOutcome
	why     string
}

func (e *drillError) Error() string { return e.why }

// failed is a backup that didn't restore, or failed a check.
func failed(format string, a ...any) error {
	return &drillError{outcome: DrillFailed, why: fmt.Sprintf(format, a...)}
}

// unchecked is a drill that couldn't say, which says nothing of the backup.
func unchecked(format string, a ...any) error {
	return &drillError{outcome: DrillNotChecked, why: fmt.Sprintf(format, a...)}
}

// Why a drill was cut short.
var (
	errBackupWanted = errors.New("a backup or restore on this server needed the disk, so this check stopped and gave it back")
	errNoRoom       = errors.New("the disk it restores to came near full, so this check stopped")
)

// drillRun is the drill under way, which a backup's room check can stop.
type drillRun struct {
	id string
	// filesystem is the disk it writes to, and need what it may write there.
	filesystem string
	need       int64
	stop       context.CancelCauseFunc
	done       chan struct{}
}

// drillDisk is the disk a drill restores to.
type drillDisk struct {
	free, size int64
	filesystem string
	// helper says a helper container measured it, as its path can't be statted here.
	helper bool
}

// DrillMemory reads how much memory the server has free for a new process, or
// false where it can't say.
type DrillMemory func() (int64, bool)

// Drill restores a backup on this server into a container and a volume of its own,
// checks it, and throws both away, whatever comes of it (ADR 0068). It runs one at a
// time on a server, and never holds the live volume, which a backup or restore of it
// may take meanwhile.
func (r *Runner) Drill(ctx context.Context, d Drill) DrillReport {
	started := r.now()
	report := DrillReport{}
	err := r.drill(ctx, d, &report)
	var de *drillError
	switch {
	case err == nil:
		report.Outcome = DrillPassed
	case errors.As(err, &de):
		report.Outcome, report.Why = de.outcome, de.why
	default:
		// Docker, the disk or the agent: nothing that says the backup is bad.
		plain, _ := resource.Explain(err)
		report.Outcome, report.Why = DrillNotChecked, plain
	}
	if report.Outcome != DrillPassed {
		r.logger().Info("a restore drill didn't pass", "drill", d.ID, "backup", d.Backup, "outcome", int(report.Outcome), "why", report.Why, "took", r.now().Sub(started).String())
	}
	return report
}

func (r *Runner) drill(ctx context.Context, d Drill, report *DrillReport) error {
	m, file, err := r.source(d.Ref, resource.RestoreSource{Backup: d.Backup})
	if err != nil {
		return unchecked("%s", strings.TrimSuffix(err.Error(), ". Choose another"))
	}
	budget := min(max(4*estimate(m), drillLeast), drillMost)
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	run := &drillRun{id: d.ID, stop: stop, done: make(chan struct{})}
	if err := r.takeDrillSlot(run); err != nil {
		return err
	}
	defer r.giveDrillSlot(run)

	r.sweepDrills(ctx, d.ID)
	err = r.drillIn(ctx, d, m, file, run, report)
	switch cause := context.Cause(ctx); {
	case err == nil:
		return nil
	case errors.Is(cause, errBackupWanted), errors.Is(cause, errNoRoom):
		return unchecked("%s", cause.Error())
	case errors.Is(cause, context.DeadlineExceeded):
		return unchecked("it took longer than %s, its most for a backup this size, so it stopped. A real restore takes about as long", human.Duration(budget))
	case ctx.Err() != nil:
		return unchecked("the agent's connection ended while it ran, so it stopped")
	}
	return err
}

// takeDrillSlot holds the server's one drill.
func (r *Runner) takeDrillSlot(run *drillRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drilling != nil {
		return unchecked("another restore check is running on this server")
	}
	r.drilling = run
	return nil
}

func (r *Runner) giveDrillSlot(run *drillRun) {
	r.mu.Lock()
	if r.drilling == run {
		r.drilling = nil
	}
	r.mu.Unlock()
	close(run.done)
}

// drillHeld is what the drill under way may still write on a disk, which a backup's
// room check counts as taken.
func (r *Runner) drillHeld(filesystem string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drilling == nil || r.drilling.filesystem == "" || r.drilling.filesystem != filesystem {
		return 0
	}
	return r.drilling.need
}

// yieldDrill stops the drill under way on a disk, for a backup or restore that needs
// the room, and waits for it to give the room back. Backups win.
func (r *Runner) yieldDrill(filesystem string) {
	r.mu.Lock()
	run := r.drilling
	r.mu.Unlock()
	if run == nil || (filesystem != "" && run.filesystem != filesystem) {
		return
	}
	r.logger().Info("a backup or restore needs the disk, so the restore drill under way stops", "drill", run.id)
	run.stop(errBackupWanted)
	select {
	case <-run.done:
	case <-time.After(drillGone):
	}
}

// drillIn runs a drill once it has the server's slot.
func (r *Runner) drillIn(ctx context.Context, d Drill, m *Manifest, file string, run *drillRun, report *DrillReport) (err error) {
	if m.OneDatabase != "" {
		// A database alone loads as its owner, whom a scratch server doesn't have.
		return unchecked("restore checks cover a whole server's backup so far, so this one of %s alone wasn't checked", m.OneDatabase)
	}
	kind := drillKind(m)
	// A drill checks PostgreSQL and MySQL so far: another database, such as a MongoDB
	// archive or a copy of its files, would read as a backup that didn't restore.
	if m.Format == formatMongoDump || kind != nil && !drillChecks(kind) {
		what := m.Database
		if what == "" && kind != nil {
			what = kind.name
		}
		return unchecked("restore checks cover PostgreSQL and MySQL so far, so this %s backup wasn't checked", cmpOr(what, "database"))
	}
	scratch := "dockwell-drill-" + d.ID
	if _, err := r.Client.CreateVolume(ctx, &docker.VolumeCreate{Name: scratch, Labels: map[string]string{labelDrill: d.ID}}); err != nil {
		return fmt.Errorf("docker can't make the drill's volume: %w", err)
	}
	var made []string // containers, to remove
	defer func() {
		gone, cancel := resource.Finishing(ctx, drillGone)
		defer cancel()
		for _, id := range made {
			if err := r.Client.RemoveOneOff(gone, id); err != nil && !docker.IsNotFound(err) {
				r.logger().Warn("can't remove a restore drill's container; the agent removes it within the hour", "container", id, "error", err.Error())
			}
		}
		if err := r.Client.RemoveVolume(gone, scratch); err != nil && !docker.IsNotFound(err) {
			r.logger().Warn("can't remove a restore drill's volume; the agent removes it within the hour", "volume", scratch, "error", err.Error())
		}
	}()
	target := volume.Target{Name: scratch, Key: "the restore check's volume"}

	// Room, where the scratch volume lives, which can be another disk than backups'.
	at, err := r.drillDiskOf(ctx, target)
	if err != nil {
		return err
	}
	need := m.Unpacked + m.Unpacked/margin
	if kind != nil {
		need += logRoom
	}
	floor := drillFloor(at.size)
	if at.free-need < floor {
		return unchecked("the disk it restores to has %s free, and a check of this backup needs %s there and leaves %s alone, so it didn't run",
			human.Size(at.free), human.Size(need), human.Size(floor))
	}
	r.mu.Lock()
	run.filesystem, run.need = at.filesystem, need
	r.mu.Unlock()
	watching, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go r.watchDrillDisk(watching, target, floor, run)

	if kind == nil {
		return r.drillCopy(ctx, m, file, target, report)
	}
	plan, err := r.drillPlan(ctx, d, m, kind, scratch)
	if err != nil {
		return err
	}
	if avail, known := r.memoryFree(); known && avail < plan.memory+memoryBeside {
		return unchecked("the server has %s of memory free, and a check takes up to %s and leaves %s beside it, so it didn't run",
			human.Size(avail), human.Size(plan.memory), human.Size(memoryBeside))
	}
	if m.Format == formatDump {
		return r.drillDump(ctx, m, file, plan, &made, report)
	}
	return r.drillFiles(ctx, m, file, target, plan, &made, report)
}

// drillChecks says a drill knows how to start a database of this kind and ask it
// what it holds.
func drillChecks(k *databaseKind) bool {
	return k.name == "PostgreSQL" || k.name == "MySQL"
}

// drillFloor is what a drill leaves alone on its disk: twice what a backup leaves,
// which stops a drill before a backup, and never past the disk-full rule's own line.
func drillFloor(size int64) int64 {
	return max(2*min(max(size/20, minReserve), maxReserve), size*(100-fullAt)/100)
}

// drillKind is the database a backup is of: by its dump, or by the image that wrote
// the files a copy holds. Nil for anything else.
func drillKind(m *Manifest) *databaseKind {
	if m.Format == formatDump {
		return kindNamed(m.Database)
	}
	if m.Image == "" {
		return nil
	}
	repo, _, _ := strings.Cut(m.Image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, k := range databaseKinds {
		if k.image == repo {
			return k
		}
	}
	return nil
}

// drillDiskOf reads the disk a volume is on: from its files where the agent sees them,
// or through the helper where it doesn't.
func (r *Runner) drillDiskOf(ctx context.Context, t volume.Target) (drillDisk, error) {
	if r.DrillDisk != nil {
		return r.DrillDisk(), nil
	}
	if v, err := r.Client.InspectVolume(ctx, t.Name); err == nil && v.Mountpoint != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(v.Mountpoint, &st); err == nil {
			return drillDisk{
				free:       int64(st.Bavail) * int64(st.Bsize), //nolint:gosec,unconvert // a disk's size fits, and Bsize's type differs by architecture
				size:       int64(st.Blocks) * int64(st.Bsize), //nolint:gosec,unconvert // as above
				filesystem: fmt.Sprint(st.Fsid),
			}, nil
		}
	}
	measured, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	stats, err := r.tool(measured, t, "measure", docker.Streams{})
	if err != nil {
		return drillDisk{}, fmt.Errorf("can't read how much is free where it restores: %w", err)
	}
	return drillDisk{free: stats.Free, size: stats.Size, filesystem: stats.Filesystem, helper: true}, nil
}

// watchDrillDisk stops a drill whose disk comes near full, as the database writes its
// bytes and the agent doesn't.
func (r *Runner) watchDrillDisk(ctx context.Context, t volume.Target, floor int64, run *drillRun) {
	tick := time.NewTicker(drillWatch)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		at, err := r.drillDiskOf(ctx, t)
		if err == nil && at.free < floor {
			run.stop(errNoRoom)
			return
		}
		if at.helper {
			tick.Reset(drillWatchHelper)
		}
	}
}

// memoryFree reads what the server has free for a new process.
func (r *Runner) memoryFree() (int64, bool) {
	if r.DrillMemory != nil {
		return r.DrillMemory()
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			return kb << 10, err == nil
		}
	}
	return 0, false
}

// drillPlan is how a drill runs a database: the image, not the service.
type drillPlan struct {
	kind *databaseKind
	// image is the image's ID, and tag what it's called, which says its release.
	image, tag string
	// entrypoint and cmd are the image's own.
	entrypoint, cmd []string
	env             []string
	data            string // where its data goes
	memory          int64
	user            string // the database's own superuser, for its checks
	password        string // made for the drill, and kept nowhere
	scratch, label  string
	cpus            int // the server's
	// preload is the live PostgreSQL's shared_preload_libraries, from its command
	// line, as an extension such as pg_cron loads only through it.
	preload string
}

// drillPlan finds the image the database runs now, or the one the backup was taken
// with when its service is gone, and what of its setup a drill takes.
func (r *Runner) drillPlan(ctx context.Context, d Drill, m *Manifest, kind *databaseKind, scratch string) (*drillPlan, error) {
	p := &drillPlan{kind: kind, memory: drillMemory, data: kind.data, password: randomHex() + randomHex(), scratch: scratch, label: d.ID, cpus: runtime.NumCPU()}
	var live []string
	users, err := r.users(ctx, volume.TargetOf(d.Ref))
	if err != nil {
		return nil, err
	}
	if db := users.database(); db != nil && db.kind == kind {
		c, err := r.Client.InspectContainer(ctx, db.id)
		if err != nil {
			return nil, err
		}
		p.image, p.tag, live = c.Image, c.Config.Image, c.Config.Env
		if kind.name == "PostgreSQL" {
			p.preload = preloadOf(append(append([]string{}, c.Config.Entrypoint...), c.Config.Cmd...))
		}
		if c.HostConfig.Memory > 0 {
			p.memory = c.HostConfig.Memory
		}
		if mount := dataMount(c, kind.dataDir(c)); mount != nil {
			p.data = mount.Destination
		}
	} else {
		if m.Image == "" {
			return nil, unchecked("its database's service is gone, and the backup doesn't say what image wrote it")
		}
		p.tag = m.Image
	}
	img, err := r.Client.InspectImage(ctx, cmpOr(p.image, p.tag))
	if docker.IsNotFound(err) {
		return nil, unchecked("the image this backup was taken with, %s, is gone from this server", p.tag)
	}
	if err != nil {
		return nil, err
	}
	p.image, p.entrypoint, p.cmd = img.ID, img.Config.Entrypoint, img.Config.Cmd
	p.env, p.user = drillEnv(kind, live, p.password)
	return p, nil
}

// drillEnv is what of a database's setup a drill takes: only the image's own setup
// variables, each password the drill's, and never one that makes local sign-ins
// ask for one.
func drillEnv(kind *databaseKind, live []string, password string) (env []string, superuser string) {
	of := func(name string) string {
		for _, e := range live {
			if k, v, _ := strings.Cut(e, "="); k == name {
				return v
			}
		}
		return ""
	}
	add := func(name, value string) {
		if value != "" {
			env = append(env, name+"="+value)
		}
	}
	switch kind.name {
	case "PostgreSQL":
		superuser = of("POSTGRES_USER")
		add("POSTGRES_USER", superuser)
		add("POSTGRES_DB", of("POSTGRES_DB"))
		add("PGDATA", of("PGDATA"))
		add("POSTGRES_INITDB_ARGS", withoutAuth(of("POSTGRES_INITDB_ARGS")))
		add("POSTGRES_PASSWORD", password)
		if superuser == "" {
			superuser = "postgres"
		}
	case "MySQL":
		superuser = "root"
		add("MYSQL_DATABASE", of("MYSQL_DATABASE"))
		add("MYSQL_ROOT_PASSWORD", password)
	}
	return env, superuser
}

// preloadOf reads shared_preload_libraries from a PostgreSQL command line, in any
// of the ways postgres takes it: -c name=value, -cname=value or --name=value, with
// dashes or underscores. A value with anything but names and commas isn't taken.
func preloadOf(args []string) string {
	value := ""
	for i, a := range args {
		setting := ""
		switch {
		case a == "-c" && i+1 < len(args):
			setting = args[i+1]
		case strings.HasPrefix(a, "-c"):
			setting = strings.TrimPrefix(a, "-c")
		case strings.HasPrefix(a, "--"):
			setting = strings.TrimPrefix(a, "--")
		}
		name, v, ok := strings.Cut(setting, "=")
		if ok && strings.ReplaceAll(name, "-", "_") == "shared_preload_libraries" {
			value = v
		}
	}
	for name := range strings.SplitSeq(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.$/-") != "" {
			return ""
		}
	}
	return value
}

// withoutAuth takes initdb's sign-in flags out of its arguments, so the drill's
// database trusts its own socket.
func withoutAuth(args string) string {
	fields := strings.Fields(args)
	out := fields[:0]
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "--auth" || f == "--auth-local" || f == "--auth-host" || f == "-A":
			i++ // and its value
		case strings.HasPrefix(f, "--auth=") || strings.HasPrefix(f, "--auth-local=") || strings.HasPrefix(f, "--auth-host=") || strings.HasPrefix(f, "-A"):
		default:
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// flags are what a drill adds to the image's own command: no durability, as nothing
// it writes is kept, which halves a load's writes to the app's disk.
func (p *drillPlan) flags(files bool) []string {
	switch p.kind.name {
	case "PostgreSQL":
		out := []string{
			"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off",
			"-c", "wal_level=minimal", "-c", "max_wal_senders=0", "-c", "max_wal_size=256MB",
		}
		if p.preload != "" {
			out = append(out, "-c", "shared_preload_libraries="+p.preload)
		}
		if files {
			out = append(out, "-c", "hba_file="+drillHBA)
		}
		return out
	case "MySQL":
		out := []string{"--skip-log-bin", "--innodb-flush-log-at-trx-commit=0", "--innodb-doublewrite=0"}
		if files {
			out = append(out, "--skip-grant-tables", "--skip-networking")
		}
		return out
	}
	return nil
}

// drillHBA is where a drill of PostgreSQL's files has its own pg_hba.conf, which
// trusts the local socket: in the container's own layer, which goes with it, as the
// copy brings the live one, which may ask for a password.
const drillHBA = "/tmp/dockwell-drill-hba.conf"

// drillRequest is a drill's container: sealed off, its memory and CPU bounded, the
// kernel's first to kill, and nothing mounted but its scratch volume, where the live
// database has its data. files says it runs on a copy of a database's files.
func drillRequest(p *drillPlan, files bool) *docker.CreateRequest {
	env := p.env
	if files {
		// A copy brings its own users and needs no password; without one, an image
		// that finds no data stops rather than make a database that's empty.
		env = slices.DeleteFunc(slices.Clone(env), func(e string) bool {
			return strings.HasPrefix(e, "POSTGRES_PASSWORD=") || strings.HasPrefix(e, "MYSQL_ROOT_PASSWORD=")
		})
	}
	cfg := docker.Config{Image: p.image, Env: env, Labels: map[string]string{labelDrill: p.label}}
	if files && p.kind.name == "PostgreSQL" {
		// The image's own command, after a line that writes the drill's pg_hba.conf.
		cfg.Entrypoint = []string{"sh", "-c", `printf 'local all all trust\n' > ` + drillHBA + ` && exec "$@"`, "drill"}
		cfg.Cmd = append(append(append([]string{}, p.entrypoint...), p.cmd...), p.flags(files)...)
	} else {
		cfg.Entrypoint, cfg.Cmd = p.entrypoint, append(append([]string{}, p.cmd...), p.flags(files)...)
	}
	cpus := min(1.0, float64(p.cpus)/2)
	return &docker.CreateRequest{
		Config: cfg,
		HostConfig: docker.HostConfig{
			NetworkMode: "none",
			Mounts:      []docker.Mount{{Type: "volume", Source: p.scratch, Target: p.data}},
			Memory:      p.memory, MemorySwap: p.memory,
			NanoCPUs:  int64(cpus * 1e9),
			CPUShares: drillShares, OomScoreAdj: drillScore,
		},
	}
}

// createDrill makes and starts a drill's database.
func (r *Runner) createDrill(ctx context.Context, p *drillPlan, files bool, made *[]string) (*database, error) {
	req := drillRequest(p, files)
	id, err := r.Client.CreateContainer(ctx, p.scratch+"-"+strconv.Itoa(len(*made)), req)
	if err != nil {
		return nil, fmt.Errorf("docker can't make the drill's database: %w", err)
	}
	*made = append(*made, id)
	if err := r.Client.StartContainer(ctx, id); err != nil {
		return nil, r.startFailed(ctx, id, p, err)
	}
	return &database{user: &user{id: id, name: "the drill's " + p.kind.name}, kind: p.kind, image: p.tag}, nil
}

// startFailed says why a drill's database didn't start or stopped: out of its memory,
// which says nothing of the backup, or the backup's own trouble.
func (r *Runner) startFailed(ctx context.Context, id string, p *drillPlan, err error) error {
	if c, inspect := r.Client.InspectContainer(ctx, id); inspect == nil && c.State.OOMKilled {
		return unchecked("it ran out of the %s of memory it may use, so it stopped", human.Size(p.memory))
	}
	return err
}

// drillDump loads a dump into a new database, reading what the dump holds as it goes,
// and checks that the database holds it.
func (r *Runner) drillDump(ctx context.Context, m *Manifest, file string, p *drillPlan, made *[]string, report *DrillReport) error {
	resource.Progress(ctx, "Starting "+p.kind.name+" from "+p.tag+" to load the backup into")
	db, err := r.createDrill(ctx, p, false, made)
	if err != nil {
		return err
	}
	// A database that won't start empty is the server's or the image's trouble, not the backup's.
	if err := r.awaitReady(ctx, db); err != nil {
		return r.drillDied(ctx, db, p, report, unchecked("%s didn't start empty, before the backup went in: %s", p.kind.name, plainOf(err)))
	}
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return failed("the dump doesn't open: %v", err)
	}
	defer gz.Close()
	held := newHeldDump(p.kind.name == "MySQL")
	resource.Progress(ctx, "Loading the backup")
	if err := r.loadPlain(ctx, db, io.TeeReader(gz, held)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if diskFilled(err) {
			return unchecked("the disk it restores to filled up as it loaded, so it wasn't checked")
		}
		if said := plainOf(err); strings.Contains(said, "shared_preload_libraries") {
			// The setting is somewhere other than the command line, such as its config file.
			return unchecked("the dump uses an extension that loads only through shared_preload_libraries, which this database sets outside its command line. Set it there to have its drills check it")
		}
		return r.drillDied(ctx, db, p, report, failed("the dump didn't load: %s", plainOf(err)))
	}
	_ = held.Close()
	report.Restored = m.Unpacked
	if p.kind.name == "MySQL" {
		// The dump brought its own users, and took the drill's password with them.
		resource.Progress(ctx, "Starting MySQL again without its users, to check it")
		// It loaded with the log written once a second, so it stops as it would be
		// stopped, and writes everything, before it starts again: a kill could lose
		// the last tables' rows, which would read as a backup that lost them.
		if err := r.Client.StopContainer(ctx, db.id, drillStopWait); err != nil {
			return err
		}
		if err := r.Client.RemoveOneOff(ctx, db.id); err != nil {
			return err
		}
		*made = (*made)[:len(*made)-1]
		if db, err = r.createDrill(ctx, p, true, made); err != nil {
			return err
		}
		if err := r.awaitSocket(ctx, db, p); err != nil {
			return r.drillDied(ctx, db, p, report, err)
		}
	}
	resource.Progress(ctx, "Checking it holds what the dump holds")
	return r.checkHeld(ctx, db, p, held, report)
}

// drillFiles unpacks a copy of a database's files, starts the database on them, and
// asks it.
func (r *Runner) drillFiles(ctx context.Context, m *Manifest, file string, t volume.Target, p *drillPlan, made *[]string, report *DrillReport) error {
	if !p.kind.sameSeries(m.Image, p.tag) {
		have := p.kind.seriesOf(m.Image)
		return failed("a copy of %s %s's files doesn't open under %s. Restore it under %s, or back it up as a dump", p.kind.name, have, p.tag, have)
	}
	if err := r.drillUnpack(ctx, m, file, t, report); err != nil {
		return err
	}
	resource.Progress(ctx, "Starting "+p.kind.name+" on the copy")
	db, err := r.createDrill(ctx, p, true, made)
	if err != nil {
		return err
	}
	if err := r.awaitSocket(ctx, db, p); err != nil {
		return r.drillDied(ctx, db, p, report, err)
	}
	return r.checkHeld(ctx, db, p, nil, report)
}

// drillCopy unpacks a copy of anything else: what it proves is that the archive
// restores whole.
func (r *Runner) drillCopy(ctx context.Context, m *Manifest, file string, t volume.Target, report *DrillReport) error {
	report.UnpackedOnly = true
	return r.drillUnpack(ctx, m, file, t, report)
}

func (r *Runner) drillUnpack(ctx context.Context, m *Manifest, file string, t volume.Target, report *DrillReport) error {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return err
	}
	defer f.Close()
	resource.Progress(ctx, "Unpacking the backup")
	stats, err := r.tool(ctx, t, "unpack", docker.Streams{Stdin: f})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if diskFilled(err) {
			return unchecked("the disk it restores to filled up as it unpacked, so it wasn't checked")
		}
		return failed("it didn't unpack: %s", plainOf(err))
	}
	report.Restored = stats.Bytes
	switch {
	case stats.Bytes < m.Unpacked:
		return failed("it unpacked %s of the %s it held", human.Size(stats.Bytes), human.Size(m.Unpacked))
	case m.Files > 0 && stats.Files < m.Files:
		return failed("it unpacked %d of the %d files it held", stats.Files, m.Files)
	}
	return nil
}

// awaitSocket waits until a drill's database answers on its own socket, as it does
// once it runs on files that have their own users, or without them.
func (r *Runner) awaitSocket(ctx context.Context, db *database, p *drillPlan) error {
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	for {
		if _, err := r.ask(ctx, db, p, "", "SELECT 1"); err == nil {
			return nil
		}
		if c, err := r.Client.InspectContainer(ctx, db.id); err == nil && (!c.State.Running || c.State.Restarting) {
			if c.State.OOMKilled {
				return unchecked("it ran out of the %s of memory it may use, so it stopped", human.Size(p.memory))
			}
			return failed("%s stopped as it started on the backup, with code %d", p.kind.name, c.State.ExitCode)
		}
		select {
		case <-ctx.Done():
			return failed("%s didn't start on the backup within %s", p.kind.name, human.Duration(readyWait))
		case <-time.After(readyEvery):
		}
	}
}

// drillDied keeps what the database printed last beside why the drill ended, and
// says a database the kernel killed for its memory wasn't checked.
func (r *Runner) drillDied(ctx context.Context, db *database, p *drillPlan, report *DrillReport, err error) error {
	report.Detail = r.lastLines(ctx, db.id)
	if c, inspect := r.Client.InspectContainer(ctx, db.id); inspect == nil && c.State.OOMKilled {
		return unchecked("it ran out of the %s of memory it may use, so it stopped", human.Size(p.memory))
	}
	// A copy drill starts with no password, so an image that finds no data where it
	// looks stops rather than make an empty database that would pass.
	if strings.Contains(report.Detail, "is uninitialized") {
		return unchecked("the copy isn't where %s looks for its data, %s, so it wasn't checked. Back it up as a dump to check it", p.tag, p.data)
	}
	var de *drillError
	if errors.As(err, &de) || ctx.Err() != nil {
		return err
	}
	return failed("%s", plainOf(err))
}

// lastLines is what a container printed last, a line each, without what it quoted.
func (r *Runner) lastLines(ctx context.Context, id string) string {
	logs, err := r.Client.ContainerLogs(ctx, id, false, docker.LogOptions{Tail: drillTail})
	if err != nil {
		return ""
	}
	defer logs.Close()
	var out []string
	for {
		e, err := logs.Next()
		if err != nil {
			break
		}
		if line := strings.TrimSpace(withoutValues(string(e.Text))); line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// diskFilled says an error is a disk that ran out of room, between the watch's looks,
// which says nothing of the backup.
func diskFilled(err error) bool {
	said := strings.ToLower(err.Error())
	return errors.Is(err, syscall.ENOSPC) || strings.Contains(said, "no space left on device") || strings.Contains(said, "could not extend file")
}

// plainOf is an error in its plain words.
func plainOf(err error) string {
	plain, _ := resource.Explain(err)
	return strings.TrimSuffix(plain, ".")
}

// ask runs a query on a drill's database as its superuser, over its own socket, and
// returns what it printed, a row a line.
func (r *Runner) ask(ctx context.Context, db *database, p *drillPlan, database, query string) (string, error) {
	var cmd []string
	switch p.kind.name {
	case "PostgreSQL":
		cmd = []string{"psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-U", p.user, "-d", cmpOr(database, "postgres"), "-c", query}
	case "MySQL":
		cmd = []string{"mysql", "-uroot", "-N", "-B", "-e", query}
		if database != "" {
			cmd = append(cmd, database)
		}
	}
	var out strings.Builder
	errs := &tail{}
	code, err := r.Client.Exec(ctx, db.id, cmd, docker.Streams{Stdout: &out, Stderr: errs})
	switch {
	case err != nil:
		return "", err
	case code != 0:
		return "", fmt.Errorf("%s answered with code %d: %s", p.kind.name, code, scrubbed(errs.String()))
	}
	return out.String(), nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// checkBatch is how many tables one query asks for a row.
const checkBatch = 200

// checkHeld asks a drill's database what it holds: that it answers, and lists its
// databases; and, for a dump, that each database and table the dump makes is there,
// and each table the dump fills has a row.
func (r *Runner) checkHeld(ctx context.Context, db *database, p *drillPlan, held *heldDump, report *DrillReport) error {
	if _, err := r.ask(ctx, db, p, "", "SELECT 1"); err != nil {
		return failed("%s doesn't answer: %s", p.kind.name, plainOf(err))
	}
	listed, err := r.ask(ctx, db, p, "", databasesQuery(p.kind))
	if err != nil {
		return failed("%s can't list its databases: %s", p.kind.name, plainOf(err))
	}
	if held == nil {
		return nil
	}
	have := lineSet(listed)
	for _, name := range held.databases {
		if !have[name] {
			return failed("the database %s, which the dump makes, isn't there", name)
		}
	}
	there := map[string]bool{}
	if p.kind.name == "MySQL" {
		got, err := r.ask(ctx, db, p, "", "SELECT table_schema, table_name FROM information_schema.tables WHERE table_type <> 'VIEW'")
		if err != nil {
			return failed("MySQL can't list its tables: %s", plainOf(err))
		}
		there = lineSet(got)
	} else {
		for _, name := range held.databases {
			got, err := r.ask(ctx, db, p, name, "SELECT schemaname, tablename FROM pg_tables")
			if err != nil {
				return failed("PostgreSQL can't list the tables of %s: %s", name, plainOf(err))
			}
			for line := range lineSet(got) {
				there[name+"\t"+line] = true
			}
		}
	}
	var filled []*dumpTable
	for _, t := range held.tables {
		key := t.database + "\t" + t.schema + "\t" + t.name
		if p.kind.name == "MySQL" {
			key = t.database + "\t" + t.name
		}
		if !there[key] {
			return failed("%s, which the dump makes, isn't there", tableName(t))
		}
		// A MEMORY table is empty after any restart, its rows never on disk.
		if t.filled && !t.memory {
			filled = append(filled, t)
		}
	}
	for start := 0; start < len(filled); start += checkBatch {
		batch := filled[start:min(start+checkBatch, len(filled))]
		empty, err := r.emptyOf(ctx, db, p, batch)
		if err != nil {
			return failed("%s can't read its tables: %s", p.kind.name, plainOf(err))
		}
		if empty != nil {
			return failed("%s has no rows, and the dump fills it", tableName(empty))
		}
	}
	report.Tables = len(held.tables)
	if held.over {
		report.Detail = fmt.Sprintf("The dump makes more than %d tables, and this checked the first %d.", maxTables, maxTables)
	}
	return nil
}

// emptyOf finds a table of a batch with no rows, which reads one page of each.
func (r *Runner) emptyOf(ctx context.Context, db *database, p *drillPlan, batch []*dumpTable) (*dumpTable, error) {
	if p.kind.name == "MySQL" {
		var parts []string
		for i, t := range batch {
			parts = append(parts, fmt.Sprintf("SELECT %d FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM %s.%s LIMIT 1)", i, quoteMySQL(t.database), quoteMySQL(t.name)))
		}
		return pick(r.ask(ctx, db, p, "", strings.Join(parts, " UNION ALL ")))(batch)
	}
	// PostgreSQL asks each database on its own.
	byDB := map[string][]int{}
	var order []string
	for i, t := range batch {
		if _, ok := byDB[t.database]; !ok {
			order = append(order, t.database)
		}
		byDB[t.database] = append(byDB[t.database], i)
	}
	for _, name := range order {
		var parts []string
		for _, i := range byDB[name] {
			t := batch[i]
			parts = append(parts, fmt.Sprintf("SELECT %d WHERE NOT EXISTS (SELECT 1 FROM %s.%s LIMIT 1)", i, quotePostgres(t.schema), quotePostgres(t.name)))
		}
		empty, err := pick(r.ask(ctx, db, p, name, strings.Join(parts, " UNION ALL ")))(batch)
		if err != nil || empty != nil {
			return empty, err
		}
	}
	return nil, nil
}

// pick reads the lowest index a query printed, so a failure names the first empty
// table the dump made, whatever order the rows came back in.
func pick(out string, err error) func([]*dumpTable) (*dumpTable, error) {
	return func(batch []*dumpTable) (*dumpTable, error) {
		if err != nil {
			return nil, err
		}
		first := -1
		for line := range strings.SplitSeq(out, "\n") {
			i, err := strconv.Atoi(strings.TrimSpace(line))
			if err == nil && i >= 0 && i < len(batch) && (first < 0 || i < first) {
				first = i
			}
		}
		if first < 0 {
			return nil, nil
		}
		return batch[first], nil
	}
}

func databasesQuery(k *databaseKind) string {
	if k.name == "MySQL" {
		return "SHOW DATABASES"
	}
	return "SELECT datname FROM pg_database"
}

// lineSet is what a query printed, a row a line.
func lineSet(out string) map[string]bool {
	set := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			set[line] = true
		}
	}
	return set
}

// tableName is a table as a person reads it: the database, then the table, in
// PostgreSQL with its schema where that isn't public.
func tableName(t *dumpTable) string {
	if t.schema == "" || t.schema == "public" {
		return t.database + "." + t.name
	}
	return t.database + "." + t.schema + "." + t.name
}

// sweepDrills removes what drills left: their containers, and the anonymous volumes
// their images declare, and their scratch volumes, but the one under way.
func (r *Runner) sweepDrills(ctx context.Context, keep string) {
	found, err := r.Client.ListContainers(ctx, map[string][]string{"label": {labelDrill}})
	if err == nil {
		for i := range found {
			if found[i].Labels[labelDrill] == keep {
				continue
			}
			if err := r.Client.RemoveOneOff(ctx, found[i].ID); err != nil && !docker.IsNotFound(err) {
				r.logger().Warn("can't remove what a restore drill left; the agent tries again within the hour", "container", found[i].ID, "error", err.Error())
			}
		}
	}
	volumes, err := r.Client.ListVolumes(ctx, labelDrill)
	if err != nil {
		return
	}
	for _, v := range volumes {
		if v.Labels[labelDrill] == keep {
			continue
		}
		if err := r.Client.RemoveVolume(ctx, v.Name); err != nil && !docker.IsNotFound(err) {
			r.logger().Warn("can't remove what a restore drill left; the agent tries again within the hour", "volume", v.Name, "error", err.Error())
		}
	}
}

// sweepIdle sweeps what drills left while none runs.
func (r *Runner) sweepIdle(ctx context.Context) {
	// It holds the drill slot while it sweeps, so a drill that starts meanwhile can't
	// lose its scratch volume to it.
	run := &drillRun{id: "sweep", stop: func(error) {}, done: make(chan struct{})}
	if r.takeDrillSlot(run) != nil {
		return
	}
	defer r.giveDrillSlot(run)
	r.sweepDrills(ctx, "")
}
