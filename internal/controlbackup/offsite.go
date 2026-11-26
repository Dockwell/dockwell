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

func (d destination) readMarker(ctx context.Context, key string) (generationMarker, string, error) {
	read, etag, err := d.bucket.GetWithETag(ctx, key)
	if err != nil {
		return generationMarker{}, "", err
	}
	defer read.Close()
	raw, err := io.ReadAll(io.LimitReader(read, 4097))
	if err != nil {
		return generationMarker{}, "", err
	}
	var got generationMarker
	if len(raw) > 4096 || json.Unmarshal(raw, &got) != nil {
		return generationMarker{}, "", errDestinationChanged
	}
	return got, etag, nil
}

func (d destination) ensureMarker(ctx context.Context, h Header, selection, trusted string) (string, error) {
	key := d.key("dockwell-control/" + h.Install + "/current.json")
	if selection == "" {
		return "", errDestinationChanged
	}
	want := generationMarker{Install: h.Install, Generation: h.Generation, GenerationID: h.GenerationID, Nonce: selection}
	data, _ := json.Marshal(want)
	got, etag, err := d.readMarker(ctx, key)
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
			got, etag, err = d.readMarker(ctx, key)
			if s3.IsNotFound(err) {
				if err := d.bucket.Put(ctx, key, data); err != nil {
					return "", err
				}
				return want.Nonce, nil
			}
		case http.StatusPreconditionFailed, http.StatusConflict:
			got, etag, err = d.readMarker(ctx, key)
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
		if err := d.bucket.PutIfMatch(ctx, key, data, etag); err != nil {
			return "", err
		}
		return want.Nonce, nil
	}
	if !validID("cbs", got.Nonce) {
		return "", errDestinationChanged
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
	nonce, err := d.ensureMarker(ctx, b.header, s.DestinationSelection, s.DestinationMarker)
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
	p.Path = d.key(keep.Prefix(b.header.Install, b.header.Generation))
	p.Problem, p.PreviousPending, err = d.store(ctx, b)
	return true, err
}

func (d destination) store(ctx context.Context, b takenBundle) (string, bool, error) {
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
	problem, previous := d.prune(ctx, prefix, b.name)
	return problem, previous, nil
}

func (d destination) prune(ctx context.Context, prefix, newest string) (string, bool) {
	keys, err := d.bucket.ListObjects(ctx, prefix)
	if err != nil {
		return "Could not list older copies. Check the bucket's list access and lifecycle rules", true
	}
	var names []string
	for _, key := range keys {
		if name, ok := strings.CutPrefix(key, prefix); ok {
			names = append(names, name)
		}
	}
	if !slices.Contains(names, newest) {
		return "The destination did not list the completed copy. Older copies were kept", true
	}
	drop := keep.Expired(names, newest)
	versioned, err := d.bucket.Versioned(ctx)
	if err != nil {
		return "Could not check bucket versioning. Older copies were kept. Check the bucket's versioning access", true
	}
	byName := make(map[string][]s3.ObjectVersion)
	if versioned {
		versions, err := d.bucket.ListObjectVersions(ctx, prefix)
		if err != nil {
			return "Could not list object versions. Older copies were kept. Check the bucket's version list access", true
		}
		visible := make(map[string]bool, len(names))
		for _, name := range names {
			visible[name] = true
		}
		for _, v := range versions {
			name, ok := strings.CutPrefix(v.Key, prefix)
			if _, valid := keep.Phrase(name); !ok || !valid || name >= newest {
				continue
			}
			if len(byName[name]) == 0 && !visible[name] {
				// A previously deleted key may still hold noncurrent versions.
				drop = append(drop, name)
			}
			byName[name] = append(byName[name], v)
		}
	}
	current, _ := keep.Phrase(newest)
	var failures []string
	var failed int
	previous := false
	for _, name := range drop {
		problem := d.removeRetired(ctx, prefix+name, byName[name], versioned)
		if problem == "" {
			continue
		}
		phrase, _ := keep.Phrase(name)
		previous = previous || phrase != current
		failed++
		if len(failures) < 3 {
			failures = append(failures, fmt.Sprintf("%s (%s)", name, problem))
		}
	}
	if failed == 0 {
		return "", false
	}
	if failed > len(failures) {
		failures = append(failures, fmt.Sprintf("%d more", failed-len(failures)))
	}
	return "Storage kept " + strings.Join(failures, ", ") + ". Check the bucket's retention and delete access", previous
}

func (d destination) removeRetired(ctx context.Context, key string, versions []s3.ObjectVersion, versioned bool) string {
	if !versioned {
		if err := d.bucket.Delete(ctx, key); err != nil {
			return d.retentionExpiry(ctx, key, "")
		}
		return ""
	}
	if len(versions) == 0 {
		return "no versions listed"
	}
	var problem string
	for _, v := range versions {
		if !v.DeleteMarker {
			if err := d.bucket.DeleteVersion(ctx, key, v.VersionID); err != nil {
				expiry := d.retentionExpiry(ctx, key, v.VersionID)
				if problem == "" || problem == "expiry unknown" {
					problem = expiry
				}
			}
		}
	}
	if problem != "" {
		return problem // Keep delete markers in place while a body is still locked.
	}
	for _, v := range versions {
		if v.DeleteMarker {
			if err := d.bucket.DeleteVersion(ctx, key, v.VersionID); err != nil {
				return "could not remove its delete marker"
			}
		}
	}
	return ""
}

func (d destination) retentionExpiry(ctx context.Context, key, version string) string {
	if until, err := d.bucket.ObjectRetention(ctx, key, version); err == nil && !until.IsZero() {
		return "locked until " + until.UTC().Format(time.DateOnly)
	}
	return "expiry unknown"
}

func storageProblem(doing string, err error) error {
	var refused *s3.Error
	if errors.As(err, &refused) {
		return fmt.Errorf("%s. Storage returned HTTP %d. Check the destination's access and bucket", doing, refused.Status)
	}
	return fmt.Errorf("%s. Check the destination and its connection", doing)
}
