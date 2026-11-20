// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionCopySecret is a secret's value copied into a plan, as a restore into a new
// database copies the original's root password secrets (ADR 0106).
const ActionCopySecret = "secret.copy"

// InstallWith gives the handler what plans a catalog install, which a restore into a
// new database makes the new database with (ADR 0106).
func (h *Handler) InstallWith(plan func(context.Context, composeapi.Install) (*dockwellv1.Change, error)) {
	h.install = plan
}

// original is a catalog database whose backup restores into a new one: its project,
// the service that runs on the backup's volume, and the pin it was installed from.
type original struct {
	project, display string
	service          string
	pin              *resourcev1.CatalogPin
	// secrets are the root password secrets its file reads, by name, with their
	// values and IDs, which the new database takes copies of.
	secrets map[string]string
	ids     map[string]string
}

// usedName finds each name a Compose file reads: ${NAME}, ${NAME:-...} and $NAME.
var usedName = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)|([A-Za-z_][A-Za-z0-9_]*))`)

// namesIn are the names file reads, as a set.
func namesIn(file string) map[string]bool {
	out := map[string]bool{}
	for _, m := range usedName.FindAllStringSubmatch(strings.ReplaceAll(file, "$$", ""), -1) {
		out[m[1]+m[2]] = true
	}
	return out
}

// restoreInto plans restoring a backup of a whole catalog database's volume into a
// new database, as the project into, leaving the database it was taken of as it
// is (ADR 0106). The new one is the original's catalog entry as it runs, from its
// pin, made on the server that holds the backup, with copies of the original's root
// password secrets, as the data the backup brings back signs in with those. Copying
// them is on the activity log. The backup restores into the new database's own
// volume, as its service's step makes it, before it first starts.
func (h *Handler) restoreInto(ctx context.Context, b *store.Backup, into, summary string) (*dockwellv1.Change, error) {
	if h.install == nil {
		return nil, h.fail(ctx, errors.New("this control plane can't make a database to restore into"))
	}
	if b.State != store.BackupDone {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is %s, so there's nothing to restore. Choose one that finished", b.ID, b.State), "not_restorable")
	}
	if b.OffServer {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on a server now. Fetch it from %s onto a server, then restore it", b.ID, b.Destination), "fetch_first")
	}
	if _, connected := h.agents.Session(b.ServerID); !connected {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the server that holds %s isn't connected, so it can't restore it now. Try again once it reconnects", b.ID), "fetch_first")
	}
	into = strings.TrimSpace(into)
	if err := (scope.ProjectType{}).CheckName(into); err != nil {
		return nil, authapi.Invalid(err)
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in to restore a backup"))
	}
	client, _ := authapi.ClientFrom(ctx)
	var o *original
	var destination string
	var taken map[uint32]bool
	var lent func(uint32) bool
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if o, err = h.originalOf(ctx, tx, b); err != nil {
			return err
		}
		if taken, err = h.portsTaken(ctx, tx); err != nil {
			return err
		}
		if lent, err = catalogapi.Lent(ctx, tx); err != nil {
			return err
		}
		destination, err = h.firstDestination(ctx, tx)
		return err
	})
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return nil, cerr
		}
		return nil, h.fail(ctx, err)
	}
	file, err := catalog.Without(string(o.pin.GetFile()), catalog.Off(o.pin.GetCompanions()))
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	variables := maps.Clone(o.pin.GetValues())
	if variables == nil {
		variables = map[string]string{}
	}
	for name := range o.secrets {
		variables[name] = ""
	}
	// The original runs on this server too, so each port it publishes from a variable
	// is taken: the new database publishes on the next that's free.
	moved := movePorts(file, variables, taken, lent)
	stands, err := catalog.StandFor(variables, slices.Collect(maps.Keys(o.secrets)))
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	maps.Copy(variables, stands)
	pin := proto.CloneOf(o.pin)
	if summary == "" {
		summary = fmt.Sprintf("Restore %s as %s, from its backup of %s", o.project, into, b.StartedAt.UTC().Format("2 Jan 2006, 15:04"))
	}
	from := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
	return h.install(ctx, composeapi.Install{
		Project: into, Summary: summary, Name: strings.TrimSpace(o.display + " (restored)"),
		File: []byte(file), Variables: variables, Secrets: o.secrets, Stands: stands, Pin: pin,
		Servers: []string{b.ServerID}, BackUpDaily: true, BackupDestination: destination,
		Notes: append([]revision.Note{{
			Reason:  "restore_into_new",
			Message: fmt.Sprintf("%s keeps running, and nothing of it changes. Apps that use it keep using it. %s takes copies of its %s, which the backup's data signs in with", o.project, into, human.List(slices.Sorted(maps.Keys(o.secrets)))),
		}}, moved...),
		Restore: &composeapi.InstallRestore{Service: o.service, Source: resource.RestoreSource{Backup: b.ID, Server: b.ServerID, From: from}},
		// Copying a secret's value into a plan is as much as revealing it: on record,
		// which secret, from which database, into which change, in the transaction
		// that plans it. A plan refused before it was made copied nothing.
		Records: func(change string) []store.Event {
			out := make([]store.Event, 0, len(o.secrets))
			for _, name := range slices.Sorted(maps.Keys(o.secrets)) {
				out = append(out, store.Event{
					ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: ActionCopySecret, Subject: o.ids[name], Target: o.ids[name], Surface: client.Surface(p),
					Detail: fmt.Sprintf("copied %s from %s into %s, to restore it as %s", name, o.project, change, into),
				})
			}
			return out
		},
	})
}

// originalOf finds the catalog database a backup was taken of: its project's pin,
// the one service that runs on the backup's volume, and the secrets its file reads.
func (h *Handler) originalOf(ctx context.Context, tx *store.Tx, b *store.Backup) (*original, error) {
	notAnInstall := authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is a backup of %s, which wasn't installed from the catalog, so nothing says what a new database of it runs. Replace its data instead", b.ID, b.Volume), "no_pin")
	env, err := tx.GetResourceByID(ctx, b.EnvironmentID)
	if errors.Is(err, store.ErrNotFound) || err == nil && env.Spec == nil {
		return nil, notAnInstall
	}
	if err != nil {
		return nil, err
	}
	project, err := tx.GetResourceByID(ctx, env.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && project.Spec == nil {
		return nil, notAnInstall
	}
	if err != nil {
		return nil, err
	}
	spec, err := h.keeper.Decode(ctx, tx, store.InRevision(project.ID, project.Kind, project.Revision), project.Spec)
	if err != nil {
		return nil, err
	}
	proj, _ := spec.(*resourcev1.Project)
	pin := proj.GetCatalog()
	if pin == nil || len(pin.GetFile()) == 0 {
		return nil, notAnInstall
	}
	if len(pin.GetNeeds()) > 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs with what other apps give it, which a new database of it wouldn't have. Replace its data instead", project.Name), "no_pin")
	}
	o := &original{project: project.Name, display: proj.GetDisplayName(), pin: pin, secrets: map[string]string{}, ids: map[string]string{}}
	if o.display == "" {
		o.display = project.Name
	}
	if o.service, err = h.databaseOf(ctx, tx, env.ID, project.Name, b, pin.GetCompanions()); err != nil {
		return nil, err
	}
	// The secrets the file reads that its pin keeps no value of are the install's own:
	// its root password, and any other it generated.
	file, err := catalog.Without(string(pin.GetFile()), catalog.Off(pin.GetCompanions()))
	if err != nil {
		return nil, err
	}
	reads := namesIn(file)
	stored, err := tx.ListResources(ctx, env.ID, "secret")
	if err != nil {
		return nil, err
	}
	for i := range stored {
		s := &stored[i]
		if s.Spec == nil || !reads[s.Name] {
			continue
		}
		if _, plain := pin.GetValues()[s.Name]; plain {
			continue
		}
		decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(s.ID, s.Kind, s.Revision), s.Spec)
		if err != nil {
			return nil, err
		}
		secret, _ := decoded.(*resourcev1.Secret)
		o.secrets[s.Name], o.ids[s.Name] = secret.GetValue(), s.ID
	}
	return o, nil
}

// hostPort finds a variable a Compose file publishes a port on the server from: the
// host side of a short port mapping, such as "${ADMIN_PORT}:8080", "$PORT:80" or
// "127.0.0.1:${PORT:-8081}:80", or a long one's published, with the default the
// file gives it, if any.
var hostPort = regexp.MustCompile(`(?m)(?:^\s*-\s*["']?(?:[0-9.]+:)?|published:\s*["']?)\$(?:\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([0-9]*))?\}|([A-Za-z_][A-Za-z0-9_]*))(?:[:"'\s]|$)`)

// movePorts moves each port file publishes from a variable to the next port nothing
// Dockwell runs has taken and no server lends to outgoing connections (#688), as
// the original holds the one it has on the same server, and says which moved. A
// variable with no value takes the file's default. One that isn't a port stays.
func movePorts(file string, variables map[string]string, taken map[uint32]bool, lent func(uint32) bool) []revision.Note {
	defaults := map[string]string{}
	for _, m := range hostPort.FindAllStringSubmatch(file, -1) {
		name := m[1] + m[3]
		if _, seen := defaults[name]; !seen || defaults[name] == "" {
			defaults[name] = m[2]
		}
	}
	var out []revision.Note
	for _, name := range slices.Sorted(maps.Keys(defaults)) {
		value := cmp.Or(variables[name], defaults[name])
		was, err := strconv.ParseUint(value, 10, 16)
		if err != nil || was == 0 {
			continue
		}
		for p := uint32(was) + 1; p <= 65535; p++ {
			if taken[p] || lent(p) {
				continue
			}
			taken[p] = true
			now := strconv.FormatUint(uint64(p), 10)
			variables[name] = now
			out = append(out, revision.Note{
				Reason: "restore_port", Names: []string{name, now}, Named: true,
				Message: fmt.Sprintf("%s is %s, as the database it was taken of publishes on %d", name, now, was),
			})
			break
		}
	}
	return out
}

// portsTaken are the ports on the server each service and container Dockwell runs
// publishes, on any server, as a catalog install's default ports avoid.
func (h *Handler) portsTaken(ctx context.Context, tx *store.Tx) (map[uint32]bool, error) {
	taken := map[uint32]bool{}
	for _, kind := range []string{string(service.Kind), string(container.Kind)} {
		stored, err := tx.ListKind(ctx, kind)
		if err != nil {
			return nil, err
		}
		for i := range stored {
			r := &stored[i]
			if r.Spec == nil {
				continue
			}
			decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return nil, err
			}
			var runs *resourcev1.Container
			switch spec := decoded.(type) {
			case *resourcev1.Service:
				runs = spec.GetApp()
			case *resourcev1.Container:
				runs = spec
			}
			for _, p := range runs.GetPorts() {
				taken[p.GetHostPort()] = true
			}
		}
	}
	return taken, nil
}

// databaseOf is the service of a database Dockwell knows, as its database page does,
// that a backup was taken of: the one that keeps its data in the backup's volume, or,
// for a backup from before a move to a new release left that volume (ADR 0080), the
// environment's one database while nothing runs on the volume now. Only a database
// entry restores as a new one: its services are databases and the entry's own
// companions, such as Adminer. A backup of an app's own volume, or of the database an
// app ships beside itself, isn't restored as a new database: a second copy of the app
// would act on whatever its secrets reach, beside the first (ADR 0106).
func (h *Handler) databaseOf(ctx context.Context, tx *store.Tx, env, project string, b *store.Backup, companions map[string]bool) (string, error) {
	services, err := tx.ListResources(ctx, env, "service")
	if err != nil {
		return "", err
	}
	var databases, apps []string
	onVolume, mountedBy := "", ""
	for i := range services {
		s := &services[i]
		if s.Spec == nil {
			continue
		}
		decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(s.ID, s.Kind, s.Revision), s.Spec)
		if err != nil {
			return "", err
		}
		svc, _ := decoded.(*resourcev1.Service)
		found, isDatabase := database.FromSpec(svc)
		if _, companion := companions[s.Name]; !isDatabase && !companion {
			apps = append(apps, s.Name)
		}
		if isDatabase {
			databases = append(databases, s.Name)
			if found.Volume == b.Volume && !found.External {
				onVolume = s.Name
			}
		}
		for _, m := range svc.GetApp().GetMounts() {
			if m.GetVolume() == b.Volume && !m.GetExternal() && mountedBy == "" {
				mountedBy = s.Name
			}
		}
	}
	notADatabase := func(why string) error {
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is a backup of %s, %s, so it isn't restored as a new database: that's for PostgreSQL, MySQL, MariaDB, MongoDB, Redis and Valkey. Replace its data instead", b.ID, b.Volume, why), "not_a_database")
	}
	switch {
	case len(apps) > 0 && (onVolume != "" || mountedBy == ""):
		return "", notADatabase(fmt.Sprintf("the database of %s, which runs %s beside it, an app of its own", project, human.List(apps)))
	case onVolume != "":
		return onVolume, nil
	case mountedBy != "":
		return "", notADatabase(fmt.Sprintf("which %s keeps its data in, and that isn't a database Dockwell knows", mountedBy))
	case len(databases) == 1:
		// A volume nothing runs on now, as a move to a new release leaves its old one.
		return databases[0], nil
	case len(databases) == 0:
		return "", notADatabase(fmt.Sprintf("and nothing in %s is a database Dockwell knows", project))
	}
	return "", authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("nothing in %s runs on %s now, and it has %s, so which one the backup was taken of isn't known. Replace its data instead", project, b.Volume, human.List(databases)), "no_pin")
}

// firstDestination is the storage a new database's daily backups also go to, as an
// install's do (#793): the first destination by name, or none.
func (h *Handler) firstDestination(ctx context.Context, tx *store.Tx) (string, error) {
	stored, err := tx.ListKind(ctx, string(backuppolicy.DestinationKind))
	if err != nil {
		return "", err
	}
	var names []string
	for i := range stored {
		if stored[i].Spec != nil {
			names = append(names, stored[i].Name)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	return slices.Min(names), nil
}
