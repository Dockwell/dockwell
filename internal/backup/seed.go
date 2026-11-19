// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// A backup restores into a new database, which the change that restores it makes,
// as the step that makes the new database's service does, before its container
// first starts (ADR 0106). The volume the backup was taken of, and whatever runs on
// it, are only read: the backup's files, in that volume's own backups. The new
// volume has to be new, made earlier in the same change, with nothing on it. A dump
// loads in a loader of the new database's own release, as its service would start,
// on no network; a copy of files unpacks into the volume. One that fails empties
// the new volume again, and the change's rollback deletes what it made.

// Seed is a backup to restore into a new service's own volume, as the service's
// step has it.
type Seed struct {
	// Service is the new service, whose scope is its environment.
	Service resource.Ref
	// Backup is the backup, in From's own backups.
	Backup string
	// From is the volume the backup was taken of. The new volume is the new
	// service's own data volume, whatever From is called, as a move to a new release
	// names a database's volume anew (ADR 0080).
	From resource.Ref
	// Change is the change that restores it.
	Change string
	// App is the new service's container, as its step makes it, which the loader
	// starts as: its image, environment, command, user and mounts.
	App *resourcev1.Container
}

// data is what the new service runs as a database, as its database page reads it:
// its engine, the volume it keeps its data in, and where it mounts that.
func (s *Seed) data() (engines.Found, bool) {
	found, ok := engines.FromSpec(&resourcev1.Service{Runs: &resourcev1.Service_App{App: s.App}})
	if !ok || found.Volume == "" || found.External {
		return engines.Found{}, false
	}
	return found, true
}

// to is the new volume: the new service's own data volume.
func (s *Seed) to() resource.Ref {
	found, _ := s.data()
	return resource.Ref{Kind: volume.Kind, Scope: s.Service.Scope, Name: found.Volume}
}

// target is where the app mounts its data volume, or "" where it keeps none.
func (s *Seed) target() string {
	found, _ := s.data()
	if found.Target == "" {
		return ""
	}
	return path.Clean(found.Target)
}

// errSameEnvironment refuses a new database in the environment the backup was taken
// in, whose volumes are the original's (ADR 0106).
var errSameEnvironment = errors.New("the new database would be in the environment the backup was taken in, among the original's volumes, so Dockwell won't restore into it. Nothing changed")

// errNotADatabase refuses a new service that isn't a database Dockwell knows: a
// second copy of an app would act on whatever its secrets reach (ADR 0106).
var errNotADatabase = errors.New("the new service isn't a database Dockwell knows, keeping its data in a volume of its own, so a backup isn't restored into it. Restore it into what it was taken of instead")

// names refuses a seed whose names would lead out of the folders they name: the
// volume it reads from, the new service's environment, whose volume of the same
// name it fills, and the change, whose note goes in that volume's backups. Each is
// one folder or file name, checked before any path is made of them.
func (s *Seed) names() error {
	if s.From.Kind != volume.Kind || !folderName(s.From.Scope) || !folderName(s.From.Name) || !folderName(s.Service.Scope) || !folderName(s.Change) {
		return errOutside
	}
	// Never into the environment the backup was taken in, where its volumes are.
	if s.Service.Scope == s.From.Scope {
		return errSameEnvironment
	}
	found, ok := s.data()
	if !ok {
		return errNotADatabase
	}
	if !folderName(found.Volume) {
		return errOutside
	}
	return nil
}

// madeBy is SeedDone where a container of the new service carries this change's ID,
// as the step that made it labels it, and "" otherwise.
func (r *Runner) madeBy(ctx context.Context, s Seed) (string, error) {
	list, err := r.Client.ListContainers(ctx, map[string][]string{"label": {
		docker.LabelProject + "=" + docker.Project(s.Service.Scope), docker.LabelService + "=" + s.Service.Name, container.LabelChange + "=" + s.Change,
	}})
	if err != nil {
		return "", err
	}
	if len(list) > 0 {
		return SeedDone, nil
	}
	return "", nil
}

// Stages of a restore into a new volume, as its note has them.
const (
	SeedFilling = "filling"
	SeedDone    = "done"
)

// SeedStage is how far this change's restore into the new service's volume got: by
// its note, SeedDone or SeedFilling; SeedDone where the note is gone because the step
// that made the database finished, as the container it made carries this change's
// ID; or "" where this change never began one. A resume of the change reads it, once
// the new database is made (ADR 0106).
func (r *Runner) SeedStage(ctx context.Context, s Seed) (string, error) {
	if err := s.names(); err != nil {
		return "", err
	}
	b, err := os.ReadFile(r.seedMarker(s))
	if errors.Is(err, os.ErrNotExist) {
		return r.madeBy(ctx, s)
	}
	if err != nil {
		return "", err
	}
	var note map[string]string
	if err := json.Unmarshal(b, &note); err != nil {
		return "", err
	}
	if note["change"] != s.Change {
		return "", nil
	}
	return note["stage"], nil
}

