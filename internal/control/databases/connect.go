// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Connect a database: an app from no entry meets a need because a person says so, on
// a server Dockwell runs, with a new database and user of its own, or a database and
// user the server has, taken on with the password the app signs in with now (ADR 0084).

// connectable are the engines an app is connected to here, by the catalog's word.
var connectable = map[dockwellv1.DatabaseEngine]catalogv1.Engine{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: catalogv1.Engine_ENGINE_POSTGRESQL,
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      catalogv1.Engine_ENGINE_MYSQL,
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    catalogv1.Engine_ENGINE_MARIADB,
}

// linkName is what a link may be called: a need's name, as an entry's is.
var linkName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// appEnv is the app's environment, as connecting reads it.
type appEnv struct {
	id, project, name string
	// projectID is the project's ID.
	projectID string
	// services are the environment's services, decoded, by name; variables and secrets
	// its values, by name, secrets with what each holds.
	services  map[string]*resourcev1.Service
	variables map[string]bool
	secrets   map[string]*resourcev1.Secret
	// revisions are each secret's revision, which a sign-in check's answer is kept by.
	revisions map[string]int
	// servers are where the app runs, by ID.
	servers []string
}

// ConnectDatabase plans an app's link to a database server Dockwell runs.
func (h *Handler) ConnectDatabase(ctx context.Context, req *connect.Request[dockwellv1.ConnectDatabaseRequest]) (*connect.Response[dockwellv1.ConnectDatabaseResponse], error) {
	m := req.Msg
	switch {
	case m.GetId() == "":
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	case m.GetApp() == "":
		return nil, invalidField("app", errors.New("the request names no app. Pass its service's name"))
	case len(m.GetSets()) == 0:
		return nil, invalidField("sets", errors.New("the request sets nothing the app reads. Name each, as DB_HOST=host or DATABASE_URL=url"))
	}
	link := cmp.Or(m.GetLink(), "db")
	if !linkName.MatchString(link) {
		return nil, invalidField("link", fmt.Errorf("%q isn't a link's name: use up to 31 lowercase letters, digits and dashes, starting with a letter, such as db", link))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	server, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	eng, ok := connectable[server.db.GetEngine()]
	if !ok {
		return nil, authapi.Invalid(fmt.Errorf("%s runs %s, and an app connects to PostgreSQL, MySQL or MariaDB for now", server.ref.Name, database.Of(server.db.GetEngine()).Name))
	}
	if len(server.servers) == 0 {
		return nil, authapi.Invalid(fmt.Errorf("%s runs on no server now, and an app connects to it where it runs. Deploy it first", server.ref.Name))
	}
	cands, err := h.Candidates(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	i := slices.IndexFunc(cands, func(c catalog.Candidate) bool { return c.ServiceID == server.db.GetId() })
	if i < 0 {
		return nil, h.fail(ctx, notFound(m.GetId()))
	}
	c := cands[i]
	app, err := h.appEnv(ctx, m.GetAppProject(), m.GetAppEnvironment(), m.GetApp(), c)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if c.AdministeredBy != "" && (c.EnvironmentID != app.id || c.AdministeredBy != m.GetApp()) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s signs in to %s as its administrator, so another app's user there could be reached through it. Give %s a user of its own first", c.AdministeredBy, c.Service, c.AdministeredBy), "administered")
	}
	// Until databases on other servers come (#573), both ends run on one.
	if len(app.servers) == 0 {
		return nil, authapi.Invalid(fmt.Errorf("%s runs on no server now, and it connects to a database on the server it runs on. Deploy it first", m.GetApp()))
	}
	for _, s := range app.servers {
		if !slices.Contains(c.Servers, s) {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s runs on %s, and %s doesn't run there. An app connects to a database on its own server for now", m.GetApp(), human.List(c.ServerNames), c.Service), "other_server")
		}
	}
	plan, err := h.connectPlan(ctx, m, link, eng, c, app)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	summary := fmt.Sprintf("Connect %s to %s, as %s on %s", m.GetApp(), c.Service, plan.user, plan.database)
	if plan.takenOn {
		summary = fmt.Sprintf("Connect %s to %s, taking on %s on %s", m.GetApp(), c.Service, plan.user, plan.database)
	}
	change, err := h.changes.Propose(ctx, summary, plan.edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ConnectDatabaseResponse{Change: change}), nil
}

// appEnv reads an app's environment, by its project and its own name or ID, which the
// caller holds a role in: one they don't reads as not there. app, where it's named, is
// a service that has to be there.
func (h *Handler) appEnv(ctx context.Context, appProject, appEnvironment, app string, c catalog.Candidate) (*appEnv, error) {
	out := &appEnv{services: map[string]*resourcev1.Service{}, variables: map[string]bool{}, secrets: map[string]*resourcev1.Secret{}, revisions: map[string]int{}}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		project := cmp.Or(appProject, c.ProjectID)
		caller, hasCaller := place.CallerFrom(ctx)
		var envs []scope.Environment
		for _, p := range tree.Projects {
			if project != p.ID && project != p.Name {
				continue
			}
			if hasCaller && place.Holds(caller.Grants, place.Place{Project: p.ID}) == "" {
				continue
			}
			out.project, out.projectID = p.Name, p.ID
			for _, e := range p.Environments {
				if appEnvironment == "" || appEnvironment == e.ID || appEnvironment == e.Name {
					envs = append(envs, e)
				}
			}
		}
		switch {
		case out.project == "":
			return invalidField("app_project", fmt.Errorf("there's no project %s", project))
		case len(envs) == 0:
			return invalidField("app_environment", fmt.Errorf("%s has no environment %s", out.project, appEnvironment))
		case len(envs) > 1:
			return invalidField("app_environment", fmt.Errorf("%s has more than one environment. Say which the app is in", out.project))
		}
		out.id, out.name = envs[0].ID, envs[0].Name
		for _, kind := range []string{string(service.Kind), string(variable.VariableKind), string(variable.SecretKind)} {
			rs, err := tx.ListResources(ctx, out.id, kind)
			if err != nil {
				return err
			}
			stored := make([]store.StoredSpec, 0, len(rs))
			live := make([]store.Resource, 0, len(rs))
			for _, r := range rs {
				if r.Spec != nil {
					stored = append(stored, store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec})
					live = append(live, r)
				}
			}
			decoded, _, err := h.keeper.DecodeEach(ctx, tx, stored)
			if err != nil {
				return err
			}
			for i, spec := range decoded {
				switch s := spec.(type) {
				case *resourcev1.Service:
					out.services[live[i].Name] = s
					if live[i].Name == app {
						deps, err := tx.LatestDeployments(ctx, []string{live[i].ID})
						if err != nil {
							return err
						}
						for j := range deps[live[i].ID] {
							d := &deps[live[i].ID][j]
							if d.State != store.DeploymentSuperseded && !slices.Contains(out.servers, d.Server) {
								out.servers = append(out.servers, d.Server)
							}
						}
					}
				case *resourcev1.Variable:
					out.variables[live[i].Name] = true
				case *resourcev1.Secret:
					out.secrets[live[i].Name] = s
					out.revisions[live[i].Name] = live[i].Revision
				}
			}
		}
		if _, ok := out.services[app]; !ok && app != "" {
			return invalidField("app", fmt.Errorf("%s/%s has no service %s", out.project, out.name, app))
		}
		return nil
	})
	return out, err
}

