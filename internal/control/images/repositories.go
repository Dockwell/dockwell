// SPDX-License-Identifier: AGPL-3.0-only

package images

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/registry"
)

// ListRegistryRepositories reads the selected registry catalog through one server.
func (s *Service) ListRegistryRepositories(ctx context.Context, req *connect.Request[v1.ListRegistryRepositoriesRequest]) (*connect.Response[v1.ListRegistryRepositoriesResponse], error) {
	m := req.Msg
	host := strings.ToLower(strings.TrimSpace(m.GetRegistry()))
	r, err := registry.Parse(host + "/fixture")
	token := m.GetPageToken()
	if err != nil || host == "" || r.Registry != host || len(m.GetNamePrefix()) > 255 || len(token) > 255 || m.GetPageSize() > 100 {
		return nil, authapi.Invalid(errors.New("choose a registry and a page of at most 100 repositories"))
	}
	session, _, err := s.session(ctx, m.GetServer())
	if err != nil {
		return nil, err
	}
	if !session.DockerImageOperations() {
		return connect.NewResponse(&v1.ListRegistryRepositoriesResponse{}), nil
	}
	call := func() *agentv1.ImageCall {
		return &agentv1.ImageCall{Ask: &agentv1.ImageCall_Repositories{Repositories: &v1.ListRegistryRepositoriesRequest{
			Registry: host, NamePrefix: m.GetNamePrefix(), PageSize: m.GetPageSize(), PageToken: token,
		}}}
	}
	var got *agentv1.ImageResult
	if m.GetLoginId() == "" {
		got, err = session.Image(ctx, call())
	}
	if got.GetRepositories().GetEnumerationSupported() {
		return connect.NewResponse(got.GetRepositories()), nil
	}
	login, auth, loginErr := session.DockerImageLogin(ctx, m.GetProject(), r.String(), m.GetLoginId())
	if loginErr != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("choose a registry login for this registry"))
	}
	if login != nil {
		got, err = session.ImageAs(ctx, call(), auth)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the registry's repositories could not be read"))
	}
	if got.GetRepositories() == nil {
		return connect.NewResponse(&v1.ListRegistryRepositoriesResponse{}), nil
	}
	return connect.NewResponse(got.GetRepositories()), nil
}