// errOutside refuses a backup asked for by a name that would lead out of the
// backups of the volume it names.
var errOutside = errors.New("the backup it names isn't one of that volume's own, so Dockwell won't read it")

// backupIn finds the backup id among from's own backups, and nowhere else: from
// names a volume, by an environment and a name that are each one folder under this
// server's backups, neither of them a link, and id is a backup's ID, whose
// manifest and file are files in that folder, not links. Anything else is refused
// before a file is opened (ADR 0106).
func (r *Runner) backupIn(from resource.Ref, backupID string) (*Manifest, string, error) {
	if from.Kind != volume.Kind || !folderName(from.Scope) || !folderName(from.Name) {
		return nil, "", errOutside
	}
	if _, err := id.Parse(id.Backup, backupID); err != nil {
		return nil, "", errOutside
	}
	root, err := filepath.Abs(r.Root)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(root, from.Scope, from.Name)
	if rel, err := filepath.Rel(root, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, "", errOutside
	}
	// Each folder between the backups and the file is a folder, not a link.
	for _, p := range []string{filepath.Join(root, from.Scope), dir} {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("this server has no backups of %s, so it doesn't have %s. Choose a backup this server holds", from.Name, backupID)
		}
		if err != nil {
			return nil, "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, "", errOutside
		}
	}
	manifest := filepath.Join(dir, backupID+".json")
	if err := plainFile(manifest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("this server no longer has the backup %s. It may have been removed by hand. Choose another", backupID)
		}
		return nil, "", err
	}
	m, err := readManifest(manifest)
	if err != nil {
		return nil, "", err
	}
	if m.ID != backupID || !slices.Contains(slices.Concat([]string{formatArchive}, dumpFormats), m.Format) {
		return nil, "", errOutside
	}
	file := filepath.Join(dir, m.ID+"."+m.Format)
	if err := plainFile(file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("the file of the backup %s is missing. Choose another", backupID)
		}
		return nil, "", err
	}
	if fi, err := os.Lstat(file); err != nil || fi.Size() != m.Size {
		return nil, "", fmt.Errorf("the file of the backup %s is a different size from when it was taken. Choose another", backupID)
	}
	return m, file, nil
}

