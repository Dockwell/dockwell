// SPDX-License-Identifier: AGPL-3.0-only

// Package databases serves dockwell.v1.DatabaseService: the services that run a
// database Dockwell knows, told apart by their specs, with what each database
// says of itself on its servers, read in its own container by the agent there.
// See docs/adr/0038-databases.md.
package databases

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// backupsRead is how many of a volume's newest backups are read for its newest whole
// one, so a run of failures doesn't hide it.
const backupsRead = 200

// askWait bounds what a server gets to answer for its databases, so a list never
// waits long on one that's slow: its rows say they couldn't be read.
const askWait = 10 * time.Second

// keepFor is how long a reading stands. Several readers in that time, as a page
// that refreshes beside the CLI, ask the database once.
const keepFor = 10 * time.Second

// Backups lists a volume's backups, newest first, as BackupService does.
type Backups interface {
	ListBackups(context.Context, *connect.Request[dockwellv1.ListBackupsRequest]) (*connect.Response[dockwellv1.ListBackupsResponse], error)
}

// Secrets opens a stored spec, as the revision store sealed it.
type Secrets interface {
	DecodeEach(ctx context.Context, tx *store.Tx, specs []store.StoredSpec) ([]proto.Message, []error, error)
}

// Changes proposes a change, as ProposeChange does, for a method that makes edits of
// its own.
type Changes interface {
	Propose(ctx context.Context, summary string, edits []*dockwellv1.Edit, field func(*scope.ChoiceError, string) string, also changes.Also) (*dockwellv1.Change, error)
}

// Handler implements DatabaseService.
type Handler struct {
	changes Changes
	agents  *agentapi.Agents
	backups Backups
	db      *store.DB
	keeper  Secrets
	logger  *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	kept map[string]keptReading
	// features is the feature compatibility version each service's database said
	// last, by the service's ID, for a plan that moves it back (LastFeatures).
	features map[string]string
}

var _ dockwellv1connect.DatabaseServiceHandler = (*Handler)(nil)

// keptReading is a reading, and when it was asked for.
type keptReading struct {
	reading *dockwellv1.DatabaseReading
	at      time.Time
	made    string
}

// New returns a handler that reads the store through keeper, asks agents what each
// database says, and backups for what's kept of each.
func New(ch Changes, agents *agentapi.Agents, backups Backups, db *store.DB, keeper Secrets, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, agents: agents, backups: backups, db: db, keeper: keeper, logger: logger, now: time.Now, kept: map[string]keptReading{}, features: map[string]string{}}
}

// placed is a database with where it runs.
type placed struct {
	db      *dockwellv1.Database
	ref     resource.Ref
	servers []store.Server
	// made is what Dockwell last changed of it: its service's revision, and each of
	// its users' revisions. A reading kept from before a change is no reading of it.
	made string
	// firstUser is who its image makes first, whose password its secret holds:
	// POSTGRES_USER, or postgres, root for MySQL and MariaDB, none for Redis.
	firstUser string
	// unguarded says it asks no one for a password (database.Found), and unprefixed
	// that a user Dockwell made on it keeps to no key prefix.
	unguarded, unprefixed bool
	// prefixed are the users Dockwell made on it that keep to a key prefix, by name.
	prefixed []string
}

// ListDatabases returns every database.
func (h *Handler) ListDatabases(ctx context.Context, _ *connect.Request[dockwellv1.ListDatabasesRequest]) (*connect.Response[dockwellv1.ListDatabasesResponse], error) {
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if err := h.complete(ctx, list); err != nil {
		return nil, h.fail(ctx, err)
	}
	out := &dockwellv1.ListDatabasesResponse{}
	for _, p := range list {
		out.Databases = append(out.Databases, p.db)
	}
	return connect.NewResponse(out), nil
}

// GetDatabase returns one database.
func (h *Handler) GetDatabase(ctx context.Context, req *connect.Request[dockwellv1.GetDatabaseRequest]) (*connect.Response[dockwellv1.GetDatabaseResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	one, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if err := h.complete(ctx, []*placed{one}); err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.GetDatabaseResponse{Database: one.db}), nil
}

// passwordLength is how long a generated password is: 62 characters to choose from,
// for about 190 bits.
const passwordLength = 32

