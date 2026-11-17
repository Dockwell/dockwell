// SPDX-License-Identifier: AGPL-3.0-only

// Package publish publishes a route's domain at Cloudflare for the control plane:
// its DNS record, and the rule a tunnel needs to send the zone to the server's
// proxy, as steps of the route's change. A tunnel's document is shared with whoever
// else edits it, the panel's own name among its rules, and Cloudflare replaces all of
// it on each write with no version check, so it's written as rarely as can be, one
// rule at a time, spliced into what was read and checked before it's sent. See
// docs/adr/0104-adding-a-domain-publishes-it.md.
package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Rule is one of a tunnel's ingress rules, as far as routing reads it. The rest of
// it, its originRequest and anything Cloudflare adds later, stays in the document.
type Rule struct {
	Hostname string `json:"hostname,omitempty"`
	Path     string `json:"path,omitempty"`
	Service  string `json:"service"`
}

// CatchAll is the rule that answers whatever no other rule does, last in every
// document Dockwell writes from nothing.
var CatchAll = Rule{Service: "http_status:404"}

// Document is a tunnel's configuration as read: the raw "config" object, and where
// each of its ingress rules sits in it, so one rule can be spliced in or out and
// every other byte goes back as it came.
type Document struct {
	raw []byte
	// ingress is where the ingress array's value sits in raw, [start, end); -1 when
	// the document has none.
	ingress [2]int
	// elems are where each rule sits in raw, [start, end).
	elems [][2]int
	// Rules are the ingress rules, in order.
	Rules []Rule
}

// ErrNoDocument is a configuration that isn't a JSON object.
var ErrNoDocument = errors.New("the tunnel's configuration isn't a JSON object")

// Parse reads a tunnel's "config" object, raw. A tunnel with no routes yet reads
// null, which parses as a document with no rules.
func Parse(raw json.RawMessage) (*Document, error) {
	trimmed := bytes.TrimSpace(raw)
	d := &Document{raw: bytes.Clone(trimmed), ingress: [2]int{-1, -1}}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		d.raw = nil
		return d, nil
	}
	dec := json.NewDecoder(bytes.NewReader(d.raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, ErrNoDocument
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("can't read the tunnel's configuration: %w", err)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("can't read the tunnel's configuration: %w", err)
		}
		end := int(dec.InputOffset())
		if key != "ingress" {
			continue
		}
		d.ingress = [2]int{end - len(value), end}
		if err := d.readIngress(value, end-len(value)); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *Document) readIngress(value []byte, at int) error {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return errors.New("the tunnel's ingress isn't a list")
	}
	for dec.More() {
		var elem json.RawMessage
		if err := dec.Decode(&elem); err != nil {
			return fmt.Errorf("can't read the tunnel's ingress: %w", err)
		}
		end := int(dec.InputOffset())
		var r Rule
		if err := json.Unmarshal(elem, &r); err != nil {
			return fmt.Errorf("can't read a rule of the tunnel's ingress: %w", err)
		}
		d.Rules = append(d.Rules, r)
		d.elems = append(d.elems, [2]int{at + end - len(elem), at + end})
	}
	return nil
}

// Raw is the document as it stands, to write.
func (d *Document) Raw() json.RawMessage {
	if d.raw == nil {
		return json.RawMessage("null")
	}
	return bytes.Clone(d.raw)
}

// Empty says the document has no rules, as a new tunnel's has none.
func (d *Document) Empty() bool { return len(d.Rules) == 0 }

// HasCatchAll says its last rule answers whatever no other rule does, as
// Cloudflare asks of every document.
func (d *Document) HasCatchAll() bool {
	return len(d.Rules) > 0 && d.Rules[len(d.Rules)-1].Hostname == "" && d.Rules[len(d.Rules)-1].Path == ""
}

