// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// gated is an interceptor on a store where usr_ada holds role on the install,
// or nothing when role is "", with Ada as a principal.
func gated(t *testing.T, role string) (*interceptor, auth.Principal, *store.DB) {
	t.Helper()
	return gatedOn(t, store.SQLite, role)
}

// gatedOn is gated on the store given.
func gatedOn(t *testing.T, backend store.Backend, role string) (*interceptor, auth.Principal, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, backend)
	key, err := seal.LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	svc, err := auth.New(auth.Config{DB: db, Key: key, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ada := store.User{ID: "usr_ada", Email: "ada@example.com", Name: "Ada", WebAuthnHandle: []byte("a"), CreatedAt: now}
	err = db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateUser(t.Context(), store.User{ID: "usr_mira", Email: "mira@example.com", Name: "Mira", Role: store.RoleOwner, WebAuthnHandle: []byte("m"), CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.CreateUser(t.Context(), ada); err != nil {
			return err
		}
		if role == "" {
			return nil
		}
		return tx.SetGrant(t.Context(), store.Grant{UserID: ada.ID, ProjectID: store.Install, Role: role, CreatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	return &interceptor{svc: svc, logger: logger}, auth.Principal{User: ada}, db
}

func reasonOf(err error) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return ""
	}
	return cerr.Meta().Get(ReasonKey)
}

// A call nothing declares is the owner's: a method outside dockwell.v1, such
// as a later version's, or one of reflection's beside the two it answers. Only
// gRPC's health and reflection need no role (ADR 0049).
func TestACallNothingDeclaresIsTheOwners(t *testing.T) {
	i, admin, _ := gated(t, store.RoleAdmin)
	for _, procedure := range []string{
		"/dockwell.v2.ServerService/ListServers",
		"/dockwell.v1.ServerService/MadeUp",
		"/grpc.reflection.v1.ServerReflection/Other",
		"/other.Service/Call",
	} {
		_, err := i.gate(t.Context(), admin, procedure)
		if reasonOf(err) != ReasonNeedsRole {
			t.Errorf("an admin calling %s: %v, want needs_role, as it's the owner's", procedure, err)
		}
	}

	i, nobody, _ := gated(t, "")
	for _, procedure := range []string{
		"/" + grpchealth.HealthV1ServiceName + "/Check",
		"/" + grpchealth.HealthV1ServiceName + "/Watch",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
		"/" + dockwellv1connect.AuthServiceName + "/SignOut",
	} {
		if _, err := i.gate(t.Context(), nobody, procedure); err != nil {
			t.Errorf("someone with no role calling %s: %v, want it to go ahead", procedure, err)
		}
	}
}

// When the grants can't be read, nothing that needs a role goes ahead.
func TestTheGateFailsClosed(t *testing.T) {
	i, owner, db := gated(t, store.RoleAdmin)
	if _, err := i.gate(t.Context(), owner, "/"+dockwellv1connect.AuditServiceName+"/ListAuditEvents"); err != nil {
		t.Fatalf("an admin reading the audit log: %v", err)
	}
	_ = db.Close()
	_, err := i.gate(t.Context(), owner, "/"+dockwellv1connect.AuditServiceName+"/ListAuditEvents")
	if connect.CodeOf(err) != connect.CodeUnavailable || reasonOf(err) != ReasonUnavailable {
		t.Errorf("reading the audit log with the grants unread: %v, want unavailable", err)
	}
	if _, err := i.gate(t.Context(), owner, "/"+dockwellv1connect.AuthServiceName+"/SignOut"); err != nil {
		t.Errorf("signing out needs no role, so no grants: %v", err)
	}
}

// fakeStream is a stream of procedure, for what reads only its Spec.
type fakeStream struct {
	connect.StreamingHandlerConn
	procedure string
}

func (f fakeStream) Spec() connect.Spec { return connect.Spec{Procedure: f.procedure} }

// A stream's recheck rides out a failed read of its caller's grants, as the
// session's own check does, and ends the stream only once reads fail several
// times in a row.
func TestAStreamRidesOutAFailedGrantsRead(t *testing.T) {
	i, ada, db := gated(t, store.RoleViewer)
	ctx := t.Context()
	c := &scopedConn{StreamingHandlerConn: fakeStream{procedure: "/" + dockwellv1connect.EventServiceName + "/WatchEvents"}, i: i, ctx: ctx}
	if err := c.recheck(ctx, ada); err != nil {
		t.Fatalf("a recheck that reads the grants: %v", err)
	}
	_ = db.Close()
	for n := 1; n < failedRechecks; n++ {
		if err := c.recheck(ctx, ada); err != nil {
			t.Fatalf("failed read %d ended the stream: %v", n, err)
		}
	}
	if err := c.recheck(ctx, ada); connect.CodeOf(err) != connect.CodeUnavailable || c.endedBy() == nil {
		t.Errorf("failed read %d: %v, want the stream ended as unavailable", failedRechecks, err)
	}
}

// The failed reads that end a stream are in a row: one that works starts the
// count again.
func TestAStreamsFailedReadsCountInARow(t *testing.T) {
	i, ada, db := gated(t, store.RoleViewer)
	ctx := t.Context()
	c := &scopedConn{StreamingHandlerConn: fakeStream{procedure: "/" + dockwellv1connect.EventServiceName + "/WatchEvents"}, i: i, ctx: ctx}
	c.failed = failedRechecks - 1 // one short of ending it
	if err := c.recheck(ctx, ada); err != nil {
		t.Fatalf("a recheck that reads the grants: %v", err)
	}
	_ = db.Close()
	for n := 1; n < failedRechecks; n++ {
		if err := c.recheck(ctx, ada); err != nil {
			t.Fatalf("failed read %d since one that worked ended the stream: %v", n, err)
		}
	}
}

// Check holds a token, as a deploy-on-push hook's is, to what the API holds one
// to: no call that needs a session, whatever its maker's role; and a call a token
// may make is checked by the role (from #253's review, L6).
func TestCheckHoldsATokenToWhatTheAPIDoes(t *testing.T) {
	i, ada, _ := gated(t, store.RoleAdmin)
	hook := ada
	hook.Token = &store.APIToken{ID: "ghk_1", UserID: ada.User.ID, Name: "Deploy on push", Scope: store.ScopeReadWrite}
	ctx := As(t.Context(), auth.Client{Addr: "203.0.113.5:1"}, hook)
	// Making a hook needs a session: a token can't, though its maker may.
	if _, err := Check(ctx, i.svc, i.logger, dockwellv1connect.GitHookServiceCreateGitHookProcedure, nil); reasonOf(err) != ReasonNeedsSession {
		t.Errorf("a token making a hook through Check: %v, want %s", err, ReasonNeedsSession)
	}
	// Planning a change is a write a token may make, as its admin maker may.
	if _, err := Check(ctx, i.svc, i.logger, dockwellv1connect.ChangeServiceProposeChangeProcedure, nil); err != nil {
		t.Errorf("a token planning through Check: %v", err)
	}
}
