// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/secrets"
)

// loader keeps what it's given, as the ingress does.
type loader struct {
	mu    sync.Mutex
	given []ingress.DNSCert
}

func (l *loader) PutCertificates(_ context.Context, certs []ingress.DNSCert) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.given = certs
	return len(certs), nil
}

func (l *loader) HearControlPlane(func() bool) {}

// The certificates the control plane proved by DNS come sealed to the stream, open
// with its key, and go to the ingress with their names and what they serve; one
// that comes without its key is refused, as is any to an agent with no ingress
// (ADR 0088).
func TestCertificatesByDNSGoToTheIngress(t *testing.T) {
	key, err := delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	l := &loader{}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{Logger: slog.New(slog.DiscardHandler), Certificates: l}, out.send, key)
	put := func(id uint64, values map[string]string, certs ...*agentv1.PutCertificate) {
		t.Helper()
		sealed, err := secrets.SealValues(key.Public(), "", resource.CertificatesRef, values)
		if err != nil {
			t.Fatal(err)
		}
		c.start(&agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_PutCertificates{PutCertificates: &agentv1.PutCertificatesCall{Sealed: sealed, Certificates: certs}}})
	}
	wild := &agentv1.PutCertificate{Id: "crt_wild", Name: "wild", Names: []string{"*.example.com"}, Serves: []string{"api.example.com"}}
	put(1, map[string]string{"crt_wild.chain": "CHAIN", "crt_wild.key": "KEY"}, wild)
	put(2, map[string]string{"crt_wild.chain": "CHAIN"}, wild)
	first, second := out.await(t, 1), out.await(t, 2)
	c.stop()
	if r := first; r.GetPutCertificates().GetLoaded() != 1 {
		t.Fatalf("the call ended with %v", r)
	}
	if len(l.given) != 1 || l.given[0].Name != "wild" || string(l.given[0].Key) != "KEY" || string(l.given[0].Chain) != "CHAIN" || !slices.Equal(l.given[0].Serves, []string{"api.example.com"}) {
		t.Errorf("the ingress was given %+v", l.given)
	}
	if r := second; !strings.Contains(r.GetError().GetMessage(), "without its chain or its key") {
		t.Errorf("a certificate without its key: %v", r)
	}

	none := newCalls(context.Background(), Config{Logger: slog.New(slog.DiscardHandler)}, out.send, key)
	sealed, _ := secrets.SealValues(key.Public(), "", resource.CertificatesRef, map[string]string{})
	none.start(&agentv1.Call{Id: 3, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_PutCertificates{PutCertificates: &agentv1.PutCertificatesCall{Sealed: sealed}}})
	third := out.await(t, 3)
	none.stop()
	if r := third; r.GetError() == nil {
		t.Errorf("an agent with no ingress loaded certificates: %v", r)
	}
}