// Insert splices rule in before the catch-all, and returns the new document. A
// document with no rules becomes rule and a catch-all that answers 404, the only
// write that isn't a splice, as there's nothing to keep.
func (d *Document) Insert(rule Rule) (*Document, int, error) {
	elem, err := json.Marshal(rule)
	if err != nil {
		return nil, 0, err
	}
	if d.raw == nil || d.ingress[0] < 0 && d.Empty() {
		if d.raw != nil {
			return nil, 0, errors.New("the tunnel's configuration has settings and no ingress, which Dockwell leaves to whoever wrote it")
		}
		catchAll, _ := json.Marshal(CatchAll)
		out, err := Parse([]byte(`{"ingress":[` + string(elem) + `,` + string(catchAll) + `]}`))
		return out, 0, err
	}
	if !d.HasCatchAll() {
		return nil, 0, errors.New("the tunnel's last rule isn't a catch-all, which Cloudflare asks for, so Dockwell won't add to it")
	}
	at := len(d.Rules) - 1
	start := d.elems[at][0]
	var b bytes.Buffer
	b.Write(d.raw[:start])
	b.Write(elem)
	b.WriteByte(',')
	b.Write(d.raw[start:])
	out, err := Parse(b.Bytes())
	return out, at, err
}

// Remove splices out the rule at i, which mustn't be the catch-all, and returns the
// new document.
func (d *Document) Remove(i int) (*Document, error) {
	if i < 0 || i >= len(d.Rules)-1 {
		return nil, errors.New("there's no such rule to remove, or it's the catch-all")
	}
	// The rule and the comma after it; the catch-all always follows.
	start, end := d.elems[i][0], d.elems[i+1][0]
	var b bytes.Buffer
	b.Write(d.raw[:start])
	b.Write(d.raw[end:])
	return Parse(b.Bytes())
}

// Elem is the rule at i as it sits in the document, with every key it has: what's
// kept of a rule taken out, to put it back whole.
func (d *Document) Elem(i int) json.RawMessage {
	if i < 0 || i >= len(d.elems) {
		return nil
	}
	return bytes.Clone(d.raw[d.elems[i][0]:d.elems[i][1]])
}

// InsertAt splices elem, a whole rule as Elem gave it, in at at, before the
// catch-all at the latest, and returns the new document and where it went: a rule
// put back where it was.
func (d *Document) InsertAt(elem json.RawMessage, at int) (*Document, int, error) {
	var r Rule
	if err := json.Unmarshal(elem, &r); err != nil {
		return nil, 0, fmt.Errorf("the rule to put back can't be read: %w", err)
	}
	if !d.HasCatchAll() {
		return nil, 0, errors.New("the tunnel's last rule isn't a catch-all, which Cloudflare asks for, so Dockwell won't add to it")
	}
	at = max(0, min(at, len(d.Rules)-1))
	start := d.elems[at][0]
	var b bytes.Buffer
	b.Write(d.raw[:start])
	b.Write(bytes.TrimSpace(elem))
	b.WriteByte(',')
	b.Write(d.raw[start:])
	out, err := Parse(b.Bytes())
	return out, at, err
}

