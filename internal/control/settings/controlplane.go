// SPDX-License-Identifier: AGPL-3.0-only

package settings

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/store"
)

// Plane is what dockwell run says of the control plane it started.
type Plane struct {
	Build   buildinfo.Info
	Started time.Time
	// DataDir is the directory it keeps its files in, and KeyFile the install's key there.
	DataDir, KeyFile string
	// AgentListen is the address servers' agents reach it on.
	AgentListen string
	// AgentURLs says where agents reach it: on the control server's own network,
	// and from elsewhere, in the order a ticket tries them.
	AgentURLs func(ctx context.Context) (local string, public []string, err error)
}

// GetControlPlane implements dockwell.v1.SettingsService.
func (h *Handler) GetControlPlane(ctx context.Context, _ *connect.Request[dockwellv1.GetControlPlaneRequest]) (*connect.Response[dockwellv1.GetControlPlaneResponse], error) {
	about, err := h.db.About(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	engine := dockwellv1.StateStoreEngine_STATE_STORE_ENGINE_SQLITE
	if h.db.Backend() == store.Postgres {
		engine = dockwellv1.StateStoreEngine_STATE_STORE_ENGINE_POSTGRES
	}
	p := h.plane
	cp := &dockwellv1.ControlPlane{
		Build:   p.Build.Proto(),
		DataDir: p.DataDir,
		KeyFile: p.KeyFile,
		Store: &dockwellv1.StateStore{
			Engine:    engine,
			Location:  h.db.Where(),
			Version:   about.Version,
			SizeBytes: about.Size,
			RoundTrip: durationpb.New(about.RoundTrip),
		},
		Panel:       h.panel(),
		Agents:      &dockwellv1.AgentListener{Listen: p.AgentListen},
		MemoryBytes: residentBytes(),
	}
	if !p.Started.IsZero() {
		cp.StartTime = timestamppb.New(p.Started)
	}
	if p.AgentURLs != nil {
		if cp.Agents.Url, cp.Agents.PublicUrls, err = p.AgentURLs(ctx); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&dockwellv1.GetControlPlaneResponse{ControlPlane: cp}), nil
}

// residentBytes is the memory this process holds, as Linux counts its resident
// set, which is what the memory budgets measure (ADR 0018). It's zero where
// there's no /proc.
func residentBytes() int64 {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	lines := bufio.NewScanner(bytes.NewReader(status))
	for lines.Scan() {
		rest, ok := bytes.CutPrefix(lines.Bytes(), []byte("VmRSS:"))
		if !ok {
			continue
		}
		kb, err := strconv.ParseInt(string(bytes.TrimSpace(bytes.TrimSuffix(bytes.TrimSpace(rest), []byte("kB")))), 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