// passwordAlphabet is what a generated password is made of, which every engine, URL
// and shell takes as it is.
const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// CreateDatabaseUser plans a user and its database, with a generated password kept as
// a secret of the environment.
func (h *Handler) CreateDatabaseUser(ctx context.Context, req *connect.Request[dockwellv1.CreateDatabaseUserRequest]) (*connect.Response[dockwellv1.CreateDatabaseUserResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	user := m.GetUser()
	if err := database.CheckSQLName("user", user); err != nil {
		return nil, invalidField("user", err)
	}
	db := cmp.Or(m.GetDatabase(), user)
	if err := database.CheckSQLName("database", db); err != nil {
		return nil, invalidField("database", err)
	}
	secret := cmp.Or(m.GetSecret(), strings.ToUpper(user)+"_DATABASE_PASSWORD")
	if err := variable.CheckName("secret", secret); err != nil {
		return nil, invalidField("secret", err)
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	server, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	engine := database.Of(server.db.GetEngine())
	if engine.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB {
		// Its users are read, and making them comes after (ADR 0038).
		return nil, authapi.Invalid(fmt.Errorf("%s runs MongoDB, and Dockwell can't make users in MongoDB yet. Make one with db.createUser in mongosh, in the database they sign in to", server.ref.Name))
	}
	if !engine.SQL() {
		return nil, authapi.Invalid(fmt.Errorf("%s runs %s, which has no users of its own", server.ref.Name, engine.Name))
	}
	if len(server.servers) == 0 {
		return nil, authapi.Invalid(fmt.Errorf("%s runs on no server now, and a user is made where it runs. Deploy it first", server.ref.Name))
	}
	env := server.ref.Scope
	err = h.db.View(ctx, func(tx *store.Tx) error {
		for _, taken := range []struct {
			kind              resource.Kind
			field, name, what string
		}{
			{database.UserKind, "user", user, fmt.Sprintf("a database user %s in this environment already. Choose another name", user)},
			{variable.SecretKind, "secret", secret, fmt.Sprintf("a secret %s in this environment already, and a new user's password goes in a new one. Choose another name for it", secret)},
			{variable.VariableKind, "secret", secret, fmt.Sprintf("a variable %s in this environment already, and a new user's password goes in a secret of its own. Choose another name for it", secret)},
		} {
			r, err := tx.GetResource(ctx, string(taken.kind), env, taken.name)
			switch {
			case err == nil && r.Spec != nil:
				return invalidField(taken.field, errors.New("there's "+taken.what))
			case err != nil && !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	password, err := generate(passwordLength)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	servers := make([]string, 0, len(server.servers))
	for i := range server.servers {
		servers = append(servers, server.servers[i].ID)
	}
	spec := &resourcev1.DatabaseUser{Service: server.ref.Name, Password: secret}
	if db != user {
		spec.DatabaseName = db
	}
	edits := []*dockwellv1.Edit{
		{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: secret},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: password}}},
		},
		{
			Resource: &dockwellv1.ResourceRef{Kind: string(database.UserKind), Scope: env, Name: user},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: spec}},
			Servers:  servers,
		},
	}
	summary := fmt.Sprintf("Add the user %s to %s, with the database %s", user, server.ref.Name, db)
	change, err := h.changes.Propose(ctx, summary, edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CreateDatabaseUserResponse{Change: change}), nil
}

