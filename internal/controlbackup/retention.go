// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/backup/s3"
)

// A place's last successful delivery stores this state. The marker scopes it
// to the actual storage selected by the owner, even after a reapproval.
type retentionState struct {
	Marker     string                          `json:"marker"`
	Locks      map[string]map[string]time.Time `json:"locks,omitempty"`
	Unverified map[string]bool                 `json:"unverified,omitempty"`
	previous   bool
}

func readRetention(encoded, marker string, now time.Time) retentionState {
	var state retentionState
	if encoded == "" || json.Unmarshal([]byte(encoded), &state) != nil || state.Marker != marker {
		return retentionState{Marker: marker}
	}
	for name, versions := range state.Locks {
		for version, expiry := range versions {
			if !now.Before(expiry) {
				delete(versions, version)
			}
		}
		if len(versions) == 0 {
			delete(state.Locks, name)
		}
	}
	return state
}

func (s *retentionState) encode() string {
	if len(s.Locks) == 0 && len(s.Unverified) == 0 {
		return ""
	}
	data, _ := json.Marshal(s)
	return string(data)
}

func (d destination) prune(ctx context.Context, prefix, newest string, state *retentionState, now time.Time) (string, bool) {
	keys, err := d.bucket.ListObjects(ctx, prefix)
	if err != nil {
		return "Could not list older copies. Check the bucket's list access and lifecycle rules", state.previous
	}
	visible := make(map[string]bool, len(keys))
	for _, key := range keys {
		if name, ok := strings.CutPrefix(key, prefix); ok {
			visible[name] = true
		}
	}
	if !visible[newest] {
		return "The destination did not list the completed copy. Older copies were kept", state.previous
	}
	current, _ := keep.Phrase(newest)
	oldPhrase := func(name string) bool {
		phrase, valid := keep.Phrase(name)
		return valid && name < newest && phrase != current
	}
	previous := state.previous
	var names []string
	for name := range visible {
		names = append(names, name)
		previous = previous || oldPhrase(name)
	}
	versioned, err := d.bucket.Versioned(ctx)
	var refused *s3.Error
	unknown := errors.As(err, &refused) && refused.Status == http.StatusForbidden
	if err != nil && !unknown {
		return "Could not check bucket versioning. Older copies were kept. Check the bucket's versioning access", previous
	}
	byName := make(map[string][]s3.ObjectVersion)
	if versioned || unknown {
		versions, err := d.bucket.ListObjectVersions(ctx, prefix)
		if err != nil && versioned {
			return "Could not list object versions. Older copies were kept. Check the bucket's version list access", previous
		}
		if err == nil {
			// Only a body makes a hidden name a recoverable copy. A delete marker
			// without a body must not displace a retained copy.
			hidden := make(map[string]bool)
			for _, v := range versions {
				name, ok := strings.CutPrefix(v.Key, prefix)
				if _, valid := keep.Phrase(name); !ok || !valid || name >= newest {
					continue
				}
				if !v.DeleteMarker && !visible[name] && !hidden[name] {
					hidden[name] = true
					names = append(names, name)
				}
				byName[name] = append(byName[name], v)
			}
			clear(state.Unverified)
		} else {
			// Retry verification of earlier old-phrase deletes that may now be
			// hidden. None of these names can count as current-phrase retention.
			for name := range state.Unverified {
				if !visible[name] && oldPhrase(name) {
					names = append(names, name)
				}
			}
		}
	} else {
		clear(state.Unverified)
	}

	var failures []string
	var failed int
	previous = false
	for _, name := range keep.Expired(names, newest) {
		old := oldPhrase(name)
		problem := d.retire(ctx, prefix, name, byName[name], versioned, unknown, visible[name], old, state, now)
		if problem == "" {
			delete(state.Unverified, name)
			continue
		}
		if unknown && old {
			if state.Unverified == nil {
				state.Unverified = make(map[string]bool)
			}
			state.Unverified[name] = true
		}
		previous = previous || old
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
	return "Storage could not retire " + strings.Join(failures, ", ") + ". Check the bucket's retention and delete access", previous
}

func (d destination) retire(ctx context.Context, prefix, name string, versions []s3.ObjectVersion, versioned, unknown, visible, old bool, state *retentionState, now time.Time) string {
	key := prefix + name
	if !versioned && visible {
		if problem := d.deleteBody(ctx, key, name, "", old, state, now); problem != "" || now.Before(state.Locks[name][""]) {
			return problem
		}
	}
	if unknown && (visible || len(versions) == 0) {
		// Without s3:GetBucketVersioning, DELETE may have made a marker. List
		// the exact retired key afterwards, so an older version cannot appear gone.
		listed, err := d.bucket.ListObjectVersions(ctx, key)
		if s3.IsNotImplemented(err) {
			return ""
		}
		if err != nil {
			if old {
				return "can't tell whether storage keeps older versions"
			}
			return ""
		}
		versions = versions[:0]
		for _, v := range listed {
			if v.Key == key {
				versions = append(versions, v)
			}
		}
	}
	if versioned && len(versions) == 0 {
		return "no versions listed"
	}
	var problem string
	locked := false
	for _, v := range versions {
		if v.DeleteMarker {
			continue
		}
		p := d.deleteBody(ctx, key, name, v.VersionID, old, state, now)
		locked = locked || now.Before(state.Locks[name][v.VersionID])
		if p != "" && (problem == "" || problem == "expiry unknown") {
			problem = p
		}
	}
	if problem != "" || locked {
		return problem // Keep delete markers in place while any body remains.
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

func (d destination) deleteBody(ctx context.Context, key, name, version string, old bool, state *retentionState, now time.Time) string {
	expiry := state.Locks[name][version]
	if !now.Before(expiry) {
		var err error
		if version == "" {
			err = d.bucket.Delete(ctx, key)
		} else {
			err = d.bucket.DeleteVersion(ctx, key, version)
		}
		if err == nil {
			return ""
		}
		expiry, err = d.bucket.ObjectRetention(ctx, key, version)
		if err != nil || !now.Before(expiry) {
			return "expiry unknown"
		}
		if state.Locks == nil {
			state.Locks = make(map[string]map[string]time.Time)
		}
		if state.Locks[name] == nil {
			state.Locks[name] = make(map[string]time.Time)
		}
		state.Locks[name][version] = expiry
	}
	if old {
		return "locked until " + expiry.UTC().Format(time.DateOnly)
	}
	return ""
}
