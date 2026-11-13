// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Certificate is one certificate by DNS-01 a server serves, its chain and key in PEM:
// the names it covers, and the routes' domains on that server it serves.
type Certificate struct {
	ID     string
	Name   string
	Names  []string
	Serves []string
	Chain  []byte
	Key    []byte
}

// putTimeout is how long an agent has to load what it's given into its ingress.
const putTimeout = 30 * time.Second

// LoadsCertificates says whether a server's agent serves the certificates the
// control plane proves by DNS-01, as its Hello said.
func (s *Session) LoadsCertificates() bool { return s.loadsCertificates }

// PutCertificates gives the agent every certificate by DNS-01 its routes are served
// with, sealed to its stream, and says how many it loaded. One it had and isn't
// given goes.
func (s *Session) PutCertificates(ctx context.Context, certs []Certificate) (int, error) {
	if !s.loadsCertificates {
		return 0, ErrOlderAgent
	}
	call := &agentv1.PutCertificatesCall{}
	values := make(map[string]string, 2*len(certs))
	for i := range certs {
		c := &certs[i]
		call.Certificates = append(call.Certificates, &agentv1.PutCertificate{Id: c.ID, Name: c.Name, Names: c.Names, Serves: c.Serves})
		values[c.ID+".chain"], values[c.ID+".key"] = string(c.Chain), string(c.Key)
	}
	defer clear(values)
	seal := func(on *Session) error {
		sealed, err := on.seal(resource.CertificatesRef, values)
		call.Sealed = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_PutCertificates{PutCertificates: call}}, putTimeout, seal)
	if err != nil {
		return 0, err
	}
	if r.GetPutCertificates() == nil {
		return 0, fmt.Errorf("%s's agent said nothing of the certificates it was given: %w", s.name, ErrOlderAgent)
	}
	return int(r.GetPutCertificates().GetLoaded()), nil
}