// ChangeDatabasePassword plans a new generated password for a database's first user,
// in the secret its service starts with, or for a user Dockwell made, in theirs.
// Setting the secret takes in the database and every app that uses it, and the agent
// changes the first user's in the database as it restarts (ADR 0038), as a user's
// own step does for theirs.
func (h *Handler) ChangeDatabasePassword(ctx context.Context, req *connect.Request[dockwellv1.ChangeDatabasePasswordRequest]) (*connect.Response[dockwellv1.ChangeDatabasePasswordResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	server, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	engine := database.Of(server.db.GetEngine())
	name, env := server.ref.Name, server.ref.Scope
	if engine.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB {
		// Its root user's password is kept in its data from the first start on, so a
		// new secret alone would no longer sign anyone in (ADR 0038).
		return nil, authapi.Invalid(fmt.Errorf("%s runs MongoDB, and Dockwell can't change its password yet, as MongoDB keeps it in its data. Change it with db.changeUserPassword in mongosh, then set its secret to match", name))
	}
	whose, secret := m.GetUser(), server.db.GetPasswordSecret()
	if whose == "" {
		if secret == "" {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s's password isn't in a secret of its environment, so Dockwell has none to change. Keep it in a secret on its app's page first", name), "no_secret")
		}
		if engine.SQL() {
			for i := range server.servers {
				on := &server.servers[i]
				s, ok := h.agents.Session(on.ID)
				switch {
				case !ok:
					// Its agent changes the password as the database restarts: one that's
					// away can't say whether it does.
					return nil, authapi.ReasonError(connect.CodeUnavailable,
						fmt.Errorf("%s isn't connected, so Dockwell can't tell whether its agent changes a password in %s as it restarts. Try again once it reconnects", on.Name, engine.Name), "unavailable")
				case !s.FirstPasswords():
					return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
						fmt.Errorf("%s's agent is too old to change a password in %s as it restarts. Update the agent on %s, then try again", on.Name, engine.Name, on.Name), "agent_outdated")
				}
			}
		}
		whose = server.firstUser
	} else {
		if err := database.CheckSQLName("user", whose); err != nil {
			return nil, invalidField("user", err)
		}
		if secret, err = h.userSecret(ctx, env, name, whose); err != nil {
			return nil, h.fail(ctx, err)
		}
	}
	password, err := generate(passwordLength)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	edits := []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: secret},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: password}}},
	}}
	summary := fmt.Sprintf("Change the password of %s in %s", whose, name)
	if whose == "" {
		summary = "Change the password of " + name
	}
	change, err := h.changes.Propose(ctx, summary, edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ChangeDatabasePasswordResponse{Change: change}), nil
}

// firstUserOf is who an engine's image makes first, from what its service's
// environment says, as its password secret is theirs: empty for Redis and Valkey,
// which have no users of their own.
func firstUserOf(e *database.Engine, env map[string]string) string {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		return cmp.Or(env["POSTGRES_USER"], "postgres")
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		return "root"
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB:
	}
	return ""
}

// userSecret is the secret a user Dockwell made in a database server, by its service's
// name, keeps their password in.
func (h *Handler) userSecret(ctx context.Context, env, server, user string) (string, error) {
	var secret string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(database.UserKind), env, user)
		switch {
		case errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil:
			return invalidField("user", fmt.Errorf("%s has no user %s that Dockwell made. Choose one from its Users tab", server, user))
		case err != nil:
			return err
		}
		// A user's spec holds nothing secret, so it's stored as it is.
		spec := &resourcev1.DatabaseUser{}
		if err := proto.Unmarshal(r.Spec, spec); err != nil {
			return fmt.Errorf("can't read the database user %s: %w", user, err)
		}
		if spec.GetService() != server {
			return invalidField("user", fmt.Errorf("the user %s is %s's, not %s's", user, spec.GetService(), server))
		}
		secret = spec.GetPassword()
		return nil
	})
	return secret, err
}

// invalidField is an invalid argument in one field of a request, which its reason
// names, as "invalid_user", so a form can say it beside that field.
func invalidField(field string, err error) *connect.Error {
	return authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid_"+field)
}

// generate makes a password of n characters, each as likely as any other.
func generate(n int) (string, error) {
	var b strings.Builder
	size := big.NewInt(int64(len(passwordAlphabet)))
	for range n {
		i, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", fmt.Errorf("generating a password: %w", err)
		}
		b.WriteByte(passwordAlphabet[i.Int64()])
	}
	return b.String(), nil
}

