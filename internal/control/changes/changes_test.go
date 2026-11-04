// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
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
