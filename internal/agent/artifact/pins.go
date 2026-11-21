// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/id"
)

// Pin journals a reviewed change's exclusive hold before it can be applied.
func (s *Spool) Pin(pin *agentv1.DockerArtifactPin, owner string) (*v1.DockerArtifactInfo, error) {
	if _, err := id.Parse(id.Change, pin.GetChangeId()); err != nil {
		return nil, ErrUnavailable
	}
	l, err := s.locked(pin.GetArtifactId(), owner, false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	if l.Pin != nil {
		if proto.Equal(l.Pin, pin) {
			return proto.CloneOf(l.Info), nil
		}
		return nil, ErrPlanned
	}
	if l.Info.GetKind() == Import && l.Info.GetState() != Ready || l.Info.GetKind() == Export && l.Info.GetState() != Checking {
		return nil, ErrUnavailable
	}
	l.Pin = proto.CloneOf(pin)
	l.PinOrphanAfter = s.now().Add(Lifetime)
	if err := l.save(); err != nil {
		l.Pin, l.PinOrphanAfter = nil, time.Time{}
		return nil, err
	}
	s.publish(l)
	return proto.CloneOf(l.Info), nil
}

// CheckPin verifies that the bytes still belong to the change that will use them.
func (s *Spool) CheckPin(lease, change string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[lease]
	if l == nil || l.pinned.GetChangeId() != change {
		return ErrUnavailable
	}
	return nil
}

// ReleasePin removes staging held by the exact ended change. A completed
// export keeps only the binding for apply retries, so its download stays available.
func (s *Spool) ReleasePin(pin *agentv1.DockerArtifactPin) error {
	l, err := s.locked(pin.GetArtifactId(), "", false)
	if err != nil {
		return err
	}
	defer l.mu.Unlock()
	if !proto.Equal(l.Pin, pin) || sealedExport(l.Info) {
		return ErrUnavailable
	}
	return s.remove(l)
}

// Pins returns the holds to check with the control plane. Only pins from an
// unsuccessful plan record may be released on an absent change.
func (s *Spool) Pins() (pins []*agentv1.DockerArtifactPin, absentExpired map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	absentExpired = make(map[string]bool)
	for lease, l := range s.leases {
		if l.pinned != nil && !sealedExport(l.snapshot) {
			pins = append(pins, proto.CloneOf(l.pinned))
			absentExpired[lease] = !now.Before(l.orphanAfter)
		}
	}
	return pins, absentExpired
}