// connected is what connecting plans.
type connected struct {
	edits          []*dockwellv1.Edit
	database, user string
	takenOn        bool
}

// connectPlan checks what the request asks of the server and the app, and plans it:
// on the server, the user's password and the user; in the app's environment, the
// link, the names it sets, and where the server is in another environment, the link
// network it joins in place, which the app's services that read the names join.
func (h *Handler) connectPlan(ctx context.Context, m *dockwellv1.ConnectDatabaseRequest, link string, eng catalogv1.Engine, c catalog.Candidate, app *appEnv) (*connected, error) {
	across := c.EnvironmentID != app.id
	need := &catalogv1.Need{Name: link, Label: "Database", Accepts: []*catalogv1.NeedAccept{{Engine: eng}}}
	password := ""
	for _, name := range slices.Sorted(maps.Keys(m.GetSets())) {
		part := catalogv1.Part(m.GetSets()[name])
		set := &catalogv1.NeedSet{Name: name, Part: part, Template: m.GetTemplates()[name]}
		_, isSecret := app.secrets[name]
		switch {
		case !app.variables[name] && !isSecret:
			return nil, invalidField("sets", fmt.Errorf("%s/%s has no variable or secret %s. Pick one of the app's own", app.project, app.name, name))
		case part == catalogv1.Part_PART_UNSPECIFIED || part == catalogv1.Part_PART_TLS || part == catalogv1.Part_PART_KEY_PREFIX:
			return nil, invalidField("sets", fmt.Errorf("%s can't be set to the %s here: a link to %s sets a host, port, address, database, user, password or URL", name, strings.ToLower(strings.TrimPrefix(part.String(), "PART_")), catalog.EngineName(eng)))
		case catalog.SecretPart(set) && !isSecret:
			return nil, invalidField("sets", fmt.Errorf("%s is a variable, and what it would hold is secret. Keep it as a secret first", name))
		case part == catalogv1.Part_PART_PASSWORD && password != "":
			return nil, invalidField("sets", fmt.Errorf("%s and %s both hold the password. Set one", password, name))
		case set.GetTemplate() != "" && part != catalogv1.Part_PART_URL:
			return nil, invalidField("templates", fmt.Errorf("%s has a template, and only a URL is written from one", name))
		}
		if part == catalogv1.Part_PART_PASSWORD {
			password = name
		}
		need.Sets = append(need.Sets, set)
	}
	for name := range m.GetTemplates() {
		if _, ok := m.GetSets()[name]; !ok {
			return nil, invalidField("templates", fmt.Errorf("%s has a template, and the link doesn't set it", name))
		}
	}
	if password == "" && !slices.ContainsFunc(need.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_URL }) {
		return nil, invalidField("sets", errors.New("nothing the link sets holds the password, so the app couldn't sign in. Set its password or its URL"))
	}
	// The name the app's password is kept in: the one set, or one named for the link,
	// which a URL reads.
	password = catalog.PasswordName(need)
	_, isSecret := app.secrets[password]
	if _, set := m.GetSets()[password]; !set && (isSecret || app.variables[password]) {
		return nil, invalidField("sets", fmt.Errorf("%s/%s has %s already, which a URL the link writes would keep its password in. Set it as the password, or rename it", app.project, app.name, password))
	}

	out := &connected{}
	var plan []*dockwellv1.Edit
	err := h.db.View(ctx, func(tx *store.Tx) error {
		// What the server has, and what Dockwell made there, by name.
		dbs, users := slices.Clone(c.Databases), slices.Clone(c.Users)
		made, err := tx.ListResources(ctx, c.EnvironmentID, string(database.UserKind))
		if err != nil {
			return err
		}
		mine := map[string]*resourcev1.DatabaseUser{}
		for _, r := range made {
			u := &resourcev1.DatabaseUser{}
			if r.Spec == nil || proto.Unmarshal(r.Spec, u) != nil {
				continue
			}
			mine[r.Name] = u
			users = append(users, r.Name)
			dbs = append(dbs, database.Owned(r.Name, u))
		}
		out.database, out.user = m.GetDatabase(), m.GetUser()
		out.takenOn = out.database != "" && slices.Contains(c.Databases, out.database)
		if out.takenOn {
			kept, err := h.keptDatabases(ctx, tx, c, mine)
			if err != nil {
				return err
			}
			switch {
			case kept[out.database]:
				return invalidField("database", fmt.Errorf("%s on %s is kept for another app, whose dumps and restores go by it. Pick the app's own database, or connect a new one", out.database, c.Service))
			case database.SystemDatabase(out.database):
				return invalidField("database", fmt.Errorf("%s is a database %s keeps for itself. Pick the app's own", out.database, c.Service))
			case out.user == "":
				return invalidField("user", fmt.Errorf("say which of %s's users the app signs in to %s as", c.Service, out.database))
			case database.ReservedUser(out.user):
				return invalidField("user", fmt.Errorf("%s is %s's administrator, which reaches every app's data. Connect the app with a user of its own", out.user, c.Service))
			case !slices.Contains(c.Users, out.user):
				return invalidField("user", fmt.Errorf("%s has no user %s, as it says of itself", c.Service, out.user))
			case mine[out.user] != nil:
				return invalidField("user", fmt.Errorf("%s is a user Dockwell keeps on %s already, which another app's link reads. Connect with a user of the app's own", out.user, c.Service))
			}
			if m.GetPasswordSecret() == "" {
				return invalidField("password_secret", fmt.Errorf("say which of the app's secrets holds %s's password, as it signs in now", out.user))
			}
			if s, ok := app.secrets[m.GetPasswordSecret()]; !ok || s.GetFrom() != nil {
				return invalidField("password_secret", fmt.Errorf("%s/%s has no secret %s that holds a password of its own", app.project, app.name, m.GetPasswordSecret()))
			}
		} else {
			if m.GetPasswordSecret() != "" {
				return invalidField("password_secret", errors.New("a new database's user gets a password Dockwell makes, so there's none to read. Leave it out, or name a database the server has"))
			}
			out.database = cmp.Or(out.database, freeName(sqlName(m.GetApp()), dbs, users))
			out.user = cmp.Or(out.user, out.database)
			if err := database.CheckSQLName("database", out.database); err != nil {
				return invalidField("database", err)
			}
			if err := database.CheckSQLName("user", out.user); err != nil {
				return invalidField("user", err)
			}
			switch {
			case database.SystemDatabase(out.database):
				return invalidField("database", fmt.Errorf("%s is a database %s keeps for itself. Name the app's own", out.database, c.Service))
			case database.ReservedUser(out.user):
				return invalidField("user", fmt.Errorf("%s is a name %s keeps for itself. Name the app's user another way", out.user, c.Service))
			case slices.Contains(users, out.user):
				return invalidField("user", fmt.Errorf("%s has a user %s already. Name the app's another way, or connect to the database it has", c.Service, out.user))
			case slices.Contains(dbs, out.database):
				return invalidField("database", fmt.Errorf("%s has a database %s already, which Dockwell made for another app", c.Service, out.database))
			}
		}
		if _, err := tx.GetResource(ctx, "link", app.id, link); err == nil {
			return invalidField("link", fmt.Errorf("%s/%s has a link %s already. Name this one another way", app.project, app.name, link))
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		// The user's password: in the app's environment beside it, or across, in a
		// secret of the server's own that the app's reads through the link.
		secret := password
		if out.takenOn && !across {
			secret = m.GetPasswordSecret()
		}
		if across {
			if secret, err = freeSecret(ctx, tx, c.EnvironmentID, strings.ToUpper(out.user)+"_DATABASE_PASSWORD"); err != nil {
				return err
			}
		}
		value := ""
		switch {
		case out.takenOn:
			value = app.secrets[m.GetPasswordSecret()].GetValue()
		default:
			if value, err = generate(passwordLength); err != nil {
				return err
			}
		}

		alias := c.Service
		if across {
			if _, ok := app.services[alias]; ok {
				alias = link + "-link"
			}
		}
		w := catalog.Way{Need: need, Engine: eng, Version: c.Version, Existing: &c, Database: out.database, User: out.user, Password: secret, Alias: alias}
		taken := slices.Concat(slices.Collect(maps.Keys(app.secrets)), slices.Collect(maps.Keys(app.variables)))
		got, err := catalog.Connect(m.GetApp(), need, w, taken)
		if err != nil {
			return invalidField("sets", err)
		}
		got.Link.Service = c.Service
		if across {
			got.Link.MeetsEnvironment = c.EnvironmentID
		}

		// On the server.
		user := &resourcev1.DatabaseUser{Service: c.Service, Password: secret, TakenOn: out.takenOn}
		if out.database != out.user {
			user.DatabaseName = out.database
		}
		on := &dockwellv1.ResourceRef{Kind: string(database.UserKind), Scope: c.EnvironmentID, Name: out.user}
		if across || !out.takenOn {
			plan = append(plan, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: c.EnvironmentID, Name: secret},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}},
			})
		}
		plan = append(plan,
			&dockwellv1.Edit{Resource: on, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: user}}, Servers: slices.Clone(c.Servers)},
			&dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: app.id, Name: link},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: got.Link}},
			},
		)

		// In the app's environment: the password, read from the user's where it's
		// across, the URLs written from the parts, and the plain parts.
		if across {
			plan = append(plan, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: app.id, Name: password},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
					Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: c.EnvironmentID, Secret: true, Name: secret, Link: link}},
				}}}},
			})
		} else if password != secret {
			return invalidField("password_secret", fmt.Errorf("the app reads its password from %s, and %s is where the user's is. In one environment they're the same secret: set %s as the password, or pick %s here", password, secret, secret, password))
		}
		for _, name := range slices.Sorted(maps.Keys(got.Written)) {
			plan = append(plan, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: app.id, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: got.Written[name]}}}}},
			})
		}
		for _, name := range slices.Sorted(maps.Keys(got.Plain)) {
			plan = append(plan, plainEdit(app, name, got.Plain[name]))
		}
		for _, name := range slices.Sorted(maps.Keys(got.Added)) {
			plan = append(plan, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(variable.VariableKind), Scope: app.id, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: got.Added[name]}}},
			})
		}
		if !across {
			return nil
		}

		// Across, the server answers on a network of the app's own to the name the app
		// reads, and each of the app's services that reads one of the names joins it.
		subnet, err := h.linkSubnet(ctx, tx)
		if err != nil {
			return err
		}
		net := link + "-link"
		if _, err := tx.GetResource(ctx, string(network.Kind), app.id, net); err == nil {
			return invalidField("link", fmt.Errorf("%s/%s has a network %s already, which the link would make. Name the link another way", app.project, app.name, net))
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		plan = append(plan, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(network.Kind), Scope: app.id, Name: net},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Network{Network: &resourcev1.Network{
				Internal: true, Subnet: subnet,
				Members: []*resourcev1.NetworkMember{{Environment: c.EnvironmentID, Service: c.Service, Alias: alias}},
			}}},
			Servers: slices.Clone(c.Servers),
		})
		names := slices.Concat(slices.Collect(maps.Keys(m.GetSets())), []string{password})
		for _, name := range slices.Sorted(maps.Keys(app.services)) {
			s := app.services[name]
			if s.GetApp() == nil || !readsAny(s, names) || slices.Contains(s.GetApp().GetNetworks(), net) {
				continue
			}
			s = proto.CloneOf(s)
			s.Revisions = nil
			s.GetApp().Networks = append(s.GetApp().GetNetworks(), net)
			plan = append(plan, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: app.id, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: s}},
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.edits = plan
	return out, nil
}

