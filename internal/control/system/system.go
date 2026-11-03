// SPDX-License-Identifier: AGPL-3.0-only

// Package system implements dockwell.v1.SystemService: what the install is
// and which build is running.
package system

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/buildinfo"
)

// Service answers SystemService calls.
type Service struct {
	build *dockwellv1.BuildInfo
}

var _ dockwellv1connect.SystemServiceHandler = (*Service)(nil)

// New returns a Service that reports build as the running build.
func New(build buildinfo.Info) *Service {
	return &Service{build: BuildInfoToProto(build)}
}

// GetVersion returns the control plane's build.
func (s *Service) GetVersion(
	context.Context, *connect.Request[dockwellv1.GetVersionRequest],
) (*connect.Response[dockwellv1.GetVersionResponse], error) {
	return connect.NewResponse(&dockwellv1.GetVersionResponse{Build: s.build}), nil
}

// BuildInfoToProto converts build information to its API form.
func BuildInfoToProto(info buildinfo.Info) *dockwellv1.BuildInfo {
	pb := &dockwellv1.BuildInfo{
		Version:   info.Version,
		Commit:    info.Commit,
		Modified:  info.Modified,
		GoVersion: info.GoVersion,
		Platform:  info.Platform,
	}
	if !info.CommitTime.IsZero() {
		pb.CommitTime = timestamppb.New(info.CommitTime)
	}
	return pb
}