// WithService points the rule at i at service, keeping every other key of it, and
// returns the new document: a rule moved to the server's proxy.
func (d *Document) WithService(i int, service string) (*Document, error) {
	if i < 0 || i >= len(d.Rules)-1 {
		return nil, errors.New("there's no such rule to move, or it's the catch-all")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(d.raw[d.elems[i][0]:d.elems[i][1]], &keys); err != nil {
		return nil, fmt.Errorf("can't read the rule to move: %w", err)
	}
	to, err := json.Marshal(service)
	if err != nil {
		return nil, err
	}
	keys["service"] = to
	elem, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(d.raw[:d.elems[i][0]])
	b.Write(elem)
	b.Write(d.raw[d.elems[i][1]:])
	return Parse(b.Bytes())
}

// CheckMoved checks, before a write, that after differs from before by the service
// of the rule at at alone, which is now service: every other key of it, every other
// rule and everything outside ingress the same, value for value.
func CheckMoved(before, after json.RawMessage, at int, service string) error {
	b, err := decodeExact(before)
	if err != nil {
		return err
	}
	a, err := decodeExact(after)
	if err != nil {
		return err
	}
	bm, _ := b.(map[string]any)
	am, _ := a.(map[string]any)
	if bm == nil || am == nil || len(bm) != len(am) {
		return ErrMoreChanged
	}
	for k, v := range bm {
		if k == "ingress" {
			continue
		}
		if w, ok := am[k]; !ok || !reflect.DeepEqual(v, w) {
			return ErrMoreChanged
		}
	}
	br, _ := bm["ingress"].([]any)
	ar, _ := am["ingress"].([]any)
	if len(br) != len(ar) || at < 0 || at >= len(br) {
		return ErrMoreChanged
	}
	for i := range br {
		if i != at {
			if !reflect.DeepEqual(br[i], ar[i]) {
				return ErrMoreChanged
			}
			continue
		}
		was, _ := br[i].(map[string]any)
		is, _ := ar[i].(map[string]any)
		if was == nil || is == nil || len(was) != len(is) || is["service"] != service {
			return ErrMoreChanged
		}
		for k, v := range was {
			if k != "service" && !reflect.DeepEqual(v, is[k]) {
				return ErrMoreChanged
			}
		}
	}
	return nil
}

// Index finds a rule by its hostname and path, or -1.
func (d *Document) Index(hostname, path string) int {
	for i, r := range d.Rules {
		if strings.EqualFold(r.Hostname, hostname) && r.Path == path {
			return i
		}
	}
	return -1
}

// ErrMoreChanged is a document about to be written that differs from what was read
// by more than the one rule meant.
var ErrMoreChanged = errors.New("the tunnel's document to write differs from what was read by more than the one rule, so Dockwell didn't write it")

// CheckSplice checks, before a write, that after differs from before by exactly one
// rule: inserted at at, or removed from at. Both are decoded again, numbers kept as
// written, and compared value for value, as Cloudflare doesn't keep key order: every
// key outside ingress equal, and every other rule equal and in the same order.
func CheckSplice(before, after json.RawMessage, at int, inserted bool) error {
	b, err := decodeExact(before)
	if err != nil {
		return err
	}
	a, err := decodeExact(after)
	if err != nil {
		return err
	}
	bm, _ := b.(map[string]any)
	am, ok := a.(map[string]any)
	if !ok {
		return ErrMoreChanged
	}
	if bm == nil {
		// A document from nothing: the rule and a catch-all, and nothing else.
		if len(am) != 1 || !inserted || at != 0 {
			return ErrMoreChanged
		}
		rules, _ := am["ingress"].([]any)
		if len(rules) != 2 {
			return ErrMoreChanged
		}
		return nil
	}
	for k, v := range bm {
		if k == "ingress" {
			continue
		}
		if w, ok := am[k]; !ok || !reflect.DeepEqual(v, w) {
			return ErrMoreChanged
		}
	}
	for k := range am {
		if _, ok := bm[k]; !ok && k != "ingress" {
			return ErrMoreChanged
		}
	}
	br, _ := bm["ingress"].([]any)
	ar, _ := am["ingress"].([]any)
	long, short := ar, br
	if !inserted {
		long, short = br, ar
	}
	if len(long) != len(short)+1 || at < 0 || at >= len(long) {
		return ErrMoreChanged
	}
	for i := range short {
		j := i
		if i >= at {
			j = i + 1
		}
		if !reflect.DeepEqual(short[i], long[j]) {
			return ErrMoreChanged
		}
	}
	return nil
}

// decodeExact decodes JSON with numbers kept as written, so two different large
// numbers never compare equal.
func decodeExact(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("can't read the tunnel's document: %w", err)
	}
	return v, nil
}