// pick finds the database a request names, by its service's ID or its name.
func (h *Handler) pick(ctx context.Context, list []*placed, named, project, environment string) (*placed, error) {
	if strings.HasPrefix(named, id.Service+"_") {
		for _, p := range list {
			if p.db.GetId() == named {
				return p, nil
			}
		}
		return nil, notFound(named)
	}
	var tree scope.Tree
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		tree, err = scope.Load(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	var found []*placed
	var where []string
	for _, p := range tree.Projects {
		if project != "" && project != p.ID && project != p.Name {
			continue
		}
		for _, e := range p.Environments {
			if environment != "" && environment != e.ID && environment != e.Name {
				continue
			}
			for _, d := range list {
				if d.ref.Scope == e.ID && d.ref.Name == named {
					found = append(found, d)
					where = append(where, p.Name+"/"+e.Name)
				}
			}
		}
	}
	switch len(found) {
	case 0:
		return nil, notFound(named)
	case 1:
		return found[0], nil
	}
	return nil, authapi.Invalid(fmt.Errorf("there's a database called %s in %s. Say which with its project and environment, or pass its ID", named, human.List(where)))
}

func notFound(named string) error {
	return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no database %s. A database is a service that runs PostgreSQL, MySQL, MariaDB, Redis or Valkey from its official image", named), "not_found")
}

// list reads every service that runs a database, by name, with where each runs.
func (h *Handler) list(ctx context.Context) ([]*placed, error) {
	var out []*placed
	err := h.db.View(ctx, func(tx *store.Tx) error {
		services, err := tx.ListKind(ctx, string(service.Kind))
		if err != nil {
			return err
		}
		specs := make([]store.StoredSpec, len(services))
		ids := make([]string, len(services))
		for i := range services {
			r := &services[i]
			specs[i] = store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec}
			ids[i] = r.ID
		}
		decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, specs)
		if err != nil {
			return err
		}
		deployments, err := tx.LatestDeployments(ctx, ids)
		if err != nil {
			return err
		}
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		byID := map[string]store.Server{}
		for i := range servers {
			if servers[i].RevokedAt == nil {
				byID[servers[i].ID] = servers[i]
			}
		}
		// A user's spec holds nothing secret, so it's stored as it is.
		made, err := tx.ListKind(ctx, string(database.UserKind))
		if err != nil {
			return err
		}
		users := map[string][]string{}
		unprefixed := map[string]bool{}
		prefixed := map[string][]string{}
		for i := range made {
			u := &made[i]
			spec := &resourcev1.DatabaseUser{}
			if err := proto.Unmarshal(u.Spec, spec); err != nil {
				return fmt.Errorf("can't read the database user %s: %w", u.Name, err)
			}
			key := u.Scope + "/" + spec.GetService()
			users[key] = append(users[key], u.ID+"@"+strconv.Itoa(u.Revision))
			unprefixed[key] = unprefixed[key] || spec.GetKeyPrefix() == ""
			if spec.GetKeyPrefix() != "" {
				prefixed[key] = append(prefixed[key], u.Name)
			}
		}
		inScope := map[string][]string{}
		for i := range services {
			inScope[services[i].Scope] = append(inScope[services[i].Scope], services[i].Name)
		}
		entries := map[string]string{}
		for i := range services {
			r := &services[i]
			if unreadable[i] != nil {
				// ListServices says which can't be read, and why.
				continue
			}
			svc, ok := decoded[i].(*resourcev1.Service)
			if !ok {
				continue
			}
			f, ok := database.FromSpec(svc)
			if !ok {
				continue
			}
			db := &dockwellv1.Database{
				Id:             r.ID,
				Resource:       resourcepb.Ref(resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}),
				Engine:         f.Engine.Kind,
				Image:          svc.GetApp().GetImage(),
				Version:        database.Version(svc.GetApp().GetImage()),
				Volume:         f.Volume,
				VolumeExternal: f.External,
				Port:           f.Port,
				Host:           r.Name,
				PasswordSecret: f.PasswordSecret,
			}
			for _, name := range inScope[r.Scope] {
				if name != r.Name {
					db.Beside = append(db.Beside, name)
				}
			}
			if f.Volume != "" && !f.External {
				v, err := tx.GetResource(ctx, "volume", r.Scope, f.Volume)
				switch {
				case err == nil && v.Spec != nil:
					db.VolumeId = v.ID
				case err != nil && !errors.Is(err, store.ErrNotFound):
					return err
				}
			}
			entry, ok := entries[r.Scope]
			if !ok {
				if entry, err = catalogEntry(ctx, tx, r.Scope); err != nil {
					return err
				}
				entries[r.Scope] = entry
			}
			db.CatalogEntry = entry
			p := &placed{
				db: db, ref: resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}, made: strconv.Itoa(r.Revision),
				firstUser: firstUserOf(f.Engine, svc.GetApp().GetEnv()), unguarded: f.Unguarded, unprefixed: unprefixed[r.Scope+"/"+r.Name], prefixed: prefixed[r.Scope+"/"+r.Name],
			}
			for _, u := range users[r.Scope+"/"+r.Name] {
				p.made += " " + u
			}
			for _, d := range deployments[r.ID] { //nolint:gocritic // a few rows, read once
				if s, ok := byID[d.Server]; ok && d.State != store.DeploymentSuperseded {
					p.servers = append(p.servers, s)
				}
			}
			slices.SortFunc(p.servers, func(a, b store.Server) int { return cmp.Compare(a.Name, b.Name) })
			out = append(out, p)
		}
		return nil
	})
	slices.SortStableFunc(out, func(a, b *placed) int { return cmp.Compare(a.ref.Name, b.ref.Name) })
	return out, err
}

