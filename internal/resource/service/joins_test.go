// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"slices"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// A database shared with an app in another environment joins the app's link network
// as its container is made, its first and each one after, answering to the app's
// name for it, so a redeploy, a rollback or a remake keeps the app reaching it
// (ADR 0084).
func TestAServiceJoinsTheLinkNetworksThatNameItAsItsMade(t *testing.T) {
	const (
		blog   = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"
		shared = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q7N"
	)
	e := dockertest.New(t)
	registry(e)
	link := network.TargetOf(resource.Ref{Kind: network.Kind, Scope: blog, Name: "db-link"}).Name
	if _, err := e.Client().CreateNetwork(t.Context(), &docker.NetworkCreate{Name: link, Internal: true}); err != nil {
		t.Fatal(err)
	}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	rt.Joins = func(target container.Target) []container.Join {
		if target.Project == docker.Project(shared) && target.Service == "db" {
			return []container.Join{{Network: link, Alias: "db"}}
		}
		return nil
	}
	local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: rt}})}
	ref := resource.Ref{Kind: service.Kind, Scope: shared, Name: "db"}
	run := func(deploy string) {
		t.Helper()
		spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "postgres:17", Env: map[string]string{"DEPLOY": deploy},
		}}}
		steps, err := local.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
		if err != nil {
			t.Fatal(err)
		}
		if err := local.Apply(t.Context(), steps[0], spec); err != nil {
			t.Fatal(err)
		}
	}
	onLink := func() []string {
		t.Helper()
		list, err := e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(shared), docker.LabelService + "=db"}})
		if err != nil || len(list) != 1 {
			t.Fatalf("the database's containers: %v, %v", list, err)
		}
		if ep := list[0].NetworkSettings.Networks[link]; ep != nil {
			return ep.Aliases
		}
		return nil
	}
	run("1")
	if got := onLink(); !slices.Contains(got, "db") {
		t.Fatalf("made, the database is on the link network as %v, want db", got)
	}
	run("2")
	if got := onLink(); !slices.Contains(got, "db") {
		t.Errorf("made anew, the database is on the link network as %v, want db", got)
	}
}
