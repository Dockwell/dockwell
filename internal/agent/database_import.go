// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/backup"
)

type databaseImporter interface {
	BeginImport(owner, importID, serviceID, database, filename, format string, sizeBytes uint64) (*backup.ImportMeta, error)
	WriteImport(ctx context.Context, importID, owner string, offset uint64, next func() ([]byte, error)) (*backup.ImportMeta, error)
	InspectImport(importID, owner string) (*backup.ImportMeta, error)
	CancelImport(importID, owner string) error
}

func (c *calls) databaseImport(ctx context.Context, ask *agentv1.DatabaseImportControlCall) *agentv1.CallResult {
	out := &agentv1.DatabaseImportControlResult{
		MaxBytes:  backup.MaxImportBytes,
		PartBytes: backup.ImportPartBytes,
	}
	importer, ok := c.cfg.Backups.(databaseImporter)
	if !ok || importer == nil {
		out.Error = "this agent doesn't have backup storage enabled, so it can't stage database dumps"
		out.Reason = "unavailable"
		return &agentv1.CallResult{Result: &agentv1.CallResult_DatabaseImport{DatabaseImport: out}}
	}
	fill := func(m *backup.ImportMeta) {
		if m == nil {
			return
		}
		out.ImportId = m.ID
		out.Offset = m.Offset
		out.SizeBytes = m.SizeBytes
		out.Complete = m.Complete
		out.Database = m.Database
		out.Filename = m.Filename
		out.Format = m.Format
		out.ServiceId = m.ServiceID
	}
	fail := func(err error) *agentv1.CallResult {
		var ie *backup.ImportError
		if errors.As(err, &ie) {
			out.Error = ie.Message
			out.Reason = ie.Reason
		} else {
			out.Error = err.Error()
			out.Reason = "failed"
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DatabaseImport{DatabaseImport: out}}
	}

	switch op := ask.GetOp().(type) {
	case *agentv1.DatabaseImportControlCall_Begin:
		b := op.Begin
		meta, err := importer.BeginImport(
			ask.GetOwner(),
			ask.GetImportId(),
			b.GetServiceId(),
			b.GetDatabase(),
			b.GetFilename(),
			b.GetFormat(),
			b.GetSizeBytes(),
		)
		if err != nil {
			return fail(err)
		}
		fill(meta)
	case *agentv1.DatabaseImportControlCall_Receive:
		if c.cfg.Files != nil {
			release, err := c.cfg.Files.WaitTransfer(ctx)
			if err != nil {
				return fail(&backup.ImportError{Reason: "upload_busy", Message: "this server is busy with other file transfers. Try again in a moment"})
			}
			defer release()
		}
		client, err := c.transferClient(true)
		if err != nil {
			return fail(err)
		}
		stream, err := client.ReceiveFile(ctx, connect.NewRequest(&agentv1.ReceiveFileRequest{Stream: op.Receive.GetStream()}))
		if err != nil {
			return fail(err)
		}
		defer stream.Close()
		next := func() ([]byte, error) {
			if stream.Receive() {
				return stream.Msg().GetData(), nil
			}
			if err := stream.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		meta, err := importer.WriteImport(ctx, ask.GetImportId(), ask.GetOwner(), op.Receive.GetOffset(), next)
		if err != nil {
			return fail(err)
		}
		fill(meta)
	case *agentv1.DatabaseImportControlCall_Inspect:
		meta, err := importer.InspectImport(ask.GetImportId(), ask.GetOwner())
		if err != nil {
			return fail(err)
		}
		fill(meta)
	case *agentv1.DatabaseImportControlCall_Cancel:
		if err := importer.CancelImport(ask.GetImportId(), ask.GetOwner()); err != nil {
			return fail(err)
		}
		out.ImportId = ask.GetImportId()
	default:
		out.Error = "unknown database import operation"
		out.Reason = "invalid"
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_DatabaseImport{DatabaseImport: out}}
}
