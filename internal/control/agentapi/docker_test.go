// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

func TestExactTerminalNeverSendsToAnOldAgent(t *testing.T) {
	a := NewAgents(nil, slog.New(slog.DiscardHandler))
	s := a.open("srv_1", "fixture-server", &agentv1.Hello{
		Terminals: true, ContainerTerminals: true,
	}, func(*agentv1.ConnectResponse) error {
		t.Fatal("an exact terminal request reached an agent that could interpret it as a host shell")
		return nil
	})
	_, err := s.OpenTerminal(t.Context(), &agentv1.OpenTerminalCall{
		Id: "trm_test", ContainerId: strings.Repeat("1", 64), Argv: []string{"/bin/sh"},
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("an older agent must refuse exact exec, got %v", err)
	}
}

func TestDatabaseConsoleNeverSendsToAnOldAgent(t *testing.T) {
	a := NewAgents(nil, slog.New(slog.DiscardHandler))
	s := a.open("srv_1", "fixture-server", &agentv1.Hello{
		Terminals: true, ContainerTerminals: true,
	}, func(*agentv1.ConnectResponse) error {
		t.Fatal("a database console request reached an agent that doesn't support it")
		return nil
	})
	_, err := s.OpenTerminal(t.Context(), &agentv1.OpenTerminalCall{
		Id: "trm_db", Container: "pg-1", DatabaseConsole: true,
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("an older agent must refuse database console, got %v", err)
	}
}
