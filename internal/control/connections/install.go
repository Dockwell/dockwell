// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
)

// CheckBeforeLinking checks a connection's bucket under prefix before a catalog
// install links an app to it, from the agent of server, where the app is to run, on
// a network made for the check, whose bridge has no address on the server (ADR
// 0099). It tries presigned URLs where presign asks.
//
// The saved keys go to that server for an install admin, as the plan they're about to
// propose sends them there with the app's values. Below one, and for Dockwell's own
// change, as an automatic update, they go only where #710 lets them, a server an app
// linked to the connection runs on, or else the control server's agent checks it.
//
// It refuses, in the check's words, a bucket that fails the check. checked is false,
// with no error, where nothing can check it now, as an agent that's away or older
// than checks: the plan then says it isn't checked.
//
// For an app that runs already, env and app name its environment and service: the
// check runs on its networks, as Connect storage's does, so a bucket it reaches on
// them, as one beside it, checks as the app reaches it (slice 2 review, M4).
func (s *Service) CheckBeforeLinking(ctx context.Context, name, server, env, app, prefix string, presign bool) (checked bool, err error) {
	saved, backupUsed, ok, err := s.connections.ForLinking(ctx, name, "")
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if backupUsed {
		return false, authapi.Invalid(fmt.Errorf("the connection %s is used for backups, so no app can link to it: an app's key could delete the backups. Add a separate S3 connection for the app", name))
	}
	spec := saved.Spec
	srv, err := s.server(server)
	if err != nil {
		return false, nil //nolint:nilerr // a server that isn't there to check from plans unchecked
	}
	who := "the app's server"
	// Dockwell's own change, as an automatic update to a release that adds the need,
	// counts as below an admin: no person chose the connection (slice 2 re-check, N3).
	_, person := place.CallerFrom(ctx)
	if admin, _ := callerProjects(ctx); (!admin || !person) && !srv.GetControlServer() {
		runs, err := s.connections.ServersOf(ctx, name)
		if err != nil {
			return false, err
		}
		if !runs[srv.GetId()] {
			// The control server's agent holds the keys already, so it checks from a
			// network of the check's own, and its words say it was the one.
			if srv = s.controlServer(); srv == nil {
				return false, nil
			}
			env, app, who = "", "", "the control server"
		}
	}
	session, srv, err := s.session(ctx, srv.GetId())
	if err != nil {
		return false, nil //nolint:nilerr // an agent that's away plans unchecked
	}
	got, err := session.ProbeConnection(ctx, env, app, probeOf(spec, prefix, presign))
	if err != nil {
		// An agent older than checks, busy, or gone mid-check: the plan says it isn't
		// checked, and the person can check it on the connection's page.
		return false, nil //nolint:nilerr // a check that can't run now plans unchecked, and says so
	}
	if !got.GetPassed() {
		check := &dockwellv1.CheckConnectionResponse{
			Passed: got.GetPassed(), Step: got.GetStep(), Code: got.GetCode(), Status: got.GetStatus(),
			Message: got.GetMessage(), Words: got.GetWords(), Server: srv.GetName(),
		}
		return false, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s. Nothing was planned, as %s checked the connection %s first. %s", checkWordsFrom(check, who), srv.GetName(), name, checkFixFrom(check, who)), "check_failed")
	}
	return true, nil
}

// controlServer is the control server's own entry in the fleet, or nil.
func (s *Service) controlServer() *dockwellv1.Server {
	for _, srv := range s.fleet.List() {
		if srv.GetControlServer() {
			return srv
		}
	}
	return nil
}
