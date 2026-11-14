// SPDX-License-Identifier: AGPL-3.0-only

// Package labels reads servers' labels and selectors of them, as the panel and the
// CLI write them (ADR 0091): a label as key:value, and a selector as such terms
// with spaces between, where a key alone matches a server that has it and ! in
// front matches one that doesn't. It's the one parser of them, for listing servers
// now and for fleet actions and pools later.
package labels

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/resource"
)

// Parse reads one label, key:value, or a key alone for an empty value.
func Parse(s string) (key, value string, err error) {
	s = strings.TrimSpace(s)
	key, value, _ = strings.Cut(s, ":")
	if err := resource.CheckLabelKey(key); err != nil {
		return "", "", err
	}
	if err := resource.CheckLabelValue(key, value); err != nil {
		return "", "", err
	}
	return key, value, nil
}

// String writes a label as key:value, or the key alone for an empty value.
func String(key, value string) string {
	if value == "" {
		return key
	}
	return key + ":" + value
}

// Strings writes labels as key:value, by key.
func Strings(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		out = append(out, String(k, labels[k]))
	}
	return out
}

// Apply gives labels with each of set set, written key:value, and each key of
// remove gone, as a new map. A key both set and removed is refused.
func Apply(labels map[string]string, set, remove []string) (map[string]string, error) {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	sets := map[string]bool{}
	for _, s := range set {
		k, v, err := Parse(s)
		if err != nil {
			return nil, err
		}
		out[k] = v
		sets[k] = true
	}
	for _, k := range remove {
		k = strings.TrimSpace(k)
		if err := resource.CheckLabelKey(k); err != nil {
			return nil, err
		}
		if sets[k] {
			return nil, fmt.Errorf("the label %s is both set and removed. Do one", k)
		}
		delete(out, k)
	}
	return out, resource.CheckLabels(out)
}

// Term is one term of a selector.
type Term struct {
	Key string
	// Value is what the key must be, when HasValue; otherwise the key need only be
	// there.
	Value    string
	HasValue bool
	// Not matches a server the rest doesn't.
	Not bool
}

// Selector is terms a server's labels must all match. An empty one matches every
// server.
type Selector []Term

// ParseSelector reads a selector, such as "pool:web region:eu !draining".
func ParseSelector(s string) (Selector, error) {
	var out Selector
	for _, field := range strings.Fields(s) {
		t := Term{}
		if rest, ok := strings.CutPrefix(field, "!"); ok {
			t.Not, field = true, rest
		}
		key, value, hasValue := strings.Cut(field, ":")
		if err := resource.CheckLabelKey(key); err != nil {
			return nil, fmt.Errorf("the selector %q: %w", s, err)
		}
		if err := resource.CheckLabelValue(key, value); err != nil {
			return nil, fmt.Errorf("the selector %q: %w", s, err)
		}
		t.Key, t.Value, t.HasValue = key, value, hasValue
		out = append(out, t)
	}
	return out, nil
}

// Matches reports whether labels match every term.
func (sel Selector) Matches(labels map[string]string) bool {
	for _, t := range sel {
		v, has := labels[t.Key]
		match := has && (!t.HasValue || v == t.Value)
		if match == t.Not {
			return false
		}
	}
	return true
}

// String writes the selector as ParseSelector reads it.
func (sel Selector) String() string {
	parts := make([]string, 0, len(sel))
	for _, t := range sel {
		s := t.Key
		if t.HasValue {
			s += ":" + t.Value
		}
		if t.Not {
			s = "!" + s
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}
