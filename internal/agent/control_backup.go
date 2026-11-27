// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/id"
)

// Only one bundle reserves room at once, even while a stream reconnects.
var keepingControlBackup sync.Mutex

func (c *calls) keepControlBackup(ctx context.Context, ask *agentv1.KeepControlBackupCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error()}}}
	}
	if c.cfg.Dir == "" {
		return fail(errors.New("this server has no place to keep control plane backups"))
	}
	root := filepath.Join(c.cfg.Dir, "control-backups")
	if !keepingControlBackup.TryLock() {
		return fail(errors.New("this server is already keeping a control plane backup"))
	}
	defer keepingControlBackup.Unlock()
	install, generation, probe := ask.GetInstall(), ask.GetGeneration(), ask.GetStream() == 0
	var dir string
	if !probe || install != "" || generation != 0 {
		if _, err := id.Parse("ins", install); err != nil || generation < 1 {
			return fail(errors.New("invalid control plane backup directory"))
		}
		if !probe && (!keep.Valid(install, generation, ask.GetName()) || len(ask.GetSha256()) != sha256.Size) {
			return fail(errors.New("invalid control plane backup name or checksum"))
		}
		dir = filepath.Join(root, keep.Prefix(install, generation))
		if err := sweepPartialControlBackups(dir); err != nil {
			return fail(err)
		}
	}
	room, err := backup.AvailableRoom(root)
	if err != nil {
		return fail(err)
	}
	if probe {
		return keptControlBackup(&agentv1.KeepControlBackupResult{Room: room})
	}
	if err := controlBackupRoom(room, ask.GetSize()); err != nil {
		return fail(err)
	}
	client, err := c.transferClient(false)
	if err != nil {
		return fail(err)
	}
	stream, err := client.ReceiveParts(ctx, &agentv1.ReceiveFileRequest{Stream: ask.GetStream()})
	if err != nil {
		return fail(err)
	}
	defer stream.Close()
	problem, err := receiveControlBackup(dir, ask.GetName(), ask.GetSize(), ask.GetSha256(), stream.Next)
	if err != nil {
		return fail(err)
	}
	return keptControlBackup(&agentv1.KeepControlBackupResult{Kept: true, PruneProblem: problem, Path: dir})
}

func keptControlBackup(out *agentv1.KeepControlBackupResult) *agentv1.CallResult {
	return &agentv1.CallResult{Result: &agentv1.CallResult_KeepControlBackup{KeepControlBackup: out}}
}

func controlBackupRoom(room, size int64) error {
	if size <= 0 || size > room {
		return errors.New("the control plane backup does not fit on this server. Free some disk space")
	}
	return nil
}

func sweepPartialControlBackups(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".partial-") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func receiveControlBackup(dir, name string, size int64, want []byte, next func() ([]byte, error)) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".partial-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	w := io.MultiWriter(file, hash)
	var got int64
	for {
		part, err := next()
		if len(part) > 0 {
			got += int64(len(part))
			if got > size {
				return "", errors.New("control plane backup exceeded its declared size")
			}
			if _, err := w.Write(part); err != nil {
				return "", err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if got != size || !bytes.Equal(hash.Sum(nil), want) {
		return "", errors.New("the control plane backup did not reach this server whole")
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return "", err
	}
	at, err := os.Open(dir) //nolint:gosec // this agent’s private backup directory
	if err != nil {
		return "", err
	}
	defer at.Close()
	if err := at.Sync(); err != nil {
		return "", err
	}
	entries, err := at.ReadDir(-1)
	if err != nil {
		return err.Error(), nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	for _, old := range keep.Expired(names, name) {
		if err := os.Remove(filepath.Join(dir, old)); err != nil {
			return fmt.Sprintf("could not remove an older copy: %v", err), nil
		}
	}
	return "", nil
}
