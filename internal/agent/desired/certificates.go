// SPDX-License-Identifier: AGPL-3.0-only

package desired

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/dockwell/dockwell/internal/resource"
)

// certificatesFile holds the certificates the control plane proved by DNS-01 for
// this server, beside the records and sealed with their key, so the agent has them
// while the control plane is away (ADR 0088). It isn't a record: no hello lists it.
const certificatesFile = "certificates.dns"

// KeepCertificates seals what the ingress keeps of its certificates by DNS.
func (s *Store) KeepCertificates(plain []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.seal(resource.CertificatesRef, sealed{Spec: plain})
	if err != nil {
		return err
	}
	return writeDurably(filepath.Join(s.dir, certificatesFile), b)
}

// KeptCertificates opens what KeepCertificates kept, or nil if it kept nothing. What
// doesn't open is put aside, and reads as nothing: the control plane sends them
// again.
func (s *Store) KeptCertificates() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, certificatesFile)
	r, _, err := s.read(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case errors.Is(err, errDamaged):
		s.putAside(path, err)
		return nil, nil
	case err != nil:
		return nil, err
	case r.Ref != resource.CertificatesRef:
		s.putAside(path, fmt.Errorf("%w: it holds %s", errDamaged, r.Ref))
		return nil, nil
	}
	return r.Spec, nil
}
