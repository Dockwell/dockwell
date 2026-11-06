// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"errors"

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

// Take takes a backup. destination is what a sealed destination opened to, if the
// call had one, which goes no further than this call.
func (c Calls) Take(ctx context.Context, call *agentv1.BackupCall, destination map[string]string) (*agentv1.BackupResult, error) {
	m, err := c.Runner.Take(ctx, Take{ID: call.GetId(), Ref: resourcepb.FromRef(call.GetVolume()), Stop: call.GetStop(), Destination: DestinationFrom(destination)})
	if err != nil {
		return nil, err
	}
	return result(m), nil
}

func result(m *Manifest) *agentv1.BackupResult {
	out := &agentv1.BackupResult{
		Method: resourcepb.BackupMethod(m.Method), Database: m.Database, SizeBytes: m.Size, Sha256: m.SHA256, FilesChanged: m.Changed,
		CopyError: m.CopyError,
	}
	if m.Copied != nil {
		out.Key, out.StoredBytes = m.Copied.Key, m.Copied.Bytes
	}
	return out
}

// Fetch brings a backup back from where it was sent.
func (c Calls) Fetch(ctx context.Context, call *agentv1.FetchBackupCall, destination map[string]string) (*agentv1.BackupResult, error) {
	d := DestinationFrom(destination)
	if d == nil {
		return nil, errors.New("this call says nothing of where the backup is")
	}
	m, err := c.Runner.Fetch(ctx, resourcepb.FromRef(call.GetVolume()), call.GetId(), call.GetKey(), d)
	if err != nil {
		return nil, err
	}
	return result(m), nil
}

// Delete removes a backup from the disk, and from where it was sent.
func (c Calls) Delete(ctx context.Context, call *agentv1.DeleteBackupCall, destination map[string]string) error {
	return c.Runner.Delete(ctx, resourcepb.FromRef(call.GetVolume()), call.GetId(), call.GetKey(), DestinationFrom(destination))
}
