// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// listerStub lists branches, and says what it was asked for. While held is open,
// each ask waits on it.
type listerStub struct {
	mu    sync.Mutex
	asked []string
	held  chan struct{}
	in    chan struct{}
}

func (l *listerStub) Resolve(context.Context, string, string) (string, string, error) {
	return "", "", errors.New("not here")
}

func (l *listerStub) Branches(ctx context.Context, url string) (string, []string, error) {
	l.mu.Lock()
	l.asked = append(l.asked, url)
	l.mu.Unlock()
	if l.held != nil {
		l.in <- struct{}{}
		select {
		case <-l.held:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	return "main", []string{"main", "next"}, nil
}

func listing(l *listerStub) *Handler {
	h := &Handler{listing: make(chan struct{}, listingsAtOnce)}
	h.SetSources(l)
	return h
}

// A repository's URL as people paste it, with no scheme, is asked as a build
// fetches it, with https:// (ADR 0046).
func TestBranchesAreAskedAtTheURLABuildFetches(t *testing.T) {
	l := &listerStub{}
	h := listing(l)
	res, err := h.ListRepositoryBranches(t.Context(), connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{Url: "  github.com/Acme/web/ "}))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.asked) != 1 || l.asked[0] != "https://github.com/Acme/web" {
		t.Errorf("the repository was asked at %q, want https://github.com/Acme/web", l.asked)
	}
	if res.Msg.GetDefaultBranch() != "main" || len(res.Msg.GetBranches()) != 2 {
		t.Errorf("the answer reads %v", res.Msg)
	}
}

// The control server lists few repositories' branches at once, for everyone, as
// each may read a large answer into memory; one more is refused, not queued.
func TestBranchListingsUnderWayAreBounded(t *testing.T) {
	l := &listerStub{held: make(chan struct{}), in: make(chan struct{})}
	h := listing(l)
	ask := func() error {
		_, err := h.ListRepositoryBranches(t.Context(), connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{Url: "https://git.example.com/acme/web"}))
		return err
	}
	done := make(chan error, listingsAtOnce)
	for range listingsAtOnce {
		go func() { done <- ask() }()
		<-l.in
	}
	if err := ask(); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a listing past %d under way: %v, want it refused as too many", listingsAtOnce, err)
	}
	close(l.held)
	for range listingsAtOnce {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// Each that ended gave its place back.
	l.held = nil
	if err := ask(); err != nil {
		t.Errorf("a listing once the others ended: %v", err)
	}
}
