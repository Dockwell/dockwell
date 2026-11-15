// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/store"
)

// CheckDatabaseSignIn asks the database's server whether a user signs in with the
// password an app's secret holds, from another host, as connecting the app takes the
// user on: the sheet asks as each is chosen, so no one learns at apply that the
// password is wrong (ADR 0084, Connect a database). The password goes sealed to the
// server's agent and nowhere else; nothing this answers, logs or refuses holds it.
func (h *Handler) CheckDatabaseSignIn(ctx context.Context, req *connect.Request[dockwellv1.CheckDatabaseSignInRequest]) (*connect.Response[dockwellv1.CheckDatabaseSignInResponse], error) {
	m := req.Msg
	switch {
	case m.GetId() == "":
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	case m.GetPasswordSecret() == "":
		return nil, invalidField("password_secret", errors.New("say which of the app's secrets holds the password"))
	}
	if err := database.CheckSQLName("user", m.GetUser()); err != nil {
		return nil, invalidField("user", err)
	}
	if err := database.CheckSQLName("database", m.GetDatabase()); err != nil {
		return nil, invalidField("database", err)
	}
	// A name the server keeps for its administrator is one, whatever it signs in with,
	// and no password goes anywhere to say so.
	if database.ReservedUser(m.GetUser()) {
		return connect.NewResponse(&dockwellv1.CheckDatabaseSignInResponse{Administrator: true}), nil
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	server, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if _, ok := connectable[server.db.GetEngine()]; !ok {
		return nil, authapi.Invalid(fmt.Errorf("%s runs %s, and an app connects to PostgreSQL, MySQL or MariaDB for now", server.ref.Name, database.Of(server.db.GetEngine()).Name))
	}
	if len(server.servers) == 0 {
		return nil, authapi.Invalid(fmt.Errorf("%s runs on no server now, so nothing can sign in to it. Deploy it first", server.ref.Name))
	}
	serverProject, err := h.projectOf(ctx, server.ref.Scope)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	c := catalog.Candidate{ProjectID: serverProject, EnvironmentID: server.ref.Scope}
	app, err := h.appEnv(ctx, m.GetAppProject(), m.GetAppEnvironment(), "", c)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	// The check uses the app's secret against the server: a developer's role in both,
	// as connecting takes, so it's no way to try a password where either isn't yours.
	if caller, ok := place.CallerFrom(ctx); ok {
		for _, at := range []place.Place{{Project: app.projectID, Environment: app.id}, {Project: serverProject, Environment: server.ref.Scope}} {
			if store.RoleRank(place.Holds(caller.Grants, at)) < store.RoleRank(store.RoleDeveloper) {
				return nil, h.fail(ctx, notFound(m.GetId()))
			}
		}
	}
	secret, ok := app.secrets[m.GetPasswordSecret()]
	if !ok || secret.GetFrom() != nil || secret.GetValue() == "" {
		return nil, invalidField("password_secret", fmt.Errorf("%s/%s has no secret %s that holds a password of its own", app.project, app.name, m.GetPasswordSecret()))
	}
	asked := fmt.Sprintf("%s %s %s %s %s@%d", server.db.GetId(), m.GetUser(), m.GetDatabase(), app.id, m.GetPasswordSecret(), app.revisions[m.GetPasswordSecret()])
	h.signInMu.Lock()
	if a, ok := h.signIns[asked]; ok && h.now().Sub(a.at) < signInKeep {
		h.signInMu.Unlock()
		return connect.NewResponse(a.res), nil
	}
	// Past the cache, a person's checks are held to signInsPerMinute.
	if caller, ok := place.CallerFrom(ctx); ok {
		now := h.now()
		recent := slices.DeleteFunc(h.signInsBy[caller.UserID], func(at time.Time) bool { return now.Sub(at) >= time.Minute })
		if len(recent) >= signInsPerMinute {
			h.signInsBy[caller.UserID] = recent
			h.signInMu.Unlock()
			return nil, authapi.ReasonError(connect.CodeResourceExhausted,
				fmt.Errorf("you've checked %d sign-ins in the last minute, as many as one person may. Try again in a minute", signInsPerMinute), "rate_limited")
		}
		h.signInsBy[caller.UserID] = append(recent, now)
	}
	h.signInMu.Unlock()
	on := server.servers[0]
	session, ok := h.agents.Session(on.ID)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s isn't connected, so nothing can sign in to %s now. Try again once it reconnects", on.Name, server.ref.Name), "unavailable")
	}
	said, err := session.CheckDatabaseSignIn(ctx, server.ref.Scope, server.ref.Name, m.GetUser(), m.GetDatabase(), secret.GetValue())
	if errors.Is(err, agentapi.ErrOlderAgent) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to check a sign-in. Update the agent on %s; the plan still checks it as it applies", on.Name, on.Name), "agent_outdated")
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, connect.NewError(connect.CodeCanceled, err)
		}
		h.logger.WarnContext(ctx, "a database sign-in couldn't be checked", "server", on.Name, "database", server.ref.Name, "error", err.Error())
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s couldn't check the sign-in now. The plan checks it again as it applies", on.Name), "unavailable")
	}
	res := &dockwellv1.CheckDatabaseSignInResponse{SignsIn: said.GetSignsIn(), Said: said.GetSaid(), Administrator: said.GetAdministrator(), NoRights: said.GetNoRights()}
	h.signInMu.Lock()
	for k, a := range h.signIns {
		if h.now().Sub(a.at) >= signInKeep {
			delete(h.signIns, k)
		}
	}
	h.signIns[asked] = signInAnswer{res: res, at: h.now()}
	h.signInMu.Unlock()
	return connect.NewResponse(res), nil
}

// projectOf is the ID of the project an environment is in.
func (h *Handler) projectOf(ctx context.Context, environment string) (string, error) {
	var project string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		env, err := tx.GetResourceByID(ctx, environment)
		project = env.Scope
		return err
	})
	return project, err
}
