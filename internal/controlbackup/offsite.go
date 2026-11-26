// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/store"
)

const storageTimeout = 30 * time.Minute

type destination struct {
	pin    string
	prefix string
	bucket s3.Bucket
}

func (m *Manager) destination(ctx context.Context, tx *store.Tx, name string) (destination, error) {
	id, spec, err := m.cfg.Destination(ctx, tx, name)
	if err != nil {
		return destination{}, err
	}
	defer clear(spec)
	hosted := spec["virtual_host"] == "true"
	endpoint, err := backuppolicy.CanonicalEndpoint(spec["endpoint"])
	if err != nil {
		return destination{}, err
	}
	if err := backuppolicy.CheckTransport(endpoint, spec["bucket"], !hosted); err != nil {
		return destination{}, err
	}
	prefix := strings.Trim(spec["prefix"], "/")
	sum := sha256.Sum256([]byte(strings.Join([]string{
		id, endpoint, spec["region"], spec["bucket"], prefix, spec["virtual_host"],
	}, "\x00")))
	return destination{
		pin: hex.EncodeToString(sum[:]), prefix: prefix,
		bucket: s3.Bucket{
			Endpoint: endpoint, Region: spec["region"], Name: spec["bucket"],
			AccessKey: spec["access_key"], SecretKey: spec["secret_key"], VirtualHost: hosted,
		},
	}, nil
}

func (d destination) key(p string) string {
	if d.prefix == "" {
		return p
	}
	return d.prefix + "/" + p
}

func (d destination) sheet(prefix string) *dockwellv1.ControlBackupDestination {
	return &dockwellv1.ControlBackupDestination{
		Endpoint: d.bucket.Endpoint, Region: d.bucket.Region, Bucket: d.bucket.Name,
		Prefix: d.key(prefix), AccessKeyId: d.bucket.AccessKey, VirtualHost: d.bucket.VirtualHost,
	}
}

type generationMarker struct {
	Install      string `json:"install"`
	Generation   int64  `json:"generation"`
	GenerationID string `json:"generation_id"`
}

func (d destination) ensureMarker(ctx context.Context, h Header) error {
	key := d.key("dockwell-control/" + h.Install + "/current.json")
	want := generationMarker{Install: h.Install, Generation: h.Generation, GenerationID: h.GenerationID}
	read, err := d.bucket.Get(ctx, key)
	if s3.IsNotFound(err) && h.Generation == 1 {
		data, _ := json.Marshal(want)
		err = d.bucket.PutIfAbsent(ctx, key, data)
		var refused *s3.Error
		if err == nil {
			return nil
		}
		if errors.As(err, &refused) && (refused.Status == http.StatusPreconditionFailed || refused.Status == http.StatusConflict) {
			read, err = d.bucket.Get(ctx, key)
		}
	}
	if err != nil {
		return err
	}
	defer read.Close()
	raw, err := io.ReadAll(io.LimitReader(read, 4097))
	if err != nil {
		return err
	}
	var got generationMarker
	if len(raw) > 4096 || json.Unmarshal(raw, &got) != nil || got != want {
		return errors.New("the destination has a different control plane generation")
	}
	return nil
}

func (m *Manager) deliverOffsite(ctx context.Context, s store.ControlBackup, b takenBundle) {
	if s.Destination == "" {
		return
	}
	var d destination
	superseded := false
	err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		current, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		if current.Destination != s.Destination || current.DestinationPin != s.DestinationPin {
			superseded = true
			return nil
		}
		d, err = m.destination(ctx, tx, s.Destination)
		return err
	})
	if superseded {
		return
	}
	switch {
	case err != nil:
		err = errors.New("the destination could not be read. Check its settings")
	case d.pin != s.DestinationPin:
		err = errors.New("the destination changed. Ask the owner to select it again")
	}
	var problem string
	if err == nil {
		d.bucket.Client = netguard.Client(netguard.Options{
			Private: true, Loopback: m.cfg.LocalS3,
			Local: m.cfg.LocalS3, MaxBody: 4 << 20, Timeout: storageTimeout,
		})
		send, cancel := context.WithTimeout(ctx, storageTimeout)
		defer cancel()
		problem, err = d.store(send, b)
	}
	m.recordDelivery(ctx, b, offsitePlace, s.DestinationPin, d.key(keep.Prefix(b.header.Install, b.header.Generation)), problem, err)
}

func (d destination) store(ctx context.Context, b takenBundle) (string, error) {
	if err := d.ensureMarker(ctx, b.header); err != nil {
		return "", storageProblem("Could not check the off-site generation", err)
	}
	prefix := d.key(keep.Prefix(b.header.Install, b.header.Generation))
	part := s3.PartSize(b.size, 1<<20)
	count := int((b.size + part - 1) / part)
	key := prefix + b.name
	var err error
	if b.size <= 5<<20 {
		err = d.bucket.PutReader(ctx, key, io.NewSectionReader(b.file, 0, b.size), b.size, b.sum)
	} else {
		length := func(i int) int64 { return min(part, b.size-int64(i)*part) }
		err = d.bucket.Store(ctx, s3.Upload{
			Key: key, Count: count, Length: length,
			Part: func(i int) s3.Part {
				return func() (io.Reader, error) { return io.NewSectionReader(b.file, int64(i)*part, length(i)), nil }
			},
		})
	}
	if err != nil {
		return "", storageProblem("Could not send the off-site copy", err)
	}
	size, err := d.bucket.Size(ctx, key)
	if err != nil || size != b.size {
		return "", errors.New("could not verify the off-site copy. Check the destination")
	}
	keys, err := d.bucket.ListObjects(ctx, prefix)
	if err != nil {
		return "Older copies may still open with the previous passphrase. Check the bucket's list access and lifecycle rules", nil //nolint:nilerr // the new copy is already whole
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			names = append(names, name)
		}
	}
	if !slices.Contains(names, b.name) {
		return "The destination did not list the completed copy. Older copies were kept", nil
	}
	for _, old := range keep.Expired(names, b.name) {
		if err := d.bucket.Delete(ctx, prefix+old); err != nil {
			expiry := "expiry unknown"
			if until, err := d.bucket.ObjectRetention(ctx, prefix+old); err == nil && !until.IsZero() {
				expiry = "locked until " + until.UTC().Format(time.DateOnly)
			}
			problem := fmt.Sprintf("Storage kept %s (%s). Older copies may still open with the previous passphrase", old, expiry)
			return problem, nil
		}
	}
	return "", nil
}

func storageProblem(doing string, err error) error {
	var refused *s3.Error
	if errors.As(err, &refused) {
		return fmt.Errorf("%s. Storage returned HTTP %d. Check the destination's access and bucket", doing, refused.Status)
	}
	return fmt.Errorf("%s. Check the destination and its connection", doing)
}
