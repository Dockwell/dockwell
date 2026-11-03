// SPDX-License-Identifier: AGPL-3.0-only

// Package ticket encodes join tickets: everything a new server needs to
// find and trust the control plane, as one string that starts "dwj1.".
package ticket

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/pki"
)

// prefix versions the ticket format.
const prefix = "dwj1."

// SecretSize is the length of a join token's secret, in bytes.
const SecretSize = 32

// ErrMalformed reports a join ticket that can't be read.
var ErrMalformed = errors.New("this isn't a valid join ticket; copy the whole ticket from dockwell token create, starting with dwj1")

// Encode renders a ticket as the string `dockwell join` takes.
func Encode(t *agentv1.JoinTicket) string {
	b, err := proto.Marshal(t)
	if err != nil {
		panic(err) // a JoinTicket always marshals
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Decode parses and checks a ticket string.
func Decode(s string) (*agentv1.JoinTicket, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !ok {
		return nil, ErrMalformed
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrMalformed
	}
	t := &agentv1.JoinTicket{}
	if err := proto.Unmarshal(b, t); err != nil {
		return nil, ErrMalformed
	}
	u, err := url.Parse(t.GetControlUrl())
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w (its control plane address %q isn't an https URL)", ErrMalformed, t.GetControlUrl())
	}
	if !strings.HasPrefix(t.GetCaPin(), pki.PinPrefix) || t.GetTokenId() == "" || len(t.GetSecret()) != SecretSize {
		return nil, ErrMalformed
	}
	return t, nil
}
