// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A client that went away while an agent planned is a cancelled call, not
// an agent that failed; an agent that ran out of time is a timeout.
func TestACancelledCallIsntAnAgentFailure(t *testing.T) {
	h := &Handler{logger: slog.New(slog.DiscardHandler)}
	err := h.rpcError(t.Context(), &revision.AgentError{Server: "hz-1", Doing: "plan", Err: context.Canceled})
	if connect.CodeOf(err) != connect.CodeCanceled {
		t.Errorf("a plan its client cancelled = %v, want canceled", err)
	}
	err = h.rpcError(t.Context(), &revision.AgentError{
		Server: "hz-1", Doing: "plan",
		Err: fmt.Errorf("hz-1's agent didn't answer within 1 minute: %w", context.DeadlineExceeded),
	})
	var cerr *connect.Error
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded || !asConnect(err, &cerr) || cerr.Meta().Get(authapi.ReasonKey) != "agent_timeout" {
		t.Errorf("a plan an agent didn't answer in time = %v, want agent_timeout", err)
	}
}

func asConnect(err error, target **connect.Error) bool {
	c, ok := err.(*connect.Error) //nolint:errorlint // rpcError returns it unwrapped
	*target = c
	return ok
}

// A project is resolved as scope.Tree resolves it: with none, there's no
// choice to make, only nothing to find.
func TestAProjectResolvesAsTheScopeTreeDoes(t *testing.T) {
	_, err := resolve(t.Context(), nil, &scope.Tree{}, resource.LevelProject, "", "")
	if !errors.Is(err, scope.ErrNotFound) {
		t.Errorf("the only project of none = %v, want scope.ErrNotFound", err)
	}
	tree := &scope.Tree{Projects: []scope.Project{{ID: "prj_1", Name: "default"}, {ID: "prj_2", Name: "shop"}}}
	if id, err := resolve(t.Context(), nil, tree, resource.LevelProject, "", "shop"); err != nil || id != "prj_2" {
		t.Errorf("project shop = %s, %v", id, err)
	}
	var choice *scope.ChoiceError
	if _, err := resolve(t.Context(), nil, tree, resource.LevelProject, "", ""); !errors.As(err, &choice) {
		t.Errorf("the only project of two = %v, want a choice", err)
	}
}

// What a server did with a confirmation reaches whoever sent it: kept is no
// error; a change whose time ran out first rolled back, and says so; nothing
// waiting is said apart.
func TestAConfirmationSaysWhatTheServerDid(t *testing.T) {
	if err := verdictError("chg_1", resource.AccessKept); err != nil {
		t.Errorf("kept: %v", err)
	}
	for v, want := range map[resource.AccessVerdict]string{
		resource.AccessRolledBack: "rolled_back",
		resource.AccessNotWaiting: "not_waiting",
		0:                         "not_waiting",
	} {
		err := verdictError("chg_1", v)
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition || cerr.Meta().Get(authapi.ReasonKey) != want {
			t.Errorf("verdict %v: %v, want %s", v, err, want)
		}
	}
}

func TestAServerToRunOnIsNamedByNameOrID(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		h, _, _, db := engineWith(t, b, 0)
		now := time.Now()
		// web-1 was removed, and a server of the same name joined after it.
		if err := db.Update(t.Context(), func(tx *store.Tx) error {
			for _, s := range []store.Server{
				{ID: "srv_old", Name: "web-1", CreatedAt: now, CertSerial: "old", CertNotAfter: now},
				{ID: "srv_web1", Name: "web-1", CreatedAt: now, CertSerial: "web1", CertNotAfter: now},
				{ID: "srv_web2", Name: "web-2", CreatedAt: now, CertSerial: "web2", CertNotAfter: now},
			} {
				if err := tx.CreateServer(t.Context(), s); err != nil {
					return err
				}
				if s.ID == "srv_old" {
					if err := tx.RevokeServer(t.Context(), s.ID, now); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		app := &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}}}
		on := func(servers ...string) ([]string, error) {
			out, err := h.edits(t.Context(), []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Scope: "production", Name: "web"}, Spec: app, Servers: servers}})
			if err != nil {
				return nil, err
			}
			return out[0].Servers, nil
		}
		for _, c := range []struct {
			given []string
			want  []string
		}{
			{[]string{"web-1"}, []string{"srv_web1"}},
			{[]string{"srv_web2", "web-1"}, []string{"srv_web2", "srv_web1"}},
			// An ID goes as it is, so planning says its server was removed.
			{[]string{"srv_old"}, []string{"srv_old"}},
			{nil, nil},
		} {
			if got, err := on(c.given...); err != nil || !slices.Equal(got, c.want) {
				t.Errorf("--on %v = %v, %v, want %v", c.given, got, err, c.want)
			}
		}
		_, err := on("web-9")
		if !errors.Is(err, scope.ErrNotFound) || err.Error() != "there's no server web-9 in the fleet. Choose web-1 or web-2, by name or ID" {
			t.Errorf("--on web-9 = %v, want no server web-9, with web-1 or web-2 to choose", err)
		}
	})
}
