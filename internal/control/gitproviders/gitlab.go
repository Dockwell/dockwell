// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/gitlabconnection"
	"github.com/dockwell/dockwell/internal/store"
)

type gitlabCheck struct {
	revision  int
	state     dockwellv1.GitLabConnectionState
	detail    string
	checkedAt time.Time
}

type keptGitLab struct {
	r    store.Resource
	spec *resourcev1.GitLabConnection
}

// allGitLab opens every saved GitLab connection, sorted by name.
func (h *Handler) allGitLab(ctx context.Context, tx *store.Tx) ([]keptGitLab, error) {
	stored, err := tx.ListKind(ctx, string(gitlabconnection.Kind))
	if err != nil {
		return nil, err
	}
	var out []keptGitLab
	for i := range stored {
		r := stored[i]
		if r.Spec == nil {
			continue
		}
		spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read the GitLab connection %s: %w", r.Name, err)
		}
		if gl, ok := spec.(*resourcev1.GitLabConnection); ok {
			out = append(out, keptGitLab{r: r, spec: gl})
		}
	}
	slices.SortFunc(out, func(a, b keptGitLab) int { return strings.Compare(a.r.Name, b.r.Name) })
	return out, nil
}

func (h *Handler) gitlabList(ctx context.Context, tx *store.Tx) ([]*dockwellv1.GitLabConnectionInfo, error) {
	all, err := h.allGitLab(ctx, tx)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*dockwellv1.GitLabConnectionInfo, 0, len(all))
	for _, k := range all {
		info := &dockwellv1.GitLabConnectionInfo{
			Id:           k.r.ID,
			Name:         k.r.Name,
			Url:          k.spec.GetInstanceUrl(),
			PathPrefix:   k.spec.GetPathPrefix(),
			EveryProject: len(k.spec.GetProjects()) == 0,
			Projects:     k.spec.GetProjects(),
			State:        dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_UNKNOWN,
			Revision:     int64(k.r.Revision),
		}
		if c, ok := h.gitlabChecks[k.r.ID]; ok && c.revision == k.r.Revision {
			info.State = c.state
			info.Detail = c.detail
			if !c.checkedAt.IsZero() {
				info.CheckedTime = timestamppb.New(c.checkedAt)
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// NoteGitLabStatus records the latest live status of a GitLab connection from
// status delivery so Settings, Integrations reflects expired tokens or missing
// scopes right away.
func (h *Handler) NoteGitLabStatus(connID string, rev int, state dockwellv1.GitLabConnectionState, detail string) {
	h.mu.Lock()
	h.gitlabChecks[connID] = gitlabCheck{revision: rev, state: state, detail: detail, checkedAt: h.now()}
	h.mu.Unlock()
}

// CheckGitLabConnection verifies a GitLab URL and access token from the control
// plane without saving anything.
func (h *Handler) CheckGitLabConnection(ctx context.Context, req *connect.Request[dockwellv1.CheckGitLabConnectionRequest]) (*connect.Response[dockwellv1.CheckGitLabConnectionResponse], error) {
	rawURL := strings.TrimSpace(req.Msg.GetUrl())
	normURL, err := gitlabconnection.NormalizeURL(rawURL)
	if err != nil {
		return nil, said(connect.CodeInvalidArgument, "invalid", "%s", err.Error())
	}
	token := strings.TrimSpace(req.Msg.GetToken())
	ref := strings.TrimSpace(req.Msg.GetName())
	var saved *keptGitLab
	if ref != "" {
		if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
			all, err := h.allGitLab(ctx, tx)
			if err != nil {
				return err
			}
			for i := range all {
				if all[i].r.Name == ref || all[i].r.ID == ref {
					k := all[i]
					saved = &k
					return nil
				}
			}
			return nil
		}); err != nil {
			return nil, h.internal(ctx, "reading the saved GitLab connection", err)
		}
	}
	usingSavedToken := false
	if token == "" {
		if saved == nil {
			return nil, said(connect.CodeInvalidArgument, "invalid", "enter a GitLab access token with the api scope")
		}
		savedURL, serr := gitlabconnection.NormalizeURL(saved.spec.GetInstanceUrl())
		if serr != nil || savedURL != normURL {
			return nil, said(connect.CodeInvalidArgument, "invalid", "enter the token for %s. A saved token isn't sent to another URL", normURL)
		}
		token = saved.spec.GetToken()
		usingSavedToken = true
	}
	if token == "" {
		return nil, said(connect.CodeInvalidArgument, "invalid", "enter a GitLab access token with the api scope")
	}

	info, err := h.cfg.GitLab.CheckToken(ctx, normURL, token)
	state, detail, scopes, tokenName := classifyGitLabCheck(info, err)
	if saved != nil {
		savedURL, _ := gitlabconnection.NormalizeURL(saved.spec.GetInstanceUrl())
		if savedURL == normURL && (usingSavedToken || token == saved.spec.GetToken()) {
			h.NoteGitLabStatus(saved.r.ID, saved.r.Revision, state, detail)
		}
	}
	return connect.NewResponse(&dockwellv1.CheckGitLabConnectionResponse{
		State:     state,
		Detail:    detail,
		Url:       normURL,
		TokenName: tokenName,
		Scopes:    scopes,
	}), nil
}

func classifyGitLabCheck(info gitlab.TokenInfo, err error) (dockwellv1.GitLabConnectionState, string, []string, string) {
	if err != nil {
		var ge *gitlab.Error
		switch {
		case errors.Is(err, netguard.ErrForbidden):
			return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_UNREACHABLE, err.Error(), nil, ""
		case errors.As(err, &ge) && (ge.Status == 401 || ge.Status == 403):
			return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_AUTH_FAILED,
				"GitLab refused this token. Check that it hasn't expired or been revoked, and carries the api scope.", nil, ""
		case errors.As(err, &ge):
			return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_UNREACHABLE, ge.Error(), nil, ""
		default:
			return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_UNREACHABLE, err.Error(), nil, ""
		}
	}
	if info.Revoked || (info.ScopesKnown && !info.Active) {
		return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_AUTH_FAILED,
			"This GitLab token is revoked or expired. Create a new token with the api scope.", info.Scopes, info.Name
	}
	if !info.HasAPIScope() {
		return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_MISSING_SCOPE,
			"This token doesn't have the api scope needed to report commit statuses. Create a token with the api scope.", info.Scopes, info.Name
	}
	return dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED, "", info.Scopes, info.Name
}
