// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// Calls answers the agent stream's calls about backups.
type Calls struct {
	Runner *Runner
}

// Preview says what backing up a volume would do.
func (c Calls) Preview(ctx context.Context, call *agentv1.BackupPreviewCall) (*agentv1.BackupPreviewResult, error) {
	p, err := c.Runner.Preview(ctx, resourcepb.FromRef(call.GetVolume()))
	if err != nil {
		return nil, err
	}
	return &agentv1.BackupPreviewResult{
		Method: resourcepb.BackupMethod(p.Method), Database: p.Database, Running: p.Running,
		EstimateBytes: p.Estimate, FreeBytes: p.Free, ReserveBytes: p.Reserve,
	}, nil
}

// Take takes a backup.
func (c Calls) Take(ctx context.Context, call *agentv1.BackupCall) (*agentv1.BackupResult, error) {
	m, err := c.Runner.Take(ctx, Take{ID: call.GetId(), Ref: resourcepb.FromRef(call.GetVolume()), Stop: call.GetStop()})
	if err != nil {
		return nil, err
	}
	return &agentv1.BackupResult{
		Method: resourcepb.BackupMethod(m.Method), Database: m.Database, SizeBytes: m.Size, Sha256: m.SHA256, FilesChanged: m.Changed,
	}, nil
}

// Delete removes a backup from the disk.
func (c Calls) Delete(ctx context.Context, call *agentv1.DeleteBackupCall) error {
	return c.Runner.Delete(ctx, resourcepb.FromRef(call.GetVolume()), call.GetId())
}
