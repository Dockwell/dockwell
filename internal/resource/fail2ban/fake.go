// SPDX-License-Identifier: AGPL-3.0-only

package fail2ban

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Fake is an in-memory Backend, for a test.
type Fake struct {
	mu    sync.Mutex
	jails map[string]*resourcev1.Jail
}

// NewFake returns fail2ban with no jails of Dockwell's.
func NewFake() *Fake { return &Fake{jails: map[string]*resourcev1.Jail{}} }

var _ Backend = (*Fake)(nil)

// Jail returns a copy of the named jail.
func (f *Fake) Jail(_ context.Context, name string) (*resourcev1.Jail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jails[name]; ok {
		return proto.CloneOf(j), nil
	}
	return nil, nil
}

// Put keeps a copy of the jail.
func (f *Fake) Put(_ context.Context, name string, jail *resourcev1.Jail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jails[name] = proto.CloneOf(jail)
	return nil
}

// Delete removes the named jail.
func (f *Fake) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jails, name)
	return nil
}
