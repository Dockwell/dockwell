// SPDX-License-Identifier: AGPL-3.0-only

// Package images serves ImageService: images read from their registries for a
// form, through the agent on the server an app will run on, which reads with
// that server's network and mirrors. The control plane keeps nothing it read.
// See docs/adr/0062-reading-an-image-from-its-registry.md.
package images

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

// Service reads images through the fleet's agents.
type Service struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
}

var _ dockwellv1connect.ImageServiceHandler = (*Service)(nil)

// New returns a Service that reads through the agents of hub's servers.
func New(hub *fleet.Hub, agents *agentapi.Agents) *Service {
	return &Service{fleet: hub, agents: agents}
}

// SearchImages asks Docker Hub, through a server, for images whose names hold a
// term.
func (s *Service) SearchImages(ctx context.Context, req *connect.Request[dockwellv1.SearchImagesRequest]) (*connect.Response[dockwellv1.SearchImagesResponse], error) {
	term := strings.TrimSpace(req.Msg.GetTerm())
	if term == "" {
		return connect.NewResponse(&dockwellv1.SearchImagesResponse{}), nil
	}
	if len(term) > 100 {
		return nil, authapi.Invalid(errors.New("a search is 100 characters at most. Shorten it"))
	}
	// Only Docker Hub is searched (ADR 0062): the engine would search a registry a term names.
	if first, _, path := strings.Cut(term, "/"); path && registry.NamesAHost(first) {
		return nil, authapi.Invalid(fmt.Errorf("only Docker Hub is searched, so %s names no image to search for. Type a name, such as nginx, or the whole reference", term))
	}
	got, err := s.read(ctx, req.Msg.GetServer(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Search{Search: &dockwellv1.SearchImagesRequest{Term: term}}})
	if err != nil {
		return nil, err
	}
	if got.GetSearch() == nil {
		return nil, s.unexpected()
	}
	return connect.NewResponse(got.GetSearch()), nil
}

// ListImageTags lists a repository's tags, newest first.
func (s *Service) ListImageTags(ctx context.Context, req *connect.Request[dockwellv1.ListImageTagsRequest]) (*connect.Response[dockwellv1.ListImageTagsResponse], error) {
	ref, err := registry.Parse(req.Msg.GetRepository())
	if err != nil {
		return nil, authapi.Invalid(fmt.Errorf("%w. Name a repository, such as nginx or ghcr.io/acme/web", err))
	}
	if ref.Tag != "" || ref.Digest != "" {
		return nil, authapi.Invalid(fmt.Errorf("%s names one image. Name its repository alone, such as %s", req.Msg.GetRepository(), ref.Name()))
	}
	got, err := s.read(ctx, req.Msg.GetServer(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Tags{Tags: &dockwellv1.ListImageTagsRequest{Repository: ref.Name()}}})
	if err != nil {
		return nil, err
	}
	if got.GetTags() == nil {
		return nil, s.unexpected()
	}
	return connect.NewResponse(got.GetTags()), nil
}

// GetImage says whether an image is there, and what it declares.
func (s *Service) GetImage(ctx context.Context, req *connect.Request[dockwellv1.GetImageRequest]) (*connect.Response[dockwellv1.GetImageResponse], error) {
	ref, err := registry.Parse(req.Msg.GetReference())
	if err != nil {
		return nil, authapi.Invalid(fmt.Errorf("%w. Write it as nginx:1.27, or ghcr.io/acme/web:1.4", err))
	}
	got, err := s.read(ctx, req.Msg.GetServer(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Image{Image: &dockwellv1.GetImageRequest{Reference: ref.String()}}})
	if err != nil {
		return nil, err
	}
	if got.GetImage() == nil {
		return nil, s.unexpected()
	}
	return connect.NewResponse(got.GetImage()), nil
}

// read sends a read to the agent on the server named, or the only one.
func (s *Service) read(ctx context.Context, named string, call *agentv1.ImageCall) (*agentv1.ImageResult, error) {
	srv, err := s.server(named)
	if err != nil {
		return nil, err
	}
	var session *agentapi.Session
	if s.agents != nil && srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so it can't read images now. Type the image, and the plan checks it; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	got, err := session.Image(ctx, call)
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't read images: %w", srv.GetName(), err))
	case err != nil:
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't read images now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return got, nil
}

// server is the server a read goes through: the one named, by ID or name, or
// the only one there is.
func (s *Service) server(named string) (*dockwellv1.Server, error) {
	if named != "" {
		if srv, err := s.fleet.Get(named); err == nil {
			return srv, nil
		}
		for _, srv := range s.fleet.List() {
			if srv.GetName() == named {
				return srv, nil
			}
		}
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", named))
	}
	all := s.fleet.List()
	switch len(all) {
	case 0:
		return nil, authapi.ReasonError(connect.CodeUnavailable, errors.New("there's no server to read images through yet. Add one first"), authapi.ReasonUnavailable)
	case 1:
		return all[0], nil
	}
	names := make([]string, 0, len(all))
	for _, srv := range all {
		names = append(names, srv.GetName())
	}
	slices.Sort(names)
	return nil, authapi.ChoiceError(&dockwellv1.Choice{Level: "server", Field: "server", Options: names},
		fmt.Errorf("there are %d servers, so say which one the app will run on: an image is read as that server's pull would find it", len(all)))
}

func (s *Service) unexpected() error {
	return authapi.ReasonError(connect.CodeUnavailable, errors.New("the agent answered with something else. Try again"), authapi.ReasonUnavailable)
}

// Digests says which build each image's tag points at now, as the agent on the
// server named, or the only one, reads it with a HEAD of its manifest, which no
// registry counts as a pull. A plan pins an entry converted on the control plane
// with it (ADR 0078). An image its registry hasn't got, or didn't answer for, is an
// error that names it.
func (s *Service) Digests(ctx context.Context, server string, refs []string) (map[string]string, error) {
	got, err := s.read(ctx, server, &agentv1.ImageCall{Ask: &agentv1.ImageCall_Digests{Digests: &agentv1.ImageDigestsCall{References: refs}}})
	if err != nil {
		return nil, err
	}
	if got.GetDigests() == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the agent can't pin images by digest: %w", agentapi.ErrOlderAgent))
	}
	out := map[string]string{}
	for _, d := range got.GetDigests().GetDigests() {
		switch d.GetAnswer() {
		case dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND:
			if d.GetDigest() != "" {
				out[d.GetReference()] = d.GetDigest()
			}
		case dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND:
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the registry of %s hasn't got it, or keeps it private, so it can't be installed", d.GetReference()), "image_not_found")
		case dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED:
			return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("the registry of %s is limiting how often it's asked. Try again in a few minutes", d.GetReference()), authapi.ReasonUnavailable)
		default:
			return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("the registry of %s didn't answer, so its build can't be pinned. Try again in a moment", d.GetReference()), authapi.ReasonUnavailable)
		}
	}
	return out, nil
}
