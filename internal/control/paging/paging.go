// SPDX-License-Identifier: AGPL-3.0-only

// Package paging reads the page sizes and page tokens that the API's list
// methods share: ListChanges, ListEvents and ListResources.
package paging

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/control/authapi"
)

// Size reads a request's page_size: unset when it is, and at most most.
// A negative size is invalid.
func Size(n int32, unset, most int) (int, error) {
	switch {
	case n < 0:
		return 0, authapi.Invalid(fmt.Errorf("page_size can't be negative; leave it out for %d", unset))
	case n == 0:
		return unset, nil
	case int(n) > most:
		return most, nil
	}
	return int(n), nil
}

// Cursor is where a page ended: its last item's time, when the list is in
// time order, and its ID.
type Cursor struct {
	At time.Time
	ID string
}

// Token is the opaque page token for the page after c.
func Token(c Cursor) string {
	ms := int64(0)
	if !c.At.IsZero() {
		ms = c.At.UnixMilli()
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ms, 10) + " " + c.ID))
}

// Read reads a page token that method, such as "ListChanges", gave with
// Token. It reports false for an empty token, which starts at the first
// page, and fails as invalid for one it didn't give.
func Read(token, method string) (Cursor, bool, error) {
	if token == "" {
		return Cursor{}, false, nil
	}
	bad := authapi.Invalid(fmt.Errorf("page_token isn't one %s gave. Pass next_page_token from the page before, "+
		"or leave it out to start with the first page", method))
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, false, bad
	}
	at, id, ok := strings.Cut(string(b), " ")
	ms, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || id == "" {
		return Cursor{}, false, bad
	}
	c := Cursor{ID: id}
	if ms != 0 {
		c.At = time.UnixMilli(ms)
	}
	return c, true, nil
}
