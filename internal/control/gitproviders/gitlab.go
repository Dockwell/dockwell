// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/gitlabconnection"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	gitlabProjectsCacheTTL     = 60 * time.Second
	gitlabProjectsCacheMaxKeys = 256
)

type gitlabProjectsCache struct {
	projects  []gitlab.Project
	hasMore   bool
	expiresAt time.Time
}

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
			h.invalidateGitLabProjectsCache(saved.r.ID)
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

func (h *Handler) invalidateGitLabProjectsCache(connID string) {
	prefix := connID + "\x00"
	h.mu.Lock()
	for k := range h.gitlabProjects {
		if strings.HasPrefix(k, prefix) {
			delete(h.gitlabProjects, k)
		}
	}
	h.mu.Unlock()
}

func (h *Handler) listGitLabProjectsCached(ctx context.Context, k keptGitLab, baseURL, query string) (projects []gitlab.Project, hasMore bool, err error) {
	key := k.r.ID + "\x00" + strconv.Itoa(k.r.Revision) + "\x00" + strings.ToLower(query)
	now := h.now()
	h.mu.Lock()
	if entry, ok := h.gitlabProjects[key]; ok && now.Before(entry.expiresAt) {
		h.mu.Unlock()
		return entry.projects, entry.hasMore, nil
	}
	h.mu.Unlock()

	projects, hasMore, err = h.cfg.GitLab.ListProjects(ctx, baseURL, k.spec.GetToken(), query)
	if err != nil {
		return nil, false, err
	}

	h.mu.Lock()
	if len(h.gitlabProjects) >= gitlabProjectsCacheMaxKeys {
		for ck, cv := range h.gitlabProjects {
			if !now.Before(cv.expiresAt) {
				delete(h.gitlabProjects, ck)
			}
		}
		if len(h.gitlabProjects) >= gitlabProjectsCacheMaxKeys {
			clear(h.gitlabProjects)
		}
	}
	h.gitlabProjects[key] = gitlabProjectsCache{
		projects:  projects,
		hasMore:   hasMore,
		expiresAt: now.Add(gitlabProjectsCacheTTL),
	}
	h.mu.Unlock()
	return projects, hasMore, nil
}

// ListGitLabProjects returns the GitLab projects reachable by a project's
// connected GitLab instances, filtered by each connection's path prefix.
// Listings are cached per connection for 60 s (N8), failed connections report
// their state instead of being dropped silently, and has_more indicates when
// server-side search should be used beyond 100 projects.
func (h *Handler) ListGitLabProjects(ctx context.Context, req *connect.Request[dockwellv1.ListGitLabProjectsRequest]) (*connect.Response[dockwellv1.ListGitLabProjectsResponse], error) {
	var (
		reachable []keptGitLab
		claimed   map[string]bool
	)
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		project := ""
		for _, p := range tree.Projects {
			if p.ID == req.Msg.GetProject() || p.Name == req.Msg.GetProject() {
				project = p.ID
				break
			}
		}
		if project == "" {
			return said(connect.CodeNotFound, "not_found", "there's no project %s", req.Msg.GetProject())
		}
		all, err := h.allGitLab(ctx, tx)
		if err != nil {
			return err
		}
		for _, k := range all {
			if gitlabconnection.Reaches(k.spec, project) {
				reachable = append(reachable, k)
			}
		}
		claimed, err = h.claimedByOtherProjectSet(ctx, tx, project)
		return err
	})
	var ce *connect.Error
	if errors.As(err, &ce) {
		return nil, err
	}
	if err != nil {
		return nil, h.internal(ctx, "listing a project's GitLab connections", err)
	}
	out := &dockwellv1.ListGitLabProjectsResponse{}
	query := strings.TrimSpace(req.Msg.GetQuery())
	seen := map[string]bool{}
	for _, k := range reachable {
		baseURL := strings.TrimRight(k.spec.GetInstanceUrl(), "/")
		connInfo := &dockwellv1.GitLabProjectConnection{
			Id:         k.r.ID,
			Name:       k.r.Name,
			Url:        baseURL,
			PathPrefix: k.spec.GetPathPrefix(),
			State:      dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED,
		}
		projects, hasMore, err := h.listGitLabProjectsCached(ctx, k, baseURL, query)
		if err != nil {
			state, detail, _, _ := classifyGitLabCheck(gitlab.TokenInfo{}, err)
			connInfo.State = state
			connInfo.Detail = detail
			h.NoteGitLabStatus(k.r.ID, k.r.Revision, state, detail)
			out.Connections = append(out.Connections, connInfo)
			continue
		}
		connInfo.HasMore = hasMore
		out.Connections = append(out.Connections, connInfo)
		for _, p := range projects {
			if !gitlab.MatchesPrefix(p.PathWithNamespace, k.spec.GetPathPrefix()) {
				continue
			}
			repoURL := strings.TrimSpace(p.WebURL)
			if repoURL == "" {
				repoURL = baseURL + "/" + strings.TrimLeft(p.PathWithNamespace, "/")
			}
			norm, err := store.NormalizeDeployRepository(repoURL)
			if err != nil || norm == "" {
				continue
			}
			key := strings.ToLower(norm)
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Projects = append(out.Projects, &dockwellv1.GitLabProject{
				PathWithNamespace:     p.PathWithNamespace,
				Url:                   norm,
				ConnectionId:          k.r.ID,
				ConnectionName:        k.r.Name,
				ClaimedByOtherProject: claimed[key],
			})
		}
	}
	slices.SortFunc(out.Projects, func(a, b *dockwellv1.GitLabProject) int {
		return strings.Compare(a.GetPathWithNamespace(), b.GetPathWithNamespace())
	})
	return connect.NewResponse(out), nil
}
