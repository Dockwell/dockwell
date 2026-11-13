// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress"
)

// A route says how its domain proves itself, automatic unless it names http or dns,
// and every route of a domain says the same, as its claim's manner holds (ADR 0088).
func TestARoutesProof(t *testing.T) {
	d := &Driver{}
	for _, proof := range []string{"", ProofHTTP, ProofDNS} {
		r := &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080, Proof: proof}
		if err := d.Validate(r); err != nil {
			t.Errorf("proof %q = %v", proof, err)
		}
	}
	if err := d.Validate(&resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080, Proof: "email"}); err == nil || !strings.Contains(err.Error(), "isn't a way") {
		t.Errorf("an unknown proof = %v", err)
	}
	if err := d.Validate(&resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080, Tls: ingress.TLSOff, Proof: ProofDNS}); err == nil || !strings.Contains(err.Error(), "proves nothing") {
		t.Errorf("a route with no certificate that proves by DNS = %v", err)
	}
	manner := func(proof string) string {
		return d.Claims(&resourcev1.Route{Domain: "app.example.com", Proof: proof})[0].Manner
	}
	if manner(ProofDNS) == manner(ProofHTTP) || manner(ProofDNS) == manner("") {
		t.Error("a domain's routes can prove it two ways")
	}
}
