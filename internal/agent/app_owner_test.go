// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ownerstep"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/secrets"
)

const ownerPassword = "Zq9VbN3kLm8RtY2wXc7PdH4s"

// ownerEngine is a fake Docker whose service web runs a container of Dockwell's
// that prints what a step gave it, and fails, and beside it one only labeled as the
// service, as anyone with Docker could make.
func ownerEngine(t *testing.T) (*dockertest.Engine, func() dockertest.Exec, *[]string) {
	t.Helper()
	e := dockertest.New(t)
	var mu sync.Mutex
	var ran dockertest.Exec
	var others []string
	e.AddLocalImage("acme/app:1", dockertest.Image{Exec: func(x dockertest.Exec) int {
		mu.Lock()
		ran = x
		mu.Unlock()
		env := map[string]string{}
		for _, kv := range x.Env {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		fmt.Fprint(x.Stderr, strings.Repeat("x", ownerstep.MaxSaid-6)+env["OWNER_PASSWORD"]+" refused for "+env["OWNER_EMAIL"])
		return 1
	}})
	e.AddLocalImage("acme/other:1", dockertest.Image{Exec: func(x dockertest.Exec) int {
		mu.Lock()
		others = append(others, strings.Join(x.Env, " "))
		mu.Unlock()
		return 0
	}})
	labels := map[string]string{docker.LabelProject: docker.Project("env_shop"), docker.LabelService: "web"}
	// One that only says the service's Compose labels, first.
	other := e.AddContainer(docker.Container{ID: "aa-other", Name: "/aa-other", Config: docker.Config{Image: "acme/other:1", Labels: labels}})
	other.State.Running, other.State.Status = true, "running"
	ours := map[string]string{docker.LabelRevision: "1"}
	for k, v := range labels {
		ours[k] = v
	}
	c := e.AddContainer(docker.Container{ID: "web-1", Name: "/web-1", Config: docker.Config{Image: "acme/app:1", Labels: ours}})
	c.State.Running, c.State.Status = true, "running"
	return e, func() dockertest.Exec {
		mu.Lock()
		defer mu.Unlock()
		return ran
	}, &others
}

func commandOwner(words ...string) *resourcev1.OwnerRun {
	return &resourcev1.OwnerRun{
		Service: "web", Email: "you@example.com", Password: "OWNER_PASSWORD",
		Steps: []*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: words, User: "git"}}}},
	}
}

// An owner's command runs in the service's running container of Dockwell's own,
// never one only labeled as the service, as the user its step names, with every
// value in its environment and none in its words; and what it prints back is
// scrubbed before it's cut, so a password placed across the cut leaves nothing of
// itself (ADR 0105, H3, M5 and the review's L2).
func TestAnOwnersStepReachesItsContainerWithItsValuesHidden(t *testing.T) {
	t.Parallel()
	e, ran, others := ownerEngine(t)
	target, err := healthyContainer(t.Context(), e.Client(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	words := []string{"sh", "-c", `make-owner --email "$OWNER_EMAIL" --password-from-env OWNER_PASSWORD`}
	res := ownerstep.Run(t.Context(), target, commandOwner(words...), map[string]string{"OWNER_EMAIL": "you@example.com", "OWNER_PASSWORD": ownerPassword}, false)
	if res.Made || res.Step != 1 {
		t.Fatalf("Run = %+v, want step 1 failed", res)
	}
	if len(res.Said) > ownerstep.MaxSaid || strings.Contains(res.Said, ownerPassword[:4]) || strings.Contains(res.Said, "you@example.com") {
		t.Errorf("what the app said holds a value: %q", res.Said[max(0, len(res.Said)-40):])
	}
	x := ran()
	if x.User != "git" || !slices.Equal(x.Cmd, words) {
		t.Errorf("the step ran %v as %q", x.Cmd, x.User)
	}
	if !slices.Contains(x.Env, "OWNER_PASSWORD="+ownerPassword) {
		t.Error("the step's environment has no password")
	}
	if len(*others) > 0 {
		t.Errorf("a container only labeled as the service got the step: %v", *others)
	}
}

// The control plane's call reaches the agent sealed, and what the agent answers holds
// none of what it opened (ADR 0105, the review's L1 and L5).
func TestMakingAnOwnerThroughTheAgentsCall(t *testing.T) {
	t.Parallel()
	e, ran, _ := ownerEngine(t)
	key, err := delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"OWNER_EMAIL": "you@example.com", "OWNER_PASSWORD": ownerPassword}
	sealed, err := secrets.SealValues(key.Public(), "srv_1", resource.AppOwnerRef("env_shop"), values)
	if err != nil {
		t.Fatal(err)
	}
	c := &calls{cfg: Config{Docker: e.Client()}, delivery: key, server: "srv_1"}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got, err := c.makeAppOwner(ctx, &agentv1.MakeAppOwnerCall{
		Environment: "env_shop", Service: "web", Values: sealed,
		Owner: commandOwner("sh", "-c", `make-owner --password-from-env OWNER_PASSWORD`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMade() || got.GetStep() != 1 || !strings.Contains(got.GetSaid(), "it exited with 1") {
		t.Fatalf("makeAppOwner = %v", got)
	}
	if !slices.Contains(ran().Env, "OWNER_PASSWORD="+ownerPassword) {
		t.Error("the sealed password didn't reach the step")
	}
	if strings.Contains(got.String(), ownerPassword[:6]) || strings.Contains(got.String(), "you@example.com") {
		t.Errorf("the agent's answer holds a value: %v", got)
	}
	// Sealed to another environment, it doesn't open.
	if _, err := c.makeAppOwner(ctx, &agentv1.MakeAppOwnerCall{Environment: "env_other", Service: "web", Values: sealed, Owner: commandOwner("true")}); err == nil {
		t.Error("values sealed to one environment opened for another")
	}
}
