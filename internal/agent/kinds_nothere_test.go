// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
)

// notReady is a kind whose driver can't run, for the reason it gives.
type notReady struct {
	resource.Driver
	kind resource.Kind
	why  error
}

func (n notReady) Kind() resource.Kind         { return n.kind }
func (notReady) Level() resource.Level         { return resource.LevelServer }
func (notReady) New() proto.Message            { return nil }
func (notReady) Validate(proto.Message) error  { return nil }
func (n notReady) Ready(context.Context) error { return n.why }

// A kind the server doesn't have what it needs for, such as nft or fail2ban, is
// said for the record, as nobody may ever have asked for it; one that should
// run and can't, such as Docker's, is a warning. Both reach the control plane
// as unavailable, with why.
func TestAKindTheServerHasNoUseForIsntAWarning(t *testing.T) {
	drivers := resource.NewRegistry(
		notReady{kind: "fail2ban.jail", why: resource.NotHere(errors.New("fail2ban isn't installed on this server"))},
		notReady{kind: "container", why: errors.New("Docker isn't answering")},
	)
	k := readyKinds(t.Context(), drivers)
	if len(k.unavailable) != 2 || !k.notHere["fail2ban.jail"] || k.notHere["container"] {
		t.Fatalf("unavailable %v, not here %v", k.unavailable, k.notHere)
	}
	var out bytes.Buffer
	k.log(t.Context(), slog.New(slog.NewTextHandler(&out, nil)))
	for _, want := range []string{
		`level=INFO msg="the agent offers some kinds only once the server has what they need" kinds=[fail2ban.jail]`,
		`level=WARN msg="the agent can't run some kinds of resource; it offers them once it can" kinds=[container]`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log has no %s:\n%s", want, out.String())
		}
	}
	if _, unavailable := k.proto(); len(unavailable) != 2 {
		t.Errorf("the control plane hears of %d", len(unavailable))
	}
}
