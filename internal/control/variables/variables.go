// SPDX-License-Identifier: AGPL-3.0-only

// Package variables serves dockwell.v1.VariableService: an environment's
// variables and secrets, and revealing a secret's value to a person who
// confirms it with a passkey. Changing them is ChangeService's. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
package variables

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes for ListVariables.
const (
	DefaultPageSize = 500
	MaxPageSize     = 1000
)

// valueKinds are what ListVariables lists, as one list by name.
var valueKinds = []string{string(variable.VariableKind), string(variable.SecretKind)}

// Handler implements VariableService.
type Handler struct {
	kinds  *resource.Registry
	db     *store.DB
	keeper *secrets.Keeper
	auth   *auth.Service
	logger *slog.Logger
}

var _ dockwellv1connect.VariableServiceHandler = (*Handler)(nil)

// New returns a handler that reads db with kinds, opens specs with keeper,
// and checks step-ups with people.
func New(kinds *resource.Registry, db *store.DB, keeper *secrets.Keeper, people *auth.Service, logger *slog.Logger) *Handler {
	return &Handler{kinds: kinds, db: db, keeper: keeper, auth: people, logger: logger}
}

type usedKey struct {
	kind resource.Kind
	name string
}

// ListVariables returns an environment's variables and secrets, by name, a
// page at a time. It opens no sealed spec: a variable's spec is never
// sealed, and what each service uses is kept in the clear beside it, by
// reference.
func (h *Handler) ListVariables(ctx context.Context, req *connect.Request[dockwellv1.ListVariablesRequest]) (*connect.Response[dockwellv1.ListVariablesResponse], error) {
	size, err := paging.Size(req.Msg.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	cursor, paged, err := paging.Read(req.Msg.GetPageToken(), "ListVariables")
	if err != nil {
		return nil, err
	}
	res := &dockwellv1.ListVariablesResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		_, env, err := tree.Resolve(req.Msg.GetProject(), req.Msg.GetEnvironment())
		if err != nil {
			return err
		}
		res.EnvironmentId = env.ID
		var after store.NamedResource
		if paged {
			last, err := tx.GetResourceByID(ctx, cursor.ID)
			if err != nil || last.Scope != env.ID {
				return authapi.Invalid(errors.New("page_token isn't one ListVariables gave for this environment. " +
					"Pass next_page_token from the page before, with the same environment, or leave it out to start with the first page"))
			}
			after.Name, after.Kind = last.Name, last.Kind
		}
		list, err := tx.PageByName(ctx, env.ID, valueKinds, after, size+1)
		if err != nil {
			return err
		}
		if len(list) > size {
			list = list[:size]
			res.NextPageToken = paging.Token(paging.Cursor{ID: list[size-1].ID})
		}
		used, err := usedBy(ctx, tx, env.ID, list)
		if err != nil {
			return err
		}
		names := actors.New().In(ctx, tx)
		for i := range list {
			r := &list[i]
			kind := resource.Kind(r.Kind)
			v := &dockwellv1.EnvironmentVariable{
				Id: r.ID, Name: r.Name, Secret: kind == variable.SecretKind, Revision: int64(r.Revision),
				UpdateTime: timestamppb.New(r.UpdatedAt), ChangeId: r.ChangeID, UsedBy: used[usedKey{kind, r.Name}],
				UpdatedBy: names.Actor(r.Actor),
			}
			if kind == variable.VariableKind {
				// A variable holds nothing secret, so its spec is never
				// sealed (resource.Sealed); one that is would fail here.
				if secrets.Sealed(r.Spec) {
					return fmt.Errorf("variable %s's spec is sealed, which no variable's is", r.ID)
				}
				spec, err := h.kinds.Decode(kind, r.Spec)
				if err != nil {
					return err
				}
				v.Value = variable.Type{}.Value(spec)
			}
			res.Variables = append(res.Variables, v)
		}
		return nil
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(res), nil
}

// usedBy finds the services that use each of a page's variables and
// secrets, from the uses their latest revisions record.
func usedBy(ctx context.Context, tx *store.Tx, envID string, list []store.NamedResource) (map[usedKey][]*dockwellv1.VariableUse, error) {
	names := make([]string, len(list))
	for i := range list {
		names[i] = list[i].Name
	}
	used := map[usedKey][]*dockwellv1.VariableUse{}
	for chunk := range slices.Chunk(names, 500) {
		users, err := tx.UsersOf(ctx, envID, "", chunk...)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			k := usedKey{resource.Kind(u.Uses.Kind), u.Uses.Name}
			used[k] = append(used[k], &dockwellv1.VariableUse{ServiceId: u.ID, Service: u.Name, Name: u.Uses.As})
		}
	}
	return used, nil
}

// RevealSecret returns a secret's value to a person who just confirmed it
// with a passkey, and audits it.
func (h *Handler) RevealSecret(ctx context.Context, req *connect.Request[dockwellv1.RevealSecretRequest]) (*connect.Response[dockwellv1.RevealSecretResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		// The interceptor refuses tokens already; this holds if it didn't.
		return nil, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	client, _ := authapi.ClientFrom(ctx)
	if err := h.auth.UseStepUp(p, req.Msg.GetProof(), auth.StepUpRevealSecret, req.Msg.GetId()); err != nil {
		h.logger.InfoContext(ctx, "refused to reveal a secret without a passkey's confirmation", slog.String("secret", req.Msg.GetId()),
			slog.String("user", p.User.ID))
		return nil, h.rpcError(ctx, err)
	}
	got, err := h.keeper.Reveal(ctx, req.Msg.GetId(), p.Actor(), client.Surface(p))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RevealSecretResponse{Value: got.Value, Revision: int64(got.Revision)}), nil
}

func (h *Handler) rpcError(ctx context.Context, err error) error {
	var choice *scope.ChoiceError
	var cerr *connect.Error
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.As(err, &choice):
		cerr := authapi.ReasonError(connect.CodeInvalidArgument, err, "choice")
		field := "environment"
		if choice.Level == scope.ProjectKind {
			field = "project"
		}
		if d, derr := connect.NewErrorDetail(&dockwellv1.Choice{Level: string(choice.Level), Field: field, Options: choice.Options, Project: choice.Project}); derr == nil {
			cerr.AddDetail(d)
		}
		return cerr
	case errors.Is(err, scope.ErrNotFound):
		return authapi.ReasonError(connect.CodeNotFound, err, "not_found")
	case errors.Is(err, secrets.ErrNotSecret):
		return authapi.ReasonError(connect.CodeNotFound, err, "not_found")
	case errors.Is(err, auth.ErrNeedsStepUp):
		return authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	case errors.Is(err, auth.ErrNeedsSession):
		return authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsSession)
	}
	h.logger.ErrorContext(ctx, "variable call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its variables and secrets; try again, and check its logs if it keeps failing"))
}
