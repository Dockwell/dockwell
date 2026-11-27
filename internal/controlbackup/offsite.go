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
		id, endpoint, spec["region"], spec["bucket"], prefix, spec["virtual_host"], spec["access_key"],
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

const destinationChanged = "The destination's storage changed, so no new off-site copy was sent. Ask the owner to select it again"

var errDestinationChanged = errors.New("the pinned off-site marker or storage changed")

type generationMarker struct {
	Install      string `json:"install"`
	Generation   int64  `json:"generation"`
	GenerationID string `json:"generation_id"`
	Nonce        string `json:"nonce,omitempty"`
}

type storedMarker struct {
	generationMarker
	info s3.ObjectInfo
	body []byte
}

func (d destination) readMarker(ctx context.Context, key string) (storedMarker, error) {
	read, info, err := d.bucket.GetWithInfo(ctx, key)
	if err != nil {
		return storedMarker{}, err
	}
	defer read.Close()
	raw, err := io.ReadAll(io.LimitReader(read, 4097))
	if err != nil {
		return storedMarker{}, err
	}
	got := storedMarker{info: info, body: raw}
	if len(raw) > 4096 || json.Unmarshal(raw, &got.generationMarker) != nil {
		return storedMarker{}, errDestinationChanged
	}
	return got, nil
}

func (d destination) ensureMarker(ctx context.Context, h Header, selection, trusted string, now time.Time) (string, error) {
	key := d.key("dockwell-control/" + h.Install + "/current.json")
	if selection == "" {
		return "", errDestinationChanged
	}
	want := generationMarker{Install: h.Install, Generation: h.Generation, GenerationID: h.GenerationID, Nonce: selection}
	data, _ := json.Marshal(want)
	got, err := d.readMarker(ctx, key)
	if s3.IsNotFound(err) {
		if trusted != "" || h.Generation != 1 {
			return "", errDestinationChanged
		}
		err = d.bucket.PutIfAbsent(ctx, key, data)
		var refused *s3.Error
		if err == nil {
			return want.Nonce, nil
		}
		if !errors.As(err, &refused) {
			return "", err
		}
		switch refused.Status {
		case http.StatusNotImplemented:
			// Recheck absence before falling back on older S3 implementations.
			got, err = d.readMarker(ctx, key)
			if s3.IsNotFound(err) {
				if err := d.bucket.Put(ctx, key, data); err != nil {
					return "", err
				}
				return want.Nonce, nil
			}
		case http.StatusPreconditionFailed, http.StatusConflict:
			got, err = d.readMarker(ctx, key)
		}
	}
	if err != nil {
		return "", err
	}
	if got.Install != want.Install || got.Generation != want.Generation || got.GenerationID != want.GenerationID ||
		(trusted != "" && got.Nonce != trusted) {
		return "", errDestinationChanged
	}
	if got.Nonce == "" && trusted == "" {
		// On explicit re-selection, bind the pre-nonce marker without replacing a concurrent update.
		if err := d.bucket.PutIfMatch(ctx, key, data, got.info.ETag); err != nil {
			return "", err
		}
		return want.Nonce, nil
	}
	if !validID("cbs", got.Nonce) {
		return "", errDestinationChanged
	}
	if !got.info.Modified.IsZero() && now.Sub(got.info.Modified) > 24*time.Hour {
		// Some older stores support neither conditional write. The marker just
		// matched, so sending is safe. A generation-scoped lifecycle keeps it.
		if err := d.bucket.PutIfMatch(ctx, key, got.body, got.info.ETag); err != nil && !s3.IsNotImplemented(err) {
			return "", err
		}
	}
	return got.Nonce, nil
}

func (m *Manager) deliverOffsite(ctx context.Context, s store.ControlBackup, b takenBundle) {
	if s.Destination == "" {
		return
	}
	var d destination
	p := store.ControlBackupPlace{Place: offsitePlace + ":" + s.DestinationPin, Target: s.DestinationPin, Name: s.Destination}
	superseded := false
	err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		current, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		if current.DestinationPin != s.DestinationPin || current.DestinationSelection != s.DestinationSelection || current.Generation != s.Generation {
			superseded = true
			return nil
		}
		s.DestinationMarker = current.DestinationMarker
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
		err = errDestinationChanged
	default:
		send, cancel := context.WithTimeout(ctx, storageTimeout)
		defer cancel()
		started, sendErr := m.sendOffsite(send, s, d, b, &p)
		if !started && sendErr == nil {
			return // Selection changed while its first marker was being prepared.
		}
		err = sendErr
	}
	m.recordDelivery(ctx, b, p, err)
}

func (m *Manager) sendOffsite(ctx context.Context, s store.ControlBackup, d destination, b takenBundle, p *store.ControlBackupPlace) (bool, error) {
	d.bucket.Client = netguard.Client(netguard.Options{
		Private: true, Loopback: m.cfg.LocalS3, Local: m.cfg.LocalS3, MaxBody: 4 << 20, Timeout: storageTimeout,
	})
	nonce, err := d.ensureMarker(ctx, b.header, s.DestinationSelection, s.DestinationMarker, m.cfg.Now())
	if err != nil {
		if errors.Is(err, errDestinationChanged) {
			return false, err
		}
		return false, storageProblem("Could not check the off-site generation", err)
	}
	if s.DestinationMarker == "" {
		var pinned bool
		err := m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			var err error
			pinned, err = tx.PinControlBackupMarker(ctx, s.Generation, s.DestinationSelection, nonce)
			return err
		})
		if err != nil || !pinned {
			return false, err
		}
	}
	now := m.cfg.Now()
	var retention retentionState
	if err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		encoded, recoveryID, previous, err := tx.ControlBackupRetention(ctx, s.Generation, p.Place)
		if err != nil {
			return err
		}
		retention = readRetention(encoded, nonce, now)
		retention.previous = previous || recoveryID != "" && recoveryID != b.state.RecoveryID
		return nil
	}); err != nil {
		return false, err
	}
	p.Path = d.key(keep.Prefix(b.header.Install, b.header.Generation))
	p.Problem, p.PreviousPending, err = d.store(ctx, b, &retention, now)
	p.RetentionState = retention.encode()
	return true, err
}

func (d destination) store(ctx context.Context, b takenBundle, retention *retentionState, now time.Time) (string, bool, error) {
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
		return "", false, storageProblem("Could not send the off-site copy", err)
	}
	size, err := d.bucket.Size(ctx, key)
	if err != nil || size != b.size {
		return "", false, errors.New("could not verify the off-site copy. Check the destination")
	}
	problem, previous := d.prune(ctx, prefix, b.name, retention, now)
	return problem, previous, nil
}

func storageProblem(doing string, err error) error {
	var refused *s3.Error
	if errors.As(err, &refused) {
		return fmt.Errorf("%s. Storage returned HTTP %d. Check the destination's access and bucket", doing, refused.Status)
	}
	return fmt.Errorf("%s. Check the destination and its connection", doing)
}