// plainEdit sets a name the app has to a plain part's value, as the kind it is: a
// secret stays a secret, holding it.
func plainEdit(app *appEnv, name, value string) *dockwellv1.Edit {
	if _, ok := app.secrets[name]; ok {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: app.id, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}},
		}
	}
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(variable.VariableKind), Scope: app.id, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: value}}},
	}
}

// readsAny reports a service that uses one of the environment's names.
func readsAny(s *resourcev1.Service, names []string) bool {
	for _, uses := range []map[string]string{s.GetVariables(), s.GetSecrets()} {
		for as, name := range uses {
			if slices.Contains(names, cmp.Or(name, as)) {
				return true
			}
		}
	}
	return false
}

// sqlName is a database's name made from an app's: lowercase letters, digits and
// underscores, starting with a letter, at most 32.
func sqlName(app string) string {
	b := []byte(strings.ToLower(app))
	for i, ch := range b {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			b[i] = '_'
		}
	}
	name := strings.Trim(string(b), "_")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "app_" + name
	}
	return strings.TrimRight(name[:min(len(name), 28)], "_")
}

// freeName is base, or base_2 and on, whichever no database or user has.
func freeName(base string, dbs, users []string) string {
	name := base
	for i := 2; slices.Contains(dbs, name) || slices.Contains(users, name) || database.SystemDatabase(name) || database.ReservedUser(name); i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}

