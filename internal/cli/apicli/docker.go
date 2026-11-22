// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
)

// dockerServer fills omitted Docker selectors before calling unary or
// streaming RPCs. Some mutating RPCs require a server explicitly, so the CLI
// uses the same rule for all Docker verbs without changing API validation.
func (r *runner) dockerServer(ctx context.Context, req proto.Message) error {
	choose := func(flag string) (string, error) {
		call := Unary[v1.ListServersRequest, v1.ListServersResponse](dockwellv1connect.ServerServiceListServersProcedure)
		var servers []*v1.Server
		err := r.do(ctx, func() error {
			return call.Invoke(ctx, r.client, &v1.ListServersRequest{}, func(msg proto.Message) error {
				servers = msg.(*v1.ListServersResponse).GetServers()
				return nil
			})
		})
		if err != nil {
			return "", err
		}
		switch len(servers) {
		case 0:
			return "", errors.New("there's no server yet. Add one first")
		case 1:
			return servers[0].GetId(), nil
		default:
			names := make([]string, 0, len(servers))
			for _, server := range servers {
				names = append(names, server.GetName())
			}
			slices.Sort(names)
			return "", usage(fmt.Errorf("pass %s to select a server. Available servers: %s", flag, strings.Join(names, ", ")))
		}
	}

	var keys []*v1.DockerObjectKey
	var flag string
	switch m := req.(type) {
	case *v1.GetDockerObjectRequest:
		keys, flag = []*v1.DockerObjectKey{m.GetKey()}, "--key"
	case *v1.PlanDockerContainerLoggingRequest:
		keys, flag = []*v1.DockerObjectKey{m.GetContainer()}, "--container"
	case *v1.PlanDockerNetworkAttachmentRequest:
		keys, flag = []*v1.DockerObjectKey{m.GetNetwork(), m.GetContainer()}, "--network and --container"
	case *v1.WatchDockerObjectsRequest:
		if m.Selection == nil {
			m.Selection = &v1.ListDockerObjectsRequest{}
		}
		if len(m.Selection.GetServerIds()) == 0 {
			id, err := choose("--selection")
			if err != nil {
				return err
			}
			m.Selection.ServerIds = []string{id}
		}
		if ids := m.Selection.GetServerIds(); len(ids) == 1 {
			for _, key := range m.GetVisibleKeys() {
				if key != nil && key.GetServerId() == "" {
					key.ServerId = ids[0]
				}
			}
		}
		return nil
	}
	if len(keys) > 0 {
		var server string
		for _, key := range keys {
			if key.GetServerId() != "" {
				server = key.GetServerId()
			}
		}
		for _, key := range keys {
			if key == nil || key.GetServerId() != "" {
				continue
			}
			if server == "" {
				id, err := choose(flag)
				if err != nil {
					return err
				}
				server = id
			}
			key.ServerId = server
		}
		return nil
	}

	m := req.ProtoReflect()
	fields := m.Descriptor().Fields()
	if f := fields.ByName("server_id"); f != nil && m.Get(f).String() == "" {
		id, err := choose("--server-id")
		if err != nil {
			return err
		}
		m.Set(f, protoreflect.ValueOfString(id))
	}
	if f := fields.ByName("server_ids"); f != nil && m.Get(f).List().Len() == 0 {
		id, err := choose("--server-ids")
		if err != nil {
			return err
		}
		m.Mutable(f).List().Append(protoreflect.ValueOfString(id))
	}
	return nil
}