// folderName is a name that's one folder: not empty, not . or .., with no separator.
func folderName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`+"\x00")
}

// plainFile refuses what isn't a regular file, a link above all.
func plainFile(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errOutside
	}
	return nil
}

// kindOfImage finds a known database by the image it runs: only the official ones,
// as kindOf does.
func kindOfImage(image string) *databaseKind {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, k := range databaseKinds {
		if repo == k.image {
			return k
		}
	}
	return nil
}

// JudgeSeed says what restoring a backup into a new service's volume does, or why
// it can't: the backup has to be on this server, of a whole volume, and something
// the new service's database can take, a dump of its kind, or a copy of files of
// its release.
func (r *Runner) JudgeSeed(ctx context.Context, s Seed) (resource.RestorePlan, error) {
	if err := s.names(); err != nil {
		return resource.RestorePlan{}, err
	}
	m, file, err := r.backupIn(s.From, s.Backup)
	if err != nil {
		return resource.RestorePlan{}, err
	}
	if m.OneDatabase != "" {
		return resource.RestorePlan{}, fmt.Errorf("the backup %s holds %s alone, an app's database, which restores from that app's page. Restore it there", s.Backup, m.OneDatabase)
	}
	if err := seedFits(m, s.App); err != nil {
		return resource.RestorePlan{}, err
	}
	if err := r.checkAtPlan(ctx, file, m); err != nil {
		return resource.RestorePlan{}, err
	}
	return resource.RestorePlan{
		Backup: s.Backup, Method: m.Method, Database: m.Database, Taken: m.Taken, Size: m.Size, Reached: m.Reached, IntoNew: true, From: s.From,
	}, nil
}

// planCheckUpTo is the largest backup read through and hashed as a restore into a
// new database is planned, which a plan's minute covers on a small server.
const planCheckUpTo = 2 << 30

// headerRead is how much of a larger backup is read as it's planned.
const headerRead = 64 << 10

// checkAtPlan checks a backup as a restore into a new database is planned, so a
// damaged one is refused before anything is placed, within the call: one up to
// PlanCheckUpTo is read through and hashed; a larger one's gzip header and first
// 64 KiB are read, and the whole of it is checked as it loads, before anything is
// written, where a damaged one fails and the change rolls back. A call that ends
// stops the read (ADR 0106).
func (r *Runner) checkAtPlan(ctx context.Context, file string, m *Manifest) error {
	upTo := r.PlanCheckUpTo
	if upTo == 0 {
		upTo = planCheckUpTo
	}
	var err error
	if m.Size <= upTo {
		err = checkWithin(ctx, file, m, verifierOf(m))
	} else {
		err = headerOf(ctx, file)
	}
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("checking the backup took longer than planning may, so it stopped: %w. Try again, or restore it in place", ctx.Err())
	case err != nil:
		return fmt.Errorf("the backup is damaged: %w. Choose another", err)
	}
	return nil
}

// headerOf reads a gzipped backup's header and the first of what it holds.
func headerOf(ctx context.Context, file string) error {
	f, err := os.Open(file) //nolint:gosec // in a volume's own backups, checked by backupIn
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(ctxReader{ctx, f})
	if err != nil {
		return fmt.Errorf("it doesn't open: %w", err)
	}
	defer gz.Close()
	if _, err := io.CopyN(io.Discard, gz, headerRead); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("its start is damaged: %w", err)
	}
	return nil
}

// verifierOf reads back a backup as its format is written.
func verifierOf(m *Manifest) verifier {
	if isDump(m.Format) {
		return verifyDump(kindNamed(m.Database))
	}
	return verifyArchive
}

// seedFits says a new database can take a backup: a dump loads into a database of
// its kind, of any release, and a copy of a database's files opens only under the
// release that wrote it.
func seedFits(m *Manifest, app *resourcev1.Container) error {
	runs := kindOfImage(app.GetImage())
	if isDump(m.Format) {
		k := kindNamed(m.Database)
		if k == nil {
			return fmt.Errorf("the backup is a dump of %s, which this agent can't load. Update it to the control plane's version", m.Database)
		}
		if runs != k {
			return fmt.Errorf("the backup is a dump of %s, and the new database runs %s, which can't load it", m.Database, app.GetImage())
		}
		return nil
	}
	if k := kindOfImage(m.Image); k != nil && m.Image != "" && !k.sameSeries(m.Image, app.GetImage()) {
		return fmt.Errorf("the backup is a copy of files %s wrote, which only that release opens, and the new database runs %s. Restore it into the database it was taken of instead", m.Image, app.GetImage())
	}
	return nil
}

// seedMarker is where a restore into a new volume notes that it fills it, so a run
// of the same change, and only that, may empty it again, and one that finished
// isn't done twice.
func (r *Runner) seedMarker(s Seed) string {
	return filepath.Join(r.dir(s.to()), "restore-into-"+s.Change+".json")
}

// SeedNew restores the backup into the new service's volume, before its container
// first starts. A dump loads in a loader of the new database's release, on no
// network; a copy of files unpacks. A restore that fails empties the volume again.
func (r *Runner) SeedNew(ctx context.Context, s Seed) error {
	if err := s.names(); err != nil {
		return err
	}
	to := s.to()
	t := volume.TargetOf(to)
	release, err := r.hold(t)
	if err != nil {
		return err
	}
	defer release()
	if done, err := r.seeded(s); err != nil || done {
		return err
	}
	m, file, err := r.backupIn(s.From, s.Backup)
	if err != nil {
		return err
	}
	if m.OneDatabase != "" || s.target() == "" {
		return fmt.Errorf("the backup %s can't be restored into %s. Plan the restore again", s.Backup, s.Service.Name)
	}
	if err := seedFits(m, s.App); err != nil {
		return err
	}
	if _, err := r.Client.InspectVolume(ctx, t.Name); err != nil {
		return r.noVolume(to, err)
	}
	if err := r.seedIsNew(ctx, s, t); err != nil {
		return err
	}
	// Before anything is written: a bad backup costs nothing.
	resource.Progress(ctx, "Checking the backup")
	if err := checkWithin(ctx, file, m, verifierOf(m)); err != nil {
		return fmt.Errorf("the backup is damaged, so nothing was restored: %w. Choose another", err)
	}
	if err := os.MkdirAll(r.dir(to), 0o700); err != nil {
		return err
	}
	if err := r.noteSeed(s, SeedFilling); err != nil {
		return err
	}
	if isDump(m.Format) {
		err = r.seedDump(ctx, s, t, m, file)
	} else {
		resource.Progress(ctx, "Unpacking the backup into "+to.Name)
		_, err = r.unpack(ctx, t, file)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("the backup didn't restore into %s: %w. The database it was taken of runs as before", s.Service.Name, err), r.emptySeed(ctx, s, t))
	}
	return r.noteSeed(s, SeedDone)
}

// seedDump loads a dump into the new volume, in a loader of the new database's own
// release, which initializes its data there as the service's container would.
func (r *Runner) seedDump(ctx context.Context, s Seed, t volume.Target, m *Manifest, file string) error {
	k := kindNamed(m.Database)
	if _, err := r.tool(ctx, t, "empty", docker.Streams{}); err != nil {
		return fmt.Errorf("can't make %s ready: %w", s.From.Name, err)
	}
	app := s.App
	env := make([]string, 0, len(app.GetEnv()))
	for _, name := range slices.Sorted(maps.Keys(app.GetEnv())) {
		env = append(env, name+"="+app.GetEnv()[name])
	}
	req := &docker.CreateRequest{
		Config: docker.Config{
			Image: app.GetImage(), Env: env, Entrypoint: app.GetEntrypoint(), Cmd: app.GetCommand(), User: app.GetUser(),
			Labels: map[string]string{labelHelper: "1"},
		},
		HostConfig: docker.HostConfig{
			NetworkMode: "none",
			Mounts:      []docker.Mount{{Type: "volume", Source: t.Name, Target: s.target()}},
		},
	}
	port := ""
	if k.portFlags {
		port = portOf(app.GetCommand())
	}
	resource.Stage(ctx, resource.PhaseBackingUp, "Loading the backup into "+s.Service.Name)
	loader, done, err := r.helperDatabase(ctx, k, req, port, s.Service.Name)
	if err != nil {
		return err
	}
	defer done()
	f, err := os.Open(file) //nolint:gosec // in a volume's own backups, checked by backupIn
	if err != nil {
		return err
	}
	defer f.Close()
	resource.Progress(ctx, "Loading the dump")
	if err := r.loadDump(ctx, loader, f); err != nil {
		return fmt.Errorf("the dump didn't load: %w", err)
	}
	return nil
}

// seeded says this change restored into the volume already, as a run cut short
// after the restore, before the container started, finds.
func (r *Runner) seeded(s Seed) (bool, error) {
	b, err := os.ReadFile(r.seedMarker(s))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var note map[string]string
	if err := json.Unmarshal(b, &note); err != nil {
		return false, err
	}
	return note["stage"] == SeedDone, nil
}

func (r *Runner) noteSeed(s Seed, stage string) error {
	b, err := json.Marshal(map[string]string{"change": s.Change, "backup": s.Backup, "from": s.From.Scope + "/" + s.From.Name, "stage": stage})
	if err != nil {
		return err
	}
	return writeFile(r.seedMarker(s), b)
}

// seedIsNew refuses filling a volume that anything mounts, or that holds data this
// change didn't put there: the restore fills only a new database's own new volume.
func (r *Runner) seedIsNew(ctx context.Context, s Seed, t volume.Target) error {
	u, err := r.users(ctx, t)
	if err != nil {
		return err
	}
	if len(u.list) > 0 {
		return fmt.Errorf("%s is mounted by %s, so Dockwell won't restore the backup into it. Nothing changed", s.From.Name, human.List(namesOf(u.list)))
	}
	if _, err := os.Stat(r.seedMarker(s)); err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	v, err := r.tool(ctx, t, "measure", docker.Streams{})
	if err != nil {
		return fmt.Errorf("can't tell whether %s is empty, so Dockwell won't restore into it: %w", s.From.Name, err)
	}
	if v.Bytes > 0 {
		return fmt.Errorf("%s already holds %s that this restore didn't put there, so Dockwell won't restore into it. Nothing changed", s.From.Name, human.Size(v.Bytes))
	}
	return nil
}

// ForgetSeed removes a restore's note once the step that made the new database is
// done, so nothing reads it after: a resume of the change knows its step by the
// container it made, which carries the change's ID (ADR 0106).
func (r *Runner) ForgetSeed(s Seed) error {
	if err := s.names(); err != nil {
		return err
	}
	if err := os.Remove(r.seedMarker(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// emptySeed empties the new volume of a restore that didn't happen, rather than keep
// what it loaded for 7 days once the rollback deletes it.
func (r *Runner) emptySeed(ctx context.Context, s Seed, t volume.Target) error {
	if _, err := os.Stat(r.seedMarker(s)); err != nil {
		return nil
	}
	ctx, cancel := resource.Finishing(ctx, r.helperCleanup()+measureWait)
	defer cancel()
	if _, err := r.tool(ctx, t, "empty", docker.Streams{}); err != nil {
		return fmt.Errorf("can't empty %s, which holds part of the backup: %w", s.From.Name, err)
	}
	if err := os.Remove(r.seedMarker(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger().Warn("can't forget a restore's note on its new volume", "error", err.Error())
	}
	return nil
}