// freeSecret is name, or name_2 and on, whichever no secret or variable of the
// environment has.
func freeSecret(ctx context.Context, tx *store.Tx, env, name string) (string, error) {
	for i := 2; ; i++ {
		taken := false
		for _, kind := range []resource.Kind{variable.SecretKind, variable.VariableKind} {
			_, err := tx.GetResource(ctx, string(kind), env, name)
			switch {
			case err == nil:
				taken = true
			case !errors.Is(err, store.ErrNotFound):
				return "", err
			}
		}
		if !taken {
			return name, nil
		}
		name = fmt.Sprintf("%s_%d", strings.TrimSuffix(name, fmt.Sprintf("_%d", i-1)), i)
	}
}

// linkSubnet is a /29 for a link network that no network on record has.
func (h *Handler) linkSubnet(ctx context.Context, tx *store.Tx) (string, error) {
	nets, err := tx.ListKind(ctx, string(network.Kind))
	if err != nil {
		return "", err
	}
	stored := make([]store.StoredSpec, 0, len(nets))
	for _, r := range nets {
		if r.Spec != nil {
			stored = append(stored, store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec})
		}
	}
	decoded, _, err := h.keeper.DecodeEach(ctx, tx, stored)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, spec := range decoded {
		if n, ok := spec.(*resourcev1.Network); ok && n.GetSubnet() != "" {
			taken[n.GetSubnet()] = true
		}
	}
	if p := network.FreeLinkSubnet(taken); p != "" {
		return p, nil
	}
	return "", authapi.ReasonError(connect.CodeResourceExhausted,
		errors.New("every network Dockwell keeps addresses for links is taken. Remove a link you don't use, or connect a database in the app's own environment"), "no_link_network")
}

