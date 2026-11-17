// SPDX-License-Identifier: AGPL-3.0-only

package ownerstep

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A where is what a check's JSON answer must say, in a small grammar that's never an
// expression (ADR 0105):
//
//	where    = path "=" value
//	path     = part { "." part } where a part follows "]" without its dot too
//	part     = key | "[" key "=" value "]"
//	key      = 1*( ALPHA / DIGIT / "_" / "-" )
//
// A key reads a field of an object, or an element of a list by its index. A part in
// brackets picks the first object of a list whose field reads the value, in any
// case, as an email does. The value after the path is the scalar that path must
// read, as it reads, and each value may name ${NAME}, filled as the check's request
// is; neither holds [, ] or =. So "[email=${OWNER_EMAIL}]._status=0" reads that the list's user of the
// owner's email has _status 0.

// wherePart is one part of a where's path: a key, or a pick of a list's element.
type wherePart struct {
	key string
	// pick says the part picks the list's object whose key reads value.
	pick  bool
	value string
}

// whereKey is a key a where's path reads.
var whereKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parseWhere reads a where into its path and the value it must read, or says why
// it's no where.
func parseWhere(w string) ([]wherePart, string, error) {
	bad := func(why string) ([]wherePart, string, error) {
		return nil, "", fmt.Errorf("says where %q, which %s. Write path=value, as [email=${OWNER_EMAIL}]._status=0", w, why)
	}
	var parts []wherePart
	i := 0
	for {
		if i >= len(w) {
			return bad("has no =value after its path")
		}
		if w[i] == '[' {
			end := strings.IndexByte(w[i:], ']')
			if end < 0 {
				return bad("opens a [ it doesn't close")
			}
			key, value, ok := strings.Cut(w[i+1:i+end], "=")
			if !ok || !whereKey.MatchString(key) || strings.ContainsAny(value, "[=") {
				return bad("picks with [" + w[i+1:i+end] + "], which isn't [key=value]")
			}
			parts = append(parts, wherePart{key: key, pick: true, value: value})
			i += end + 1
		} else {
			end := strings.IndexAny(w[i:], ".[=")
			if end < 0 {
				return bad("has no =value after its path")
			}
			key := w[i : i+end]
			if !whereKey.MatchString(key) {
				return bad(fmt.Sprintf("reads the key %q, which takes letters, digits, _ and - alone", key))
			}
			parts = append(parts, wherePart{key: key})
			i += end
		}
		switch {
		case i < len(w) && w[i] == '=':
			value := w[i+1:]
			if strings.ContainsAny(value, "[]=") {
				return bad("compares with a value that holds [, ] or =")
			}
			return parts, value, nil
		case i < len(w) && w[i] == '.':
			i++
		case i < len(w) && w[i] == '[':
		default:
			return bad("has no =value after its path")
		}
	}
}

// Where reads whether a JSON answer says what where does, with values filling each
// ${NAME} in it.
func Where(where string, answer []byte, values map[string]string) (bool, error) {
	parts, want, err := parseWhere(where)
	if err != nil {
		return false, err
	}
	var doc any
	if err := json.Unmarshal(answer, &doc); err != nil {
		return false, errors.New("its answer isn't JSON, so it can't say what where does")
	}
	same := func(s string) string { return s }
	for _, p := range parts {
		switch d := doc.(type) {
		case map[string]any:
			if p.pick {
				return false, nil
			}
			doc = d[p.key]
		case []any:
			if !p.pick {
				n, ok := index(p.key, len(d))
				if !ok {
					return false, nil
				}
				doc = d[n]
				continue
			}
			value := Fill(p.value, values, same)
			found := false
			for _, el := range d {
				if m, ok := el.(map[string]any); ok {
					if v, ok := scalar(m[p.key]); ok && strings.EqualFold(v, value) {
						doc, found = m, true
						break
					}
				}
			}
			if !found {
				return false, nil
			}
		default:
			return false, nil
		}
	}
	got, ok := scalar(doc)
	return ok && got == Fill(want, values, same), nil
}

// index is a key as an index of a list of n, where it's one.
func index(key string, n int) (int, bool) {
	i, err := strconv.Atoi(key)
	return i, err == nil && i >= 0 && i < n
}

// scalar is a JSON scalar as text: a string as it is, a number as it's written
// shortest, true, false or null.
func scalar(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}