// catalogEntry is the catalog entry an environment's project was installed from,
// or "".
func catalogEntry(ctx context.Context, tx *store.Tx, environment string) (string, error) {
	env, err := tx.GetResourceByID(ctx, environment)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	project, err := tx.GetResourceByID(ctx, env.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && project.Spec == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// A project is never sealed (ADR 0022).
	spec := &resourcev1.Project{}
	if err := proto.Unmarshal(project.Spec, spec); err != nil {
		return "", fmt.Errorf("can't read the project %s: %w", project.Name, err)
	}
	return spec.GetCatalog().GetEntry(), nil
}

// complete adds what's not in the store: each database's newest backups, and what
// each says of itself on its servers.
func (h *Handler) complete(ctx context.Context, list []*placed) error {
	for _, p := range list {
		if p.db.GetVolumeId() == "" {
			continue
		}
		res, err := h.backups.ListBackups(ctx, connect.NewRequest(&dockwellv1.ListBackupsRequest{VolumeId: p.db.GetVolumeId(), PageSize: backupsRead}))
		if errors.Is(err, context.Canceled) {
			return err
		}
		if err != nil {
			// One database's backups that can't be read cost it its backups, not the list.
			h.logger.WarnContext(ctx, "can't read a database's backups", slog.String("database", p.db.GetId()), slog.String("error", err.Error()))
			continue
		}
		for _, b := range res.Msg.GetBackups() {
			if b.GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE {
				p.db.NewestBackup = b
				break
			}
			if p.db.LatestBackup == nil {
				p.db.LatestBackup = b
			}
		}
	}
	h.read(ctx, list)
	return h.made(ctx, list)
}

// made marks the users Dockwell made in each database, with the secret each signs in
// with and the services that use it. A spec of a database user holds nothing secret,
// so it's stored as it is.
func (h *Handler) made(ctx context.Context, list []*placed) error {
	byScope := map[string][]*placed{}
	for _, p := range list {
		byScope[p.ref.Scope] = append(byScope[p.ref.Scope], p)
	}
	return h.db.View(ctx, func(tx *store.Tx) error {
		for env, dbs := range byScope {
			users, err := tx.ListResources(ctx, env, string(database.UserKind))
			if err != nil {
				return err
			}
			if len(users) == 0 {
				continue
			}
			uses, err := tx.UsersOf(ctx, env, string(variable.SecretKind))
			if err != nil {
				return err
			}
			for i := range users {
				u := &users[i]
				spec := &resourcev1.DatabaseUser{}
				if err := proto.Unmarshal(u.Spec, spec); err != nil {
					return fmt.Errorf("can't read the database user %s: %w", u.Name, err)
				}
				var usedBy []string
				for j := range uses {
					if uses[j].Kind == string(service.Kind) && uses[j].Uses.Name == spec.GetPassword() && !slices.Contains(usedBy, uses[j].Name) {
						usedBy = append(usedBy, uses[j].Name)
					}
				}
				for _, p := range dbs {
					if p.ref.Name != spec.GetService() {
						continue
					}
					for _, r := range p.db.GetReadings() {
						for _, a := range r.GetUsers() {
							if a.GetName() == u.Name {
								a.Secret, a.UsedBy, a.ResourceId = spec.GetPassword(), usedBy, u.ID
							}
						}
					}
				}
			}
		}
		return nil
	})
}

// read asks each server about its databases at once, all servers together, and
// keeps what each said for keepFor.
func (h *Handler) read(ctx context.Context, list []*placed) {
	type ask struct {
		server store.Server
		dbs    []*placed
	}
	byServer := map[string]*ask{}
	var order []string
	now := h.now()
	for _, p := range list {
		for i := range p.servers {
			s := p.servers[i]
			if r, ok := h.fresh(s.ID, p.db.GetId(), p.made, now); ok {
				p.db.Readings = append(p.db.Readings, r)
				continue
			}
			a := byServer[s.ID]
			if a == nil {
				a = &ask{server: s}
				byServer[s.ID] = a
				order = append(order, s.ID)
			}
			a.dbs = append(a.dbs, p)
		}
	}
	readings := make([][]*dockwellv1.DatabaseReading, len(order))
	var wg sync.WaitGroup
	for i, server := range order {
		a := byServer[server]
		wg.Go(func() { readings[i] = h.ask(ctx, a.server, a.dbs) })
	}
	wg.Wait()
	for i, server := range order {
		for j, p := range byServer[server].dbs {
			p.db.Readings = append(p.db.Readings, readings[i][j])
		}
	}
	for _, p := range list {
		slices.SortFunc(p.db.Readings, func(a, b *dockwellv1.DatabaseReading) int { return cmp.Compare(a.GetServerName(), b.GetServerName()) })
	}
}

// fresh is a reading taken a moment ago, of the database as Dockwell last changed it.
func (h *Handler) fresh(server, db, made string, now time.Time) (*dockwellv1.DatabaseReading, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.kept[server+"/"+db]
	if !ok || now.Sub(k.at) >= keepFor || k.made != made {
		return nil, false
	}
	return proto.CloneOf(k.reading), true
}

// ask asks one server about its databases, and says for each why it couldn't.
func (h *Handler) ask(ctx context.Context, server store.Server, dbs []*placed) []*dockwellv1.DatabaseReading {
	out := make([]*dockwellv1.DatabaseReading, len(dbs))
	call := &agentv1.DatabaseCall{}
	for _, p := range dbs {
		call.Databases = append(call.Databases, &agentv1.DatabaseAsked{Service: resourcepb.Ref(p.ref), Engine: p.db.GetEngine()})
	}
	at := h.now()
	why := ""
	// What a server that isn't connected says costs nothing to ask again, and it may
	// be back in a moment.
	keep := true
	var got *agentv1.DatabaseResult
	if s, ok := h.agents.Session(server.ID); !ok {
		why, keep = server.Name+" isn't connected, so it can't say how its databases are", false
	} else {
		ctx, cancel := context.WithTimeout(ctx, askWait)
		var err error
		got, err = s.Databases(ctx, call)
		cancel()
		switch {
		case errors.Is(err, resource.ErrNotConnected), errors.Is(err, resource.ErrLost):
			why, keep = server.Name+" isn't connected, so it can't say how its databases are", false
		case errors.Is(err, context.DeadlineExceeded):
			why = fmt.Sprintf("%s didn't answer within %s", server.Name, askWait)
		case err != nil:
			plain, _ := resource.Explain(err)
			why = fmt.Sprintf("%s can't read its databases: %s", server.Name, strings.TrimSuffix(plain, "."))
		}
	}
	// A reader that left took the answer with it.
	keep = keep && ctx.Err() == nil
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, p := range dbs {
		r := &dockwellv1.DatabaseReading{Unread: why}
		if why == "" {
			r = got.GetReadings()[i]
		}
		r.ServerId, r.ServerName, r.ReadTime = server.ID, server.Name, timestamppb.New(at)
		out[i] = r
		if fcv := r.GetFeatureCompatibility(); fcv != "" {
			h.features[p.db.GetId()] = fcv
		}
		if keep {
			h.kept[server.ID+"/"+p.db.GetId()] = keptReading{reading: proto.CloneOf(r), at: at, made: p.made}
		}
	}
	for k, v := range h.kept {
		if at.Sub(v.at) >= keepFor {
			delete(h.kept, k)
		}
	}
	return out
}

// LastFeatures is the feature compatibility version a service's database said in its
// last reading since the control plane started, or "" where none did.
func (h *Handler) LastFeatures(serviceID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.features[serviceID]
}

// fail turns what went wrong into what the API says. An error made for the caller
// passes through as it is.
func (h *Handler) fail(ctx context.Context, err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "a database call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its databases; try again, and check its logs if it keeps failing"))
}
