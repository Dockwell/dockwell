// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/opener"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// EventOpen is what goes on the audit log as a person opens an app through the panel.
const EventOpen = "service.open"

// Opener issues the one-time addresses OpenService hands out (opener.Opener).
type Opener interface {
	Ticket(userID, sessionID string, site auth.Site, t opener.Target, follow opener.Follow) (url, origin string, expires time.Time, err error)
}

// opened is what the audit log says of an app opened through the panel.
type opened struct {
	Service string `json:"service"`
	Port    uint32 `json:"port"`
	Origin  string `json:"origin"`
	// From is the address the panel was asked from.
	From string `json:"from,omitempty"`
}

// OpenService gives the caller a one-time address that opens a service's web UI
// through the panel, on an origin of its own (ADR 0061).
func (h *Handler) OpenService(ctx context.Context, req *connect.Request[dockwellv1.OpenServiceRequest]) (*connect.Response[dockwellv1.OpenServiceResponse], error) {
	m := req.Msg
	if h.Opener == nil || h.Auth == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane opens no apps"))
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Session.ID == "" {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("an app opens for a person signed in to the panel, not for an API token"), "needs_session")
	}
	client, _ := authapi.ClientFrom(ctx)
	if !opener.Opens(client.Site) {
		// Before the passkey, so nobody confirms what can't happen.
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, opener.ErrNotLocalhost, "open_on_localhost")
	}
	r, err := h.find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	var target opener.Target
	err = h.db.View(ctx, func(tx *store.Tx) error {
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		svc, err := asService(spec)
		if err != nil {
			return err
		}
		port, err := webPort(r.Name, svc.GetApp().GetPorts(), m.GetPort())
		if err != nil {
			return err
		}
		deployments, err := tx.LatestDeployments(ctx, []string{r.ID})
		if err != nil {
			return err
		}
		server := ""
		for _, d := range deployments[r.ID] { //nolint:gocritic // a few rows
			if d.State != store.DeploymentSuperseded {
				server = d.Server
				break
			}
		}
		if server == "" {
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs on no server now, so there's nothing to open. Deploy it, then open it again", r.Name), "not_running")
		}
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		project := ""
		for _, pr := range tree.Projects {
			for _, e := range pr.Environments {
				if e.ID == r.Scope {
					project = pr.ID
				}
			}
		}
		target = opener.Target{ServiceID: r.ID, Service: r.Name, Project: docker.Project(r.Scope), ProjectID: project, Server: server, Port: port}
		return nil
	})
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return nil, cerr
		}
		return nil, h.internal(ctx, "open a service", err)
	}
	// An admin tool may sign whoever reaches it in, so opening one takes a passkey,
	// as revealing a secret does, once nothing else refuses. A proof opens it once.
	if m.GetProof() == "" {
		h.Auth.AskedForStepUp(p, auth.StepUpOpenService, r.ID, func() {})
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("%s may sign in whoever opens it, so opening it takes your passkey. Confirm with it, then open it again", r.Name),
			authapi.ReasonNeedsStepUp)
	}
	giveBack, err := h.Auth.HoldStepUp(p, m.GetProof(), auth.StepUpOpenService, r.ID)
	if err != nil {
		h.logger.InfoContext(ctx, "refused to open an app with a confirmation that is used, expired or for something else",
			slog.String("service", r.ID), slog.String("user", p.User.ID))
		return nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	// A proof that opened nothing isn't spent.
	done := false
	defer func() {
		if !done {
			giveBack()
		}
	}()
	// What was opened stays open while the session that asked does, and while its
	// role still lets it open this service where it is now, protected or not.
	still := authapi.Recheck(h.Auth, h.logger, dockwellv1connect.ServiceServiceOpenServiceProcedure, p,
		&dockwellv1.OpenServiceRequest{Id: r.ID, Port: target.Port})
	follow := func() (context.Context, func()) { return h.Auth.Follow(context.Background(), client, p, still) }
	url, origin, expires, err := h.Opener.Ticket(p.User.ID, p.Session.ID, client.Site, target, follow)
	switch {
	case errors.Is(err, opener.ErrNotLocalhost):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "open_on_localhost")
	case err != nil:
		return nil, h.internal(ctx, "open a service", err)
	}
	from, _, err := net.SplitHostPort(client.Addr)
	if err != nil {
		from = client.Addr
	}
	detail, _ := json.Marshal(opened{Service: r.Name, Port: target.Port, Origin: origin, From: from})
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: time.Now(), Actor: p.Actor(), Action: EventOpen, Subject: r.ID, Target: target.Server,
			Surface: client.Surface(p), Detail: string(detail),
		})
	}); err != nil {
		// What can't go on record doesn't open.
		return nil, h.internal(ctx, "record an app opened", err)
	}
	done = true
	return connect.NewResponse(&dockwellv1.OpenServiceResponse{Url: url, Origin: origin, ExpireTime: timestamppb.New(expires)}), nil
}

// webPort is the container port a service's web UI answers on: the one asked for,
// which it must publish, or the one port it publishes.
func webPort(name string, ports []*resourcev1.PortMapping, asked uint32) (uint32, error) {
	var tcp []uint32
	for _, p := range ports {
		if p.GetHostPort() != 0 && p.GetProtocol() != resourcev1.Protocol_PROTOCOL_UDP {
			tcp = append(tcp, p.GetContainerPort())
		}
	}
	if asked != 0 {
		for _, p := range tcp {
			if p == asked {
				return asked, nil
			}
		}
		return 0, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s publishes no port for %d. Publish it on its page, then open it again", name, asked), "not_published")
	}
	switch len(tcp) {
	case 0:
		return 0, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s publishes no port, so there's nothing to open. Publish its web UI's port on its page", name), "not_published")
	case 1:
		return tcp[0], nil
	}
	return 0, authapi.Invalid(fmt.Errorf("%s publishes %d ports. Say which one its web UI answers on, in its container", name, len(tcp)))
}
