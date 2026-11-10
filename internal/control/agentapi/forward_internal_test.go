// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"log/slog"
	"net"
	"strings"
	"testing"
)

// A forward's stream is taken once, by the agent of the server it's for: another
// server's agent that presents its token gets nothing, and neither does the right
// agent a second time.
func TestAForwardIsTakenOnceByItsOwnServer(t *testing.T) {
	a := NewAgents(nil, slog.New(slog.DiscardHandler))
	mine, theirs := net.Pipe()
	defer mine.Close()
	defer theirs.Close()
	l := a.openForward("srv_1", theirs)
	if got := a.forwardFor("srv_2", l.token); got != nil {
		t.Error("another server's agent took the forward")
	}
	if got := a.forwardFor("srv_1", l.token); got != l {
		t.Fatalf("its own server's agent got %v", got)
	}
	if got := a.forwardFor("srv_1", l.token); got != nil {
		t.Error("the forward was taken a second time")
	}
	a.closeForward(l)
	if got := a.forwardFor("srv_1", l.token); got != nil {
		t.Error("a closed forward was taken")
	}
}

// An agent too old to forward is told to update, before anything is asked of it.
func TestAnOldAgentIsToldToUpdateToForward(t *testing.T) {
	s := &Session{name: "web-1", server: "srv_1", agents: NewAgents(nil, slog.New(slog.DiscardHandler))}
	_, err := s.Forward(t.Context(), "default-production", "phpmyadmin", 80)
	if err == nil || !strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "Update the agent on web-1") {
		t.Errorf("an old agent's forward: %v", err)
	}
}
