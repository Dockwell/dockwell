// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dockerdaemon"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

func TestDockerSettingsStartupRecoveryKeepsAgentConnected(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	root := t.TempDir()
	blocked := filepath.Join(root, "docker-settings")
	// A lasting error that does not depend on root or on a Docker daemon.
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.Run(ctx, agent.Config{
			Logger: slog.New(slog.DiscardHandler), Dir: root, Identity: id, Build: buildinfo.Read(),
			Collector: fakeCollector(), DockerSettings: &dockerdaemon.Driver{Dir: blocked},
			Drivers: resource.NewRegistry(&firewall.Driver{Backend: firewall.NewFake()}),
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	a := waitForAgent(t, s.agents, id.ServerID, true)
	session, ok := s.agents.Session(id.ServerID)
	if !ok {
		t.Fatal("agent session missing")
	}
	waitUntil(t, "Docker settings recovery finding", func() bool {
		result, err := session.DockerSettings(t.Context(), &agentv1.DockerSettingsCall{
			Input: &agentv1.DockerSettingsCall_Read{Read: true},
		})
		return err == nil && strings.Contains(result.GetSettings().GetRecoveryAttention(), "Check the agent service logs")
	})
	if _, err := a.Plan(t.Context(), []resource.Desired{{
		Ref:  resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"},
		Spec: &spec.FirewallRule{Port: 80},
	}}); err != nil {
		t.Fatalf("unrelated plans stopped working: %v", err)
	}
}