// keptDatabases are the databases on a server that are kept for an app already: one
// a user Dockwell keeps there owns, or one a link reads, as a catalog install's own
// does through the user its image made. A taken-on link can't name one, as a link's
// dumps and restores go by its database, as its owner (ADR 0084).
func (h *Handler) keptDatabases(ctx context.Context, tx *store.Tx, c catalog.Candidate, mine map[string]*resourcev1.DatabaseUser) (map[string]bool, error) {
	out := map[string]bool{}
	for name, u := range mine {
		out[database.Owned(name, u)] = true
	}
	links, err := tx.ListKind(ctx, "link")
	if err != nil {
		return nil, err
	}
	stored := make([]store.StoredSpec, 0, len(links))
	live := make([]store.Resource, 0, len(links))
	for _, r := range links {
		if r.Spec != nil {
			stored = append(stored, store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec})
			live = append(live, r)
		}
	}
	decoded, _, err := h.keeper.DecodeEach(ctx, tx, stored)
	if err != nil {
		return nil, err
	}
	for i, spec := range decoded {
		l, ok := spec.(*resourcev1.Link)
		if ok && l.GetService() == c.Service && cmp.Or(l.GetMeetsEnvironment(), live[i].Scope) == c.EnvironmentID && l.GetDatabaseName() != "" {
			out[l.GetDatabaseName()] = true
		}
	}
	return out, nil
}
