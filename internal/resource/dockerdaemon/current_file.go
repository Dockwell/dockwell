// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// rememberActive keeps an exact recovery base before a local edit is made.
// It shares the worker lock so an unacknowledged candidate can't become the base.
func (d *Driver) rememberActive(ctx context.Context) {
	if d.Client == nil {
		return
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	src, err := d.localManager().source(check)
	if err != nil || src.started.IsZero() {
		return
	}
	if os.MkdirAll(d.Dir, 0o700) != nil || trustedDirectory(d.Dir) != nil {
		return
	}
	lock, err := openTransactionLock(d.Dir)
	if err != nil {
		return
	}
	defer lock.Close()
	if tryTransactionLock(lock) != nil {
		return
	}
	data, _, _, err := readConfig(src.path)
	if err != nil {
		return
	}
	held, err := d.record()
	if err != nil || (held.ActiveKnown && !held.ActiveAt.Before(src.started) && held.ConfigDigest == configDigest(data)) || checkConfigAge(src, data, held) != nil {
		return
	}
	held.setActive(data)
	// The next reviewed edit records its own verified base even if this
	// opportunistic snapshot could not be saved.
	_ = writeRecord(filepath.Join(d.Dir, "owned.json"), held)
}

func (r *record) setActive(data []byte) {
	r.ActiveKnown, r.Active, r.ConfigDigest, r.ActiveAt = true, data, configDigest(data), time.Now()
}

func activeFileChanges(before, after config) ([]*v1.FieldChange, *spec.DockerDaemon, error) {
	draft := &spec.DockerDaemon{}
	var changes []*v1.FieldChange
	add := func(key string, old, next json.RawMessage) {
		if equalJSON(old, next) {
			return
		}
		op := v1.FieldOp_FIELD_OP_MODIFIED
		switch {
		case old == nil:
			op = v1.FieldOp_FIELD_OP_ADDED
		case next == nil:
			op = v1.FieldOp_FIELD_OP_REMOVED
		}
		changes = append(changes, &v1.FieldChange{Path: key, Op: op, From: string(old), To: string(next)})
	}
	keys := []string{"registry-mirrors", "log-driver", "log-opts", "default-address-pools"}
	unchanged := func(doc, other config) bool {
		for key, value := range doc {
			if !slices.Contains(keys, key) && !equalJSON(value, other[key]) {
				return false
			}
		}
		return true
	}
	if !unchanged(before, after) || !unchanged(after, before) {
		return nil, nil, errors.New("daemon.json changes settings outside this editor. Open Docker service and logs to review the service")
	}
	if !equalJSON(before["registry-mirrors"], after["registry-mirrors"]) {
		var old, next []string
		if !mirrorList(before["registry-mirrors"], &old) || !mirrorList(after["registry-mirrors"], &next) {
			return nil, nil, errors.New("the local registry mirrors include values this editor cannot safely show")
		}
		draft.Mirrors = &spec.DockerRegistryMirrors{Urls: next}
		add("registry-mirrors", before["registry-mirrors"], after["registry-mirrors"])
	}
	if !equalJSON(before["log-driver"], after["log-driver"]) || !equalJSON(before["log-opts"], after["log-opts"]) {
		oldDriver, nextDriver := "json-file", "json-file"
		if raw := before["log-driver"]; raw != nil && json.Unmarshal(raw, &oldDriver) != nil {
			return nil, nil, errSource
		}
		if raw := after["log-driver"]; raw != nil && json.Unmarshal(raw, &nextDriver) != nil {
			return nil, nil, errSource
		}
		if (oldDriver != "json-file" && oldDriver != "local") || (nextDriver != "json-file" && nextDriver != "local") {
			return nil, nil, errors.New("the local log driver is outside this editor")
		}
		old, err := rotationFields(before["log-opts"])
		if err != nil {
			return nil, nil, err
		}
		next, err := rotationFields(after["log-opts"])
		if err != nil {
			return nil, nil, err
		}
		draft.Logging = logSettings(nextDriver, after["log-opts"])
		add("log-driver", before["log-driver"], after["log-driver"])
		for _, key := range []string{"max-size", "max-file"} {
			add("log-opts."+key, old[key], next[key])
		}
	}
	if !equalJSON(before["default-address-pools"], after["default-address-pools"]) {
		old, err := reviewedPools(before["default-address-pools"])
		if err != nil {
			return nil, nil, err
		}
		next, err := reviewedPools(after["default-address-pools"])
		if err != nil {
			return nil, nil, err
		}
		if next != nil {
			draft.AddressPools = &spec.DockerDefaultAddressPools{Pools: poolSettings(next)}
		}
		add("default-address-pools", old, next)
	}
	if len(changes) == 0 {
		return nil, nil, nil
	}
	if draft.GetLogging() != nil || draft.GetAddressPools() != nil || draft.GetMirrors() != nil {
		if err := validate(draft); err != nil {
			return nil, nil, err
		}
	}
	return changes, draft, nil
}

func mirrorList(raw json.RawMessage, urls *[]string) bool {
	return raw == nil || (json.Unmarshal(raw, urls) == nil && len(*urls) <= 16 &&
		!slices.ContainsFunc(*urls, func(url string) bool { return !validMirror(url) }))
}

func reviewedPools(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	var rows []poolEntry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&rows) != nil || len(rows) == 0 || len(rows) > 16 {
		return nil, errPools
	}
	// Only projected values reach the review; the full file is validated separately.
	data, err := json.Marshal(rows)
	if err != nil || len(poolSettings(data)) != len(rows) {
		return nil, errPools
	}
	return data, nil
}

func rotationFields(raw json.RawMessage) (config, error) {
	if raw == nil {
		return nil, nil
	}
	var doc config
	if json.Unmarshal(raw, &doc) != nil {
		return nil, errors.New("the local log options cannot be reviewed")
	}
	for key, val := range doc {
		var word string
		if (key != "max-size" && key != "max-file") || len(val) > 20 || json.Unmarshal(val, &word) != nil {
			return nil, errors.New("the local logging change includes options outside this editor")
		}
		for _, c := range word {
			if c != '-' && (c < '0' || c > '9') && !bytes.ContainsRune([]byte("bBkKmMgG"), c) {
				return nil, errors.New("the local log limits cannot be reviewed")
			}
		}
	}
	return doc, nil
}

func equalJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	var first, second bytes.Buffer
	return json.Compact(&first, a) == nil && json.Compact(&second, b) == nil && bytes.Equal(first.Bytes(), second.Bytes())
}
